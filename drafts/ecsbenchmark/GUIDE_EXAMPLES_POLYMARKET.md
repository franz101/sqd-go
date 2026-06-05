# Polymarket ECS Example Structure Guide

This guide explains how to structure a Polymarket simulation using the ECS library.
Source code: `../ecs/`

---

## 1. Directory Layout

```
examples/polymarket/
  main.go                  # Entry point: create world, register components, build scheduler
  component.go             # All component type definitions
  system_matching.go       # OrderMatchingSystem
  system_settlement.go     # SettlementSystem
  system_price_oracle.go   # PriceOracle system
  system_order_inject.go   # Order injection / test data
  event.go                 # Custom event types (OrderFilled, TradeExecuted, etc.)
```

Keep files focused. Components are simple data — one file is fine. Each system gets its own file because systems contain logic.

---

## 2. Component Definitions

Components are just Go structs. They must be **small, value types with no pointers** — the ECS stores components flat in arrays (SoA layout). Pointer fields defeat cache locality.

```go
package main

import "github.com/unitoftime/ecs"

// ---- Core Market Components ----

// Market represents a prediction market with a yes/no outcome.
type Market struct {
    Ticker      string  // e.g. "TRUMP-WIN-2028"
    Description string
    CloseTime   int64   // unix timestamp
    Volume24h   float64
    Liquidity   float64
}

// Outcome ties a market to one of its binary outcomes.
type Outcome struct {
    MarketID ecs.Id   // entity id of the parent Market
    Label    string   // "Yes" or "No"
    PriceNow float64  // current marginal price in $ (0..1)
}

// Price contains raw outcome price info, updated by the matching engine.
// Stored per-outcome entity.
type Price struct {
    BestBid  float64
    BestAsk  float64
    Midpoint float64
    Spread   float64
}

// Volume tracks trade volume per outcome.
type Volume struct {
    TotalShares   float64
    TotalValueUsd float64
}

// ---- Order Components ----

// Order is a single limit order placed by a user.
type Order struct {
    Owner     ecs.Id    // entity id of the user
    OutcomeID ecs.Id    // entity id of the target Outcome
    Side      OrderSide // ecs.Buy or ecs.Sell
    Price     float64   // limit price in $ per share (0..1)
    Quantity  float64   // number of shares
    Filled    float64   // shares already filled
    PlacedAt  int64     // unix timestamp
    Status    OrderStatus
}

type OrderSide uint8
const (
    Buy  OrderSide = iota
    Sell
)

type OrderStatus uint8
const (
    OrderActive    OrderStatus = iota
    OrderFilled
    OrderCancelled
    OrderExpired
)

// ---- Token / Wallet Components ----

// Token is a user's wallet / balance component.
// Attached to user entities. DO NOT use pointers inside components.
type Token struct {
    UsdcBalance float64
    SharesOwned map[ecs.Id]float64  // OutcomeID -> share count
    // Note: map fields are OK for infrequent reads but avoid them in
    // hot-path components that MapId iterates over every frame.
    // Consider a separate flat SharesHeld component for hot paths.
}

// SharesHeld is a flat, hot-path-friendly per-position component.
// Attached to every (user, outcome) pair you want to track fast.
type SharesHeld struct {
    Owner     ecs.Id
    OutcomeID ecs.Id
    Quantity  float64
}

// ---- Book Liquidity (optional, for L2 order book) ----

// BookLiquidity is attached to an Outcome entity and represents
// aggregate depth at a price level. Use multiple entities per outcome
// for each price bucket.
type BookLiquidity struct {
    OutcomeID    ecs.Id
    PriceLevel   float64
    BidVolume    float64
    AskVolume    float64
}
```

### Component Design Best Practices

1. **No pointers in component fields.** Pointers force heap allocations and ruin cache locality. Use value types or ecs.Id to reference other entities.
2. **Small is fast.** If a component has many fields only used occasionally, split it into multiple components. The ECS only iterates the components you query.
3. **Prefer value receivers.** Components are stored flat in `[]T` slices. The `comp[T]` wrapper (`ecs.C(val)`) copies by value.
4. **Use integer IDs for references, not pointers.** Reference other entities with `ecs.Id` (uint32), not `*Entity`.
5. **Constants for enums.** Use `uint8`/`uint16` typed constants for small state enums — they pack better than `string`.
6. **Avoid maps in hot-path components.** `map` fields cause allocations when iterated in `MapIdParallel`. Use a separate flat component (like `SharesHeld`) for bulk operations.

---

## 3. Custom Events

Define events for cross-system communication. Observers react to events.

```go
package main

// OrderFilled fires when a match occurs.
type OrderFilled struct {
    OrderID     ecs.Id
    OutcomeID   ecs.Id
    FilledQty   float64
    FillPrice   float64
    Counterparty ecs.Id
}
var OrderFilledEventId = ecs.NewEvent[OrderFilled]()
func (e OrderFilled) EventId() ecs.EventId { return OrderFilledEventId }

// TradeExecuted fires for settlement after both sides are matched.
type TradeExecuted struct {
    BuyerID   ecs.Id
    SellerID  ecs.Id
    OutcomeID ecs.Id
    Quantity  float64
    Price     float64
}
var TradeExecutedEventId = ecs.NewEvent[TradeExecuted]()
func (e TradeExecuted) EventId() ecs.EventId { return TradeExecutedEventId }

// OutcomeUpdated fires when the price oracle recalibrates.
type OutcomeUpdated struct {
    OutcomeID ecs.Id
    OldPrice  float64
    NewPrice  float64
}
var OutcomeUpdatedEventId = ecs.NewEvent[OutcomeUpdated]()
func (e OutcomeUpdated) EventId() ecs.EventId { return OutcomeUpdatedEventId }
```

---

## 4. Writing Systems

Systems are functions with the signature `func(dt time.Duration, ...injected)`.
The scheduler calls them once per stage tick.

### Option A: Closure-based systems (queries captured at startup)

```go
// system_matching.go
func OrderMatchingSystem_A(world *ecs.World) ecs.System {
    // Create queries ONCE at system construction time.
    orders := ecs.Query2[Order, Token](world)
    outcomes := ecs.Query1[Outcome](world)
    cmd := ecs.NewCommandQueue(world)

    return ecs.NewSystem(func(dt time.Duration) {
        orders.MapId(func(id ecs.Id, order *Order, token *Token) {
            if order.Status != OrderActive {
                return
            }
            // ... match against order book ...
            // Fire event when matched:
            cmd.Trigger(OrderFilled{
                OrderID:   id,
                OutcomeID: order.OutcomeID,
                FilledQty: 10,
                FillPrice: order.Price,
            })
        })
    })
}
```

### Option B: Injected systems (simpler for stateless logic)

```go
func OrderMatchingSystem_B(
    dt      time.Duration,
    query   *ecs.View2[Order, SharesHeld],
    outcome *ecs.View1[Outcome],
    cmd     *ecs.CommandQueue,
) {
    query.MapId(func(id ecs.Id, order *Order, shares *SharesHeld) {
        if order.Status != OrderActive {
            return
        }
        // Matching logic here, use cmd.Trigger() for events
    })
}
```

Use Option A when:
- You need to configure query filters (e.g., `ecs.Without(OrderCancelled{})`)
- The system needs internal mutable state (accumulators, caches)
- You want to reuse queries across multiple lambdas

Use Option B when:
- The system is pure logic with no internal state
- You want the framework to auto-inject dependencies via `GetInjectable`

### OrderMatchingSystem (detailed example)

```go
func OrderMatchingSystem(world *ecs.World) ecs.System {
    // Query for active orders only
    activeOrders := ecs.Query2[Order, Token](world)
    // Snapshot outcomes for price reference
    outcomes := ecs.Query2[Outcome, Price](world)
    cmd := ecs.NewCommandQueue(world)

    return ecs.NewSystem(func(dt time.Duration) {
        // Build an order book from active orders
        // (This is simplified — a real L2 book would track depth)

        activeOrders.MapId(func(id ecs.Id, order *Order, token *Token) {
            if order.Status != OrderActive || order.Filled >= order.Quantity {
                return
            }

            // Find counterparty logic...
            // For simplicity: match against best price

            remaining := order.Quantity - order.Filled
            fillQty := remaining // match full quantity

            order.Filled += fillQty
            if order.Filled >= order.Quantity {
                order.Status = OrderFilled
            }

            // Fire event for settlement
            cmd.Trigger(OrderFilled{
                OrderID:   id,
                OutcomeID: order.OutcomeID,
                FilledQty: fillQty,
                FillPrice: order.Price,
            })
        })
        // cmd.Execute() is called automatically by the scheduler after each system step
    })
}
```

### SettlementSystem (reacts to OrderFilled events)

```go
func SettlementSystem(
    dt      time.Duration,
    query   *ecs.View1[Token],
    query2  *ecs.View1[Order],
    cmd     *ecs.CommandQueue,
) {
    // This system processes OrderFilled events by adjusting token balances.
    // The observer below fires per-event, so this system is lightweight.
    // Actual settlement logic runs inside the observer handler.
}
```

For event-driven settlement, register an observer:

```go
world.AddObserver(
    ecs.NewHandler(func(trigger ecs.Trigger[OrderFilled]) {
        data := trigger.Data
        // Read buyer/seller tokens, adjust balances
        buyerCmd := cmd.Write(data.BuyerID)
        // ... adjust balances ...
    }),
)
```

### PriceOracle system

```go
func PriceOracle(world *ecs.World) ecs.System {
    outcomes := ecs.Query2[Outcome, Price](world)

    return ecs.NewSystem(func(dt time.Duration) {
        outcomes.MapId(func(id ecs.Id, outcome *Outcome, price *Price) {
            // Recalculate midpoint from order book
            price.Midpoint = (price.BestBid + price.BestAsk) / 2.0
            if price.Midpoint > 0 {
                price.Spread = price.BestAsk - price.BestBid
            }
            outcome.PriceNow = price.Midpoint
        })
    })
}
```

---

## 5. Composing Systems with the Scheduler

In `main.go`, create the world and scheduler, then add systems to the right stages.

```go
package main

import (
    "github.com/unitoftime/ecs"
)

func main() {
    world := ecs.NewWorld()

    // ---- Register Observers ----
    cmd := ecs.NewCommandQueue(world)
    world.AddObserver(
        ecs.NewHandler(func(trigger ecs.Trigger[OrderFilled]) {
            // Settlement: transfer tokens, update balances
            data := trigger.Data
            // ... logic ...
        }),
    )

    // ---- Populate test data (optional) ----
    PopulateTestMarkets(world)

    // ---- Build Scheduler ----
    scheduler := ecs.NewScheduler(world)
    scheduler.SetFixedTimeStep(100 * time.Millisecond) // 10 Hz matching

    // StageFixedUpdate: systems that must run on a fixed timestep
    scheduler.AddSystems(ecs.StageFixedUpdate,
        OrderMatchingSystem(world),       // Closure-based
        ecs.NewSystem1(SettlementSystem),  // Injected
        PriceOracle(world),
    )

    // StageUpdate: systems that run every frame (variable dt)
    scheduler.AddSystems(ecs.StageUpdate,
        // UI, metrics collection, logging, etc.
    )

    // Blocking run loop
    scheduler.Run()
}
```

### Stage Reference

| Stage              | dt behavior       | Use case                          |
|--------------------|-------------------|-----------------------------------|
| `StageStartup`     | dt=0 (once)       | Initialize state, spawn entities  |
| `StagePreUpdate`   | variable          | Input handling, pre-physics work  |
| `StageFixedUpdate` | fixed (16ms def.) | Physics, matching, settlement     |
| `StageUpdate`      | variable          | Rendering, UI, logging            |
| `StageLast`        | variable          | Cleanup, post-frame work          |

- `StageFixedUpdate` ticks accumulate. If your matching runs at 100ms, the scheduler ensure exactly that interval regardless of wall-clock jitter.
- `StageUpdate` runs once per tick with the actual elapsed wall-clock dt.

---

## 6. Query Types: MapId vs MapSlices vs MapIdParallel

### MapId — Standard iteration

```go
query.MapId(func(id ecs.Id, order *Order, price *Price) {
    // id: entity identifier
    // order: *Order (mutable pointer to component)
    // price: *Price  (mutable pointer to component)
    order.Status = OrderFilled
})
```

- Sequential, single-threaded.
- Best for systems that mutate shared state or fire commands.
- Returns pointers to components — you can modify them in-place.
- Use when mutation is required and you want simple sequential logic.

**Important:** Calling `ecs.Write()` or `ecs.Delete()` inside `MapId` can invalidate pointers (archetype moves). Use `CommandQueue` instead.

### MapSlices — Batch iteration (deprecated, tentative)

```go
query.MapSlices(func(ids []ecs.Id, orders []Order, prices []Price) {
    for i := range ids {
        orders[i].Status = OrderFilled
    }
})
```

- Passes entire component slices to your lambda.
- One call per archetype (not per entity).
- Useful for bulk operations or SIMD-style processing.
- Marked deprecated — prefer MapId for most cases.

### MapIdParallel — Multi-threaded iteration

```go
query.MapIdParallel(func(id ecs.Id, order *Order, price *Price) {
    order.Filled += 10 // safe if each goroutine owns its entity
})
```

- Splits work across `runtime.NumCPU()` goroutines.
- Uses a work-stealing channel with `sync.WaitGroup`.
- **Only safe when your lambda does NOT mutate shared state** (no writes to other entities, no command queue mutations, no map writes).
- Great for read-only computations across many entities (e.g., aggregating stats with per-goroutine accumulators).
- The template divides work greedily: `totalWork / numThreads` items per channel message.

### Choosing the right query

| Query type      | Threading | Mutable pointers | Use when...                         |
|-----------------|-----------|------------------|-------------------------------------|
| `MapId`         | Single    | Yes              | Mutating components, firing cmds    |
| `MapIdParallel` | Multi     | Yes (per-entity) | Read-only or per-entity-only writes |
| `MapSlices`     | Single    | Yes (slices)     | Batch arithmetic, SIMD-like ops     |

---

## 7. Entity / Component Lifecycle

### Spawning entities

```go
// Direct spawn (immediate, no command queue)
id := world.Spawn(
    ecs.C(Market{Ticker: "TRUMP-2028"}),
    ecs.C(Outcome{Label: "Yes"}),
)

// Via command queue (deferred, batched, safer for loops)
cmd := ecs.NewCommandQueue(world)
cmd.SpawnEmpty().
    Insert(ecs.C(Market{Ticker: "TRUMP-2028"})).
    Insert(ecs.C(Outcome{Label: "Yes"}))
cmd.Execute()
```

`world.Spawn()` is immediate — use it during initialization. `CommandQueue.SpawnEmpty()` is deferred — use it inside systems.

### Writing components to existing entities

```go
// Immediate write
ecs.Write(world, entityId, ecs.C(Price{BestBid: 0.55}))

// Via command queue (safe inside MapId loops)
cmd := cmdQueue.Write(entityId)
cmd.Insert(ecs.C(Price{BestBid: 0.55}))
```

### Deleting entities / components

```go
// Delete entire entity
ecs.Delete(world, entityId)

// Delete specific components
ecs.DeleteComponent(world, entityId, ecs.C(Order{}), ecs.C(Token{}))
```

### OnInsert callback (for components with initialization logic)

If a component type implements `onInsert` (`OnInsert(EntityCommand)`), it runs automatically when added to a bundler via `Insert()`:

```go
type Order struct { ... }
func (o Order) CompId() ecs.CompId { /* required by Component */ }
func (o Order) CompWrite(w ecs.W) { /* required by Writer */ }
func (o Order) OnInsert(ent ecs.EntityCommand) {
    // Initialization when this component is first added to a spawn/write command
    // e.g., validate that OutcomeID exists, set PlacedAt = time.Now()
}
```

---

## 8. Bundlers for Batched Writes

The `Bundler` is the internal mechanism behind `CommandQueue`. You rarely interact with it directly — use `CommandQueue` instead. However, understanding bundlers helps with debugging.

A `Bundler` accumulates components keyed by `CompId`, then writes them all at once when `.Write(world, id)` is called. This enables:
- **Archetype-optimized writes**: All components are known before the archetype move, so the engine makes a single move instead of N moves.
- **Avoiding intermediate archetypes**: Writing components one-by-one creates temporary archetypes. The bundler avoids this.

```go
// User-facing CommandQueue usage (recommended):
cmd := cq.Write(entityId)
cmd.Insert(ecs.C(Price{BestBid: 0.51}))
cmd.Insert(ecs.C(Volume{TotalShares: 1000}))
// Both components are bundled and written in one archetype move.
```

---

## 9. Hooks (onAdd) and Observers (events)

### Hooks: onAdd

Hooks fire when a specific component is **first added** to an entity. Only one hook per component type.

```go
// Register hook: fires whenever a Token component is added to any entity
world.SetHookOnAdd(ecs.C(Token{}),
    ecs.NewHandler(func(trigger ecs.Trigger[ecs.OnAdd]) {
        entityId := trigger.Id
        fmt.Printf("Token added to entity %d\n", entityId)
        // Initialize wallet, log, etc.
    }),
)
```

Hooks are tracked via `archEngine.finalizeOnAdd` — a list of `CompId` that accumulated during the current write/spawn operation. After all writes complete, `runFinalizedHooks(id)` dispatches them.

Use hooks for:
- Initializing secondary systems when a component appears
- Logging / auditing
- Triggering events when state enters a new phase

**Limitation**: Only one hook per component. Multiple handlers per component type are not supported — use events instead.

### Observers: Events

Observers react to explicitly triggered events. Multiple observers can listen to the same event.

```go
// Define event
type OrderFilled struct { OrderID ecs.Id; Qty float64 }
var OrderFilledEventId = ecs.NewEvent[OrderFilled]()
func (e OrderFilled) EventId() ecs.EventId { return OrderFilledEventId }

// Register observer
world.AddObserver(
    ecs.NewHandler(func(trigger ecs.Trigger[OrderFilled]) {
        fmt.Printf("Order %d filled: %f shares\n", trigger.Id, trigger.Data.Qty)
    }),
)

// Trigger event (inside a system, via CommandQueue)
cmd.Trigger(OrderFilled{OrderID: id, Qty: 10})
// Also trigger on specific entity:
cmd.Trigger(OrderFilled{...}, entityId)
```

Events are dispatched during `cmd.Execute()` — the same command queue that processes spawns and writes.

### Hooks vs Observers

| Feature       | Hooks (onAdd)               | Observers (events)            |
|---------------|-----------------------------|-------------------------------|
| Trigger       | Automatic (component added) | Explicit (cmd.Trigger)        |
| Max handlers  | 1 per component             | Unlimited                     |
| Entity ID     | Yes                         | Optional (InvalidEntity if non-entity) |
| Use case      | Lifecycle reactions         | Cross-system communication    |

Best practice: Use hooks for automatic lifecycle reactions (e.g., "when Token is added, log it"). Use observers for all business-logic events (OrderFilled, TradeExecuted, etc.).

---

## 10. Systems with Stages: Complete Example

Here is the full wiring for a Polymarket simulation:

```go
// main.go
func main() {
    world := ecs.NewWorld()

    // ---- Observers (event handlers) ----
    world.AddObserver(
        ecs.NewHandler(func(trigger ecs.Trigger[OrderFilled]) {
            data := trigger.Data
            // Transfer tokens between buyer and seller
            // Could read Token components via world.Read(...)
        }),
    )

    // ---- Hooks ----
    world.SetHookOnAdd(ecs.C(Token{}),
        ecs.NewHandler(func(trigger ecs.Trigger[ecs.OnAdd]) {
            // Initialize empty wallet for new users
        }),
    )

    // ---- Spawn initial markets ----
    PopulateTestMarkets(world)

    // ---- Scheduler Setup ----
    scheduler := ecs.NewScheduler(world)
    scheduler.SetFixedTimeStep(100 * time.Millisecond)
    scheduler.SetMaxPhysicsLoopCount(10) // prevent spiral of death

    // Fixed update systems (run at 100ms fixed intervals)
    scheduler.AddSystems(ecs.StageFixedUpdate,
        OrderMatchingSystem(world),
        PriceOracle(world),
        ecs.NewSystem1(OrderExpirySystem),
    )

    // Variable update systems (run every frame)
    scheduler.AddSystems(ecs.StageUpdate,
        ecs.NewSystem1(MetricsSystem),
        ecs.NewSystem1(LoggingSystem),
    )

    // One-time startup
    scheduler.AddSystems(ecs.StageStartup,
        ecs.NewSystem1(StartupSystem),
    )

    scheduler.Run()
}
```

### System ordering

Systems within a stage execute in the order they were added. To enforce dependencies:
- Place dependent systems in the same stage, ordered correctly.
- Or use events: System A triggers an event, System B (observer) reacts.

Example: `OrderMatchingSystem` runs first (matches orders), then `PriceOracle` runs second (recomputes prices from updated order books).

---

## 11. Filters: Optional, With, Without

Filters refine which entities a query iterates over.

```go
// Query all entities with Order and Token, but WITHOUT OrderCancelled status
// (Note: Without filters on component presence, not field values)
query := ecs.Query2[Order, Token](world,
    ecs.Without(BookLiquidity{}),  // exclude if entity has BookLiquidity
)

// Optional makes a component not required for matching
query := ecs.Query3[Order, Token, Price](world,
    ecs.Optional(Price{}), // iterate even without Price; *Price will be nil
)

// With adds additional required components beyond the query generics
query := ecs.Query1[Order](world,
    ecs.With(SharesHeld{}), // also require SharesHeld
)
```

---

## 12. Performance Notes for Polymarket

1. **Split hot and cold data.** Order matching iterates every active order. Don't attach debug metadata (log strings, timestamps) to the `Order` component — use a separate component queried only in debug systems.

2. **Use `MapIdParallel` for read-only aggregation.** If computing total volume or top prices, use parallel.

3. **Command queues are your friend.** Never call `ecs.Write()` or `ecs.Delete()` inside `MapId` unless you understand archetype move semantics. Always use `CommandQueue`.

4. **Cleanup holes periodically.** If you delete many entities (e.g., filled/cancelled orders), call `world.CleanupHoles()` periodically to repack arrays.

5. **Pre-allocate query objects at system construction time.** Don't create new queries inside the system function — the `ecs.QueryN()` call registers component storages and builds filter lists.

6. **Max components: 255.** The archetype mask uses 4x uint64 = 256 bits (index 0-255). Component IDs start at 1 (0 is invalid). If you need more, increase `numMaskBlocks` in `mask.go`.

---

## 13. Component Registration and ID Assignment

Components are registered automatically the first time you call `ecs.C(val)` or any `ecs.QueryN()`:

```go
ecs.C(Market{})  // registers Market -> CompId 1
ecs.C(Order{})   // registers Order  -> CompId 2
```

The `name()` function in `name.go` uses `reflect.TypeOf(t)` to create a unique `CompId`. The registry is global (shared across worlds). IDs are assigned sequentially starting at 1.

To inspect a component's ID:
```go
comp := ecs.Comp(Order{})
fmt.Println(comp.CompId()) // prints 2 (or whatever)
```
