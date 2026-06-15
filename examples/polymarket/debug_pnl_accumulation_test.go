package polymarket

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestMultipleSellsAccumulatePnL tests that multiple small sells correctly accumulate PnL
func TestMultipleSellsAccumulatePnL(t *testing.T) {
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

	// Buy 774.082556 tokens at 0.497623
	avgPrice := decimal.NewFromFloat(0.497623)
	totalBought := decimal.NewFromFloat(774.082556)

	updateUserPositionWithBuy(state, wallet, posIDNo, avgPrice, totalBought, decimal.Zero, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Sell 224.192556 tokens at 0.5 (in one transaction)
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

	// Verify the position after sell
	pos := getUserPosition(state, wallet, posIDNo)
	if pos == nil {
		t.Fatal("position was deleted after sell")
	}

	amt := toDecimal(pos.Amount)
	avgP := toDecimal(pos.AvgPrice)
	rPnL := toDecimal(pos.RealizedPnL)
	tb := toDecimal(pos.TotalBought)

	t.Logf("After sell: Amount=%s, AvgPrice=%s, RealizedPnL=%s, TotalBought=%s",
		amt.String(), avgP.String(), rPnL.String(), tb.String())

	// Expected values
	expectedAmt := decimal.NewFromFloat(549.89) // 774.082556 - 224.192556
	expectedPnL := decimal.NewFromFloat(0.532905)

	// Check amount
	diffAmt := expectedAmt.Sub(amt).Abs()
	if diffAmt.Cmp(decimal.NewFromFloat(0.0001)) > 0 {
		t.Errorf("Amount mismatch: got %s, want %s, diff=%s", amt.String(), expectedAmt.String(), diffAmt.String())
	}

	// Check PnL
	if rPnL.IsZero() {
		t.Fatal("PnL is zero - this is the bug!")
	}

	diffPnL := expectedPnL.Sub(rPnL).Abs()
	if diffPnL.Cmp(decimal.NewFromFloat(0.000001)) > 0 {
		t.Errorf("PnL mismatch: got %s, want %s, diff=%s", rPnL.String(), expectedPnL.String(), diffPnL.String())
	}

	// Now test the round-trip through Position state Save/Get
	state.Position.Save(pos, generated.EventMeta{
		BlockNumber:    1001,
		BlockTimestamp: time.Unix(1001, 0),
	})

	posAfter, ok := state.Position.Get(wallet, tokenIDHash(posIDNo))
	if !ok {
		t.Fatal("position not found after Save")
	}

	rPnLAfter := toDecimal(posAfter.RealizedPnL)
	t.Logf("After Save/Get: RealizedPnL=%s", rPnLAfter.String())

	if rPnLAfter.IsZero() {
		t.Error("PnL is zero after Save/Get - this is the bug!")
	}

	diffPnLAfter := expectedPnL.Sub(rPnLAfter).Abs()
	if diffPnLAfter.Cmp(decimal.NewFromFloat(0.000001)) > 0 {
		t.Errorf("PnL mismatch after Save/Get: got %s, want %s, diff=%s", rPnLAfter.String(), expectedPnL.String(), diffPnLAfter.String())
	}

	// Now test the round-trip through hot state
	state.HotState.UpdateMemoryUserPosition(*posAfter)

	key := generated.NewUserPositionsClockKey(*posAfter)
	posFromHot, ok := state.HotState.UserPositions.Get(key)
	if !ok {
		t.Fatal("position not found in hot state")
	}

	rPnLFromHot := toDecimal(posFromHot.RealizedPnL)
	t.Logf("After hot state round-trip: RealizedPnL=%s", rPnLFromHot.String())

	if rPnLFromHot.IsZero() {
		t.Error("PnL is zero after hot state round-trip - this is the bug!")
	}

	diffPnLFromHot := expectedPnL.Sub(rPnLFromHot).Abs()
	if diffPnLFromHot.Cmp(decimal.NewFromFloat(0.000001)) > 0 {
		t.Errorf("PnL mismatch after hot state round-trip: got %s, want %s, diff=%s", rPnLFromHot.String(), expectedPnL.String(), diffPnLFromHot.String())
	}

	t.Log("All PnL round-trip tests passed!")
}

// TestSmallPnLValuesThroughState tests very small PnL values
func TestSmallPnLValuesThroughState(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tokenIDVal := *uint256.NewInt(12345)

	// Create a position with small PnL
	smallPnL := decimal.NewFromFloat(0.532905)
	pos := &generated.Position{
		User:        wallet,
		TokenID:     tokenIDHash(tokenIDVal),
		Amount:      fromDecimal(decimal.NewFromInt(1000)),
		AvgPrice:    fromDecimal(decimal.NewFromFloat(0.497623)),
		RealizedPnL: fromDecimal(smallPnL),
		TotalBought: fromDecimal(decimal.NewFromInt(1000)),
		Tombstone:   false,
	}

	// Save to Position state
	state.Position.Save(pos, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Get from Position state
	retrieved, ok := state.Position.Get(wallet, tokenIDHash(tokenIDVal))
	if !ok {
		t.Fatal("position not found")
	}

	rPnL := toDecimal(retrieved.RealizedPnL)
	if rPnL.IsZero() {
		t.Errorf("PnL is zero after Position Get/Save! Expected: %s", smallPnL.String())
	}

	diff := smallPnL.Sub(rPnL).Abs()
	if diff.Cmp(decimal.NewFromFloat(0.000001)) > 0 {
		t.Errorf("PnL mismatch: expected %s, got %s, diff %s", smallPnL.String(), rPnL.String(), diff.String())
	}

	// Now save to hot state
	state.HotState.UpdateMemoryUserPosition(*retrieved)

	// Get from hot state
	key := generated.NewUserPositionsClockKey(*retrieved)
	fromHot, ok := state.HotState.UserPositions.Get(key)
	if !ok {
		t.Fatal("position not found in hot state")
	}

	rPnLFromHot := toDecimal(fromHot.RealizedPnL)
	if rPnLFromHot.IsZero() {
		t.Errorf("PnL is zero after hot state round-trip! Expected: %s", smallPnL.String())
	}

	diffFromHot := smallPnL.Sub(rPnLFromHot).Abs()
	if diffFromHot.Cmp(decimal.NewFromFloat(0.000001)) > 0 {
		t.Errorf("PnL mismatch after hot state: expected %s, got %s, diff %s", smallPnL.String(), rPnLFromHot.String(), diffFromHot.String())
	}

	t.Log("Small PnL test passed!")
}

// TestSnapshotRoundTrip tests the snapshot save/restore cycle
func TestSnapshotRoundTrip(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tokenIDVal := *uint256.NewInt(12345)

	// Create a position with small PnL
	smallPnL := decimal.NewFromFloat(0.532905)
	pos := generated.MemoryUserPosition{
		User:        wallet,
		TokenID:     tokenIDHash(tokenIDVal),
		Amount:      fromDecimal(decimal.NewFromInt(1000)),
		AvgPrice:    fromDecimal(decimal.NewFromFloat(0.497623)),
		RealizedPnL: fromDecimal(smallPnL),
		TotalBought: fromDecimal(decimal.NewFromInt(1000)),
		Tombstone:   false,
	}

	// Save to hot state
	state.HotState.UserPositions.Set(pos)

	// Save snapshot
	blockNumber := uint64(1000)
	state.SaveSnapshot(blockNumber)

	// Clear the hot state
	state.HotState = generated.NewHotState(generated.DefaultClockCacheCapacity)

	// Restore from snapshot
	restored, err := state.RestoreToBlock(blockNumber)
	if err != nil {
		t.Fatalf("RestoreToBlock failed: %v", err)
	}
	if restored != blockNumber {
		t.Errorf("RestoreToBlock returned wrong block: got %d, want %d", restored, blockNumber)
	}

	// Get the position from restored hot state
	key := generated.NewUserPositionsClockKey(pos)
	fromRestored, ok := state.HotState.UserPositions.Get(key)
	if !ok {
		t.Fatal("position not found after snapshot restore")
	}

	rPnL := toDecimal(fromRestored.RealizedPnL)
	if rPnL.IsZero() {
		t.Errorf("PnL is zero after snapshot restore! Expected: %s", smallPnL.String())
	}

	diff := smallPnL.Sub(rPnL).Abs()
	if diff.Cmp(decimal.NewFromFloat(0.000001)) > 0 {
		t.Errorf("PnL mismatch after snapshot restore: expected %s, got %s, diff %s", smallPnL.String(), rPnL.String(), diff.String())
	}

	t.Log("Snapshot round-trip test passed!")
}

// TestLoadFromDatabaseInTestMode tests the LoadFromDatabase behavior in test mode
func TestLoadFromDatabaseInTestMode(t *testing.T) {
	// This test verifies that LoadFromDatabase works correctly in test mode
	// (when ClickHouse is not available)

	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tokenIDVal := *uint256.NewInt(12345)

	// Create a position with small PnL
	smallPnL := decimal.NewFromFloat(0.532905)
	pos := generated.MemoryUserPosition{
		User:        wallet,
		TokenID:     tokenIDHash(tokenIDVal),
		Amount:      fromDecimal(decimal.NewFromInt(1000)),
		AvgPrice:    fromDecimal(decimal.NewFromFloat(0.497623)),
		RealizedPnL: fromDecimal(smallPnL),
		TotalBought: fromDecimal(decimal.NewFromInt(1000)),
		Tombstone:   false,
	}

	// Save to hot state
	state.HotState.UserPositions.Set(pos)

	// Save snapshot
	blockNumber := uint64(1000)
	state.SaveSnapshot(blockNumber)

	// Simulate LoadFromDatabase in test mode
	// (this should load from ClickHouse, but if TEST_MODE is set, it skips and just saves snapshot)
	t.Setenv("TEST_MODE", "1")

	err = proc.LoadFromDatabase(t.Context(), blockNumber)
	if err != nil {
		t.Fatalf("LoadFromDatabase failed: %v", err)
	}

	// In test mode, LoadFromDatabase should have saved a snapshot and set LastSyncBlock
	if state.LastSyncBlock != blockNumber {
		t.Errorf("LastSyncBlock not set: got %d, want %d", state.LastSyncBlock, blockNumber)
	}

	// Restore from snapshot
	restored, err := state.RestoreToBlock(blockNumber)
	if err != nil {
		t.Fatalf("RestoreToBlock failed: %v", err)
	}
	if restored != blockNumber {
		t.Errorf("RestoreToBlock returned wrong block: got %d, want %d", restored, blockNumber)
	}

	// Get the position from restored hot state
	key := generated.NewUserPositionsClockKey(pos)
	fromRestored, ok := state.HotState.UserPositions.Get(key)
	if !ok {
		t.Fatal("position not found after LoadFromDatabase + RestoreToBlock")
	}

	rPnL := toDecimal(fromRestored.RealizedPnL)
	if rPnL.IsZero() {
		t.Errorf("PnL is zero after LoadFromDatabase + RestoreToBlock! Expected: %s", smallPnL.String())
	}

	diff := smallPnL.Sub(rPnL).Abs()
	if diff.Cmp(decimal.NewFromFloat(0.000001)) > 0 {
		t.Errorf("PnL mismatch after LoadFromDatabase + RestoreToBlock: expected %s, got %s, diff %s", smallPnL.String(), rPnL.String(), diff.String())
	}

	t.Log("LoadFromDatabase test mode test passed!")
}
