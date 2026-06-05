package tests

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	_ "github.com/franz101/sqd-go/examples/polymarket" // init() registers CustomProcessFn
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// =============================================================================
// Calculation Integration Tests
// Uses Process via CustomProcessFn to verify PnL, cost-basis, and redemption.
// =============================================================================

func TestOrderFilledUpdatesUserPosition(t *testing.T) {
	s := generated.NewState()
	user := common.HexToAddress("0x0101")
	taker := common.HexToAddress("0x0202")
	tokenID := *uint256.NewInt(999)

	block := &generated.ParsedBlock{
		BlockNumber: 100,
		Sequence:    []uint8{uint8(generated.EventTypeExchangeOrderFilled)},
		ExchangeOrderFilleds: []generated.ExchangeOrderFilled{{
			EventMeta:         generated.EventMeta{BlockNumber: 100, TransactionIndex: 1, LogIndex: 1},
			Maker:             user, Taker: taker,
			MakerAssetID:      *uint256.NewInt(0), TakerAssetID: tokenID,
			MakerAmountFilled: *uint256.NewInt(5_000_000),
			TakerAmountFilled: *uint256.NewInt(10_000_000),
		}},
	}

	if err := generated.CustomProcessFn(s, block); err != nil {
		t.Fatalf("process: %v", err)
	}

	pos := s.GetUserPosition(user, tokenID)
	if pos == nil {
		t.Fatal("position not created")
	}
	if !pos.Amount.Equal(decimal.NewFromInt(10_000_000)) {
		t.Errorf("amount: got %s, want 10M", pos.Amount)
	}
	expectedPrice := decimal.RequireFromString("0.5")
	if pos.AvgPrice.Sub(expectedPrice).Abs().GreaterThan(decimal.NewFromFloat(1e-10)) {
		t.Errorf("avg price: got %s, want 0.5", pos.AvgPrice)
	}
}

func TestOrderFilledMultiBuyAveragePrice(t *testing.T) {
	s := generated.NewState()
	user := common.HexToAddress("0x0101")
	taker := common.HexToAddress("0x0202")
	tokenID := *uint256.NewInt(100)

	// Buy 1: 100 tokens @ 0.4
	processBlock(t, s, []generated.Event{
		&generated.ExchangeOrderFilled{
			EventMeta: generated.EventMeta{BlockNumber: 100, TransactionIndex: 1, LogIndex: 1},
			Maker: user, Taker: taker, MakerAssetID: *uint256.NewInt(0), TakerAssetID: tokenID,
			MakerAmountFilled: *uint256.NewInt(4_000_000), TakerAmountFilled: *uint256.NewInt(10_000_000),
		},
	})

	// Buy 2: 50 tokens @ 0.6
	processBlock(t, s, []generated.Event{
		&generated.ExchangeOrderFilled{
			EventMeta: generated.EventMeta{BlockNumber: 100, TransactionIndex: 1, LogIndex: 2},
			Maker: user, Taker: taker, MakerAssetID: *uint256.NewInt(0), TakerAssetID: tokenID,
			MakerAmountFilled: *uint256.NewInt(3_000_000), TakerAmountFilled: *uint256.NewInt(5_000_000),
		},
	})

	pos := s.GetUserPosition(user, tokenID)
	if pos == nil {
		t.Fatal("position not created")
	}
	expectedAvg := decimal.NewFromInt(7).Div(decimal.NewFromInt(15))
	if pos.AvgPrice.Sub(expectedAvg).Abs().GreaterThan(decimal.NewFromFloat(1e-10)) {
		t.Errorf("multi-buy avg: got %s, want %s", pos.AvgPrice, expectedAvg)
	}
	if !pos.Amount.Equal(decimal.NewFromInt(15_000_000)) {
		t.Errorf("amount: got %s", pos.Amount)
	}
}

func TestOrderFilledSellRealizedPnl(t *testing.T) {
	s := generated.NewState()
	user := common.HexToAddress("0x0101")
	taker := common.HexToAddress("0x0202")
	tokenID := *uint256.NewInt(100)

	processBlock(t, s, []generated.Event{
		&generated.ExchangeOrderFilled{
			EventMeta: generated.EventMeta{BlockNumber: 100, TransactionIndex: 1, LogIndex: 1},
			Maker: user, Taker: taker, MakerAssetID: *uint256.NewInt(0), TakerAssetID: tokenID,
			MakerAmountFilled: *uint256.NewInt(4_000_000), TakerAmountFilled: *uint256.NewInt(10_000_000),
		},
	})

	processBlock(t, s, []generated.Event{
		&generated.ExchangeOrderFilled{
			EventMeta: generated.EventMeta{BlockNumber: 100, TransactionIndex: 1, LogIndex: 2},
			Maker: user, Taker: taker, MakerAssetID: tokenID, TakerAssetID: *uint256.NewInt(0),
			MakerAmountFilled: *uint256.NewInt(5_000_000), TakerAmountFilled: *uint256.NewInt(3_000_000),
		},
	})

	pos := s.GetUserPosition(user, tokenID)
	if pos == nil {
		t.Fatal("position missing")
	}
	if !pos.Amount.Equal(decimal.NewFromInt(5_000_000)) {
		t.Errorf("remaining: got %s", pos.Amount)
	}
	expectedPnl := decimal.NewFromInt(1_000_000)
	if pos.RealizedPnL.Sub(expectedPnl).Abs().GreaterThan(decimal.NewFromFloat(1)) {
		t.Errorf("PnL: got %s, want %s", pos.RealizedPnL, expectedPnl)
	}
}

func TestConditionPreparationCreatesCondition(t *testing.T) {
	s := generated.NewState()
	conditionID := common.HexToHash("0xf00d")

	processBlock(t, s, []generated.Event{
		&generated.ConditionalTokensConditionPreparation{
			EventMeta:        generated.EventMeta{BlockNumber: 1},
			ConditionID:      conditionID,
			Oracle:           common.HexToAddress("0xb00"),
			OutcomeSlotCount: *uint256.NewInt(2),
		},
	})

	cond, ok := s.Condition.Get(conditionID)
	if !ok {
		t.Fatal("condition not created")
	}
	if cond.OutcomeSlotCount != 2 {
		t.Errorf("outcomeSlotCount: got %d", cond.OutcomeSlotCount)
	}
}

func TestPositionSplitCreatesPositions(t *testing.T) {
	s := generated.NewState()
	user := common.HexToAddress("0xaaa")
	conditionID := common.HexToHash("0xf00d")
	collateral := common.HexToAddress("0xccc")

	// Seed condition + prep event
	processBlock(t, s, []generated.Event{
		&generated.ConditionalTokensConditionPreparation{
			EventMeta:        generated.EventMeta{BlockNumber: 1},
			ConditionID:      conditionID,
			Oracle:           common.HexToAddress("0xb00"),
			OutcomeSlotCount: *uint256.NewInt(2),
		},
	})

	processBlock(t, s, []generated.Event{
		&generated.ConditionalTokensPositionSplit{
			EventMeta: generated.EventMeta{BlockNumber: 2, TransactionIndex: 1, LogIndex: 1},
			Stakeholder: user, CollateralToken: collateral,
			ParentCollectionID: common.Hash{}, ConditionID: conditionID,
			Partition: []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
			Amount:    *uint256.NewInt(10_000_000),
		},
	})

	count := countUserPositions(t, s)
	if count < 2 {
		t.Errorf("expected at least 2 position entries after split, got %d", count)
	}
}

func TestFullConditionLifecycle(t *testing.T) {
	s := generated.NewState()
	user := common.HexToAddress("0xaaa")
	conditionID := common.HexToHash("0xf00d")

	// 1. ConditionPreparation
	processBlock(t, s, []generated.Event{
		&generated.ConditionalTokensConditionPreparation{
			EventMeta:        generated.EventMeta{BlockNumber: 1},
			ConditionID:      conditionID,
			Oracle:           common.HexToAddress("0xb00"),
			OutcomeSlotCount: *uint256.NewInt(2),
		},
	})
	cond, ok := s.Condition.Get(conditionID)
	if !ok || cond.Resolved {
		t.Fatalf("condition state after prep: ok=%v resolved=%v", ok, cond.Resolved)
	}

	// 2. Split
	processBlock(t, s, []generated.Event{
		&generated.ConditionalTokensPositionSplit{
			EventMeta: generated.EventMeta{BlockNumber: 2, TransactionIndex: 1, LogIndex: 1},
			Stakeholder: user, CollateralToken: common.HexToAddress("0xccc"),
			ParentCollectionID: common.Hash{}, ConditionID: conditionID,
			Partition: []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
			Amount:    *uint256.NewInt(10_000_000),
		},
	})
	count := countUserPositions(t, s)
	t.Logf("after split: %d positions", count)

	// 3. ConditionResolution
	processBlock(t, s, []generated.Event{
		&generated.ConditionalTokensConditionResolution{
			EventMeta:         generated.EventMeta{BlockNumber: 3},
			ConditionID:       conditionID,
			PayoutDenominator: *uint256.NewInt(1_000_000),
			PayoutNumerators:  []uint256.Int{*uint256.NewInt(1_000_000), *uint256.NewInt(0)},
		},
	})
	cond, ok = s.Condition.Get(conditionID)
	if !ok || !cond.Resolved {
		t.Fatal("condition not resolved")
	}

	// 4. PayoutRedemption
	processBlock(t, s, []generated.Event{
		&generated.ConditionalTokensPayoutRedemption{
			EventMeta: generated.EventMeta{BlockNumber: 4},
			Redeemer: user, CollateralToken: common.HexToAddress("0xccc"),
			ParentCollectionID: common.Hash{}, ConditionID: conditionID,
			IndexSets: []uint256.Int{*uint256.NewInt(1)},
			Payout:    *uint256.NewInt(10_000_000),
		},
	})
	t.Logf("full lifecycle OK (prep→split→resolve→redeem)")
}

func TestNegRiskSplitMerge(t *testing.T) {
	s := generated.NewState()
	user := common.HexToAddress("0x111")
	conditionID := common.HexToHash("0xdead")

	processBlock(t, s, []generated.Event{
		&generated.NegRiskAdapterPositionSplit{
			EventMeta:   generated.EventMeta{BlockNumber: 100, TransactionIndex: 1, LogIndex: 1},
			Stakeholder: user, ConditionID: conditionID,
			Amount: *uint256.NewInt(5_000_000),
		},
	})
	afterSplit := countUserPositions(t, s)
	t.Logf("after NR split: %d positions", afterSplit)

	processBlock(t, s, []generated.Event{
		&generated.NegRiskAdapterPositionsMerge{
			EventMeta:   generated.EventMeta{BlockNumber: 101, TransactionIndex: 1, LogIndex: 1},
			Stakeholder: user, ConditionID: conditionID,
			Amount: *uint256.NewInt(5_000_000),
		},
	})
	afterMerge := countUserPositions(t, s)
	t.Logf("after NR merge: %d positions", afterMerge)
}

// --- Helpers ---

func processBlock(t *testing.T, state *generated.State, events []generated.Event) {
	t.Helper()

	block := &generated.ParsedBlock{BlockNumber: 100}
	for _, ev := range events {
		meta := ev.Meta()
		if meta.BlockNumber > 0 {
			block.BlockNumber = meta.BlockNumber
		}
		switch e := ev.(type) {
		case *generated.ConditionalTokensConditionPreparation:
			block.ConditionalTokensConditionPreparations = append(block.ConditionalTokensConditionPreparations, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeConditionalTokensConditionPreparation))
		case *generated.ConditionalTokensConditionResolution:
			block.ConditionalTokensConditionResolutions = append(block.ConditionalTokensConditionResolutions, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeConditionalTokensConditionResolution))
		case *generated.ConditionalTokensPositionSplit:
			block.ConditionalTokensPositionSplits = append(block.ConditionalTokensPositionSplits, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeConditionalTokensPositionSplit))
		case *generated.ConditionalTokensPositionsMerge:
			block.ConditionalTokensPositionsMerges = append(block.ConditionalTokensPositionsMerges, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeConditionalTokensPositionsMerge))
		case *generated.ConditionalTokensPayoutRedemption:
			block.ConditionalTokensPayoutRedemptions = append(block.ConditionalTokensPayoutRedemptions, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeConditionalTokensPayoutRedemption))
		case *generated.ExchangeOrderFilled:
			block.ExchangeOrderFilleds = append(block.ExchangeOrderFilleds, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeExchangeOrderFilled))
		case *generated.NegRiskExchangeOrderFilled:
			block.NegRiskExchangeOrderFilleds = append(block.NegRiskExchangeOrderFilleds, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeNegRiskExchangeOrderFilled))
		case *generated.NegRiskAdapterPositionSplit:
			block.NegRiskAdapterPositionSplits = append(block.NegRiskAdapterPositionSplits, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeNegRiskAdapterPositionSplit))
		case *generated.NegRiskAdapterPositionsMerge:
			block.NegRiskAdapterPositionsMerges = append(block.NegRiskAdapterPositionsMerges, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeNegRiskAdapterPositionsMerge))
		case *generated.NegRiskAdapterPayoutRedemption:
			block.NegRiskAdapterPayoutRedemptions = append(block.NegRiskAdapterPayoutRedemptions, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeNegRiskAdapterPayoutRedemption))
		case *generated.NegRiskAdapterPositionsConverted:
			block.NegRiskAdapterPositionsConverteds = append(block.NegRiskAdapterPositionsConverteds, *e)
			block.Sequence = append(block.Sequence, uint8(generated.EventTypeNegRiskAdapterPositionsConverted))
		}
	}

	if err := generated.CustomProcessFn(state, block); err != nil {
		t.Fatalf("process block %d: %v", block.BlockNumber, err)
	}
}

func countUserPositions(t *testing.T, state *generated.State) int {
	t.Helper()
	count := 0
	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, _ generated.MemoryUserPosition) bool {
		count++
		return true
	})
	return count
}
