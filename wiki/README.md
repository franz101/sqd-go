# sqd-go Wiki

EVM log indexer -- config-driven, ClickHouse-backed, with codegen and custom processing.

## Pages

| Page | Description |
|------|-------------|
| [Quickstart](QUICKSTART.md) | Install, create a project, run, query -- in 5 minutes |
| [config.yaml](CONFIG.md) | Full config schema: chains, contracts, events, state, metadata |
| [Custom Schema](CUSTOM_SCHEMA.md) | Define ClickHouse tables as Go structs with hot state caching |
| [Custom Processor](CUSTOM_PROCESSOR.md) | Write ETL logic: event handling, state management, fork recovery |
| [PnL Example](PNL_EXAMPLE.md) | End-to-end token balance and profit/loss tracker |
| [CLI Reference](CLI.md) | All commands, flags, environment variables, pipeline flow |
| [Init](INIT.md) | Project scaffolding: interactive, ABI import, templates |
