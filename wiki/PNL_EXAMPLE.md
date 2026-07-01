# PnL Example: ERC20 Token Balance Tracker

End-to-end example of tracking token balances and computing per-wallet profit/loss from `Transfer` events. Uses the `examples/uniswap/` project as reference.

## What It Does

For every `Transfer(from, to, value)` event on a token contract:
- Decrements sender's balance, accumulates total outflow
- Increments receiver's balance, accumulates total inflow
- Persists state to ClickHouse with snapshot-based fork recovery

The result is a `user_positions_log` history table in ClickHouse (plus a
`user_positions_live` view for current state) with balances and flow totals for
every wallet that has ever touched the token.

## Step 1: Config

```yaml
# config.yaml
name: case_1_lbtc_event_only
chains:
  - id: 1
    # LBTC was deployed around block 20.5M; use a post-deployment range.
    start_block: 20600000
    end_block: 22200000
    contracts:
      - name: LBTC
        address: "0x8236a87084f8B84306f72007F36F2618A5634494"
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
```

One contract, one event, one chain. The `name` field becomes the ClickHouse database.

## Step 2: Custom Schema

```go
// custom_schema.go
package uniswap

import (
    "time"

    "github.com/ethereum/go-ethereum/common"
    "github.com/holiman/uint256"
)

// pk: Address
type UserPositionSchema struct {
    Address        common.Address
    TotalIn        uint256.Int
    TotalOut       uint256.Int
    TransferCount  uint64
    UpdatedAtBlock uint64
    UpdatedAt      time.Time
}
```

Balance is derived on read as `total_in - total_out` rather than stored, so
there is no separate `balance` column.

The `// pk: Address` comment tells codegen that `Address` is the primary key
(the pk/sk source order is: `// pk:` comment, then a matching config.yaml
`state:` key, then an `ID` field, then the first field). Codegen generates:

- ClickHouse history table `user_positions_log` (plus the `user_positions_live` view) with columns: `address FixedString(20)`, `total_in UInt256`, `total_out UInt256`, `transfer_count UInt64`, `updated_at_block UInt64`, `updated_at DateTime64(3)`
- Go type `generated.UserPosition` with the same fields
- Hot state map `state.UserPosition` with `Get(address)` and `Save(pos, meta)` methods
- Snapshot/restore/commit/prune lifecycle

## Step 3: Custom Processor

```go
// custom_processor.go
package uniswap // SAME package name as custom_schema.go

import (
    "github.com/ethereum/go-ethereum/common"
    generated "github.com/franz101/sqd-go/examples/uniswap/generated"

    "github.com/franz101/sqd-go/sqd" // PUBLIC facade — never import internal/
)

func Process(state *generated.State, block *generated.ParsedBlock) error {
    for ev := range block.EventsIter() {
        switch e := ev.(type) {
        case *generated.LBTCTransfer:
            zero := common.Address{}

            // Debit sender
            if e.From != zero {
                pos, ok := state.UserPosition.Get(e.From)
                if !ok {
                    pos = &generated.UserPosition{
                        Address: e.From,
                    }
                }
                pos.Balance.Sub(&pos.Balance, &e.Value)
                pos.TotalOut.Add(&pos.TotalOut, &e.Value)
                state.UserPosition.Save(pos, e.EventMeta)
            }

            // Credit receiver
            if e.To != zero {
                pos, ok := state.UserPosition.Get(e.To)
                if !ok {
                    pos = &generated.UserPosition{
                        Address: e.To,
                    }
                }
                pos.Balance.Add(&pos.Balance, &e.Value)
                pos.TotalIn.Add(&pos.TotalIn, &e.Value)
                state.UserPosition.Save(pos, e.EventMeta)
            }
        }
    }
    return nil
}

func ProcessProto(state *generated.State, block *generated.ProtoEventBlock) error {
    return Process(state, block.ToParsedBlock())
}

func init() {
    generated.CustomProcessFn = Process
    generated.CustomProcessProtoFn = ProcessProto
    sqd.RegisterProcessor(generated.ProjectName, func() (sqd.Processor, error) {
        return generated.NewProcessor(sqd.GetProtoMode())
    })
}
```

### Key Patterns

**Get-or-create**: `state.UserPosition.Get(key)` returns `(entity, found)`. If not found, create a zero-value struct with the key field set.

**In-place mutation**: `uint256.Int` methods mutate in place. `pos.Balance.Sub(&pos.Balance, &e.Value)` subtracts value from the existing balance.

**Save with metadata**: `state.UserPosition.Save(pos, e.EventMeta)` marks the entity dirty and records the block/tx/log ordering. The framework commits dirty entities to ClickHouse at `STATE_SNAPSHOT_INTERVAL` blocks (default 4000).

**Skip zero address**: `Transfer` events with `from = 0x0` are mints, `to = 0x0` are burns. Skip the zero-address side to avoid a synthetic wallet.

## Step 4: Run

```bash
# Build and start
go build -o tmp/main .
CLICKHOUSE_DATABASE=case_1_lbtc_event_only tmp/main start examples/uniswap --restart
```

Or via the Makefile:

```bash
make dev-polymarket
```

## Step 5: Query Results

Each custom table is generated as a pair: an append-only history table suffixed
`_log` (one row per commit) and a `_live` **view** that resolves to the current
row per primary key (latest `block_number, transaction_index, log_index` wins).
Query `_live` for current state; query `_log` for history/time-series.

Top holders by balance:

```sql
SELECT
  hex(address) as wallet,
  toInt256(total_in) - toInt256(total_out) as balance,
  total_in,
  total_out
FROM case_1_lbtc_event_only.user_positions_live
ORDER BY balance DESC
LIMIT 20
```

Wallets with the most cumulative outflow:

```sql
SELECT
  hex(address) as wallet,
  total_out,
  toInt256(total_in) - toInt256(total_out) as balance
FROM case_1_lbtc_event_only.user_positions_live
ORDER BY total_out DESC
LIMIT 20
```

The `_live` view does the deduplication for you (`ORDER BY address,
block_number DESC, ... LIMIT 1 BY address`), so no `FINAL` is needed — and no
`FINAL` on the `_log` table either: it is a plain `MergeTree` history, so
`FINAL` there would NOT collapse rows (the sort key includes
`block_number/transaction_index/log_index`). Always read current state through
`_live`.

## How It Works Under the Hood

```
SQD HTTP → zstd JSONL → Parse → Decode Transfer events
                                       ↓
                                 Ring Buffer (1024 blocks)
                                       ↓
                              ParsedBlock per block
                                       ↓
                              Process(state, block)
                                       ↓
                           state.UserPosition.Get / Save
                                       ↓
                        Every 4000 blocks: Commit to ClickHouse
                                       ↓
                        Every CLICKHOUSE_PRUNE_INTERVAL blocks: Prune _log to
                        one snapshot per (pk, N-block bucket)
```

1. **Fetch**: SQD portal delivers finalized blocks as zstd-compressed JSONL
2. **Parse**: FastJSONParser extracts per-block log arrays
3. **Decode**: `UnpackLogWithMeta` matches topic0 to `Transfer` signature, returns typed `*LBTCTransfer`
4. **Ring buffer**: Block events are pushed into a 1024-slot ring buffer for fork recovery
5. **Process**: Your `Process` function runs per block with the typed `ParsedBlock`
6. **State**: CLOCK cache maps with O(1) get/save. On cache eviction, cold tier (Pebble) serves the entry
7. **Commit**: Dirty entities flushed to ClickHouse via native protocol (async insert)
8. **Prune**: Every `CLICKHOUSE_PRUNE_INTERVAL` blocks the `_log` table is compacted to one snapshot per `(primary key, intDiv(block_number, interval))` bucket — bounding growth while keeping a block-bucketed history for points/time-series. Only rows more than 1000 blocks below the sync head are touched, so pruning never crosses the finalized head.

## Recent Enhancements (2026)

### Improved State Management

- **Bounded Pruning** - State pruning now uses windowed operations to prevent ClickHouse OOM during mutations
- **Disk Spillover** - Large aggregation operations can temporarily spill to disk for memory management
- **Better Recovery** - Provisional checkpoint persistence at reindex floor enables safer recovery after failures

### Performance Optimizations

- **Zero-Allocation Paths** - Hot state operations are verified zero-allocation for maximum throughput
- **CLOCK Cache** - Improved cache eviction policies for better hit rates on hot entities
- **Snapshot Optimization** - Reduced memory footprint during snapshot operations

### Enhanced Error Handling

- **Collateral Validation** - For Polymarket processors, automatic collateral validation prevents scaling errors
- **Decimal Precision** - Improved decimal handling for financial calculations using `protomath.Decimal256`
- **Type Safety** - Enhanced type checking for schema definitions to prevent runtime errors

## Recent Enhancements (2026)

### Improved State Management

- **Bounded Pruning** - State pruning now uses windowed operations to prevent ClickHouse OOM during mutations
- **Disk Spillover** - Large aggregation operations can temporarily spill to disk for memory management
- **Better Recovery** - Provisional checkpoint persistence at reindex floor enables safer recovery after failures

### Performance Optimizations

- **Zero-Allocation Paths** - Hot state operations are verified zero-allocation for maximum throughput
- **CLOCK Cache** - Improved cache eviction policies for better hit rates on hot entities
- **Snapshot Optimization** - Reduced memory footprint during snapshot operations

### Enhanced Error Handling

- **Collateral Validation** - For Polymarket processors, automatic collateral validation prevents scaling errors
- **Decimal Precision** - Improved decimal handling for financial calculations using `protomath.Decimal256`
- **Type Safety** - Enhanced type checking for schema definitions to prevent runtime errors

## Extending to Full PnL

For real PnL (realized profit/loss with average cost tracking), add `AvgPrice`, `RealizedPnL`, and `TotalBought` fields to the schema and implement weighted-average cost basis:

```go
package uniswap

import (
    "time"

    "github.com/ethereum/go-ethereum/common"
    "github.com/franz101/sqd-go/protomath"
)

// pk: User, TokenID
type MemoryUserPositionSchema struct {
    User           common.Address
    TokenID        common.Hash
    Amount         protomath.Decimal256     // current position size
    AvgPrice       protomath.Decimal256     // weighted average entry price
    RealizedPnL    protomath.Decimal256     // cumulative realized PnL
    TotalBought    protomath.Decimal256     // lifetime buy volume
    UpdatedAtBlock uint64
    UpdatedAt      time.Time
}
```

Buy logic (weighted average):

```go
import "github.com/shopspring/decimal"

func updateAvgPrice(currentAvg, currentAmt, newPrice, newAmt decimal.Decimal) decimal.Decimal {
    denom := currentAmt.Add(newAmt)
    if denom.IsZero() {
        return currentAvg
    }
    numer := currentAvg.Mul(currentAmt).Add(newPrice.Mul(newAmt))
    return numer.Div(denom)
}
```

Sell logic (realize PnL):

```go
pnl := sellAmount.Mul(sellPrice.Sub(avgPrice))
position.RealizedPnL = position.RealizedPnL.Add(pnl)
position.Amount = position.Amount.Sub(sellAmount)
```

The Polymarket example in `examples/polymarket/` implements full PnL with this pattern across multiple contract types (CLOB exchange, AMM, conditional tokens, neg-risk adapter).

## Generated ClickHouse DDL

For the simple balance tracker:

```sql
CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.`user_positions_log` (
  `address` FixedString(20),
  `total_in` UInt256,
  `total_out` UInt256,
  `transfer_count` UInt64,
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = MergeTree()
PRIMARY KEY (`address`)
ORDER BY (`address`, `block_number`, `transaction_index`, `log_index`);

CREATE VIEW IF NOT EXISTS `case_1_lbtc_event_only`.`user_positions_live` AS
SELECT * FROM `case_1_lbtc_event_only`.`user_positions_log`
ORDER BY `address`, block_number DESC, transaction_index DESC, log_index DESC
LIMIT 1 BY `address`;
```

The physical `_log` table is a plain `MergeTree` append-only history: the
`PRIMARY KEY` is the pk/sk columns only, while `block_number`,
`transaction_index`, and `log_index` appear only in `ORDER BY` (and in the
`_live` view's filter) — never in the primary key. Every commit appends a row,
so an address touched N times has up to N rows. The paired `_live` view
resolves to the single latest row per primary key via `LIMIT 1 BY` — query it
for current balances; query `_log` when you need the full history (e.g. points
accrual over time). Periodic pruning bounds `_log` to one snapshot per
`(address, N-block bucket)` where `N = CLICKHOUSE_PRUNE_INTERVAL`.
