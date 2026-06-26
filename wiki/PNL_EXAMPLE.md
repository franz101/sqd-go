# PnL Example: ERC20 Token Balance Tracker

End-to-end example of tracking token balances and computing per-wallet profit/loss from `Transfer` events. Uses the `examples/uniswap/` project as reference.

## What It Does

For every `Transfer(from, to, value)` event on a token contract:
- Decrements sender's balance, accumulates total outflow
- Increments receiver's balance, accumulates total inflow
- Persists state to ClickHouse with snapshot-based fork recovery

The result is a `user_positions` table in ClickHouse with real-time balances and flow totals for every wallet that has ever touched the token.

## Step 1: Config

```yaml
# config.yaml
name: case_1_lbtc_event_only
chains:
  - id: 1
    start_block: 0
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
    Balance        uint256.Int
    TotalIn        uint256.Int
    TotalOut       uint256.Int
    UpdatedAtBlock uint64
    UpdatedAt      time.Time
}
```

The `// pk: Address` comment tells codegen that `Address` is the primary key. Codegen generates:

- ClickHouse table `user_positions` with columns: `address FixedString(20)`, `balance UInt256`, `total_in UInt256`, `total_out UInt256`, `updated_at_block UInt64`, `updated_at DateTime64(3)`
- Go type `generated.UserPosition` with the same fields
- Hot state map `state.UserPosition` with `Get(address)` and `Save(pos, meta)` methods
- Snapshot/restore/commit/prune lifecycle

## Step 3: Custom Processor

```go
// custom_processor.go
package uniswap

import (
    "github.com/ethereum/go-ethereum/common"
    generated "github.com/franz101/sqd-go/examples/uniswap/generated"
    "github.com/franz101/sqd-go/internal/cli"
    "github.com/franz101/sqd-go/internal/ingestion"
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

func init() {
    generated.CustomProcessFn = Process
    cli.RegisterProcessor(generated.ProjectName, func() (ingestion.Processor, error) {
        return generated.NewProcessor(cli.GetProtoMode())
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

Top holders by balance:

```sql
SELECT
  hex(address) as wallet,
  balance,
  total_in,
  total_out
FROM case_1_lbtc_event_only.user_positions
FINAL
ORDER BY balance DESC
LIMIT 20
```

Wallets with net outflow (potential sellers):

```sql
SELECT
  hex(address) as wallet,
  total_out - total_in as net_flow,
  balance
FROM case_1_lbtc_event_only.user_positions
FINAL
WHERE total_out > total_in
ORDER BY net_flow DESC
LIMIT 20
```

The `FINAL` keyword is important -- ClickHouse uses `ReplacingMergeTree` and `FINAL` deduplicates rows that haven't been merged yet.

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
                        Every 100000 blocks: Prune old rows
```

1. **Fetch**: SQD portal delivers finalized blocks as zstd-compressed JSONL
2. **Parse**: FastJSONParser extracts per-block log arrays
3. **Decode**: `UnpackLogWithMeta` matches topic0 to `Transfer` signature, returns typed `*LBTCTransfer`
4. **Ring buffer**: Block events are pushed into a 1024-slot ring buffer for fork recovery
5. **Process**: Your `Process` function runs per block with the typed `ParsedBlock`
6. **State**: CLOCK cache maps with O(1) get/save. On cache eviction, cold tier (Pebble) serves the entry
7. **Commit**: Dirty entities flushed to ClickHouse via native protocol (async insert)
8. **Prune**: Old `ReplacingMergeTree` rows cleaned up periodically

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
CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.`user_positions` (
  `address` FixedString(20),
  `balance` UInt256,
  `total_in` UInt256,
  `total_out` UInt256,
  `updated_at_block` UInt64,
  `updated_at` DateTime64(3, 'UTC') DEFAULT now64(3),
  `block_number` UInt64,
  `transaction_index` UInt64,
  `log_index` UInt64
) ENGINE = ReplacingMergeTree(block_number)
PRIMARY KEY (`address`)
ORDER BY (`address`, `block_number`, `transaction_index`, `log_index`);
```

The `ReplacingMergeTree(block_number)` engine keeps the latest row per primary key after compaction. Multiple updates to the same address during indexing produce multiple rows; `FINAL` or background merges collapse them to the latest.
