# Quickstart

Index EVM logs into ClickHouse in under 5 minutes.

## Prerequisites

- Go 1.23+
- Docker (for local ClickHouse)

## 1. Install

```bash
go install github.com/franz101/sqd-go@latest
```

Or clone and build:

```bash
git clone https://github.com/franz101/sqd-go.git
cd sqd-go
go build -o sqd-go .
```

## 2. Create a Project

### From an ABI

Download the [Uniswap V2 Factory ABI](https://etherscan.io/address/0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f#code) or use any contract ABI JSON:

```bash
sqd-go init contract-import local \
  --abi uniswap_v2_factory.json \
  --name UniswapV2Factory \
  --blockchain ethereum \
  --start-block 10000835 \
  --address 0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f
```

> **The ABI must define at least one event.** `contract-import` extracts `"type": "event"` entries from the ABI; if there are none it fails with `no events found in ABI`. Router and library contracts (e.g. the Uniswap V2 Router02) emit no events of their own — index the contract that actually emits them instead (for Uniswap V2: the Factory emits `PairCreated`, each Pair emits `Swap`/`Sync`/`Mint`/`Burn`).

### From the ERC20 Template

```bash
sqd-go init template erc20
```

### Interactive

```bash
sqd-go
```

All three produce the same structure:

```
uniswapv2factory/
  config.yaml       # event definitions, chain, start block
  .env              # ClickHouse connection
  compose.yml       # local ClickHouse via Docker
  abis/             # ABI JSON (if ABI source)
```

## 3. Run

### Dev mode (manages Docker for you)

```bash
sqd-go dev uniswapv2factory/
```

This runs `docker compose up`, starts indexing, and tears down on exit.

### Start mode (bring your own ClickHouse)

```bash
docker compose -f uniswapv2factory/compose.yml up -d
sqd-go start uniswapv2factory/
```

Press `Ctrl+C` to stop. The indexer saves a cursor and resumes from where it left off.

### Re-index from scratch

```bash
sqd-go start uniswapv2factory/ --restart
```

## 4. Query ClickHouse

```bash
docker exec -it clickhouse clickhouse-client \
  --password sqd-clickhouse \
  --query "SELECT * FROM UniswapV2Factory.uniswap_v2_factory_pair_created_events LIMIT 10"
```

The database is named after the config `name` (here `UniswapV2Factory`, override with `CLICKHOUSE_DATABASE`). Every event gets its own typed table named `<contract>_<event>_events`. The `PairCreated(address indexed token0, address indexed token1, address pair, uint256)` event on contract `UniswapV2Factory` produces `uniswap_v2_factory_pair_created_events` with columns `token0`, `token1`, `pair`, plus metadata columns (`block_number`, `transaction_index`, `log_index`, `block_timestamp`). A `Transfer(address indexed from, address indexed to, uint256 value)` event on contract `MyToken` would produce `my_token_transfer_events`.

## 5. Add Custom Logic

Once basic indexing works, add derived state (balances, PnL, aggregations):

```bash
sqd-go init uniswapv2factory/
```

This scaffolds two files:

- `custom_schema.go` -- define ClickHouse tables as Go structs
- `custom_processor.go` -- write your ETL logic

Then re-run:

```bash
sqd-go start uniswapv2factory/ --restart
```

See [Custom Schema](CUSTOM_SCHEMA.md) and [Custom Processor](CUSTOM_PROCESSOR.md) for the full guide.

## Common Flags

```
--restart             Drop DB and re-index from scratch
--start-block <n>     Override config start block
--end-block <n>       Stop at this block (0 = infinite)
--blockchain <name>   Override chain (ethereum, polygon, or numeric ID)
--no-proto            Use V1 legacy parsed mode
--no-cold-cache       Disable Pebble cold tier (on by default)
--pagesize <n>        Fixed fetch page size (0 = adaptive)
--cpuprofile <file>   Write CPU profile
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CLICKHOUSE_HOST` | `127.0.0.1` | ClickHouse host |
| `CLICKHOUSE_NATIVE_PORT` | `9000` | Native protocol port |
| `CLICKHOUSE_HTTP_PORT` | `8123` | HTTP port |
| `CLICKHOUSE_USER` | `default` | User |
| `CLICKHOUSE_PASSWORD` | `sqd-clickhouse` | Password |
| `CLICKHOUSE_DATABASE` | project name | Database name |

## What's Next

- **[New to Go?](GO_FOR_BEGINNERS.md)** -- Go concepts explained for sqd-go beginners
- [config.yaml reference](CONFIG.md) -- full config schema
- [Custom Schema](CUSTOM_SCHEMA.md) -- define derived state tables
- [Custom Processor](CUSTOM_PROCESSOR.md) -- write ETL logic
- [PnL Example](PNL_EXAMPLE.md) -- end-to-end token balance + PnL tracker
- [CLI Reference](CLI.md) -- all commands and flags
- [Init](INIT.md) -- project scaffolding details
