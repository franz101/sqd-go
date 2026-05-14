split into multiple modules: ✓
main -> cli ✓
client -> http -> zstd -> jsonl ✓
parser -> abi parser ✓
database -> ingestion ✓
templating -> config validation -> codegen (manifest + sql + views + go types ✓, templating TODO)

end goal ✓
go run . codegen examples/uniswap ✓ (manifest, schema.sql, views.sql, generated Go)
go run . start examples/uniswap ✓

envio-style CLI ✓
├── codegen ✓ (manifest + ClickHouse SQL/views + Go types)
├── start ✓
├── dev ✓ (codegen + docker compose up + start)
├── stop ✓ (docker compose down + drop DB)
├── init
│   ├── contract-import local ✓ (ABI → config.yaml)
│   ├── template (TODO)

Next:
- init template (erc20, etc.)
- formal templating package (config validation → codegen from templates)
- multi-contract, multi-chain support (already in config schema)
- proper binary build (go install / brew)

RESOURCES:
- envio end to end example:
    /home/dev/CODING/polymarket_lowram/open-indexer-benchmark/sentio-benchmarks-may-2025/case_1_lbtc_event_only/envio
- have sqd working fetching jsonl zstd abi parse send to clickhouse:
    /home/dev/CODING/polymarket_lowram/sqd-go-cli
- have envio style cli with minimum focus on codegen and start and init:
    /home/dev/CODING/polymarket_lowram/hyperindex/packages/cli
- work in sqd-go until the uniswap example is working ✓
