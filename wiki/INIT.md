# Init — Project Scaffolding

The `init` command family creates new sqd-go indexer projects. Three modes are available:

1. **Interactive** — `sqd-go` or `sqd-go init` (no extra args)
2. **Contract import** — `sqd-go init contract-import local --abi <file> --name <name>`
3. **Template** — `sqd-go init template [erc20] [path]`

All modes produce the same output structure: a project directory containing `config.yaml`, `.env`, `compose.yml`, `custom_schema.go`, `custom_processor.go`, and optionally an `abis/` folder.

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
  custom_schema.go  # derived-state schema
  custom_processor.go # typed event processor and registration
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

## State scaffolding performed by init

After writing `config.yaml`, every init mode:

1. Loads the project config from `<path>`
2. Uses an enclosing Go module when present, or a standalone module-relative import otherwise
3. Determines the import path for the `generated/` package
4. Renders `custom_schema.go` from the config (a working address-position schema for ERC-20 Transfer; a generic state schema otherwise)
5. Renders `custom_processor.go` with config-derived generated event and field names

The generated package itself is produced by `sqd-go codegen <path>` or automatically by
`sqd-go start <path> --state`.

The generated `custom_processor.go` contains:

```go
package <project> // SAME package name as custom_schema.go

import (
    generated "<module>/<project>/generated"

    "github.com/franz101/sqd-go/sqd" // PUBLIC facade — never import internal/
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

func ProcessProto(state *generated.State, block *generated.ProtoEventBlock) error {
    return Process(state, block.ToParsedBlock())
}

func init() {
    generated.CustomProcessFn = Process
    generated.CustomProcessProtoFn = ProcessProto
    sqd.RegisterProcessor(generated.ProjectName, func() (sqd.Processor, error) {
        return generated.NewProcessor(sqd.GetProtoMode())
    })
}
```

Because each project compiles as its own Go module, the scaffolded processor imports the public `sqd` facade rather than any `internal/` package — Go forbids importing another module's `internal/` packages, so this is what lets the project build standalone. The `init()` function registers the custom processor so the CLI's `runStartPipeline` can look it up via `processorForProject(projectName)`. Registering under `generated.ProjectName` (rather than a hardcoded string) keeps the registration name in sync with your config. The generated `ProcessProto` bridge keeps the same business logic in the default proto decoder; it can be replaced with direct proto-view iteration later.

ERC-20 `Transfer(address,address,uint256)` projects receive a working address-position example. Other ABIs receive a compiling type switch for the first configured event. Both paths derive generated Go names and fields from `config.yaml`; directory and project names do not select behavior.

## Init: Config Path (`sqd-go init:<config-path>`)

An undocumented escape hatch: `sqd-go init:examples/foo/config.yaml` behaves identically to `sqd-go init examples/foo/config.yaml` — it's the intermediate init path. The `parseArgs` function splits on the first `:` to form the command `init:` + subcommand.

## ABI Event Extraction

The `extractEventsFromABI()` function in `internal/cli/init.go` parses the ABI JSON using geth's `abi.JSON()` for validation, then reads the raw JSON to find all `"type": "event"` entries. For each event, it:

1. Resolves name conflicts (geth's `ResolveNameConflict`)
2. Builds the Solidity signature string: `EventName(type1 indexed name1, type2 name2, ...)`
3. Returns the canonical signature strings

These become the `event:` values in `config.yaml`'s `events` list.

## Recent Improvements (2026)

### Enhanced State Scaffolding

The init command now generates more sophisticated state scaffolding:

- **ERC20 Transfer Example** - Working address-position tracking with proper decimal handling
- **Generic Schema** - For non-ERC20 contracts, a generic state schema is provided
- **Processor Registration** - Automatic registration with proper public facade imports

### Better Error Messages

Init now provides clearer error messages:
- `no events found in ABI` - When the ABI contains no events (common for router/library contracts)
- `config.yaml already exists` - Prevents accidental overwrites
- `invalid contract address` - Validates address format (42-char hex)

### Module Handling

Improved Go module handling:
- Auto-detects existing Go modules
- Generates proper module-relative imports
- Ensures public facade usage (`sqd` package) instead of internal imports

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
