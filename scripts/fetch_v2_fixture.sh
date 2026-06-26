#!/usr/bin/env bash
# Regenerate the Polymarket V2 OrderFilled parity fixture used by
# examples/polymarket/v2_realworld_e2e_test.go.
#
# The fixture is intentionally NOT committed (see .gitignore: debugger/data/).
# Run this once after a fresh checkout to let the V2 e2e tests run instead of
# skipping. Requires network access to the SQD portal.
set -euo pipefail

cd "$(dirname "$0")/.."

OUT="${OUT:-debugger/data/polymarket_v2_orderfilled/blocks_87200028_87200177.jsonl.zstd}"

echo "Regenerating $OUT ..."
go run debugger/fetch_v2_fixture.go -out "$OUT" "$@"
echo "Done. Run: go test ./examples/polymarket/ -run TestPolymarketV2"
