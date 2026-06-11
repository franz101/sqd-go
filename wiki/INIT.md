# Init — Project Scaffolding

The `init` command family creates new sqd-go indexer projects. Three modes are available:

1. **Interactive** — `sqd-go` or `sqd-go init` (no extra args)
2. **Contract import** — `sqd-go init contract-import local --abi <file> --name <name>`
3. **Template** — `sqd-go init template [erc20] [path]`

All modes produce the same output structure: a project directory containing `config.yaml`, `.env`, `compose.yml`, and optionally an `abis/` folder.

## Interactive Init (`sqd-go` / `sqd-go init`)

Prompts walk through:

1. **Project directory** — folder name (sanitized, must not already contain `config.yaml`)
2. **Source** — choose "From ABI File" or "Template: ERC20"
3. **ABI file path** — local path to Solidity ABI JSON (if ABI option chosen)
4. **Contract name** — defaults to the ABI filename, PascalCased
5. **Chain** — Ethereum Mainnet (1), Polygon Mainnet (137), or custom chain ID
6. **Start block** — defaults to 0
7. **Contract address** — optional, validated as 42-char hex (`0x` + 40 hex digits)

Output:
```
<project-dir>/
  config.yaml       # generated from inputs
  .env              # ClickHouse connection defaults
  compose.yml       # Docker Compose for local ClickHouse
  abis/             # ABI JSON copy (if ABI source)
```

The config.yaml contains one chain, one contract, and all events extracted from the ABI.

## Contract Import (`sqd-go init contract-import local`)

Non-interactive equivalent. Faster for scripting and CI.

```
sqd-go init contract-import local [path] \
  --abi <file.json> \
  --name <ContractName> \
  [--address 0x...] \
  [--blockchain 1|137|ethereum|polygon] \
  [--start-block 0]
```

If `[path]` (project directory) is omitted, it's derived from the contract name: lowercased, with runs of non-alphanumeric characters replaced by a single dash (camelCase is not split). E.g. `--name MyToken` → `mytoken/`, `--name UniswapV2Factory` → `uniswapv2factory/`, `--name my token` → `my-token/`.

The ABI is read, all `"type": "event"` entries are extracted into Solidity event signatures like `Transfer(address indexed from, address indexed to, uint256 value)`, and these are written as the contract's event list in `config.yaml`.

If the ABI defines no events, the command fails with `no events found in ABI`. This is common with router and library contracts (e.g. the Uniswap V2 Router02), which emit no events themselves — pass the ABI of the contract that actually emits the events you want to index (for Uniswap V2: the Factory's `PairCreated`, or a Pair's `Swap`/`Sync`/`Mint`/`Burn`).

## Template (`sqd-go init template [erc20] [path]`)

Scaffolds a pre-configured ERC20 indexer with USDC (`0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48`) on Ethereum mainnet. Two events: `Transfer` and `Approval`.

Flags: `--name`, `--blockchain`, `--start-block`, `--address` work the same as contract-import.

## Intermediate Init (`sqd-go init <path>`)

For projects that already have a `config.yaml` but need the custom processor scaffolding. This command:

1. Loads the project config from `<path>`
2. Finds the Go module (`go.mod` walking up the directory tree)
3. Determines the import path for the `generated/` package
4. Writes `custom_schema.go` — a user-editable struct definition file with a `UserPositionSchema` example
5. Writes `custom_processor.go` — a user-editable processor file wired into the CLI via `init()` registration
6. Runs `codegen` to produce the `generated/` package (events, state, hotstate, compaction, etc.)

The generated `custom_processor.go` contains:

```go
package <project>

import (
    generated "<module>/<project>/generated"
    "github.com/franz101/sqd-go/internal/cli"
    "github.com/franz101/sqd-go/internal/ingestion"
)

func Process(state *generated.State, block *generated.ParsedBlock) error {
    // add code here
    // for ev := range block.EventsIter() {
    //     switch e := ev.(type) {
    //     case *generated.MyEvent:
    //         // handle event
    //     }
    // }
    //
    // state.UserPosition.Save(entity, meta)
    return nil
}

func init() {
    generated.CustomProcessFn = Process
    cli.RegisterProcessor(generated.ProjectName, func() (ingestion.Processor, error) {
        return generated.NewProcessor(cli.GetProtoMode())
    })
}
```

The `init()` function registers the custom processor so the CLI's `runStartPipeline` can look it up via `processorForProject(projectName)`. When the ingestion pipeline runs, it calls `Processor.Process()` which decodes logs, fills the event ring buffer, and calls `CustomProcessFn` (i.e., your `Process` function) for each block.

**Special case: `uniswap_pnl`** — if the package name is `uniswap_pnl`, a richer template is emitted with Transfer event handling (sender/receiver balance tracking using `uint256.Int` arithmetic).

## Init: Config Path (`sqd-go init:<config-path>`)

An undocumented escape hatch: `sqd-go init:examples/foo/config.yaml` behaves identically to `sqd-go init examples/foo/config.yaml` — it's the intermediate init path. The `parseArgs` function splits on the first `:` to form the command `init:` + subcommand.

## ABI Event Extraction

The `extractEventsFromABI()` function in `internal/cli/init.go` parses the ABI JSON using geth's `abi.JSON()` for validation, then reads the raw JSON to find all `"type": "event"` entries. For each event, it:

1. Resolves name conflicts (geth's `ResolveNameConflict`)
2. Builds the Solidity signature string: `EventName(type1 indexed name1, type2 name2, ...)`
3. Returns the canonical signature strings

These become the `event:` values in `config.yaml`'s `events` list.

## Scaffold Support Files

Every init path generates:

### `.env`
```
CLICKHOUSE_HOST=127.0.0.1
CLICKHOUSE_HTTP_PORT=8123
CLICKHOUSE_NATIVE_PORT=9000
CLICKHOUSE_USER=default
CLICKHOUSE_PASSWORD=sqd-clickhouse
CLICKHOUSE_DATABASE=<project-name>
```

### `compose.yml`
```yaml
name: <project-name>

services:
  clickhouse:
    image: clickhouse/clickhouse-server:latest
    environment:
      CLICKHOUSE_USER: "${CLICKHOUSE_USER:-default}"
      CLICKHOUSE_PASSWORD: "${CLICKHOUSE_PASSWORD:-sqd-clickhouse}"
      CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: "1"
    ports:
      - "${CLICKHOUSE_HTTP_PORT:-8123}:8123"
      - "${CLICKHOUSE_NATIVE_PORT:-9000}:9000"
    volumes:
      - clickhouse_data:/var/lib/clickhouse
    ulimits:
      nofile:
        soft: 262144
        hard: 262144

volumes:
  clickhouse_data:
```

The compose project name is derived as `sqd-go-<sanitized-project-name>`.

## File Safety

All writes use `writeFileExclusive` (O_EXCL) to avoid overwriting existing `config.yaml`. Support files (`.env`, `compose.yml`, ABI copy) use `writeFileIfMissing` — they won't overwrite existing files. This makes `sqd-go init` idempotent for the support files but fails fast if `config.yaml` already exists.
