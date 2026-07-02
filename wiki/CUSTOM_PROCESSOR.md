# Custom Processor

The custom processor is your hook into the ingestion pipeline. It receives decoded blockchain events grouped by block and lets you run arbitrary business logic — balance tracking, PnL calculation, derived state, cross-event lookups — all in Go, backed by auto-generated ClickHouse persistence and snapshot-based fork recovery.

## Architecture

```
SQD JSONL → Parse → Decode → Insert typed event tables
                                  ↓
                            Decoded Logs
                                  ↓
                          Ring Buffer Push
                                  ↓
                      ParsedBlock (per block)
                                  ↓
                      CustomProcessFn(state, block)  ← YOUR CODE
                                  ↓
                      SaveSnapshot / Commit / Prune
```

Two types of custom processor exist, selected by codegen based on whether custom schema tables are present.

### Without Custom Schema (HotStateTables == 0)

Simplest path. Generated `custom_processor.go` provides:

```go
package generated

import "context"

func CustomProcessing(ctx context.Context, store Store, entities *Entities) error {
    // entities has typed slices: entities.Transfer, entities.Approval, etc.
    return nil
}
```

You get all decoded events for the batch, grouped by type. No state management, no snapshots. Write your logic directly inside `CustomProcessing`.

### With Custom Schema (HotStateTables > 0)

Full-featured path with state management, ring buffers, snapshots, and fork recovery. Generated `custom_processor.go` provides the framework; you define your logic in a separate project-level `custom_processor.go`.

## Project-Level `custom_processor.go`

Place this in your project root (next to `config.yaml`). It registers with the CLI and defines your event handling logic.

> **Import the public `sqd` facade, never `internal/`.** Your project builds as its own module, so it must import the public `sqd` facade, never `internal/`. `custom_schema.go` and `custom_processor.go` must share ONE package name. Register under `generated.ProjectName` (not a hardcoded string) so the name always matches your config.

### Minimal Template

```go
package myproject // SAME package name as custom_schema.go

import (
    generated "myproject/generated" // your module-relative generated package

    "github.com/franz101/sqd-go/sqd" // PUBLIC facade — never import internal/
)

// Process is called once per block with all decoded events in the parsed block.
func Process(state *generated.State, block *generated.ParsedBlock) error {
    for ev := range block.EventsIter() {
        switch e := ev.(type) {
        case *generated.ERC20Transfer:
            _ = e // handle Transfer event: e.From, e.To, e.Value
        case *generated.ERC20Approval:
            _ = e // handle Approval event: e.Owner, e.Spender, e.Value
        }
    }
    return nil
}

func ProcessProto(state *generated.State, block *generated.ProtoEventBlock) error {
    return Process(state, block.ToParsedBlock())
}

// init registers the processor so the CLI can find it by project name.
func init() {
    generated.CustomProcessFn = Process
    generated.CustomProcessProtoFn = ProcessProto
    sqd.RegisterProcessor(generated.ProjectName, func() (sqd.Processor, error) {
        return generated.NewProcessor(sqd.GetProtoMode())
    })
}
```

### Registration Flow

1. `init()` runs at program startup (before `main`)
2. `sqd.RegisterProcessor(generated.ProjectName, factory)` stores the factory in a global `sync.Map`
3. When `sqd-go start myproject/` runs, `processorForProject("myproject")` looks up the factory and calls it
4. The returned value satisfies `sqd.Processor` (a public alias for the ingestion processor interface). You never implement this interface yourself — `generated.NewProcessor` returns the generated implementation, and your business logic lives in the `Process` function wired up via `generated.CustomProcessFn`.

### Generated `Processor.Process()` Internals

The generated processor:

1. Decodes each `CustomLog` via `UnpackLogWithMeta()` — hex-decodes log data, matches topic0 to event type, returns `*DecodedLog`
2. Groups decoded logs by block number
3. Pushes each block's events into the `OrderedHistoricRingBuffer`
4. Retrieves `ParsedBlock` which wraps decoded events with metadata
5. Batches blocks in groups of 8 for prefetch (loads existing state from ClickHouse)
6. Calls `CustomProcessFn` or `CustomProcessProtoFn` for each block
7. At the commit interval (`SQD_COMMIT_INTERVAL` blocks, default 5000, **or** `SQD_COMMIT_MAX_INTERVAL`, default 3s — whichever first): saves a snapshot and commits hot state to ClickHouse. Pruning runs on its own separate cadence (`CLICKHOUSE_PRUNE_INTERVAL` blocks).

### `ParsedBlock.EventsIter()`

Instead of a rigid multi-argument callback method, the `ParsedBlock` type exposes a push iterator method `EventsIter()` returning `iter.Seq[Event]` (introduced in Go 1.23):

```go
type Event interface {
    Meta() EventMeta
}

func (b *ParsedBlock) EventsIter() iter.Seq[Event]
```

This allows type-safe, standard Go range loops over events in their exact sequence order. This gives you type-safe access to event fields without manual type assertions.

### Working with `State`

When custom schema tables are defined, `state` provides:

```go
type State struct {
    HotState       *HotState              // CLOCK cache maps
    Store          Store                  // ClickHouse native connection
    // Per-entity handles:
    Conditions     entityStateHandle[MemoryCondition]
    UserPositions  entityStateHandle[MemoryUserPosition]
    // ...
}
```

**Reading state:**

```go
pos, ok := state.UserPositions.Get(key)
if !ok {
    pos = &MemoryUserPosition{User: user, TokenID: tokenID}
}
```

**Writing state:**

```go
pos.Balance.Add(&pos.Balance, &amount) // uint256.Int mutates in place
state.UserPositions.Save(pos, eventMeta)
```

The entity state handle (`entityStateHandle[T]`) wraps the hot state map and provides `Get`, `Set`, and `Save` methods. `Save` marks the entity dirty for the next commit cycle.

**Reading existing state from ClickHouse (prefetch):**

The prefetch system automatically queries ClickHouse for entity state matching keys in the current block's events. For example, if a block contains `Transfer(user=0xABC, token=0xDEF)`, the prefetch query loads the latest existing `user_positions_log` row for `(0xABC, 0xDEF)` before `CustomProcessFn` runs. This means you can always assume the latest state is loaded.

### Snapshots and Fork Recovery

The `State` maintains a 32-slot ring buffer of deep-copied snapshots, one taken at each commit (see the **Commit interval** section below). On fork recovery:

1. `RestoreToBlock(safeBlock)` finds the newest snapshot ≤ `safeBlock`
2. All entity maps are restored from the snapshot
3. Snapshots above `safeBlock` are purged
4. Ring buffer events above `safeBlock` are replayed through `CustomProcessFn`

This avoids re-fetching from the network — the ring buffer holds the last N blocks of decoded events.

### Commit interval (when `Save()` becomes durable)

`Save()` does **not** write to ClickHouse on the spot. It updates the in-memory hot-state
cache (a CLOCK cache per entity) and marks the entity dirty. The runtime then commits the
accumulated dirty state to ClickHouse on a **hybrid cadence** — it commits whenever *either*
bound below is reached, whichever comes first:

| Bound | Env var | Default | Role |
|-------|---------|---------|------|
| Blocks since last commit | `SQD_COMMIT_INTERVAL` | `5000` | The **crash re-fetch budget**: on a crash, at most this many blocks are re-fetched and re-processed from the last durable checkpoint. Dominates during backfill. |
| Wall-clock since last commit | `SQD_COMMIT_MAX_INTERVAL` | `3s` | The **durability-latency bound**: keeps the slow live tail (a few blocks/sec) becoming durable promptly instead of waiting `SQD_COMMIT_INTERVAL` blocks (which at the head could be ~30 min). Dominates at the chain head. |

If you come from Subsquid's `processor.run()`, this is the framework-managed equivalent of
its end-of-batch `ctx.store.save([...])`: there you accumulate into maps across `ctx.blocks`
and flush once per batch; here your per-block `Save()` accumulates in the hot cache and the
runtime flushes on the interval above. You never write the commit yourself.

The commit runs **asynchronously, off the block-processing hot path** (on a dedicated
ClickHouse connection), so a multi-second flush doesn't stall parsing. Each commit appends the
dirty rows to the per-entity `_log` history table via the ClickHouse native protocol (ch-go)
with `async_insert=1, wait_for_async_insert=0` for throughput:

```go
INSERT INTO <db>.user_positions_log (address, total_in, total_out, ...) VALUES ...
```

**The checkpoint never leads the durable horizon.** The ingestion checkpoint only advances to
the highest block whose derived state has actually committed (`CommittedBlock()`), so a crash
resumes from the last commit and re-fetches the bounded gap — un-committed `Save()`s are never
silently lost. On a clean `--end-block` exit the runtime forces a final flush of the tail.

## Real-World Example: Uniswap PnL Transfer Tracker

```go
package uniswap

import (
    "github.com/ethereum/go-ethereum/common"
    generated "github.com/franz101/sqd-go/examples/uniswap/generated"
)

func Process(state *generated.State, block *generated.ParsedBlock) error {
    for ev := range block.EventsIter() {
        switch e := ev.(type) {
        case *generated.LBTCTransfer:
            zero := common.Address{}
            if e.From != zero {
                pos, ok := state.UserPosition.Get(e.From)
                if !ok {
                    pos = &generated.UserPosition{Address: e.From}
                }
                pos.Balance.Sub(&pos.Balance, &e.Value)
                pos.TotalOut.Add(&pos.TotalOut, &e.Value)
                pos.UpdatedAtBlock = e.EventMeta.BlockNumber
                pos.UpdatedAt = e.EventMeta.BlockTimestamp
                state.UserPosition.Save(pos, e.EventMeta)
            }
            if e.To != zero {
                pos, ok := state.UserPosition.Get(e.To)
                if !ok {
                    pos = &generated.UserPosition{Address: e.To}
                }
                pos.Balance.Add(&pos.Balance, &e.Value)
                pos.TotalIn.Add(&pos.TotalIn, &e.Value)
                pos.UpdatedAtBlock = e.EventMeta.BlockNumber
                pos.UpdatedAt = e.EventMeta.BlockTimestamp
                state.UserPosition.Save(pos, e.EventMeta)
            }
        }
    }
    return nil
}
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SQD_COMMIT_INTERVAL` | `5000` | Blocks between durable commits of derived state (the crash re-fetch budget). Commit fires on this **or** `SQD_COMMIT_MAX_INTERVAL`, whichever first. |
| `SQD_COMMIT_MAX_INTERVAL` | `3s` | Max wall-clock between commits (Go duration or bare seconds); keeps the slow live tail durable without waiting `SQD_COMMIT_INTERVAL` blocks. |
| `CLICKHOUSE_PRUNE_INTERVAL` | `100000` | Blocks between compaction prune cycles |
| `CLICKHOUSE_HTTP_PORT` | `8123` | Port for `LoadFromDatabase` HTTP queries |
| `SQD_STATE_CACHE_CAPACITY` | `100000` | The maximum number of entries to keep in the in-memory hot CLOCK cache per entity |
| `SQD_DEBUG_STATE_PRUNE` | `false` | Enable detailed state pruning debug logging |

## Recent Improvements (2026)

### Collateral Validation

Polymarket processors now include collateral validation guards to prevent scaling errors:

```go
// Whitelist-supported collaterals
supportedCollateral = [...]common.Address{
    common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"), // bridged USDC (6 dec)
    common.HexToAddress("0x3c499c542cEF5E3811e1192ce70D8cC03d5c3359"), // native USDC (6 dec)
    common.HexToAddress("0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB"), // pUSD
}

func isSupportedCollateral(addr common.Address) bool {
    // Check if address is in supported list
}
```

This prevents events like CTF splits with unsupported collateral (e.g., WMATIC) from being processed with incorrect decimal scaling.

### State Pruning Improvements

- **Bounded Memory** - State pruning is now windowed to prevent ClickHouse OOM during mutations
- **Disk Spillover** - Large aggregation operations can spill to disk to manage memory
- **Provisional Checkpoints** - Checkpoint persistence at reindex floor for safer recovery

### Performance Optimizations

- **Zero-Allocation Paths** - Hot rescale functions (`usdcRawToDec18`, `rawIntToDec18`) are verified zero-allocation
- **MinLZ Compression** - Cold cache profile uses MinLZ compression for better memory efficiency
- **Fast-Path Improvements** - Optimized topic0 string handling in parser

## Without Custom Schema (Legacy Pattern)

If you don't need persistent state, fork recovery, or snapshots, you can use the simpler path. The generated `CustomProcessing` function receives `entities *Entities` with all decoded events pre-grouped:

```go
package generated

import "context"

func CustomProcessing(ctx context.Context, store Store, entities *Entities) error {
    for _, ev := range entities.ERC20Transfer {
        _ = ev // process Transfer events
    }
    for _, ev := range entities.ERC20Approval {
        _ = ev // process Approval events
    }
    return nil
}
```

No state management, no `init()` registration needed — just replace the body of the codegen-generated `CustomProcessing` in `generated/custom_processor.go` (or write your own in the project root, which takes precedence).

## Pitfalls

1. **Numeric types**: Use `uint256.Int` for amounts (not `big.Int`). Use `decimal.Decimal` for prices/USD. Never pass raw `float64` through arithmetic.
2. **EventMeta**: Always pass `ev.EventMeta` to `Save()` — it carries block number, timestamp, transaction hash, and log index needed for ClickHouse ordering.
3. **State reads are cached**: The prefetch loads state from ClickHouse before `Process` runs. Reads are from the in-memory CLOCK cache. Don't query ClickHouse directly inside `Process`.
4. **Concurrency**: `Process` is called sequentially per block. The state lock (`sync.RWMutex`) is held during snapshot/commit. Your code runs under the read lock during `CustomProcessFn`.
5. **init() order**: The `init()` in your `custom_processor.go` must register the processor before CLI dispatch. Since Go runs all `init()` functions before `main`, this is guaranteed as long as your package is imported (e.g., via `_ "<module>/myproject"` in the main binary).
