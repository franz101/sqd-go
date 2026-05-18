BUILD_DIR := tmp

.PHONY: dev-build build codegen-uniswap start-uniswap dev-uniswap restart-uniswap db-reset stop

CLICKHOUSE_CONTAINER ?= sqd-go-clickhouse-1
CLICKHOUSE_PASSWORD ?= sqd-clickhouse
CLICKHOUSE_DATABASE ?= case_1_lbtc_event_only

dev-build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/main .

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/sqd-go .

# Uniswap example
UNISWAP_DIR := examples/uniswap

codegen-uniswap:
	go run . codegen $(UNISWAP_DIR)

start-uniswap:
	go run . start $(UNISWAP_DIR)

dev-uniswap:
	go run . dev $(UNISWAP_DIR)

restart-uniswap:
	go run . start $(UNISWAP_DIR) --restart

db-reset:
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS $(CLICKHOUSE_DATABASE) SYNC"

stop:
	go run . stop

polymarket-tail: build
	@HEAD=$$(curl -s https://polygon-bor-rpc.publicnode.com -X POST -H "Content-Type: application/json" --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' | jq -r .result | xargs printf "%d") && \
	START_BLOCK=$$(($$HEAD - 2000)) && \
	echo "Tailing 2000 blocks from Polygon head (starting at $$START_BLOCK)..." && \
	$(BUILD_DIR)/sqd-go start examples/polymarket --blockchain polygon --start-block $$START_BLOCK --end-block 0 --restart
