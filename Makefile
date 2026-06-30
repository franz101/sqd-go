# sqd-go Makefile
# Top-level targets for development, testing, and deployment.
# Polymarket-example targets (codegen-polymarket, dev-v2, tmux, e2e, pnl, …) live
# on the polymarket-example branch.

BUILD_DIR := tmp

.PHONY: build dev-build test vet benchmark benchmark-fast \
	test-config-matrix benchmark-live-matrix \
	codegen-uniswap start-uniswap dev-uniswap restart-uniswap uniswap-e2e uniswap-fast \
	db-reset stop

# Load .env file if it exists (for local ClickHouse credentials)
ifneq (,$(wildcard .env))
include .env
export
endif

# ClickHouse container detection and defaults
DETECTOR_CONTAINER := $(shell docker ps --filter "publish=9003" --format "{{.Names}}" | head -n 1)
CLICKHOUSE_CONTAINER ?= $(if $(DETECTOR_CONTAINER),$(DETECTOR_CONTAINER),$(shell docker ps --filter "name=clickhouse" --format "{{.Names}}" | head -n 1))
CLICKHOUSE_PASSWORD ?= sqd-clickhouse
CLICKHOUSE_DATABASE ?= case_1_lbtc_event_only

# Uniswap example configuration
UNISWAP_DIR := examples/uniswap

# Focused benchmark controls. Repeating short benches is more useful for this
# codebase than one long, noisy run.
BENCH_TIME ?= 2s
BENCH_COUNT ?= 3

# === Build & Test ===

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/sqd-go .

dev-build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/main .

test:
	go test ./... -count=1 -timeout 120s

test-config-matrix:
	scripts/test_make_matrix.sh

vet:
	go vet ./...

benchmark:
	$(MAKE) benchmark-fast

benchmark-fast:
	go test ./internal/codegen -run '^$$' -bench 'BenchmarkUInt256(View_At|View_ForEach|Slice_Conversion|Slice_Iteration)$$' -benchmem -benchtime=$(BENCH_TIME) -count=$(BENCH_COUNT)
	go test ./internal/ingestion -run '^$$' -bench '^BenchmarkReplayBufferGetBlockFull$$' -benchmem -benchtime=$(BENCH_TIME) -count=$(BENCH_COUNT)
	SQD_COLDFILTER_BITS=0 SQD_COLDCACHE_OPTIM= go test ./coldcache -run '^$$' -bench 'BenchmarkColdHit_(Get|GetInto)$$|BenchmarkEvictionSpill_(PerKeyPut|BatchedPut)$$' -benchmem -benchtime=$(BENCH_TIME) -count=$(BENCH_COUNT)

benchmark-live-matrix:
	scripts/benchmark_live_matrix.sh

# === Uniswap Development ===

codegen-uniswap:
	go run . codegen $(UNISWAP_DIR)

start-uniswap:
	go run . start $(UNISWAP_DIR)

dev-uniswap:
	go run . dev $(UNISWAP_DIR)

uniswap-fast:
	SQD_PARSE_DECODE_V2=1 SQD_METRICS_CH=1 SQD_METRICS_CH_INTERVAL=2s SQD_PARALLEL_RPS=10 SQD_PARALLEL_FETCHERS=12 go run . dev $(UNISWAP_DIR) --parallel-fetch --no-replay --restart

uniswap-fast-tmux:
	-tmux kill-session -t sqd-fast 2>/dev/null
	tmux new-session -d -s sqd-fast
	tmux send-keys -t sqd-fast "SQD_PARSE_DECODE_V2=1 SQD_METRICS_CH=1 SQD_METRICS_CH_INTERVAL=2s SQD_PARALLEL_RPS=10 SQD_PARALLEL_FETCHERS=12 go run . dev examples/uniswap --parallel-fetch --no-replay" Enter
	tmux attach-session -t sqd-fast

restart-uniswap:
	go run . start $(UNISWAP_DIR) --restart

uniswap-e2e: codegen-uniswap build
	go test ./$(UNISWAP_DIR)/generated/ -run 'TestCustom.*E2E|TestAppendDecodedLog' -v -count=1
	$(BUILD_DIR)/sqd-go start $(UNISWAP_DIR) --restart

# === Database Operations ===

db-reset:
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS $(CLICKHOUSE_DATABASE) SYNC"
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS uniswap SYNC"

stop:
	go run . stop
