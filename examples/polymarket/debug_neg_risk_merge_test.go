package polymarket

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestNegRiskPositionsMergePnLCalculation tests that handleNegRiskPositionsMerge correctly
// calculates and saves PnL when selling neg-risk tokens.
func TestNegRiskPositionsMergePnLCalculation(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0xf05B670C0F91F8171984db945A28D2Ad0F170cC4")
	conditionID := common.HexToHash("0x8a4c788f043023b8b28a762216d037e9f148532b")

	// Create a condition
	cond := &generated.Condition{
		ID:               conditionID,
		Oracle:           negRiskAdapterAddr,
		Resolved:         false,
		OutcomeSlotCount: 2,
	}
	state.Condition.Save(cond, generated.EventMeta{
		BlockNumber:    39103113,
		BlockTimestamp: time.Unix(1675960928, 0),
	})

	// Get the neg-risk position IDs
	posIDNo := getNegRiskPositionIDByCondition(conditionID, 1)

	t.Logf("NO token ID: %s", posIDNo.Hex())

	// Simulate buying 774.082556 NO tokens at 0.497623
	avgPrice := decimal.NewFromFloat(0.497623)
	totalBought := decimal.NewFromFloat(774.082556)

	updateUserPositionWithBuy(state, wallet, posIDNo, avgPrice, totalBought, decimal.Zero, generated.EventMeta{
		BlockNumber:    39103113,
		BlockTimestamp: time.Unix(1675960928, 0),
	})

	// Verify initial position
	pos := getUserPosition(state, wallet, posIDNo)
	if pos == nil {
		t.Fatal("position was not created")
	}
	t.Logf("Initial position: Amount=%s, AvgPrice=%s, RealizedPnL=%s",
		toDecimal(pos.Amount).String(), toDecimal(pos.AvgPrice).String(), toDecimal(pos.RealizedPnL).String())

	// Simulate a NegRiskAdapterPositionsMerge event
	// This should sell 224.192556 tokens at 0.5
	mergeAmount := decimal.NewFromFloat(224.192556)
	mergeEv := &generated.NegRiskAdapterPositionsMerge{
		EventMeta: generated.EventMeta{
			BlockNumber:      39270331,
			BlockTimestamp:   time.Unix(1676349422, 0),
			TransactionIndex: 40,
			LogIndex:         162,
		},
		Stakeholder:  wallet,
		ConditionID:  conditionID,
		Amount:       *uint256.NewInt(224192556), // Raw amount in 1e6 units
	}

	// Calculate expected PnL: amount * (fiftyCents - avgPrice)
	expectedPnL := mergeAmount.Mul(fiftyCents.Sub(avgPrice))
	t.Logf("Expected PnL for this merge: %s", expectedPnL.String())

	// Process the merge event
	handleNegRiskPositionsMerge(state, mergeEv)

	// Verify the position after merge
	pos = getUserPosition(state, wallet, posIDNo)
	if pos == nil {
		t.Fatal("position was deleted after merge")
	}
	t.Logf("Position after merge: Amount=%s, AvgPrice=%s, RealizedPnL=%s",
		toDecimal(pos.Amount).String(), toDecimal(pos.AvgPrice).String(), toDecimal(pos.RealizedPnL).String())

	// Check if PnL was generated
	if toDecimal(pos.RealizedPnL).IsZero() {
		t.Error("PnL is zero after merge - this is the bug!")
	}

	// Verify the amount decreased
	expectedAmountAfter := totalBought.Sub(mergeAmount)
	actualAmountAfter := toDecimal(pos.Amount)
	diffAmount := expectedAmountAfter.Sub(actualAmountAfter).Abs()
	if diffAmount.Cmp(decimal.NewFromFloat(0.0001)) > 0 {
		t.Errorf("Amount mismatch: got %s, want %s", actualAmountAfter.String(), expectedAmountAfter.String())
	}

	// Verify the PnL is close to expected
	actualPnL := toDecimal(pos.RealizedPnL)
	diffPnL := expectedPnL.Sub(actualPnL).Abs()
	maxDiffPnL := decimal.NewFromFloat(0.000001)
	if diffPnL.Cmp(maxDiffPnL) > 0 {
		t.Errorf("PnL mismatch: got %s, want %s, diff=%s", actualPnL.String(), expectedPnL.String(), diffPnL.String())
	}
}

// TestNegRiskPositionsMergeMultipleMerges tests multiple merge events
func TestNegRiskPositionsMergeMultipleMerges(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0xf05B670C0F91F8171984db945A28D2Ad0F170cC4")
	conditionID := common.HexToHash("0x8a4c788f043023b8b28a762216d037e9f148532b")

	// Create a condition
	cond := &generated.Condition{
		ID:               conditionID,
		Oracle:           negRiskAdapterAddr,
		Resolved:         false,
		OutcomeSlotCount: 2,
	}
	state.Condition.Save(cond, generated.EventMeta{
		BlockNumber:    39103113,
		BlockTimestamp: time.Unix(1675960928, 0),
	})

	// Get the neg-risk position IDs
	posIDNo := getNegRiskPositionIDByCondition(conditionID, 1)

	// Simulate buying 774.082556 NO tokens at 0.497623
	avgPrice := decimal.NewFromFloat(0.497623)
	totalBought := decimal.NewFromFloat(774.082556)

	updateUserPositionWithBuy(state, wallet, posIDNo, avgPrice, totalBought, decimal.Zero, generated.EventMeta{
		BlockNumber:    39103113,
		BlockTimestamp: time.Unix(1675960928, 0),
	})

	// Simulate multiple merge events (from the actual blockchain data)
	mergeAmounts := []decimal.Decimal{
		decimal.NewFromFloat(224.192556), // 0x1c9c38 = 19562424 (scaled by 1e6)
		decimal.NewFromFloat(200),         // Approximate from events
		decimal.NewFromFloat(100),         // Approximate from events
		decimal.NewFromFloat(50),          // Approximate from events
	}

	for i, mergeAmount := range mergeAmounts {
		mergeEv := &generated.NegRiskAdapterPositionsMerge{
			EventMeta: generated.EventMeta{
				BlockNumber:      uint64(39270331 + i),
				BlockTimestamp:   time.Unix(int64(1676349422+i), 0),
				TransactionIndex: 40,
				LogIndex:         uint64(162 + i),
			},
			Stakeholder:  wallet,
			ConditionID:  conditionID,
			Amount:       *uint256.NewInt(uint64(mergeAmount.Mul(decimal.NewFromInt(1e6)).IntPart())),
		}

		handleNegRiskPositionsMerge(state, mergeEv)

		pos := getUserPosition(state, wallet, posIDNo)
		t.Logf("After merge %d: RealizedPnL=%s, Amount=%s", i+1, toDecimal(pos.RealizedPnL).String(), toDecimal(pos.Amount).String())
	}

	// Final check
	pos := getUserPosition(state, wallet, posIDNo)
	finalPnL := toDecimal(pos.RealizedPnL)
	t.Logf("Final PnL: %s", finalPnL.String())

	// Calculate expected PnL: total sold * (0.5 - 0.497623)
	totalSold := decimal.Zero
	for _, amt := range mergeAmounts {
		totalSold = totalSold.Add(amt)
	}
	expectedPnL := totalSold.Mul(fiftyCents.Sub(avgPrice))
	diff := expectedPnL.Sub(finalPnL).Abs()
	maxDiff := decimal.NewFromFloat(0.000001)

	if finalPnL.IsZero() {
		t.Fatal("Final PnL is zero - this is the bug!")
	}

	if diff.Cmp(maxDiff) > 0 {
		t.Errorf("Final PnL mismatch: got %s, want %s, diff=%s", finalPnL.String(), expectedPnL.String(), diff.String())
	}
}
