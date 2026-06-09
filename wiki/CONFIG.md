# config.yaml Reference

The `config.yaml` file defines what your indexer tracks: which chain, which contracts, which events, and how state is managed.

## Full Schema

```yaml
# Required. Used as ClickHouse database name and processor lookup key.
name: uniswap_v2

# Optional. Only "evm" is supported.
ecosystem: evm

# Optional. Store raw log hex data in a separate table.
store_raw_logs: false

# Optional. Store block headers in a separate table.
store_blocks: false

# Optional. Include extra metadata columns on typed event tables.
# By default only block_number, block_timestamp, transaction_index, log_index are included.
include_metadata:
  - chain_id
  - block_hash
  - contract_address
  - transaction_hash

# Optional. Drop specific fields from typed event tables.
# Format: EventName: fieldName
exclude_metadata:
  - OrderFilled: orderhash

# Optional. Hot state declarations for custom schema entities.
# Links a custom_schema.go struct to the hot state system for in-memory
# caching, ClickHouse persistence, and snapshot-based fork recovery.
state:
  - name: Position                      # must match struct name minus "Schema" suffix
    source_table: memory_user_positions  # ClickHouse table name (default: auto from struct name)
    key:                                 # primary key fields used for Get() lookups and ClickHouse ORDER BY
      - User
      - TokenID

# Required. At least one chain.
chains:
  - id: 1                               # 1 = Ethereum, 137 = Polygon
    start_block: 10000835                # Uniswap V2 Router deploy block
    contracts:
      - name: UniswapV2Router
        address: "0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"
        events:
          - event: Swap(address indexed sender, uint256 amount0In, uint256 amount1In, uint256 amount0Out, uint256 amount1Out, address indexed to)
      - name: UniswapV2Factory
        address: "0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"
        events:
          - event: PairCreated(address indexed token0, address indexed token1, address pair, uint256 allPairsLength)
```

## Fields

### Top-Level

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | -- | Project name. Becomes the ClickHouse database name. |
| `ecosystem` | string | no | `"evm"` | Only `"evm"` supported. |
| `store_raw_logs` | bool | no | `false` | Create a `raw_logs` table with hex-encoded log data. |
| `store_blocks` | bool | no | `false` | Create a `blocks` table with block headers. |
| `include_metadata` | string[] | no | `[]` | Extra metadata columns on event tables. |
| `exclude_metadata` | map[] | no | `[]` | Drop fields from event tables. Key = event name, value = field name. |
| `state` | StateConfig[] | no | `[]` | Hot state entity declarations. |

### `chains[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | uint64 | yes | -- | EVM chain ID. `1` = Ethereum, `137` = Polygon. |
| `start_block` | uint64 | yes | `0` | First block to index. |
| `end_block` | uint64 | no | infinite | Last block to index. Omit or set 0 for live tailing. |
| `contracts` | Contract[] | yes | -- | Contracts to index on this chain. |

### `chains[].contracts[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | -- | Contract name. Used in table names and Go type prefixes. |
| `address` | string or string[] | no | -- | Contract address(es). Omit for factory-pattern contracts that emit events from dynamic addresses. |
| `events` | EventConfig[] | yes | -- | Events to index. |

### `chains[].contracts[].events[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `event` | string | yes | -- | Solidity event signature. Must include parameter names. |
| `name` | string | no | auto | Override the event name used in table/type generation. |
| `omit` | string[] | no | `[]` | Fields to exclude from the typed table. |

### `state[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | -- | Entity name matching `custom_schema.go` struct (minus `Schema` suffix). |
| `source_table` | string | no | auto | Override the ClickHouse table name. Default is pluralized snake_case of name. |
| `key` | string[] | no | auto | Primary key for this entity. Used for `Get()` lookups, ClickHouse `ORDER BY`, and prefetch queries. Must match the `// pk:` comment on your schema struct. |

## Event Signature Format

Event signatures follow the Solidity ABI format:

```
EventName(type1 indexed param1, type2 param2, ...)
```

Supported types: `address`, `uint256`, `int256`, `bool`, `string`, `bytes`, `bytes32`, all `uint`/`int` sizes, and arrays of these (`uint256[]`, `bytes32[]`).

The `indexed` keyword must appear for indexed parameters. Parameter names are required.

## Address Formats

Single address:

```yaml
address: "0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"
```

Multiple addresses (same contract ABI deployed at multiple addresses):

```yaml
# Polymarket CLOB Exchange + NegRisk Exchange share the same OrderFilled event
address:
  - "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
  - "0xC5d563A36AE78145C45a50134d48A1215220f80a"
```

No address (factory pattern -- index events from any address matching the topic0):

```yaml
# Uniswap V2 pairs are deployed dynamically by the factory
- name: UniswapV2Pair
  events:
    - event: Swap(address indexed sender, uint256 amount0In, uint256 amount1In, uint256 amount0Out, uint256 amount1Out, address indexed to)
    - event: Sync(uint112 reserve0, uint112 reserve1)
```

## Minimal Example

```yaml
name: usdc_transfers
chains:
  - id: 1
    start_block: 6082465
    contracts:
      - name: USDC
        address: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
```

## Multi-Contract Example (Polymarket on Polygon)

```yaml
name: polymarket
ecosystem: evm
store_blocks: false
store_raw_logs: false
exclude_metadata:
  - OrderFilled: orderhash
state:
  - name: Condition
    source_table: memory_conditions
    key:                          # primary key: single field
      - ID
  - name: Position
    source_table: memory_user_positions
    key:                          # composite primary key: two fields
      - User
      - TokenID
  - name: NegRiskEvent
    source_table: memory_neg_risk_events
    key:
      - ID
chains:
  - id: 137                      # Polygon
    start_block: 33605403
    contracts:
      - name: ConditionalTokens
        address: "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"
        events:
          - event: ConditionPreparation(bytes32 indexed conditionId, address indexed oracle, bytes32 indexed questionId, uint256 outcomeSlotCount)
          - event: ConditionResolution(bytes32 indexed conditionId, address indexed oracle, bytes32 indexed questionId, uint256 payoutDenominator, uint256[] payoutNumerators)
          - event: PositionSplit(address indexed stakeholder, address collateralToken, bytes32 indexed parentCollectionId, bytes32 indexed conditionId, uint256[] partition, uint256 amount)
          - event: PositionsMerge(address indexed stakeholder, address collateralToken, bytes32 indexed parentCollectionId, bytes32 indexed conditionId, uint256[] partition, uint256 amount)
          - event: PayoutRedemption(address indexed redeemer, address indexed collateralToken, bytes32 indexed parentCollectionId, bytes32 conditionId, uint256[] indexSets, uint256 payout)
      - name: Exchange
        address: "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint256 makerAssetId, uint256 takerAssetId, uint256 makerAmountFilled, uint256 takerAmountFilled, uint256 fee)
      - name: NegRiskAdapter
        address: "0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296"
        events:
          - event: MarketPrepared(bytes32 indexed marketId, address indexed creator, uint256 feeBips, bytes data)
          - event: QuestionPrepared(bytes32 indexed marketId, bytes32 indexed questionId, uint256 index, bytes data)
          - event: PositionSplit(address indexed stakeholder, bytes32 indexed conditionId, uint256 amount)
          - event: PositionsMerge(address indexed stakeholder, bytes32 indexed conditionId, uint256 amount)
      - name: FixedProductMarketMaker
        events:
          - event: FPMMBuy(address indexed buyer, uint256 investmentAmount, uint256 feeAmount, uint256 indexed outcomeIndex, uint256 outcomeTokensBought)
          - event: FPMMSell(address indexed seller, uint256 returnAmount, uint256 feeAmount, uint256 indexed outcomeIndex, uint256 outcomeTokensSold)
```

## Override via CLI

CLI flags override config values:

```bash
sqd-go start examples/polymarket/ \
  --blockchain polygon \
  --start-block 80000000 \
  --end-block 80001000 \
  --restart
```

Override via environment:

```bash
CLICKHOUSE_DATABASE=polymarket_dev sqd-go start examples/polymarket/
```
