# Code Review: `sqd-go`

This code review is based on the objectives outlined in `TODO.md` and an analysis of the `internal/` implementation.

## 1. TODO.md Status Review

### Module Splits (Completed)
- **`main -> cli`**: Successfully implemented in `internal/cli/cli.go` and `internal/cli/run.go`. The CLI provides a solid, extensible `envio`-style structure.
- **`client -> http -> zstd -> jsonl`**: Handled correctly. `internal/client/client.go` implements HTTP fetching with native zstd decompression.
- **`parser -> abi parser`**: Implemented in `internal/parser/abi.go`. Uses `go-ethereum/accounts/abi` dynamically to parse events and indexed topics based on generated config signatures.
- **`database -> ingestion`**: Implemented in `internal/database/clickhouse.go` and `internal/ingestion/ingestion.go`. ClickHouse schema creation, data insertion via `ch-go` proto columns, and the ingestion loop are properly encapsulated.
- **`templating -> config validation -> codegen`**: Handled well in `internal/codegen/codegen.go`. It effectively outputs the `manifest.json`, ClickHouse schemas (`schema.sql`), ClickHouse Views (`views.sql`), and Go types (`events.go`). The `TODO.md` correctly indicates that `init template` logic is still outstanding.

### CLI Commands (Completed)
- `codegen` (✓)
- `start` (✓)
- `dev` (✓)
- `stop` (✓)
- `init contract-import local` (✓)
- `init template` (Next/TODO)

## 2. Efficiency and Optimization Opportunities

The codebase is functional and cleanly structured. However, as an indexer designed to process potentially millions of events, there are significant performance bottlenecks related to memory allocations and serialization. **Without changing the code yet**, the following areas could be optimized for higher efficiency:

### A. Database Storage & JSON Serialization (Critical)
In `internal/database/clickhouse.go`, the `InsertLogs` method marshals the event parameters (`ev.Params`) into a JSON string for *every single event*:
```go
paramsJSON, _ := json.Marshal(ev.Params)
```
- **Inefficiency:** `json.Marshal` heavily relies on reflection and allocates new memory for every row. Storing a JSON string in a standard ClickHouse `String` column is also inefficient for analytical querying.
- **Optimization:** 
  1. The codegen step should ideally generate explicit native ClickHouse columns for event arguments rather than packing them into a single `String` JSON column.
  2. If JSON is absolutely necessary, using a faster JSON library (like `jsoniter`, `simdjson`, or `sonic`) or manually building the JSON string would dramatically reduce CPU cycles and garbage collection pauses.

### B. Dynamic ABI Unpacking (High)
In `internal/parser/abi.go`, the code uses `e.abi.UnpackIntoMap(result, e.eventName, data)`:
- **Inefficiency:** `UnpackIntoMap` from the `go-ethereum` ABI package uses heavy reflection to map dynamic types into a generic `map[string]any`. It allocates a lot of heap memory.
- **Optimization:** Since `codegen` is already implemented, the event schema is known ahead of time. You could generate specialized Go code that unpacks the ABI binary payloads directly by reading byte offsets (e.g., fast unpackers) instead of relying on runtime reflection.

### C. String Allocations in Deduplication (Medium)
In `internal/parser/abi.go`, functions like `dedupeFilters`, `dedupeStrings`, and `flattenAddresses` convert addresses to lowercase strings (`strings.ToLower`).
- **Inefficiency:** `strings.ToLower` creates a new string allocation every time it's called. For a large number of addresses/filters, this puts pressure on the garbage collector.
- **Optimization:** Perform deduplication using `common.Address` (which is a `[20]byte` array) and `common.Hash` (`[32]byte`) native types. Byte arrays can be used as map keys directly in Go without allocating heap strings.

### D. Sync State Table Growth (Low-Medium)
In `internal/database/clickhouse.go`, the `UpdateSyncState` function executes an `INSERT INTO sync_state`. 
- **Inefficiency:** Since ClickHouse tables (like `MergeTree`) are append-only, this creates multiple rows for the same `chain_id`. The `LastBlock` query runs `SELECT max(last_block)` scanning the entire table. As the indexer runs over time, the `sync_state` table will grow infinitely.
- **Optimization:** Use a `ReplacingMergeTree` engine for the `sync_state` table, or periodically clean up old sync state rows. ClickHouse's `AggregatingMergeTree` could also be utilized to only retain the maximum block automatically.

## Summary
The project successfully achieves its short-term goal of an `envio`-style CLI and functioning pipeline for `examples/uniswap`. To scale and benchmark competitively, the next phase of development should shift focus toward removing reflection, reducing memory allocations (garbage), and shifting from dynamic JSON parameter storage to native column types in ClickHouse.