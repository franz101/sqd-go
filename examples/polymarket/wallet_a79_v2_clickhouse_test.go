package polymarket

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/shopspring/decimal"
)

// TestWalletA79V2ClickHouse drives the real V2 proto processor
// (NewProcessor(true) -> proto ring -> CustomProcessingProto -> ClickHouse commit)
// against the complete wallet fixture, then reads positions back from ClickHouse
// and verifies parity with V1: realized -$13.93, open value ~$3.00.
//
// This is the V2 correctness gate. V1 (TestWalletA79V1ClickHouse) already passes;
// this test localizes any proto-path divergence.
func TestWalletA79V2ClickHouse(t *testing.T) {
	ctx := context.Background()
	store := newPolymarketIntegrationStore(t, ctx)

	wallet := common.HexToAddress("0xa79af3bab636f41f1f7bd1c568857dbdf4650beb")
	logs := loadWalletCustomLogs(t, "../../tests/wallet_0xa79af3b_all.jsonl")
	t.Logf("loaded %d custom logs", len(logs))

	proc, err := generated.NewProcessor(true) // V2 proto mode
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	if err := proc.Process(ctx, store, logs); err != nil {
		t.Fatalf("processor.Process: %v", err)
	}
	// Flush any state not yet committed by the periodic (every-1000-block) commit.
	if err := proc.State.Commit(ctx, store); err != nil {
		t.Fatalf("final commit: %v", err)
	}

	// Read positions back from ClickHouse into a fresh cache.
	fresh := generated.NewState()
	if err := fresh.HotState.UserPositions.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover positions from clickhouse: %v", err)
	}

	realized := decimal.Zero
	openValueHalf := decimal.Zero
	var n, nonzero int
	fresh.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.User != wallet {
			return true
		}
		n++
		realized = realized.Add(toDecimal(pos.RealizedPnL))
		if !pos.Amount.IsZero() {
			nonzero++
			openValueHalf = openValueHalf.Add(toDecimal(pos.Amount).Mul(decimal.NewFromFloat(0.5)))
		}
		return true
	})

	pnl := realized
	openVal := openValueHalf
	t.Logf("[V2 ClickHouse] positions=%d nonzero=%d realized=$%s open=$%s",
		n, nonzero, pnl.StringFixed(4), openVal.StringFixed(4))

	if n == 0 {
		t.Fatal("no positions read back from ClickHouse for wallet")
	}
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("V2 ClickHouse realized PnL = %s, want -13.93", pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("V2 ClickHouse open positions value = %s, want 3.00", openVal.StringFixed(4))
	}
}
