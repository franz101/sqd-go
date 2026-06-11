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

### Minimal Template

```go
package myproject

import (
    generated "<module>/myproject/generated"
    "github.com/franz101/sqd-go/internal/cli"
    "github.com/franz101/sqd-go/internal/ingestion"
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

// init registers the processor so the CLI can find it by project name.
func init() {
    generated.CustomProcessFn = Process
    cli.RegisterProcessor(generated.ProjectName, func() (ingestion.Processor, error) {
        return generated.NewProcessor(cli.GetProtoMode())
    })
}
```

### Registration Flow

1. `init()` runs at program startup (before `main`)
2. `cli.RegisterProcessor("myproject", factory)` stores the factory in a global `sync.Map`
3. When `sqd-go start myproject/` runs, `processorForProject("myproject")` looks up the factory and calls it
4. The returned `Processor` implements `ingestion.Processor` interface:

```go
type Processor interface {
    Process(ctx context.Context, store *database.Store, logs []CustomLog) error
    RestoreToBlock(blockNumber uint64) error
    LoadFromDatabase(blockNumber uint64) error
}
```

### Generated `Processor.Process()` Internals

The generated processor:

1. Decodes each `CustomLog` via `UnpackLogWithMeta()` — hex-decodes log data, matches topic0 to event type, returns `*DecodedLog`
2. Groups decoded logs by block number
3. Pushes each block's events into the `OrderedHistoricRingBuffer`
4. Retrieves `ParsedBlock` which wraps decoded events with metadata
5. Batches blocks in groups of 8 for prefetch (loads existing state from ClickHouse)
6. Calls `CustomProcessFn(state, block)` for each block
7. At `STATE_SNAPSHOT_INTERVAL` (default 4000) blocks: saves snapshot, commits hot state to ClickHouse, prunes old rows

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

The prefetch system automatically queries ClickHouse for entity state matching keys in the current block's events. For example, if a block contains `Transfer(user=0xABC, token=0xDEF)`, the prefetch query loads the existing `user_positions` row for `(0xABC, 0xDEF)` before `CustomProcessFn` runs. This means you can always assume the latest state is loaded.

### Snapshots and Fork Recovery

The `State` maintains a 32-slot ring buffer of deep-copied snapshots, taken every `STATE_SNAPSHOT_INTERVAL` blocks. On fork recovery:

1. `RestoreToBlock(safeBlock)` finds the newest snapshot ≤ `safeBlock`
2. All entity maps are restored from the snapshot
3. Snapshots above `safeBlock` are purged
4. Ring buffer events above `safeBlock` are replayed through `CustomProcessFn`

This avoids re-fetching from the network — the ring buffer holds the last N blocks of decoded events.

### Commit and Persistence

At snapshot intervals, `State.Commit()` flushes all dirty entities to ClickHouse:

```go
// Generated per-entity INSERT:
INSERT INTO <db>.user_positions (user, token_id, amount, ...) VALUES
// Uses ClickHouse native protocol (ch-go) with async inserts
```

Settings: `async_insert=1`, `wait_for_async_insert=0` for maximum throughput.

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
| `STATE_SNAPSHOT_INTERVAL` | `4000` | Blocks between state snapshots/commits |
| `CLICKHOUSE_PRUNE_INTERVAL` | `100000` | Blocks between compaction prune cycles |
| `CLICKHOUSE_HTTP_PORT` | `8123` | Port for `LoadFromDatabase` HTTP queries |

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
