# Custom Schema

Custom schemas let you define additional ClickHouse tables alongside the auto-generated event tables. These tables are backed by in-memory state with CLOCK cache eviction, snapshot-based fork recovery, and automatic ClickHouse persistence.

## Overview

The pipeline has two types of custom tables:

| Source | File | Purpose |
|--------|------|---------|
| **custom_types.go** | Project root | Legacy custom tables (event-level, older pattern) |
| **custom_schema.go** | Project root, `generated/`, or config dir | Hot-state tables with memory snapshots, prefetch, and fork recovery |

`custom_schema.go` is the modern path. It defines Go structs that the codegen parses into ClickHouse DDL, Go state maps, snapshot logic, compaction queries, and a CLOCK cache-backed `HotState`.

## Defining a Schema

Create `custom_schema.go` next to your `config.yaml`. Struct naming convention:

- Name MUST end in `Schema` (e.g., `UserPositionSchema`, `MemoryConditionSchema`)
- The struct name minus `Schema` becomes the entity name: `UserPositionSchema` → `UserPosition`
- Table name is the pluralized snake_case: `UserPositionSchema` → `user_positions`

### Struct Comments: Primary Key

Use a `pk:` comment to declare the primary key. This is the complete `custom_schema.go` backing the Uniswap PnL transfer tracker from [CUSTOM_PROCESSOR.md](CUSTOM_PROCESSOR.md) (see `examples/uniswap_pnl/`):

```go
package uniswap_pnl

import (
    "time"

    "github.com/ethereum/go-ethereum/common"
    "github.com/holiman/uint256"
)

// pk: Address
type UserPositionSchema struct {
    Address        common.Address
    Balance        uint256.Int
    TotalIn        uint256.Int
    TotalOut       uint256.Int
    UpdatedAtBlock uint64
    UpdatedAt      time.Time
}
```

If no `pk:` comment, the codegen infers it: field named `ID` wins, then the first non-reserved field, then falls back to `block_number, transaction_index, log_index`.

### Supported Go Types → ClickHouse Types

| Go Type | ClickHouse Type |
|---------|----------------|
| `bool` | `UInt8` |
| `uint8`, `uint16`, `uint32`, `uint64`, `uint` | `UInt8`–`UInt64` |
| `int8`, `int16`, `int32`, `int64`, `int` | `Int8`–`Int64` |
| `float32`, `float64` | `Float32`, `Float64` |
| `string` | `String` |
| `time.Time` | `DateTime64(3, 'UTC')` (default `now64(3)`) |
| `common.Address` | `FixedString(20)` |
| `common.Hash` | `FixedString(32)` |
| `uint256.Int` | `UInt256` |
| `decimal.Decimal` | `Decimal(38, 18)` |
| `protomath.Decimal256` | `Decimal256(18)` |
| `[]byte` | `String` |
| `[]T` | `Array(T)` |
| Unknown | `String` |

### Required Fields (Auto-Added)

Three fields are appended to every custom table automatically:

```
block_number UInt64
transaction_index UInt64
log_index UInt64
```

These enable the `ReplacingMergeTree(block_number)` engine to deduplicate rows from reorgs.

## Generated DDL

For each schema struct, codegen produces (this is the actual output for `UserPositionSchema` above):

```sql
CREATE TABLE IF NOT EXISTS <db>.user_positions (
  address FixedString(20),
  balance UInt256,
  total_in UInt256,
  total_out UInt256,
  updated_at_block UInt64,
  updated_at DateTime64(3, 'UTC') DEFAULT now64(3),
  block_number UInt64,
  transaction_index UInt64,
  log_index UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (address)
ORDER BY (address, block_number, transaction_index, log_index);
```

Output paths:
- `.sqd/generated/custom_schema.sql` — canonical
- `generated/custom_schema.sql` — copy in Go output dir

## Generated Go Code

Codegen produces a complete state management layer:

### `generated/hotstate.go`

CLOCK cache-backed maps for each entity (plural names):

```go
type HotState struct {
    UserPositions         *UserPositionsClockCache
    UserPositionsResolver *UserPositionBatchResolver
    dirtyUserPositions    map[UserPositionsClockKey]struct{}
    // ... one cache/resolver/dirty-set per schema type
}
```

The CLOCK cache provides O(1) amortized lookups with cache-friendly eviction when memory pressure builds. The `HotState` is embedded in `State` and auto-persisted to ClickHouse at commit intervals.

### `generated/state.go`

The `State` struct ties everything together. Each entity gets a **singular** typed handle — this is what you use inside your `Process` function (`state.UserPosition.Get(...)`, not `state.UserPositions`):

```go
type State struct {
    HotState       *HotState
    Store          Store
    UserPosition   UserPositionState  // one handle per entity, named after the entity
    mu             sync.RWMutex
    LastSyncBlock  uint64
    LastPruneBlock uint64
    snapshots      []memorySnapshot
    snapshotIdx    int
}
```

Key methods:
- `SaveSnapshot(blockNumber)` — deep-copies all entity maps into a ring buffer (128 slots)
- `RestoreToBlock(blockNumber)` — finds nearest snapshot ≤ blockNumber, restores maps, purges later snapshots
- `Commit(ctx, store)` — flushes dirty entities to ClickHouse via native protocol
- `LoadFromClickHouse(ctx, blockNumber)` — restores from ClickHouse at startup

### `generated/compaction.go`

Per-entity ClickHouse DELETE queries for pruning old rows. Uses the `block_number` column to remove duplicates and stale entries. Generated functions like `CompactionPruneState()` handle the full cycle.

### `generated/custom_processor.go`

If codegen detects hot state tables, it generates a `Processor` struct that:
1. Maintains an `OrderedHistoricRingBuffer` for fork recovery
2. Decodes raw logs into typed events via `UnpackLogWithMeta`
3. Groups events by block, pushes to ring buffer
4. Calls `CustomProcessFn` (your user-defined function) per block slot
5. Auto-commits state on a hybrid cadence: every `SQD_COMMIT_INTERVAL` blocks (default 20000) or `SQD_COMMIT_MAX_INTERVAL` of wall time (default 3s)
6. Auto-prunes ClickHouse at `CLICKHOUSE_PRUNE_INTERVAL` blocks (default 100000)

## Config: `state` Section (Optional)

Every struct in `custom_schema.go` automatically becomes a hot-state table with a generated state handle — no config needed. The Uniswap example above works with a `config.yaml` that has no `state:` section at all.

The handle name is the entity name with a leading `Memory` prefix stripped: `UserPositionSchema` → `state.UserPosition`, `MemoryConditionSchema` → `state.Condition`.

Use the `state:` section when you want to override defaults (table name, prefetch keys, handle name) or to back a state entity by an existing *event* table instead of a schema struct:

```yaml
state:
  - name: Condition
    source_table: memory_conditions
    key:
      - ID
    mode: hotstate
  - name: Position
    source_table: memory_user_positions
    key:
      - User
      - TokenID
    mode: hotstate
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Entity/handle name (e.g. `Condition` for `MemoryConditionSchema`) |
| `source_table` | No | Override the table name (default: pluralized+snake_cased) |
| `mode` | No | `hotstate` or `db_prefetch` (default: `hotstate`) |
| `key` | No | Override prefetch key fields |

The `state` config overrides:
- Prefetch queries (load existing state from ClickHouse before processing)
- State handle naming in `state.go`
- Ring buffer wiring in `custom_processor.go`

## Where to Place `custom_schema.go`

Codegen searches these paths in order (first found wins):

1. `<project-root>/custom_schema.go`
2. `<project-root>/generated/custom_schema.go`
3. `<config-dir>/custom_schema.go`
4. `<config-dir>/generated/custom_schema.go`

This allows placing the schema either next to config.yaml or in the project root.

## Multi-Entity Example: Polymarket

A larger schema with several entities (see `examples/polymarket/`). Note the `Memory` prefix — it is stripped from the generated handle names (`state.Condition`, `state.UserPosition`, `state.NegRiskEvent`):

```go
// pk: ID
type MemoryConditionSchema struct {
    ID               common.Hash
    Oracle           common.Address
    QuestionID       common.Hash
    OutcomeSlotCount uint8
    Resolved         bool
    Payouts          []uint256.Int
}

// pk: User, TokenID
type MemoryUserPositionSchema struct {
    User           common.Address
    TokenID        common.Hash
    Amount         protomath.Decimal256
    AvgPrice       protomath.Decimal256
    RealizedPnL    protomath.Decimal256
    TotalBought    protomath.Decimal256
}

// pk: ID
type MemoryNegRiskEventSchema struct {
    ID             common.Hash
    QuestionCount  uint32
    QuestionIDs    []common.Hash
}
```

These produce three ClickHouse tables (`memory_conditions`, `memory_user_positions`, `memory_neg_risk_events`) with full snapshot/restore/commit/prune lifecycle. The custom processor (`custom_processor.go`) runs the PnL logic using these in-memory maps and periodically commits to ClickHouse.
