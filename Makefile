BUILD_DIR := tmp

# Load .env file if it exists
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: dev dev-build build test vet benchmark codegen-uniswap start-uniswap dev-uniswap restart-uniswap uniswap-e2e codegen-polymarket dev-polymarket dev-v2-live dev-tmux dev-tmux-reindex dev-e2e dev-e2e-v2 dev-v2-e2e db-reset stop polymarket-fork fetch-polymarket inmem memch initpnl pnl pnl-all

DETECTOR_CONTAINER := $(shell docker ps --filter "publish=9003" --format "{{.Names}}" | head -n 1)
CLICKHOUSE_CONTAINER ?= $(if $(DETECTOR_CONTAINER),$(DETECTOR_CONTAINER),$(shell docker ps --filter "name=clickhouse" --format "{{.Names}}" | head -n 1))
CLICKHOUSE_PASSWORD ?= sqd-clickhouse
CLICKHOUSE_DATABASE ?= case_1_lbtc_event_only

dev-build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/main .

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/sqd-go .

test:
	go test ./... -count=1 -timeout 120s

vet:
	go vet ./...

benchmark:
	scripts/profile_make_targets.sh dev dev-v1 dev-v2

# Uniswap example
UNISWAP_DIR := examples/uniswap
POLYMARKET_DIR := examples/polymarket
POLYMARKET_DATABASE ?= polymarket
POLYMARKET_ARGS ?=
POLYMARKET_TMUX_SESSION ?= sqd-polymarket-live
POLYMARKET_TMUX_LOG ?= tmp/dev-v2-live.log
POLYMARKET_PRUNE_INTERVAL ?= 999999999999
INMEM_ARGS ?=

initpnl: build
	$(BUILD_DIR)/sqd-go init examples/uniswap_pnl/config.yaml

codegen-uniswap:
	go run . codegen $(UNISWAP_DIR)

start-uniswap:
	go run . start $(UNISWAP_DIR)

dev-uniswap:
	go run . dev $(UNISWAP_DIR)

restart-uniswap:
	go run . start $(UNISWAP_DIR) --restart

uniswap-e2e: codegen-uniswap build
	go test ./$(UNISWAP_DIR)/generated/ -run 'TestCustom.*E2E|TestAppendDecodedLog' -v -count=1
	$(BUILD_DIR)/sqd-go start $(UNISWAP_DIR) --restart

# Polymarket example
codegen-polymarket:
	go run . codegen $(POLYMARKET_DIR)

dev-polymarket: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket $(POLYMARKET_ARGS)

dev-v2: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block 3664531 $(POLYMARKET_ARGS)

dev-v2-live: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block 2364531 --end-block 0 $(POLYMARKET_ARGS)

dev-tmux:
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) $(MAKE) dev-v2-live POLYMARKET_ARGS=--state 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session: $(POLYMARKET_TMUX_SESSION):0"; \
	fi

dev-tmux-reindex:
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION):0"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) $(MAKE) dev-v2-live POLYMARKET_ARGS=\"--state --reindex-from 8400000\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started tmux session: $(POLYMARKET_TMUX_SESSION):0"; \
	fi

dev-v1: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block 3664531 --no-proto $(POLYMARKET_ARGS)

# Full v1 end-to-end run over the wallet 0xa79af3b active range (26M -> 40M).
# Sanity check after it finishes: realized PnL should be -$13.93, open ~ $3.00.
E2E_START_BLOCK ?= 26000000
E2E_END_BLOCK ?= 40000000
dev-e2e: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $(E2E_START_BLOCK) --end-block $(E2E_END_BLOCK) --no-proto --restart $(POLYMARKET_ARGS)

# Same range as dev-e2e but the V2 (proto) pipeline + Pebble cold tier (removes the
# per-miss ClickHouse SELECT storm; finalized backfill from --restart is authoritative).
# V2 is the default pipeline; cold cache is on by default.
# Sanity: -$13.93 / ~$3.00.
dev-e2e-v2: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $(E2E_START_BLOCK) --end-block $(E2E_END_BLOCK) --restart $(POLYMARKET_ARGS)

dev-v2-e2e: dev-e2e-v2

db-reset:
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS $(CLICKHOUSE_DATABASE) SYNC"
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS polymarket SYNC"
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS uniswap SYNC"

stop:
	go run . stop

polymarket-tail: codegen-polymarket build
	@HEAD=$$(curl -s https://polygon-bor-rpc.publicnode.com -X POST -H "Content-Type: application/json" --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' | jq -r .result | xargs printf "%d") && \
	START_BLOCK=$$(($$HEAD - 2000)) && \
	echo "Tailing 2000 blocks from Polygon head (starting at $$START_BLOCK)..." && \
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $$START_BLOCK --end-block 0 --restart $(POLYMARKET_ARGS)

polymarket-fork: codegen-polymarket build
	@HEAD=$$(curl -s https://portal.sqd.dev/datasets/polygon-mainnet/head | jq -r .number) && \
	START_BLOCK=$$(($$HEAD - 6000)) && \
	echo "Tailing 6000 blocks from Polygon head (starting at $$START_BLOCK)..." && \
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) $(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $$START_BLOCK --end-block 0 --restart $(POLYMARKET_ARGS)

fetch-polymarket:
	go run debugger/fetchUntil.go \
		-endpoint https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream \
		-start 33605403 -end 40206663 \
		-out debugger/data/wallet_0xf05b67_full

inmem:
	go run debugger/inMemoryProcessor.go $(INMEM_ARGS)

mem:
	go run debugger/clickhouseProcessor.go $(MEMCH_ARGS)

memch:
	go run debugger/clickhouseProcessor.go $(MEMCH_ARGS)

# PnL comparison against Local CH, Remote CH, and Goldsky
pnl:
	@python3 scripts/ch_pnl.py $(WALLET)

# Run PnL comparison for all wallets in drafts/pnl_wallets.yaml
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
