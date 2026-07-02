# sqd-go

A high-performance EVM log indexer written in Go.


## Installation (IN DEVELOPMENT)

The easiest way to install the CLI is using the installation script:

```bash
curl -sSL https://raw.githubusercontent.com/franz101/sqd-go/main/install.sh | bash
```
*Note: Go is required to run the CLI and generate/compile the project code.*
Install go from https://go.dev/dl/

### Get started

Clone the repository from source:

```bash
git clone https://github.com/franz101/sqd-go.git
cd sqd-go
go install .
```

## CLI Commands

The CLI provides an interactive workflow or config based.

### 1. `codegen`
Validates the project's `config.yaml` or `config.yml` and generates the SQL schema, SQL views, and Go event structs.

```bash
go run . codegen examples/uniswap
```
This generates:
- `.sqd/generated/schema.sql`
- `.sqd/generated/views.sql`
- `generated/events.go`
- `.sqd/generated/manifest.json`

### 2. `dev`
Starts the local development environment: runs `codegen`, spins up the Docker Compose stack (ClickHouse), and starts ingestion. Use `--restart` to clear existing data.

```bash
go run . dev examples/uniswap --restart
```

### 3. `start`
Starts data ingestion and syncing without modifying the Docker container state.

```bash
go run . start examples/polymarket
```

### 4. `stop`
Stops the Docker Compose stack and drops the local ClickHouse database.

```bash
go run . stop
```

## Custom Processors (In Development)

`sqd-go` allows, that you can build custom processors to transform your data.

## Forking (In Development)


## The config file

The base of a project is the config.yml which is similar to the graph network.yaml or envio config.yaml

```
name: case_1_lbtc_event_only
chains:
  - id: 1
    # LBTC was deployed around block 20.5M; do not scan from genesis.
    start_block: 20600000
    end_block: 22200000
    contracts:
      - name: LBTC
        address: "0x8236a87084f8B84306f72007F36F2618A5634494"
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
```

## Architecture

```
sqd-go/
├── main.go                  # Entry point → internal/cli.Run(os.Args)
├── compose.yml              # ClickHouse (+ Grafana) Docker stack
├── Makefile                 # Build / test / codegen helpers
│
├── internal/                # Private implementation packages
│   ├── cli/                 # Command routing: codegen · dev · start · stop · init
│   ├── config/              # config.yaml / config.yml schema + loader
│   ├── client/              # Subsquid portal HTTP wrapper (block/log fetch)
│   ├── codegen/             # config → schema.sql, views.sql, events.go, parser.go,
│   │                        #   inserter, hot-state, compaction, custom-table code
│   ├── template/            # Embedded Go/SQL code templates (templates/code, /sql)
│   ├── templating/          # `init`: scaffold a project from a raw ABI
│   ├── parser/              # JSONL decoders, arena allocator, ABI event decoding
│   ├── ingestion/           # Core indexing loop: checkpoints, parallel fetch,
│   │                        #   recovery, replay, adaptive page size, fork handling
│   ├── database/            # ClickHouse client + stateful batched inserter
│   ├── monitoring/          # Opt-in runtime/throughput metrics → ClickHouse → Grafana
│   ├── envconfig/           # Centralized SQD_* environment-variable config
│   └── fork_sqd/            # Unfinalized-block / reorg tracking
│
├── sqd/                     # Public API surface generated projects compile against
├── abiunpack/               # Zero-reflection, zero-alloc ABI decoding (public)
├── coldcache/               # Pebble / flat cold tier under the hot caches (public)
├── protomath/               # uint256 / Decimal256 math on ch-go proto values (public)
│
├── examples/                # Sample projects
│   ├── uniswap/             # Uniswap V2/V3 event indexing
│   └── polymarket/          # Polymarket CTF indexing + custom processor
│       └── generated/       # Codegen output (events, parser, inserter, state, …)
│
├── grafana/                 # Provisioned dashboards + ClickHouse datasource
├── clickhouse/              # ClickHouse user config (Grafana access)
├── wiki/                    # CLI, config, and example documentation
├── debugger/                # Ad-hoc block-fetch debugging tool
└── benchmarks/ · experiments/   # Perf notes and reporting
```

The four top-level packages (`sqd`, `abiunpack`, `coldcache`, `protomath`) are
**public**: a generated project and its hand-written `custom_processor.go` import
these — not the module's `internal/` packages — so a project can be built as its
own standalone Go module (Go forbids importing another module's `internal/`).

## Project Structure

- `examples/`: Sample `config.yaml` setups like `uniswap` and `polymarket`.
- `internal/`:
  - `cli/`: Command-line interface definitions and routing.
  - `client/`: Subsquid HTTP API wrapper.
  - `codegen/`: `config.yaml` parser, SQL/Go generator.
  - `config/`: Configuration definitions.
  - `database/`: ClickHouse client wrapper and stateful batched inserter.
  - `ingestion/`: Core indexing loop and checkpoint management.
  - `parser/`: Event decoders, JSONL parsers, and ABI event decoding.

### `init` from scratch
Scaffold a new project from a local ABI file.

```bash
go run . init contract-import local --abi erc20.json --name USDC --address 0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48
```
This generates `config.yaml` plus template-driven `custom_schema.go` and
`custom_processor.go` files using the ABI's actual generated event names.
