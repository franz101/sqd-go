# CLI Reference

`sqd-go` is a config-driven EVM log indexer with an Envio-style CLI. It ingests EVM logs from the Subsquid (SQD) finalized stream into ClickHouse, decoding them from Solidity event signatures at runtime — no pre-compiled ABIs needed.

## Entry Point

```
main.go → cli.Run(os.Args[1:]) → parseArgs + dispatch
```

The CLI auto-loads `.env` from the current working directory and the binary's directory before dispatching.

## Commands

### `sqd-go` (no args)

Runs interactive init. Prompts for project name, source (ABI file or ERC20 template), contract details, chain ID, start block, and contract address. Produces a project directory with `config.yaml`, `.env`, `compose.yml`, and an `abis/` folder.

### `sqd-go init`

Same as no-args — interactive project setup.

### `sqd-go init <path>`

Scaffolds `custom_schema.go`, `custom_processor.go` templates, and runs codegen for an existing `config.yaml`. The `<path>` can be a directory containing `config.yaml`/`config.yml` or a direct path to the config file. This is the "I have a config, now give me the full custom processor skeleton" command.

### `sqd-go init contract-import local --abi <file> --name <name> [--address <addr>]`

Non-interactive project scaffolding from an ABI JSON file. Parses the ABI, extracts all event signatures, generates `config.yaml`, `.env`, `compose.yml`, and copies the ABI to `abis/`. Produces a project in a directory named after the contract (sanitized to `kebab-case`). Accepts `--blockchain` (chain ID/name, default ethereum) and `--start-block` (default 0).

### `sqd-go init template [erc20] [path]`

Scaffolds an ERC20 template project (Transfer + Approval events, USDC on Ethereum mainnet). Accepts `--name`, `--blockchain`, `--start-block`, `--address`.

### `sqd-go codegen <path>`

Validates config, builds event specs, generates all outputs:

| Output | Location | Description |
|--------|----------|-------------|
| `manifest.json` | `.sqd/generated/` | Machine-readable project manifest |
| `schema.sql` | `.sqd/generated/` + `generated/` | ClickHouse DDL for event tables |
| `custom_schema.sql` | `.sqd/generated/` + `generated/` | DDL for tables from `custom_schema.go` |
| `views.sql` | `.sqd/generated/` | ClickHouse views for querying |
| `events.go` | `generated/` | Go types + ABIs + UnpackLog* decoders |
| `events_easyjson.go` | `generated/` | easyjson generated code (via `easyjson -all`) |
| `schema.go` | `generated/` | GraphQL-like schema structs |
| `inserter.go` | `generated/` | ClickHouse column builders per event type |
| `custom_processor.go` | `generated/` | Codegen-owned processor framework |
| `state.go` | `generated/` | In-memory state with snapshots (if `custom_schema.go` exists) |
| `hotstate.go` | `generated/` | Hot state maps backed by CLOCK cache (if custom tables) |
| `compaction.go` | `generated/` | ClickHouse compaction/pruning logic |
| `ringbuffer.go` | `generated/` | Ring buffer for block event slots |

### `sqd-go start <path> [--restart] [--no-resume] [--start-block <n>] [--end-block <n>] [--blockchain <id>] [--pagesize <n>]`

Runs `codegen` then starts the ingestion pipeline. Connects to ClickHouse, creates tables, fetches from SQD in pages, parses JSONL, decodes events, inserts into ClickHouse. Runs until `SIGINT`/`SIGTERM` or end block reached.

Flags:
- `--restart`: Drop database and re-index from scratch
- `--no-resume`: Drop database and start from configured `start_block` (drops saved cursor)
- `--start-block <n>`: Override config start block
- `--end-block <n>`: Override config end block (0 = infinite)
- `--blockchain <id|name>`: Override config chain ID (e.g. `1`, `137`, `ethereum`, `polygon`)
- `--pagesize <n>`: Fixed page size per SQD fetch (default 0 = dynamic)
- `--cpuprofile <file>`: Write CPU profile to file

### `sqd-go dev <path> [--restart] [--no-resume] [--start-block <n>] [--end-block <n>]`

Like `start` but manages the full lifecycle: runs `docker compose up -d` before ingestion and `docker compose down -v` on exit (via deferred cleanup). Designed for local development with the auto-generated `compose.yml` from init.

### `sqd-go stop`

Runs `docker compose down -v` in the current directory (if `compose.yml`/`docker-compose.yml` found).

### `sqd-go help` / `sqd-go -h` / `sqd-go --help`

Shows usage text.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CLICKHOUSE_HOST` | `127.0.0.1` | ClickHouse native host |
| `CLICKHOUSE_NATIVE_PORT` | `9000` | ClickHouse native protocol port |
| `CLICKHOUSE_HTTP_PORT` | `8123` | ClickHouse HTTP port (for LoadFromDatabase) |
| `CLICKHOUSE_USER` | `default` | ClickHouse user |
| `CLICKHOUSE_PASSWORD` | `sqd-clickhouse` | ClickHouse password |
| `CLICKHOUSE_DATABASE` | project name | ClickHouse database name |
| `STATE_SNAPSHOT_INTERVAL` | `4000` | Blocks between state snapshots |
| `CLICKHOUSE_PRUNE_INTERVAL` | `100000` | Blocks between compaction/prune cycles |
| `SQD_COLDCACHE_MB` | RAM/8, clamped 256–8192 | Cold-tier (Pebble) block cache cap in MiB |
| `SQD_PPROF_ADDR` | unset | e.g. `localhost:6060` — expose net/http/pprof on a live run |
| `SQD_PORTAL_ENDPOINT` | unset | Override the SQD portal URL (testing/mirrors) |

## Config Format (config.yaml)

```yaml
name: my-indexer
ecosystem: evm
fork: default          # default | sqd | ringbuffer
store_raw_logs: false  # if true, raw logs table is created
store_blocks: false    # if true, blocks table is created
include_metadata:      # optional — columns to include on per-event typed tables
  - chain_id
  - block_hash
  - contract_address
  - transaction_hash
exclude_metadata:      # optional — drop fields from typed tables
  - OrderFilled: orderHash
state:                 # optional — hot state table declarations
  - name: Conditions
    source_table: memory_conditions
    mode: hotstate
chains:
  - id: 1              # 1=Ethereum, 137=Polygon
    start_block: 21000000
    end_block: 21000100  # optional
    contracts:
      - name: MyContract
        address: "0x..."  # string or array of strings
        events:
          - event: "Transfer(address indexed from, address indexed to, uint256 value)"
          - event: "Approval(address indexed owner, address indexed spender, uint256 value)"
```

Fork modes:
- `default`: Uses `CollapsingMergeTree` for natural fork handling
- `sqd`: SQD's fork recovery protocol (HTTP 409 → find ancestor → rollback)
- `ringbuffer`: In-memory ring buffer replay on fork

## Ingestion Pipeline Flow

```
codegen: config.yaml → manifest.json + schema.sql + events.go + ...
   ↓
start: connect ClickHouse → ensure tables → load cursor
   ↓
fetch: SQD HTTP POST (zstd) → decompress JSONL
   ↓   (live runs fetch via /finalized-stream while >512 blocks below the
   ↓    finalized head — the hot /stream endpoint paces deep catch-up reads —
   ↓    then switch to /stream at the head)
parse: FastJSONParser → per-block EVMLog arrays
   ↓
decode: UnpackLogWithMeta → typed Go event structs
   ↓
insert: ClickHouse native protocol (typed tables + optional raw_logs)
   ↓
custom: Processor.Process() → user-defined ETL logic
   ↓
commit: periodic snapshots + ClickHouse compaction
```

## Error Handling

- Exit code 0: success
- Exit code 1: runtime error (config, ClickHouse, ingestion)
- Exit code 2: usage error (missing/invalid arguments)
