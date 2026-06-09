package polymarket

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestPositionsMergePnLCalculation tests that handlePositionsMerge correctly
// calculates and saves PnL when selling tokens at fiftyCents.
func TestPositionsMergePnLCalculation(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")
	collateral := common.HexToAddress("0x3A3BD7bb9528E159577F7C2e685CC81A765002E2")
	conditionID := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	// Create a condition
	cond := &generated.Condition{
		ID:               conditionID,
		Oracle:           negRiskAdapterAddr,
		Resolved:         false,
		OutcomeSlotCount: 2,
	}
	state.Condition.Save(cond, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Calculate position IDs for YES and NO tokens
	indexSetYes := new(uint256.Int).Lsh(uint256.NewInt(1), 0) // outcome 0
	indexSetNo := new(uint256.Int).Lsh(uint256.NewInt(1), 1)  // outcome 1
	collIDYes := getCollectionID(common.Hash{}, conditionID, indexSetYes.ToBig())
	collIDNo := getCollectionID(common.Hash{}, conditionID, indexSetNo.ToBig())
	posIDYes := getPositionID(collateral, collIDYes)
	posIDNo := getPositionID(collateral, collIDNo)

	t.Logf("YES token ID: %s", posIDYes.Hex())
	t.Logf("NO token ID: %s", posIDNo.Hex())

	// Buy 1000 tokens at 0.497623 (avg price)
	avgPrice := decimal.NewFromFloat(0.497623)
	amount := decimal.NewFromInt(1000)

	updateUserPositionWithBuy(state, wallet, posIDNo, avgPrice, amount, decimal.Zero, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Verify initial position
	pos := getUserPosition(state, wallet, posIDNo)
	if pos == nil {
		t.Fatal("position was not created")
	}
	t.Logf("Initial position: Amount=%s, AvgPrice=%s, RealizedPnL=%s",
		toDecimal(pos.Amount).String(), toDecimal(pos.AvgPrice).String(), toDecimal(pos.RealizedPnL).String())

	// Simulate a merge event that sells 224.192556 tokens
	mergeAmount := decimal.NewFromFloat(224.192556)
	mergeEv := &generated.ConditionalTokensPositionsMerge{
		EventMeta: generated.EventMeta{
			BlockNumber:      1001,
			BlockTimestamp:   time.Unix(1001, 0),
			TransactionIndex: 0,
			LogIndex:         0,
		},
		Stakeholder:        wallet,
		CollateralToken:    collateral,
		ParentCollectionID: common.Hash{},
		ConditionID:        conditionID,
		Partition:         []uint256.Int{*indexSetNo},
		Amount:             *uint256.NewInt(224192556), // Raw amount in 1e6 units
	}

	// Calculate expected PnL: amount * (fiftyCents - avgPrice)
	expectedPnL := mergeAmount.Mul(fiftyCents.Sub(avgPrice))
	t.Logf("Expected PnL for this merge: %s", expectedPnL.String())

	// Process the merge event
	handlePositionsMerge(state, mergeEv)

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
	expectedAmountAfter := amount.Sub(mergeAmount)
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

// TestPositionsMergePnLRoundTrip tests the full round-trip through Decimal256
func TestPositionsMergePnLRoundTrip(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")
	collateral := common.HexToAddress("0x3A3BD7bb9528E159577F7C2e685CC81A765002E2")
	conditionID := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	// Create a condition
	cond := &generated.Condition{
		ID:               conditionID,
		Oracle:           negRiskAdapterAddr,
		Resolved:         false,
		OutcomeSlotCount: 2,
	}
	state.Condition.Save(cond, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Calculate position ID for NO token
	indexSetNo := new(uint256.Int).Lsh(uint256.NewInt(1), 1)
	collIDNo := getCollectionID(common.Hash{}, conditionID, indexSetNo.ToBig())
	posIDNo := getPositionID(collateral, collIDNo)

	// Buy 1000 tokens at 0.497623
	avgPrice := decimal.NewFromFloat(0.497623)
	amount := decimal.NewFromInt(1000)

	updateUserPositionWithBuy(state, wallet, posIDNo, avgPrice, amount, decimal.Zero, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Simulate multiple small merge events to accumulate PnL
	// Total of 224.192556 over 10 events = 22.4192556 per event
	for i := 0; i < 10; i++ {
		mergeEv := &generated.ConditionalTokensPositionsMerge{
			EventMeta: generated.EventMeta{
				BlockNumber:      uint64(1001 + i),
				BlockTimestamp:   time.Unix(int64(1001+i), 0),
				TransactionIndex: 0,
				LogIndex:         0,
			},
			Stakeholder:        wallet,
			CollateralToken:    collateral,
			ParentCollectionID: common.Hash{},
			ConditionID:        conditionID,
			Partition:         []uint256.Int{*indexSetNo},
			Amount:            *uint256.NewInt(22419256), // Raw amount in 1e6 units
		}

		handlePositionsMerge(state, mergeEv)

		pos := getUserPosition(state, wallet, posIDNo)
		t.Logf("After merge %d: RealizedPnL=%s", i+1, toDecimal(pos.RealizedPnL).String())
	}

	// Final check
	pos := getUserPosition(state, wallet, posIDNo)
	finalPnL := toDecimal(pos.RealizedPnL)
	t.Logf("Final PnL: %s", finalPnL.String())

	// Expected PnL: 224.192556 * (0.5 - 0.497623) = 0.532905...
	expectedPnL := decimal.NewFromFloat(224.192556).Mul(fiftyCents.Sub(avgPrice))
	diff := expectedPnL.Sub(finalPnL).Abs()
	maxDiff := decimal.NewFromFloat(0.000001)

	if finalPnL.IsZero() {
		t.Fatal("Final PnL is zero - this is the bug!")
	}

	if diff.Cmp(maxDiff) > 0 {
		t.Errorf("Final PnL mismatch: got %s, want %s, diff=%s", finalPnL.String(), expectedPnL.String(), diff.String())
	}
}
