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

Use a `pk:` comment to declare the primary key:

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

For each schema struct, codegen produces:

```sql
CREATE TABLE IF NOT EXISTS <db>.user_positions (
  id FixedString(32),
  oracle FixedString(20),
  question_id FixedString(32),
  outcome_slot_count UInt8,
  resolved UInt8,
  updated_at_block UInt64,
  updated_at DateTime64(3, 'UTC') DEFAULT now64(3),
  block_number UInt64,
  transaction_index UInt64,
  log_index UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (id)
ORDER BY (id, block_number, transaction_index, log_index);
```

Output paths:
- `.sqd/generated/custom_schema.sql` — canonical
- `generated/custom_schema.sql` — copy in Go output dir

## Generated Go Code

Codegen produces a complete state management layer:

### `generated/hotstate.go`

CLOCK cache-backed maps for each entity:

```go
type HotState struct {
    Conditions  *clock.CLock[string, MemoryCondition]
    UserPositions *clock.CLock[userPositionKey, MemoryUserPosition]
    // ... one per schema type
}
```

The CLOCK cache provides O(1) amortized lookups with cache-friendly eviction when memory pressure builds. The `HotState` is embedded in `State` and auto-persisted to ClickHouse at snapshot intervals.

### `generated/state.go`

The `State` struct ties everything together:

```go
type State struct {
    HotState       *HotState
    Store          Store
    Conditions     entityStateHandle[MemoryCondition]
    UserPositions  entityStateHandle[MemoryUserPosition]
    mu             sync.RWMutex
    LastSyncBlock  uint64
    LastPruneBlock uint64
    snapshots      []memorySnapshot
    snapshotIdx    int
}
```

Key methods:
- `SaveSnapshot(blockNumber)` — deep-copies all entity maps into a ring buffer (32 slots)
- `RestoreToBlock(blockNumber)` — finds nearest snapshot ≤ blockNumber, restores maps, purges later snapshots
- `Commit(ctx, store)` — flushes dirty entities to ClickHouse via native protocol
- `LoadFromClickHouse(ctx, httpPort, blockNumber)` — restores from ClickHouse at startup (if `LoadStateFromClickHouseFn` is set)

### `generated/compaction.go`

Per-entity ClickHouse DELETE queries for pruning old rows. Uses the `block_number` column to remove duplicates and stale entries. Generated functions like `CompactionPruneState()` handle the full cycle.

### `generated/custom_processor.go`

If codegen detects hot state tables, it generates a `Processor` struct that:
1. Maintains an `OrderedHistoricRingBuffer` for fork recovery
2. Decodes raw logs into typed events via `UnpackLogWithMeta`
3. Groups events by block, pushes to ring buffer
4. Calls `CustomProcessFn` (your user-defined function) per block slot
5. Auto-commits state at `STATE_SNAPSHOT_INTERVAL` blocks
6. Auto-prunes ClickHouse at `CLICKHOUSE_PRUNE_INTERVAL` blocks

## Config: `state` Section

To connect a custom schema to the hot state system, declare it in `config.yaml`:

```yaml
state:
  - name: Conditions
    source_table: memory_conditions
    mode: hotstate
  - name: UserPositions
    source_table: memory_user_positions
    mode: hotstate
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Entity name (matches schema struct name minus `Schema`) |
| `source_table` | No | Override the table name (default: pluralized+snake_cased) |
| `mode` | No | `hotstate` or `db_prefetch` (default: `hotstate`) |
| `key` | No | Override prefetch key fields |

The `state` config drives:
- Prefetch queries (load existing state from ClickHouse before processing)
- State handle generation in `state.go`
- Ring buffer wiring in `custom_processor.go`

## Where to Place `custom_schema.go`

Codegen searches these paths in order (first found wins):

1. `<project-root>/custom_schema.go`
2. `<project-root>/generated/custom_schema.go`
3. `<config-dir>/custom_schema.go`
4. `<config-dir>/generated/custom_schema.go`

This allows placing the schema either next to config.yaml or in the project root.

## Real-World Example: Polymarket

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

These produce three ClickHouse tables (`memory_conditions`, `memory_user_positions`, `memory_negrisk_events`) with full snapshot/restore/commit/prune lifecycle. The custom processor (`custom_processor.go`) runs the PnL logic using these in-memory maps and periodically commits to ClickHouse.
