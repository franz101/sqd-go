# Review 2: `TODO.md`, `sqd-go-cli`, and `sqd-go`

Scope: review `sqd-go/TODO.md` against the current `sqd-go` implementation, compare it with `/home/dev/CODING/polymarket_lowram/sqd-go-cli`, and recommend where to go next. This is a review only; no code changes are proposed here.

## Summary

`sqd-go` is the better foundation for the product shape: config-first projects, `codegen/start/dev/stop`, generated SQL/views, generated Go metadata, cursor-style SQD fetching, `chain_id` in storage, `start_block`/`end_block`, and a cleaner package split.

`sqd-go-cli` is the better reference for the hard performance path: ABI-driven generated decoders, generated per-event ClickHouse tables and batches, richer parser benchmarks, generated-code tests, and a more serious low-allocation direction.

The right path is not to go back to `sqd-go-cli`. Keep the `sqd-go` CLI/config/runtime shape, then port the specific proven pieces from `sqd-go-cli`: generated direct ABI unpackers, typed per-event ClickHouse batches, parser/decompression reuse, and the stronger test/benchmark harness.

## TODO.md Status

The TODO is broadly accurate:

- The module split is done: CLI, client, parser, database, ingestion, config, codegen.
- `codegen`, `start`, `dev`, `stop`, and `init contract-import local` exist.
- `codegen` now emits `manifest.json`, `schema.sql`, `views.sql`, and Go files.
- `examples/uniswap` is wired as the working LBTC-style example.
- `init template`, formal templating, proper packaging, and full multi-chain maturity are still real next items.

One wording issue: `multi-contract, multi-chain support (already in config schema)` should not be treated as runtime-complete. The schema supports it, but `chainEndpoint` is hard-coded to Ethereum and Polygon in `internal/ingestion/ingestion.go`, and per-chain errors are logged but do not fail the run. The TODO should distinguish "schema supports it" from "runtime hardened".

## Better In `sqd-go-cli`

1. Generated event decoding is much stronger.

`sqd-go-cli/generator.go` generates `UnpackLog`, address/topic dispatch, and event-specific unpack functions. This is the main piece v2 still needs. The generated code switches on known contract addresses and topic hashes, then decodes directly into typed structs instead of going through a generic `map[string]any`.

In contrast, v2 generates event structs and constants, but ingestion does not use those generated structs. Runtime decoding still goes through `internal/parser/abi.go` and produces `parser.DecodedEvent` with `Params map[string]any`.

2. Generated ClickHouse writes are closer to the final architecture.

`sqd-go-cli` generates per-event ClickHouse tables and per-event `proto` column batches. That is better than v2's current base `logs` table with JSON `params` and views that call `JSONExtract...` at query time.

`sqd-go`'s current approach is useful for getting an end-to-end pipeline working, but it pays decode/JSON cost during ingestion and query-time JSON extraction cost in ClickHouse.

3. Parser and benchmark work is deeper.

`sqd-go-cli` has parser parity tests, multiple JSON parser benchmarks, decompression benchmarks, and pipeline benchmarks. It also has parser reuse patterns where block/log buffers are reused across callbacks.

`sqd-go` has a fastjson parser, but it allocates a new `Block`, `[]Log`, and `[]string` topics for each parsed record in `internal/parser/jsonl.go`. That is simpler, but it leaves performance on the table.

4. Client error modeling is better.

`sqd-go-cli/sqd/client.go` has a typed `StatusError`, retriable-status classification, and finalized-head support. `sqd-go/internal/client/client.go` is simpler and now supports cursor mode, but it returns plain formatted errors and lacks retry classification.

5. Test coverage is broader.

`sqd-go-cli` has tests for generated Go/SQL, generated unpackers against geth ABI behavior, ClickHouse checks, parser tests, and benchmarks. `sqd-go` now has focused tests for codegen artifacts and filter correctness, but it needs generated decoder parity tests and ingestion restart tests before it is safe to treat as the main implementation.

## Better In `sqd-go`

1. The product shape is cleaner.

`sqd-go` has the right external workflow: project `config.yaml`, `codegen`, `start`, `dev`, `stop`, and `init contract-import local`. This is much closer to the desired Envio-style CLI than `sqd-go-cli`, which is still centered around `init/generate` from ABI folders and `networks.yaml`.

2. Config-driven codegen is easier to use.

`sqd-go` can generate from a small config with chains, contracts, events, addresses, `start_block`, and `end_block`. This is the right direction for benchmark cases like `case_1_lbtc_event_only`, where the event set is explicit and small.

3. SQD cursor fetching is better in v2.

`sqd-go` changed `toBlock` to an optional pointer and omits it in cursor mode. That matches the faster SQD usage pattern. The client also explicitly sends `includeAllBlocks: false` and lowercases/keeps SQD log filters aligned with address/topic filters.

4. Address/topic filtering is now safer.

The v2 parser keeps filters separate per contract and enforces contract address matching locally before decode. That fixes the earlier cross-product risk where unrelated address/topic combinations could slip through.

5. Storage is more normalized than the old raw-log base.

`sqd-go` stores `chain_id`, `block_hash`, `address`, `topic0`, and params in one generic event log table and generates views per event. That is not the final high-performance layout, but it is a good generic baseline and better for quick project bring-up than the older raw-log-only orientation.

## Current Risks In `sqd-go`

1. Restart/checkpoint safety is not complete.

`internal/database/clickhouse.go` already has `TruncateAfterBlock`, but `internal/ingestion/ingestion.go` does not call it after reading `sync_state`. On restart, v2 reads `LastBlock`, starts at `last + 1`, and leaves any rows above that checkpoint intact.

That matters because ingestion inserts blocks while parsing, then inserts logs later. If the process dies or log insertion fails after some block rows were inserted but before `sync_state` was updated, restart can leave rows above the trusted checkpoint. The requested rule should be implemented exactly:

- read the persisted `sync_state.last_block`;
- synchronously delete/truncate all data rows with `block_number > last_block`;
- start from `last_block + 1`;
- only insert/update `sync_state` after the ClickHouse writes for that page have succeeded.

The code is close, but not there yet.

2. Blocks are inserted before the page is fully durable.

`InsertBlock` is called inside the parse callback, before all logs for the page are inserted. If `InsertLogs` fails, blocks for that page may already exist without corresponding logs and without a matching checkpoint. A safer structure is to parse the whole page into block rows and event rows, insert logs/event tables, insert block rows, then update `sync_state`.

3. Generated Go is not yet part of the runtime path.

`generated/events.go` contains useful constants and structs, but ingestion does not import or call generated unpackers. `generated/inserter.go` is also still a generic JSON-param inserter. The codegen claim is true at the artifact level, but not yet at the performance/runtime level.

4. SQL views are a temporary convenience, not the benchmark target.

The current generated views extract params from JSON strings. For serious benchmarks, generated event tables with native ClickHouse columns are better: less JSON serialization in Go, less JSON extraction in ClickHouse, and simpler analytical queries.

5. Multi-chain is not runtime-complete.

`config.Chain` has `ID`, `start_block`, and `end_block`, but the SQD endpoint is selected by a hard-coded switch. Unknown chains silently fall back to Polygon. Chain errors are logged in `Run`, then the command still returns success. That is not safe for multi-chain production behavior.

6. Retry/rate-limit behavior is too simple.

Fetch errors sleep for 5 seconds and retry forever. It needs typed status handling, `Retry-After` support for `429`/`503`, exponential backoff with jitter, and non-retry behavior for bad queries.

7. `init contract-import` uses string scanning for ABI JSON.

The scaffold path works for simple compact ABI JSON, but it should use `encoding/json` or go-ethereum ABI parsing. This is less urgent than checkpointing and generated runtime code, but it will break on formatted or unusual ABI JSON.

## Recommended Direction

1. Fix checkpoint and restart correctness first.

Implement the restart contract before optimizing more: checkpoint after successful page writes, truncate rows above the checkpoint before resuming, and add an e2e test that simulates a partial page write.

2. Move v2 runtime onto generated code.

Port the `sqd-go-cli` generated dispatcher and direct unpack functions into v2 codegen. The v2 ingestion loop should decode through generated per-project code instead of `parser.DecodedEvent` maps. This is the biggest performance and correctness improvement.

3. Generate typed ClickHouse event tables and batches.

Keep the generic `logs` table only if it is explicitly configured as raw/debug output. The benchmark path should write native per-event tables with generated `proto` columns. Views can remain as readable helpers, not as the primary typed representation.

4. Reuse the parser/decompression wins from `sqd-go-cli`.

Bring over parser buffer reuse and reusable zstd decode buffers. Keep the unsafe string lifetime contract explicit: strings are valid only inside the parse callback unless copied.

5. Harden SQD client behavior.

Add typed errors, retry classification, `Retry-After`, and tests for `200`, `204`, `400`, `429`, and `503`. Cursor mode should remain the default for unbounded fetches.

6. Make chain endpoints explicit config.

Add chain endpoint/dataset configuration instead of hard-coded fallback. Unknown chain IDs should fail loudly unless a custom endpoint is provided.

7. Expand tests around the actual target behavior.

Minimum next tests:

- restart truncates rows above `sync_state.last_block`;
- checkpoint is inserted only after successful page writes;
- generated direct unpackers match go-ethereum ABI decoding for representative events;
- generated typed ClickHouse batches write expected columns;
- cursor requests omit `toBlock`;
- multi-contract filters do not create address/topic cross-products;
- unknown chain ID fails instead of using Polygon.

## Practical Next Milestone

The next milestone should be "safe generated runtime for one event":

Use the LBTC `Transfer(address indexed from, address indexed to, uint256 value)` case, generate a direct unpacker and typed ClickHouse batch for `lbtc_transfer`, make ingestion use that generated path, and prove restart safety with `sync_state` truncation. After that works, generalize to the broader ABI type matrix from `sqd-go-cli`.

