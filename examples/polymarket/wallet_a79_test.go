package polymarket

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/shopspring/decimal"
)

// Regression: wallet 0xa79af3bab636f41f1f7bd1c568857dbdf4650beb
// Expected full PnL = -$13.93, open positions value ~$3.00 (Polymarket API).
func TestWalletA79V1PnL(t *testing.T) {
	wallet := common.HexToAddress("0xa79af3bab636f41f1f7bd1c568857dbdf4650beb")
	data, err := os.ReadFile("../../tests/wallet_0xa79af3b_all.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	ring, err := generated.NewOrderedHistoricRingBuffer(8192)
	if err != nil {
		t.Fatal(err)
	}
	state := generated.NewState()
	p := parser.NewFastJSONLParser(2048)

	var group []generated.DecodedLog
	var curBlock uint64
	var curHash string
	flush := func() {
		if len(group) == 0 {
			return
		}
		ring.Push(curBlock, curHash, group)
		block, ok := ring.GetParsedBlock(curBlock)
		if !ok {
			t.Fatalf("block %d not found", curBlock)
		}
		if err := generated.CustomProcessing(context.Background(), generated.Store(nil), state, block); err != nil {
			t.Fatalf("custom processing block %d: %v", curBlock, err)
		}
		group = group[:0]
	}

	if err := p.Parse(data, func(block *parser.Block) error {
		for _, lg := range block.Logs {
			meta := generated.EventMeta{
				BlockNumber:      block.Header.Number,
				BlockTimestamp:   time.Unix(int64(block.Header.Timestamp), 0).UTC(),
				BlockHash:        common.HexToHash(block.Header.Hash),
				ContractAddress:  common.HexToAddress(lg.Address),
				TransactionHash:  common.HexToHash(lg.TransactionHash),
				TransactionIndex: lg.TransactionIndex,
				LogIndex:         lg.LogIndex,
			}
			decoded, err := generated.UnpackLogWithMeta(lg.Address, lg.Topics, common.FromHex(lg.Data), meta)
			if err != nil || decoded == nil || decoded.Value == nil {
				continue
			}
			if len(group) > 0 && block.Header.Number != curBlock {
				flush()
			}
			if len(group) == 0 {
				curBlock = block.Header.Number
				curHash = block.Header.Hash
			}
			group = append(group, *decoded)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	flush()

	million := decimal.NewFromInt(1_000_000)
	realized := decimal.Zero
	openValueHalf := decimal.Zero // value @ $0.50 marker
	var n, nonzero int
	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.User != wallet {
			return true
		}
		n++
		realized = realized.Add(toDecimal(pos.RealizedPnL))
		if !pos.Amount.IsZero() {
			nonzero++
			openValueHalf = openValueHalf.Add(toDecimal(pos.Amount).Mul(decimal.NewFromFloat(0.5)))
		}
		t.Logf("pos token=%s amt=%s avg=%s pnl=%s",
			pos.TokenID.Hex()[:12],
			toDecimal(pos.Amount).Div(million).StringFixed(4),
			toDecimal(pos.AvgPrice).StringFixed(6),
			toDecimal(pos.RealizedPnL).Div(million).StringFixed(4))
		return true
	})

	pnl := realized.Div(million)
	openVal := openValueHalf.Div(million)
	t.Logf("positions=%d nonzero=%d", n, nonzero)
	t.Logf("Realized PnL = $%s (expected -13.93)", pnl.StringFixed(4))
	t.Logf("Open value @0.50 = $%s (expected 3.00)", openVal.StringFixed(4))

	// Polymarket API: full PnL -$13.93, open positions value ~$3.00.
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("realized PnL = %s, want -13.93", pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("open positions value = %s, want 3.00", openVal.StringFixed(4))
	}
}
