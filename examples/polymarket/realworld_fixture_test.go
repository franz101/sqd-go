package polymarket

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/parser"
)

func TestRealWorldExchangeOrderFilledFixtureDecodesAndProcesses(t *testing.T) {
	data, err := os.ReadFile("../../sqd/testdata/exchange_orderfilled.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	state := generated.NewState()
	p := parser.NewFastJSONLParser(2048)
	var decodedOrders int
	checkedFirst := false

	if err := p.Parse(data, func(block *parser.Block) error {
		for _, lg := range block.Logs {
			decoded, err := generated.UnpackLog(lg.Address, lg.Topics, common.FromHex(lg.Data))
			if err != nil {
				return fmt.Errorf("decode block=%d tx=%s log=%d: %w", block.Header.Number, lg.TransactionHash, lg.LogIndex, err)
			}
			if decoded == nil {
				continue
			}
			ev, ok := decoded.Value.(*generated.ExchangeOrderFilled)
			if !ok {
				return fmt.Errorf("decoded %T from exchange_orderfilled fixture", decoded.Value)
			}
			decodedOrders++
			if !checkedFirst {
				checkedFirst = true
				if block.Header.Number != 78000000 || lg.LogIndex != 286 {
					return fmt.Errorf("first real order log moved: block=%d log=%d", block.Header.Number, lg.LogIndex)
				}
				if want := common.HexToAddress("0x492494973c94e901e3be9f75796dea83057cfac2"); ev.Maker != want {
					return fmt.Errorf("first real order maker = %s, want %s", ev.Maker, want)
				}
				if want := common.HexToAddress("0xba2c47e32555714e5dc3f623f9b1a1ade2fc050e"); ev.Taker != want {
					return fmt.Errorf("first real order taker = %s, want %s", ev.Taker, want)
				}
				if !ev.MakerAssetID.IsZero() || ev.MakerAmountFilled.String() != "4150000" || ev.TakerAmountFilled.String() != "5000000" {
					return fmt.Errorf("first real order amounts mismatch: makerAsset=%s makerFilled=%s takerFilled=%s", ev.MakerAssetID.String(), ev.MakerAmountFilled.String(), ev.TakerAmountFilled.String())
				}
			}
			handleOrderFilled(state, ev)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if decodedOrders != 9040 {
		t.Fatalf("decoded real exchange orders = %d, want 9040", decodedOrders)
	}
	var positions, nonZeroPositions int
	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, v generated.MemoryUserPosition) bool {
		positions++
		if !v.Amount.IsZero() {
			nonZeroPositions++
		}
		return true
	})
	if positions < 100 || nonZeroPositions == 0 {
		t.Fatalf("real fixture produced too few positions: positions=%d nonZero=%d", positions, nonZeroPositions)
	}
}

func TestRealWorldFiveBlockFixtureDecodesGeneratedEventsThroughRingBuffer(t *testing.T) {
	data, err := os.ReadFile("../../sqd/testdata/5blocks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ring, err := generated.NewOrderedHistoricRingBuffer(8)
	if err != nil {
		t.Fatal(err)
	}

	p := parser.NewFastJSONLParser(2048)
	blocks := 0
	counts := make(map[reflect.Type]int)
	if err := p.Parse(data, func(block *parser.Block) error {
		var decodedLogs []generated.DecodedLog
		for _, lg := range block.Logs {
			decoded, err := generated.UnpackLog(lg.Address, lg.Topics, common.FromHex(lg.Data))
			if err != nil {
				return fmt.Errorf("decode block=%d tx=%s log=%d: %w", block.Header.Number, lg.TransactionHash, lg.LogIndex, err)
			}
			if decoded == nil {
				continue
			}
			counts[reflect.TypeOf(decoded.Value)]++
			decodedLogs = append(decodedLogs, *decoded)
		}
		ring.Push(block.Header.Number, block.Header.Hash, decodedLogs)
		blocks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if blocks != 5 {
		t.Fatalf("decoded blocks = %d, want 5", blocks)
	}
	assertRealWorldDecodedCount(t, counts, &generated.ExchangeOrderFilled{}, 255)
	assertRealWorldDecodedCount(t, counts, &generated.ConditionalTokensPositionSplit{}, 182)
	assertRealWorldDecodedCount(t, counts, &generated.ConditionalTokensPayoutRedemption{}, 23)

	block, ok := ring.GetParsedBlock(82000004)
	if !ok {
		t.Fatal("expected last real fixture block in ring")
	}
	callbacks := 0
	for ev := range block.EventsIter() {
		switch ev.(type) {
		case *generated.ConditionalTokensConditionPreparation,
			*generated.ConditionalTokensConditionResolution,
			*generated.ConditionalTokensPositionSplit,
			*generated.ConditionalTokensPositionsMerge,
			*generated.ConditionalTokensPayoutRedemption,
			*generated.ExchangeOrderFilled,
			*generated.NegRiskExchangeOrderFilled,
			*generated.NegRiskAdapterMarketPrepared,
			*generated.NegRiskAdapterQuestionPrepared,
			*generated.NegRiskAdapterPositionSplit,
			*generated.NegRiskAdapterPositionsMerge,
			*generated.NegRiskAdapterPositionsConverted,
			*generated.NegRiskAdapterPayoutRedemption,
			*generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation,
			*generated.FixedProductMarketMakerFPMMBuy,
			*generated.FixedProductMarketMakerFPMMSell,
			*generated.FixedProductMarketMakerFPMMFundingAdded,
			*generated.FixedProductMarketMakerFPMMFundingRemoved:
			callbacks++
		}
	}
	if callbacks != len(block.Sequence) {
		t.Fatalf("reconstruct callbacks = %d, want sequence len %d", callbacks, len(block.Sequence))
	}
}

func assertRealWorldDecodedCount(t *testing.T, counts map[reflect.Type]int, sample any, want int) {
	t.Helper()
	if got := counts[reflect.TypeOf(sample)]; got != want {
		t.Fatalf("decoded %T count = %d, want %d", sample, got, want)
	}
}
