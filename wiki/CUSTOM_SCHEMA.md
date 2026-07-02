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
- The physical table is the pluralized snake_case name suffixed `_log`: `UserPositionSchema` → `user_positions_log` (an append-only history), paired with a `user_positions_live` **view** that resolves to the current row per primary key. Config `state:` entries and `source_table:` still reference the base name (`user_positions`) — the `_log` suffix is added automatically.

### Struct Comments: Primary Key

Use a `pk:` comment to declare the primary key. A complete `custom_schema.go` looks like this (the package name must match your project package):

```go
package myproject

import (
    "github.com/ethereum/go-ethereum/common"
    "github.com/holiman/uint256"
)

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

Primary-key resolution order: the `pk:` comment first; then a matching
config.yaml `state:` entry's `key:`; then a field named `ID`; then the first
non-reserved field; finally `block_number, transaction_index, log_index`. The
`PRIMARY KEY` is always just these pk/sk columns — `block_number`,
`transaction_index`, and `log_index` are added to `ORDER BY` (and the `_live`
view's filter) only, never to the primary key.

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

These order the append-only history and let the `_live` view pick the latest
write per key. Reorgs are handled by deleting `block_number > lastBlock` rows on
reindex, not by engine-level dedup.

## Generated DDL

For each schema struct, codegen produces a plain `MergeTree` `_log` history
table plus a `_live` view:

```sql
CREATE TABLE IF NOT EXISTS <db>.memory_conditions_log (
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
) ENGINE = MergeTree()
PRIMARY KEY (id)
ORDER BY (id, block_number, transaction_index, log_index);

CREATE VIEW IF NOT EXISTS <db>.memory_conditions_live AS
SELECT * FROM <db>.memory_conditions_log
ORDER BY id, block_number DESC, transaction_index DESC, log_index DESC
LIMIT 1 BY id;
```

Query `_live` for current state; query `_log` for history. Periodic compaction
prunes `_log` to one snapshot per `(primary key, intDiv(block_number,
CLICKHOUSE_PRUNE_INTERVAL))` bucket, bounding growth while keeping a
block-bucketed history, and never touches rows within 1000 blocks of the sync
head (so it stays below the finalized head).

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
5. Auto-commits state at the commit interval (`SQD_COMMIT_INTERVAL` blocks / `SQD_COMMIT_MAX_INTERVAL`, whichever first)
6. Auto-prunes ClickHouse at `CLICKHOUSE_PRUNE_INTERVAL` blocks

## Config: `state` Section

To connect a custom schema to the hot state system, declare it in `config.yaml`:

```yaml
state:
  - name: Condition
    source_table: memory_conditions
    key:                          # primary key — used for Get() lookups and ClickHouse ORDER BY
      - ID
  - name: Position
    source_table: memory_user_positions
    key:                          # composite primary key
      - User
      - TokenID
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Entity name (matches schema struct name minus `Schema`) |
| `source_table` | No | Override the table name (default: pluralized+snake_cased) |
| `key` | No | Primary key field(s). Must match the `// pk:` comment on the schema struct. Used for `Get()` lookups, ClickHouse `ORDER BY`, and prefetch queries. |

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
package polymarket

import (
    "github.com/ethereum/go-ethereum/common"
    "github.com/franz101/sqd-go/protomath"
    "github.com/holiman/uint256"
)

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

These produce three ClickHouse history tables (`memory_conditions_log`, `memory_user_positions_log`, `memory_negrisk_events_log`), each with a paired `_live` view and the full snapshot/restore/commit/prune lifecycle. The custom processor (`custom_processor.go`) runs the PnL logic using these in-memory maps and periodically commits to ClickHouse.
