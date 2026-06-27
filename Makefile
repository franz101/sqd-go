# sqd-go Makefile
# Top-level targets for development, testing, and deployment.
# The codegen is example-agnostic (template engine, internal/template/*.tmpl);
# all polymarket specifics live in examples/polymarket/{config.yaml,custom_*.go}.

BUILD_DIR := tmp

.PHONY: build dev-build test vet benchmark benchmark-fast \
	test-config-matrix benchmark-live-matrix \
	codegen-uniswap start-uniswap dev-uniswap restart-uniswap uniswap-e2e uniswap-fast \
	codegen-polymarket dev-polymarket-live polymarket-fast-tmux polymarket-stop \
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

# === Polymarket Development (examples/polymarket; Polygon, chain 137) ===
# Custom-processor project: --state is mandatory (regenerates, blank-imports the
# project so init() registers the processor, builds, re-execs). The fast path is
# default now: SQD_PARSE_DECODE_V2 is on unless NO_*=1, the state-prune is the
# bounded windowed prune, and the cold tier is Pebble v2 (MinLZ). No polymarket
# specifics live in the codegen — only in examples/polymarket.

POLYMARKET_DIR := examples/polymarket
POLYMARKET_DATABASE ?= polymarket
POLYMARKET_START_BLOCK ?= 25000000
POLYMARKET_PRUNE_INTERVAL ?= 1000000
POLYMARKET_TMUX_SESSION ?= sqd-polymarket-live
POLYMARKET_TMUX_LOG ?= tmp/polymarket-fast.log
POLYMARKET_ARGS ?=

codegen-polymarket:
	go run . codegen $(POLYMARKET_DIR)

# Live backfill from POLYMARKET_START_BLOCK, following the chain head (--end-block 0).
# Resumes by default (no --restart, which would DROP the DB); pass extra flags via
# POLYMARKET_ARGS, e.g. POLYMARKET_ARGS=--reindex-from <block>.
dev-polymarket-live: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) \
		$(BUILD_DIR)/sqd-go start $(POLYMARKET_DIR) --blockchain polygon \
		--start-block $(POLYMARKET_START_BLOCK) --end-block 0 $(POLYMARKET_ARGS)

# Fast detached backfill: parallel fetch + read-set prefetch in a tmux session.
# RPS=20 is the measured fetch sweet spot (sweep: 5->20 RPS ~+25%, plateaus ~20;
# no portal throttling observed up to 60). Attach: tmux attach -t $(POLYMARKET_TMUX_SESSION).
polymarket-fast-tmux: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION) (attach: tmux attach -t $(POLYMARKET_TMUX_SESSION))"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && SQD_PARALLEL_RPS=20 SQD_PARALLEL_FETCHERS=16 SQD_METRICS_CH=1 SQD_STATS_INTERVAL=300 \
			$(MAKE) dev-polymarket-live POLYMARKET_ARGS=\"--state --parallel-fetch --prefetch $(POLYMARKET_ARGS)\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started fast backfill: $(POLYMARKET_TMUX_SESSION) (attach: tmux attach -t $(POLYMARKET_TMUX_SESSION); log: $(POLYMARKET_TMUX_LOG))"; \
	fi

polymarket-stop:
	-tmux kill-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null

# === Database Operations ===

db-reset:
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS $(CLICKHOUSE_DATABASE) SYNC"
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS uniswap SYNC"

stop:
	go run . stop
