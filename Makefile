# sqd-go Makefile
# Top-level targets for development, testing, and deployment

BUILD_DIR := tmp

.PHONY: build dev-build test vet benchmark benchmark-fast \
	test-config-matrix benchmark-live-matrix \
	codegen-polymarket dev-polymarket dev-v2 dev-v2-live show-polymarket-config \
	dev-tmux dev-sequential-tmux dev-prefetch-tmux dev-parallel-tmux \
	dev-parallel-prefetch-tmux dev-fast-tmux dev-fastest-tmux dev-fast-tmux-profiling \
	dev-tmux-reindex dev-tmux-profiling codegen-uniswap start-uniswap \
	dev-uniswap restart-uniswap dev-e2e dev-e2e-v2 dev-v2-e2e \
	uniswap-e2e db-reset stop polymarket-tail polymarket-fork \
	fetch-polymarket inmem memch pnl pnl-all

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

# Polymarket example configuration
POLYMARKET_DIR := examples/polymarket
POLYMARKET_DATABASE ?= polymarket
POLYMARKET_ARGS ?=
POLYMARKET_TMUX_SESSION ?= sqd-polymarket-live
POLYMARKET_TMUX_LOG ?= tmp/dev-v2-live.log
POLYMARKET_PRUNE_INTERVAL ?= 999999999999

# Fast defaults for the production-sized Polymarket state pipeline. Parallel
# fetch stays opt-in because it changes portal traffic and is not faster when
# the state processor is already the bottleneck. Prefetch is also opt-in on
# resume: recovery rebuilds an authoritative cold tier, so the dry pass normally
# resolves zero misses and only runs every handler twice.
POLYMARKET_STATE ?= 1
PREFETCH ?= 0
PARALLEL_FETCH ?= 0
SQD_COLDCACHE_OPTIM ?= largemem
SQD_COMMIT_INTERVAL ?= 20000
SQD_COMMIT_MAX_INTERVAL ?= 120s
SQD_STATS_INTERVAL ?= 60
SQD_PARALLEL_FETCHERS ?= 4
SQD_PARALLEL_PAGE_SIZE ?= 1000
SQD_PARALLEL_RPS ?= 5
SQD_PARSE_DECODE_V2 ?= 0
SQD_SINGLE_PARSE ?= 1
SQD_PRODUCER_PARSE ?= 1
POLYMARKET_METRICS ?= 0

enabled = $(filter 1 true yes on,$(strip $(1)))
POLYMARKET_MODE_ARGS = \
	$(if $(call enabled,$(POLYMARKET_STATE)),--state) \
	$(if $(call enabled,$(PREFETCH)),--prefetch) \
	$(if $(call enabled,$(PARALLEL_FETCH)),--parallel-fetch)
POLYMARKET_ENV = \
	SQD_COLDCACHE_OPTIM="$(SQD_COLDCACHE_OPTIM)" \
	SQD_COMMIT_INTERVAL="$(SQD_COMMIT_INTERVAL)" \
	SQD_COMMIT_MAX_INTERVAL="$(SQD_COMMIT_MAX_INTERVAL)" \
	SQD_STATS_INTERVAL="$(SQD_STATS_INTERVAL)" \
	SQD_PARALLEL_FETCHERS="$(SQD_PARALLEL_FETCHERS)" \
	SQD_PARALLEL_PAGE_SIZE="$(SQD_PARALLEL_PAGE_SIZE)" \
	SQD_PARALLEL_RPS="$(SQD_PARALLEL_RPS)" \
	SQD_PARSE_DECODE_V2="$(SQD_PARSE_DECODE_V2)" \
	SQD_SINGLE_PARSE="$(SQD_SINGLE_PARSE)" \
	SQD_PRODUCER_PARSE="$(SQD_PRODUCER_PARSE)" \
	SQD_METRICS_CH="$(POLYMARKET_METRICS)"

# Uniswap example configuration
UNISWAP_DIR := examples/uniswap

# Debug arguments
INMEM_ARGS ?=
MEMCH_ARGS ?=

# E2E test range
E2E_START_BLOCK ?= 26000000
E2E_END_BLOCK ?= 40000000

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

# === Polymarket Development ===

# Generate code for Polymarket
codegen-polymarket:
	go run . codegen $(POLYMARKET_DIR)

# Run in cursor mode (catch-up from start block)
dev-polymarket: codegen-polymarket build
	$(POLYMARKET_ENV) CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket $(POLYMARKET_MODE_ARGS) $(POLYMARKET_ARGS)

# Run from specific start block
dev-v2: codegen-polymarket build
	$(POLYMARKET_ENV) CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block 3664531 $(POLYMARKET_MODE_ARGS) $(POLYMARKET_ARGS)

# Live tailing (follows the chain head)
dev-v2-live: codegen-polymarket build
	$(POLYMARKET_ENV) CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block 2364531 --end-block 0 $(POLYMARKET_MODE_ARGS) $(POLYMARKET_ARGS)

show-polymarket-config:
	@echo "env: $(POLYMARKET_ENV)"
	@echo "args: $(POLYMARKET_MODE_ARGS) $(POLYMARKET_ARGS)"

# === Tmux-based Development ===

# Generic tmux session. PREFETCH and PARALLEL_FETCH are resolved per invocation.
dev-tmux: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) make dev-v2-live PREFETCH=$(PREFETCH) PARALLEL_FETCH=$(PARALLEL_FETCH) SQD_PARSE_DECODE_V2=$(SQD_PARSE_DECODE_V2) SQD_SINGLE_PARSE=$(SQD_SINGLE_PARSE) SQD_PRODUCER_PARSE=$(SQD_PRODUCER_PARSE) 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session (prefetch=$(PREFETCH), parallel-fetch=$(PARALLEL_FETCH)): $(POLYMARKET_TMUX_SESSION):0"; \
	fi

dev-sequential-tmux: PREFETCH=0
dev-sequential-tmux: PARALLEL_FETCH=0
dev-sequential-tmux: dev-tmux

dev-prefetch-tmux: PREFETCH=1
dev-prefetch-tmux: PARALLEL_FETCH=0
dev-prefetch-tmux: dev-tmux

dev-parallel-tmux: PREFETCH=0
dev-parallel-tmux: PARALLEL_FETCH=1
dev-parallel-tmux: dev-tmux

dev-parallel-prefetch-tmux: PREFETCH=1
dev-parallel-prefetch-tmux: PARALLEL_FETCH=1
dev-parallel-prefetch-tmux: dev-tmux

# The generated producer-parse pipeline overlaps finalized-page fetching and
# ordered state application. Prefetch stays off because it runs handlers twice.
# Commit every 15 seconds during backfill instead of every 3s default.
dev-fast-tmux: PREFETCH=0
dev-fast-tmux: PARALLEL_FETCH=1
dev-fast-tmux: SQD_PARSE_DECODE_V2=1
dev-fast-tmux: SQD_SINGLE_PARSE=1
dev-fast-tmux: SQD_PRODUCER_PARSE=1
dev-fast-tmux: SQD_COMMIT_INTERVAL=50000
dev-fast-tmux: SQD_COMMIT_MAX_INTERVAL=15
dev-fast-tmux: dev-tmux

# Compatibility alias: the fast settings now live in the normal defaults.
dev-fastest-tmux: dev-fast-tmux

# Fast tmux with CPU profiling
dev-fast-tmux-profiling: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)) tmp/profiles; \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) make dev-v2-live PREFETCH=$(PREFETCH) PARALLEL_FETCH=$(PARALLEL_FETCH) SQD_PARSE_DECODE_V2=1 SQD_SINGLE_PARSE=1 SQD_PRODUCER_PARSE=1 POLYMARKET_ARGS=\"--cpuprofile tmp/profiles/polymarket-fast.pprof\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session (fast+profiling -> tmp/profiles/polymarket-fast.pprof): $(POLYMARKET_TMUX_SESSION):0"; \
	fi

# Tmux with reindex
dev-tmux-reindex: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) make dev-v2-live PREFETCH=0 PARALLEL_FETCH=1 SQD_PARSE_DECODE_V2=1 SQD_SINGLE_PARSE=1 SQD_PRODUCER_PARSE=1 SQD_COMMIT_INTERVAL=50000 SQD_COMMIT_MAX_INTERVAL=15 POLYMARKET_ARGS=\"--reindex-from 84000000\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session: $(POLYMARKET_TMUX_SESSION):0"; \
	fi

# Tmux with CPU profiling
dev-tmux-profiling: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)) tmp/profiles; \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) make dev-v2-live PARALLEL_FETCH=0 POLYMARKET_ARGS=\"--cpuprofile tmp/profiles/polymarket-live.pprof\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session: $(POLYMARKET_TMUX_SESSION):0"; \
	fi

# === Uniswap Development ===

codegen-uniswap:
	go run . codegen $(UNISWAP_DIR)

start-uniswap:
	go run . start $(UNISWAP_DIR)

dev-uniswap:
	go run . dev $(UNISWAP_DIR)

restart-uniswap:
	go run . start $(UNISWAP_DIR) --restart

# === E2E Testing ===

# V1 E2E (no-proto mode)
dev-e2e: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $(E2E_START_BLOCK) --end-block $(E2E_END_BLOCK) --no-proto --restart $(POLYMARKET_ARGS)

# V2 E2E (proto mode with Pebble cold tier)
# Sanity check: realized PnL should be -$13.93, open ~ $3.00
dev-e2e-v2: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $(E2E_START_BLOCK) --end-block $(E2E_END_BLOCK) --restart $(POLYMARKET_ARGS)

dev-v2-e2e: dev-e2e-v2

# V1 E2E with test runner
uniswap-e2e: codegen-uniswap build
	go test ./$(UNISWAP_DIR)/generated/ -run 'TestCustom.*E2E|TestAppendDecodedLog' -v -count=1
	$(BUILD_DIR)/sqd-go start $(UNISWAP_DIR) --restart

# === Database Operations ===

db-reset:
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS $(CLICKHOUSE_DATABASE) SYNC"
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS polymarket SYNC"
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS uniswap SYNC"

stop:
	go run . stop

# === Live Tailing ===

# Tail last 2000 blocks from live RPC
polymarket-tail: codegen-polymarket build
	@HEAD=$$(curl -s https://polygon-bor-rpc.publicnode.com -X POST -H "Content-Type: application/json" --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' | jq -r .result | xargs printf "%d") && \
	START_BLOCK=$$(($$HEAD - 2000)) && \
	echo "Tailing 2000 blocks from Polygon head (starting at $$START_BLOCK)..." && \
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $$START_BLOCK --end-block 0 --restart $(POLYMARKET_ARGS)

# Tail last 6000 blocks from SQD portal
polymarket-fork: codegen-polymarket build
	@HEAD=$$(curl -s https://portal.sqd.dev/datasets/polygon-mainnet/head | jq -r .number) && \
	START_BLOCK=$$(($$HEAD - 6000)) && \
	echo "Tailing 6000 blocks from Polygon head (starting at $$START_BLOCK)..." && \
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $$START_BLOCK --end-block 0 --restart $(POLYMARKET_ARGS)

# === Debug Tools ===

# Fetch raw data from portal
fetch-polymarket:
	go run debugger/fetchUntil.go \
		-endpoint https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream \
		-start 33605403 -end 40206663 \
		-out debugger/data/wallet_0xf05b67_full

# Fetch full history for wallet 0x10f5b9bd e2e test
fetch-wallet-0x10f5b9bd:
	go run debugger/fetchUntil.go \
		-endpoint https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream \
		-start 33605403 -end 40206663 \
		-out debugger/data/wallet_0x10f5b9bd_full

# Run e2e PnL test for wallet 0x10f5b9bd
e2e-wallet-0x10f5b9bd: codegen-polymarket build
	go test ./examples/polymarket/... -run TestWallet0x10f5b9bd_PnL -v

# In-memory processor
inmem:
	go run debugger/inMemoryProcessor.go $(INMEM_ARGS)

# ClickHouse processor
memch:
	go run debugger/clickhouseProcessor.go $(MEMCH_ARGS)

# Fetch all Polymarket Gamma markets into ClickHouse
fetch-markets:
	go run debugger/fetchMarkets.go

# === PnL Analysis ===

# Single wallet PnL comparison
pnl:
	@python3 scripts/ch_pnl.py $(WALLET)

# All active wallets PnL comparison
pnl-all:
	@echo "Running PnL comparison for all wallets..."
	@for wallet in $$(python3 -c "import yaml; print('\n'.join(w['address'] for w in yaml.safe_load(open('drafts/pnl_wallets.yaml'))['wallets'] if w.get('active', True)))"); do \
		echo ""; \
		echo "========================================"; \
		echo "Wallet: $$wallet"; \
		echo "========================================"; \
		python3 scripts/ch_pnl.py $$wallet || echo "Failed: $$wallet"; \
	done
	@echo ""
	@echo "All results saved in tmp/compare_*.json"

# === Deprecated/Removed Targets ===
# dev-optimized-tmux: Use dev-fast-tmux instead
# dev-v1: V1 mode deprecated, use dev-v2
# initpnl: Migrate to new config system
# mem: Use memch instead
