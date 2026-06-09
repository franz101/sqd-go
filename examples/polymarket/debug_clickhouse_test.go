package polymarket

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestPositionsMergePnLInMemorySaveLoad tests the PnL round-trip through
// the in-memory hot state (without ClickHouse).
func TestPositionsMergePnLInMemorySaveLoad(t *testing.T) {
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

	// Simulate a merge event
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
		Partition:          []uint256.Int{*indexSetNo},
		Amount:             *uint256.NewInt(224192556),
	}

	handlePositionsMerge(state, mergeEv)

	// Get the position immediately after merge
	posBeforeSave := getUserPosition(state, wallet, posIDNo)
	pnLBeforeSave := toDecimal(posBeforeSave.RealizedPnL)
	t.Logf("PnL before save: %s", pnLBeforeSave.String())

	// Save the position to the hot state (simulating what happens during Commit)
	state.Position.Save(posBeforeSave, generated.EventMeta{
		BlockNumber:    1001,
		BlockTimestamp: time.Unix(1001, 0),
	})

	// Read the position back from the hot state
	posAfterLoad := getUserPosition(state, wallet, posIDNo)
	if posAfterLoad == nil {
		t.Fatal("position not found after save/load")
	}

	pnLAfterLoad := toDecimal(posAfterLoad.RealizedPnL)
	t.Logf("PnL after load: %s", pnLAfterLoad.String())

	// Check if PnL was preserved
	if pnLAfterLoad.IsZero() && !pnLBeforeSave.IsZero() {
		t.Errorf("PnL was lost during save/load! Before: %s, After: %s", pnLBeforeSave.String(), pnLAfterLoad.String())
	}

	diff := pnLBeforeSave.Sub(pnLAfterLoad).Abs()
	maxDiff := decimal.NewFromFloat(0.000001)
	if diff.Cmp(maxDiff) > 0 {
		t.Errorf("PnL changed during save/load! Before: %s, After: %s, diff: %s", pnLBeforeSave.String(), pnLAfterLoad.String(), diff.String())
	}
}

// TestPositionStateGetSave tests the Position state Get/Save cycle
func TestPositionStateGetSave(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tokenID := *uint256.NewInt(12345)

	// Create a position with small PnL
	pos := &generated.Position{
		User:        wallet,
		TokenID:     tokenIDHash(tokenID),
		Amount:      fromDecimal(decimal.NewFromInt(1000)),
		AvgPrice:    fromDecimal(decimal.NewFromFloat(0.497623)),
		RealizedPnL: fromDecimal(decimal.NewFromFloat(0.532905)),
		TotalBought: fromDecimal(decimal.NewFromInt(1000)),
		Tombstone:   false,
	}

	// Save the position
	state.Position.Save(pos, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Get the position back
	retrieved, ok := state.Position.Get(wallet, tokenIDHash(tokenID))
	if !ok {
		t.Fatal("position not found after save")
	}

	// Check the PnL
	retrievedPnL := toDecimal(retrieved.RealizedPnL)
	expectedPnL := decimal.NewFromFloat(0.532905)

	if retrievedPnL.IsZero() {
		t.Errorf("PnL is zero after Get/Save! Expected: %s", expectedPnL.String())
	}

	diff := expectedPnL.Sub(retrievedPnL).Abs()
	maxDiff := decimal.NewFromFloat(0.000001)
	if diff.Cmp(maxDiff) > 0 {
		t.Errorf("PnL mismatch after Get/Save! Expected: %s, Got: %s, diff: %s", expectedPnL.String(), retrievedPnL.String(), diff.String())
	}

	t.Logf("Position Get/Save round-trip successful. PnL: %s", retrievedPnL.String())
}

// TestHotStateUserPositionsRoundTrip tests the round-trip through hot state cache
func TestHotStateUserPositionsRoundTrip(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tokenID := *uint256.NewInt(12345)

	// Create a position with small PnL
	pos := generated.MemoryUserPosition{
		User:        wallet,
		TokenID:     tokenIDHash(tokenID),
		Amount:      fromDecimal(decimal.NewFromInt(1000)),
		AvgPrice:    fromDecimal(decimal.NewFromFloat(0.497623)),
		RealizedPnL: fromDecimal(decimal.NewFromFloat(0.532905)),
		TotalBought: fromDecimal(decimal.NewFromInt(1000)),
		Tombstone:   false,
	}

	// Save to hot state
	state.HotState.UpdateMemoryUserPosition(pos)

	// Get from hot state
	key := generated.NewUserPositionsClockKey(pos)
	retrieved, ok := state.HotState.UserPositions.Get(key)
	if !ok {
		t.Fatal("position not found in hot state")
	}

	// Check the PnL
	retrievedPnL := toDecimal(retrieved.RealizedPnL)
	expectedPnL := decimal.NewFromFloat(0.532905)

	if retrievedPnL.IsZero() {
		t.Errorf("PnL is zero after hot state round-trip! Expected: %s", expectedPnL.String())
	}

	diff := expectedPnL.Sub(retrievedPnL).Abs()
	maxDiff := decimal.NewFromFloat(0.000001)
	if diff.Cmp(maxDiff) > 0 {
		t.Errorf("PnL mismatch after hot state round-trip! Expected: %s, Got: %s, diff: %s", expectedPnL.String(), retrievedPnL.String(), diff.String())
	}

	t.Logf("Hot state round-trip successful. PnL: %s", retrievedPnL.String())
}
