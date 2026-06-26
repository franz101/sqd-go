# sqd-go Makefile
# Top-level targets for development, testing, and deployment

BUILD_DIR := tmp

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

# Uniswap example configuration
UNISWAP_DIR := examples/uniswap

# Debug arguments
INMEM_ARGS ?=
MEMCH_ARGS ?=

# E2E test range
E2E_START_BLOCK ?= 26000000
E2E_END_BLOCK ?= 40000000

# === Build & Test ===

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/sqd-go .

dev-build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/main .

test:
	go test ./... -count=1 -timeout 120s

vet:
	go vet ./...

benchmark:
	scripts/profile_make_targets.sh dev dev-v1 dev-v2

# === Polymarket Development ===

# Generate code for Polymarket
codegen-polymarket:
	go run . codegen $(POLYMARKET_DIR)

# Run in cursor mode (catch-up from start block)
dev-polymarket: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket $(POLYMARKET_ARGS)

# Run from specific start block
dev-v2: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block 3664531 $(POLYMARKET_ARGS)

# Live tailing (follows the chain head)
dev-v2-live: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block 2364531 --end-block 0 $(POLYMARKET_ARGS)

# === Tmux-based Development ===

# Standard tmux session (sequential fetch)
dev-tmux: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) $(MAKE) dev-v2-live POLYMARKET_ARGS=--state 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session: $(POLYMARKET_TMUX_SESSION):0"; \
	fi

# Fast tmux session (parallel fetch + prefetch)
dev-fast-tmux: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) SQD_STATS_INTERVAL=300 $(MAKE) dev-v2-live POLYMARKET_ARGS=\"--state --prefetch --parallel-fetch\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session (fast: --parallel-fetch, stats 5m): $(POLYMARKET_TMUX_SESSION):0"; \
	fi

# Fast tmux with CPU profiling
dev-fast-tmux-profiling: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)) tmp/profiles; \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) SQD_STATS_INTERVAL=300 $(MAKE) dev-v2-live POLYMARKET_ARGS=\"--state --parallel-fetch --prefetch --cpuprofile tmp/profiles/polymarket-fast.pprof\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session (fast+profiling -> tmp/profiles/polymarket-fast.pprof): $(POLYMARKET_TMUX_SESSION):0"; \
	fi

# Tmux with reindex
dev-tmux-reindex: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) $(MAKE) dev-v2-live POLYMARKET_ARGS=\"--state --reindex-from 84000000\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session: $(POLYMARKET_TMUX_SESSION):0"; \
	fi

# Tmux with CPU profiling
dev-tmux-profiling: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)) tmp/profiles; \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) $(MAKE) dev-v2-live POLYMARKET_ARGS=\"--state --cpuprofile tmp/profiles/polymarket-live.pprof\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
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

# In-memory processor
inmem:
	go run debugger/inMemoryProcessor.go $(INMEM_ARGS)

# ClickHouse processor
memch:
	go run debugger/clickhouseProcessor.go $(MEMCH_ARGS)

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
