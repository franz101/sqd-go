package tests

import (
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

// =============================================================================
// Helpers
// =============================================================================

func newTestMeta(blockNum uint64, logIdx uint64) generated.EventMeta {
	return generated.EventMeta{
		BlockNumber:      blockNum,
		BlockTimestamp:   time.Unix(1700000000+int64(blockNum), 0).UTC(),
		BlockHash:        common.HexToHash(fmt.Sprintf("0x%064x", blockNum)),
		ContractAddress:  common.HexToAddress("0x1111111111111111111111111111111111111111"),
		TransactionHash:  common.HexToHash(fmt.Sprintf("0x%064x", blockNum*100+logIdx)),
		TransactionIndex: uint64(logIdx),
		LogIndex:         logIdx,
	}
}

func newUint256(v uint64) uint256.Int {
	var u uint256.Int
	u.SetUint64(v)
	return u
}

func newUint256FromDec(s string) uint256.Int {
	u, err := uint256.FromDecimal(s)
	if err != nil {
		panic(err)
	}
	return *u
}

// =============================================================================
// 1. ProtoEventBlock Append + Reset Lifecycle
// =============================================================================

func TestProtoEventBlockAppendResetLifecycle(t *testing.T) {
	block := generated.NewProtoEventBlock()
	if block == nil {
		t.Fatal("NewProtoEventBlock returned nil")
	}

	meta := newTestMeta(20000000, 1)
	ev := &generated.FixedProductMarketMakerFPMMBuy{
		EventMeta:           meta,
		Buyer:               common.HexToAddress("0xABCDEF0000000000000000000000000000000000"),
		InvestmentAmount:    newUint256(1000000),
		FeeAmount:           newUint256(100),
		OutcomeIndex:        newUint256(0),
		OutcomeTokensBought: newUint256(500000),
	}

	block.AppendFixedProductMarketMakerFPMMBuy(meta, ev)

	if len(block.Sequence) != 1 {
		t.Fatalf("expected Sequence length 1, got %d", len(block.Sequence))
	}
	if block.Sequence[0] != uint8(generated.EventTypeFixedProductMarketMakerFPMMBuy) {
		t.Errorf("expected Sequence[0]=%d, got %d", generated.EventTypeFixedProductMarketMakerFPMMBuy, block.Sequence[0])
	}
	if block.FixedProductMarketMakerFPMMBuy_meta_index.Rows() != 1 {
		t.Fatalf("expected 1 FPMMBuy row, got %d", block.FixedProductMarketMakerFPMMBuy_meta_index.Rows())
	}

	view := block.FixedProductMarketMakerFPMMBuyProtoAt(0)
	if view.Buyer() != ev.Buyer {
		t.Errorf("Buyer mismatch: got %s, want %s", view.Buyer().Hex(), ev.Buyer.Hex())
	}
	if view.InvestmentAmount() != ev.InvestmentAmount {
		t.Errorf("InvestmentAmount mismatch")
	}

	metaRead := view.Meta()
	if metaRead.BlockNumber != meta.BlockNumber {
		t.Errorf("BlockNumber mismatch: got %d, want %d", metaRead.BlockNumber, meta.BlockNumber)
	}
	if metaRead.LogIndex != meta.LogIndex {
		t.Errorf("LogIndex mismatch: got %d, want %d", metaRead.LogIndex, meta.LogIndex)
	}

	block.Reset()

	// After Reset, all columns should be empty
	if len(block.Sequence) != 0 {
		t.Errorf("expected Sequence length 0 after Reset, got %d", len(block.Sequence))
	}
	if block.FixedProductMarketMakerFPMMBuy_meta_index.Rows() != 0 {
		t.Errorf("expected 0 FPMMBuy rows after Reset, got %d", block.FixedProductMarketMakerFPMMBuy_meta_index.Rows())
	}
	if block.HeaderBlockNumber != 0 {
		t.Errorf("HeaderBlockNumber not reset, got %d", block.HeaderBlockNumber)
	}

	// Can append again after Reset
	meta2 := newTestMeta(20000001, 0)
	ev2 := &generated.FixedProductMarketMakerFPMMBuy{
		EventMeta:           meta2,
		Buyer:               common.HexToAddress("0x2222222222222222222222222222222222222222"),
		InvestmentAmount:    newUint256(500000),
		FeeAmount:           newUint256(50),
		OutcomeIndex:        newUint256(1),
		OutcomeTokensBought: newUint256(250000),
	}
	block.AppendFixedProductMarketMakerFPMMBuy(meta2, ev2)

	if len(block.Sequence) != 1 {
		t.Errorf("expected Sequence length 1 after re-append, got %d", len(block.Sequence))
	}
	if block.FixedProductMarketMakerFPMMBuy_meta_index.Rows() != 1 {
		t.Errorf("expected 1 row after re-append, got %d", block.FixedProductMarketMakerFPMMBuy_meta_index.Rows())
	}
}

func TestProtoEventBlockMultipleAppendsInSequence(t *testing.T) {
	block := generated.NewProtoEventBlock()

	// Append multiple events of the same type
	for i := 0; i < 5; i++ {
		meta := newTestMeta(20000000, uint64(i))
		ev := &generated.FixedProductMarketMakerFPMMBuy{
			EventMeta:           meta,
			Buyer:               common.HexToAddress(fmt.Sprintf("0x%040d", i)),
			InvestmentAmount:    newUint256(1000000 + uint64(i)),
			FeeAmount:           newUint256(100),
			OutcomeIndex:        newUint256(uint64(i)),
			OutcomeTokensBought: newUint256(500000),
		}
		block.AppendFixedProductMarketMakerFPMMBuy(meta, ev)
	}

	if len(block.Sequence) != 5 {
		t.Fatalf("expected 5 Sequence entries, got %d", len(block.Sequence))
	}
	if block.FixedProductMarketMakerFPMMBuy_meta_index.Rows() != 5 {
		t.Fatalf("expected 5 rows, got %d", block.FixedProductMarketMakerFPMMBuy_meta_index.Rows())
	}

	// Verify Sequence order
	for i := 0; i < 5; i++ {
		if block.Sequence[i] != uint8(generated.EventTypeFixedProductMarketMakerFPMMBuy) {
			t.Errorf("Sequence[%d] expected %d, got %d", i, generated.EventTypeFixedProductMarketMakerFPMMBuy, block.Sequence[i])
		}
	}

	// Verify each row via ProtoAt
	for i := 0; i < 5; i++ {
		view := block.FixedProductMarketMakerFPMMBuyProtoAt(i)
		meta := view.Meta()
		if meta.LogIndex != uint64(i) {
			t.Errorf("row %d: LogIndex mismatch: got %d, want %d", i, meta.LogIndex, uint64(i))
		}
	}
}

func TestProtoEventBlockQueryIteration(t *testing.T) {
	block := generated.NewProtoEventBlock()

	for i := 0; i < 3; i++ {
		meta := newTestMeta(20000000, uint64(i))
		ev := &generated.FixedProductMarketMakerFPMMBuy{
			EventMeta:           meta,
			Buyer:               common.HexToAddress(fmt.Sprintf("0x%040d", i)),
			InvestmentAmount:    newUint256(uint64(1000000 + i)),
			FeeAmount:           newUint256(100),
			OutcomeIndex:        newUint256(uint64(i)),
			OutcomeTokensBought: newUint256(500000),
		}
		block.AppendFixedProductMarketMakerFPMMBuy(meta, ev)
	}

	count := 0
	block.QueryFixedProductMarketMakerFPMMBuy().Map(func(view generated.FixedProductMarketMakerFPMMBuyProtoView) {
		if view.FeeAmount() != newUint256(100) {
			t.Errorf("unexpected FeeAmount in query iteration")
		}
		count++
	})
	if count != 3 {
		t.Errorf("expected 3 iterations, got %d", count)
	}

	// Test MapWithIndex
	count = 0
	block.QueryFixedProductMarketMakerFPMMBuy().MapWithIndex(func(i int, view generated.FixedProductMarketMakerFPMMBuyProtoView) {
		if i != count {
			t.Errorf("index mismatch: got %d, want %d", i, count)
		}
		count++
	})
	if count != 3 {
		t.Errorf("expected 3 MapWithIndex iterations, got %d", count)
	}
}

// =============================================================================
// 2. ProtoRingBuffer Creation / Fill / Wrap / Eviction
// =============================================================================

func TestProtoRingBufferCreation(t *testing.T) {
	// Power-of-two size
	rb, err := generated.NewProtoRingBuffer(16)
	if err != nil {
		t.Fatalf("NewProtoRingBuffer(16) failed: %v", err)
	}
	if rb.Len() != 0 {
		t.Errorf("empty ring buffer Len() should be 0, got %d", rb.Len())
	}

	// Non-power-of-two must error
	_, err = generated.NewProtoRingBuffer(10)
	if err == nil {
		t.Error("expected error for non-power-of-two size")
	}

	// Zero must error
	_, err = generated.NewProtoRingBuffer(0)
	if err == nil {
		t.Error("expected error for zero size")
	}
}

func TestProtoRingBufferFill(t *testing.T) {
	rb, err := generated.NewProtoRingBuffer(8)
	if err != nil {
		t.Fatal(err)
	}

	for blockNum := uint64(100); blockNum < 104; blockNum++ {
		slot := rb.NextProtoSlot(blockNum, fmt.Sprintf("0x%064x", blockNum))
		if slot == nil {
			t.Fatalf("NextProtoSlot(%d) returned nil", blockNum)
		}
		if slot.HeaderBlockNumber != blockNum {
			t.Errorf("HeaderBlockNumber = %d, want %d", slot.HeaderBlockNumber, blockNum)
		}
		if slot.HeaderBlockHash != fmt.Sprintf("0x%064x", blockNum) {
			t.Errorf("HeaderBlockHash mismatch for block %d", blockNum)
		}

		// Append some events
		meta := newTestMeta(blockNum, 0)
		ev := &generated.ExchangeOrderFilled{
			EventMeta:         meta,
			Maker:             common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
			Taker:             common.HexToAddress("0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"),
			MakerAssetID:      newUint256(1),
			TakerAssetID:      newUint256(2),
			MakerAmountFilled: newUint256(1000),
			TakerAmountFilled: newUint256(500),
			Fee:               newUint256(10),
		}
		slot.AppendExchangeOrderFilled(meta, ev)
	}

	if rb.Len() != 4 {
		t.Errorf("Len() after filling 4 blocks: want 4, got %d", rb.Len())
	}

	// Verify retrieval
	slot, ok := rb.GetProtoEventBlock(101)
	if !ok {
		t.Fatal("GetProtoEventBlock(101) not found")
	}
	if slot.HeaderBlockNumber != 101 {
		t.Errorf("retrieved block number = %d, want 101", slot.HeaderBlockNumber)
	}

	// Verify non-existent block
	_, ok = rb.GetProtoEventBlock(999)
	if ok {
		t.Error("GetProtoEventBlock(999) should not be found")
	}
}

func TestProtoRingBufferWrapAndEviction(t *testing.T) {
	rb, err := generated.NewProtoRingBuffer(4)
	if err != nil {
		t.Fatal(err)
	}

	// Fill with 4 blocks (no eviction yet)
	for blockNum := uint64(1); blockNum <= 4; blockNum++ {
		slot := rb.NextProtoSlot(blockNum, fmt.Sprintf("0x%064x", blockNum))
		meta := newTestMeta(blockNum, 0)
		ev := &generated.NegRiskAdapterPositionSplit{
			EventMeta:   meta,
			Stakeholder: common.HexToAddress("0xCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"),
			ConditionID: common.HexToHash(fmt.Sprintf("0x%064x", blockNum)),
			Amount:      newUint256(100),
		}
		slot.AppendNegRiskAdapterPositionSplit(meta, ev)
	}

	if rb.Len() != 4 {
		t.Errorf("Len() after first 4: want 4, got %d", rb.Len())
	}

	// Block 1 should be retrievable
	_, ok := rb.GetProtoEventBlock(1)
	if !ok {
		t.Error("block 1 should exist before wrap")
	}

	// Add one more — evicts block 1
	slot := rb.NextProtoSlot(5, fmt.Sprintf("0x%064x", 5))
	meta := newTestMeta(5, 0)
	ev := &generated.NegRiskAdapterPositionSplit{
		EventMeta:   meta,
		Stakeholder: common.HexToAddress("0xDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"),
		ConditionID: common.HexToHash("0x" + fmt.Sprintf("%064x", 5)),
		Amount:      newUint256(200),
	}
	slot.AppendNegRiskAdapterPositionSplit(meta, ev)

	if rb.Len() != 4 {
		t.Errorf("Len() after wrap: want 4, got %d", rb.Len())
	}

	// Block 1 should be evicted
	_, ok = rb.GetProtoEventBlock(1)
	if ok {
		t.Error("block 1 should be evicted after wrap")
	}

	// Blocks 2-5 should still be there
	for blockNum := uint64(2); blockNum <= 5; blockNum++ {
		_, ok := rb.GetProtoEventBlock(blockNum)
		if !ok {
			t.Errorf("block %d should exist after wrap", blockNum)
		}
	}
}

func TestProtoRingBufferReset(t *testing.T) {
	rb, err := generated.NewProtoRingBuffer(4)
	if err != nil {
		t.Fatal(err)
	}

	for blockNum := uint64(1); blockNum <= 3; blockNum++ {
		rb.NextProtoSlot(blockNum, fmt.Sprintf("0x%064x", blockNum))
	}

	if rb.Len() != 3 {
		t.Errorf("Len before Reset: want 3, got %d", rb.Len())
	}

	rb.Reset()

	if rb.Len() != 0 {
		t.Errorf("Len after Reset: want 0, got %d", rb.Len())
	}

	// After Reset, no blocks should be findable
	_, ok := rb.GetProtoEventBlock(1)
	if ok {
		t.Error("get after Reset should return false")
	}

	// Should be able to reuse after Reset
	slot := rb.NextProtoSlot(100, "0x"+fmt.Sprintf("%064x", 100))
	if slot == nil {
		t.Fatal("NextProtoSlot after Reset returned nil")
	}
	if slot.HeaderBlockNumber != 100 {
		t.Errorf("HeaderBlockNumber after Reset: got %d, want 100", slot.HeaderBlockNumber)
	}
}

// =============================================================================
// 3. ProtoEventBlock ToParsedBlock Parity with ParsedBlock
// =============================================================================

func TestProtoEventBlockToParsedBlockParity(t *testing.T) {
	block := generated.NewProtoEventBlock()
	block.HeaderBlockNumber = 42000000
	block.HeaderBlockHash = "0xdeadbeef"

	// Append one of each major type
	meta1 := newTestMeta(42000000, 0)
	ev1 := &generated.ConditionalTokensConditionPreparation{
		EventMeta:        meta1,
		ConditionID:      common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
		Oracle:           common.HexToAddress("0x2222222222222222222222222222222222222222"),
		QuestionID:       common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333"),
		OutcomeSlotCount: newUint256(2),
	}
	block.AppendConditionalTokensConditionPreparation(meta1, ev1)

	meta2 := newTestMeta(42000000, 1)
	ev2 := &generated.ExchangeOrderFilled{
		EventMeta:         meta2,
		Maker:             common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
		Taker:             common.HexToAddress("0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"),
		MakerAssetID:      newUint256(100),
		TakerAssetID:      newUint256(200),
		MakerAmountFilled: newUint256(5000),
		TakerAmountFilled: newUint256(2500),
		Fee:               newUint256(25),
	}
	block.AppendExchangeOrderFilled(meta2, ev2)

	parsed := block.ToParsedBlock()
	if parsed == nil {
		t.Fatal("ToParsedBlock returned nil")
	}

	if parsed.BlockNumber != 42000000 {
		t.Errorf("BlockNumber: got %d, want 42000000", parsed.BlockNumber)
	}
	if parsed.BlockHash != "0xdeadbeef" {
		t.Errorf("BlockHash: got %s, want 0xdeadbeef", parsed.BlockHash)
	}

	// Sequence should match
	if len(parsed.Sequence) != 2 {
		t.Fatalf("expected 2 Sequence entries, got %d", len(parsed.Sequence))
	}
	if parsed.Sequence[0] != uint8(generated.EventTypeConditionalTokensConditionPreparation) {
		t.Errorf("Sequence[0] mismatch")
	}
	if parsed.Sequence[1] != uint8(generated.EventTypeExchangeOrderFilled) {
		t.Errorf("Sequence[1] mismatch")
	}

	// Check ConditionPreparation
	if len(parsed.ConditionalTokensConditionPreparations) != 1 {
		t.Fatalf("expected 1 ConditionPreparation, got %d", len(parsed.ConditionalTokensConditionPreparations))
	}
	cp := parsed.ConditionalTokensConditionPreparations[0]
	if cp.ConditionID != ev1.ConditionID {
		t.Errorf("ConditionID mismatch")
	}
	if cp.OutcomeSlotCount != ev1.OutcomeSlotCount {
		t.Errorf("OutcomeSlotCount mismatch")
	}

	// Check ExchangeOrderFilled
	if len(parsed.ExchangeOrderFilleds) != 1 {
		t.Fatalf("expected 1 ExchangeOrderFilled, got %d", len(parsed.ExchangeOrderFilleds))
	}
	eof := parsed.ExchangeOrderFilleds[0]
	if eof.Maker != ev2.Maker {
		t.Errorf("Maker mismatch")
	}
	if eof.Taker != ev2.Taker {
		t.Errorf("Taker mismatch")
	}
	if eof.Fee != ev2.Fee {
		t.Errorf("Fee mismatch")
	}

	// Ensure empty slices for types we didn't append
	if len(parsed.ConditionalTokensConditionResolutions) != 0 {
		t.Errorf("expected 0 ConditionResolutions")
	}
}

func TestProtoEventBlockToParsedBlockNil(t *testing.T) {
	var nilBlock *generated.ProtoEventBlock
	parsed := nilBlock.ToParsedBlock()
	if parsed != nil {
		t.Error("ToParsedBlock on nil should return nil")
	}
}

// =============================================================================
// 4. ProtoEventBlock Events Iter via Sequence Walk
// =============================================================================

func TestProtoEventBlockEventsIterSequenceOrder(t *testing.T) {
	block := generated.NewProtoEventBlock()
	block.HeaderBlockNumber = 100

	// Append events in a specific order
	meta := newTestMeta(100, 0)
	cp := &generated.ConditionalTokensConditionPreparation{
		EventMeta:        meta,
		ConditionID:      common.HexToHash("0xAAAA"),
		Oracle:           common.HexToAddress("0xBBBB"),
		QuestionID:       common.HexToHash("0xCCCC"),
		OutcomeSlotCount: newUint256(2),
	}
	block.AppendConditionalTokensConditionPreparation(meta, cp)

	meta2 := newTestMeta(100, 1)
	res := &generated.ConditionalTokensConditionResolution{
		EventMeta:         meta2,
		ConditionID:       common.HexToHash("0xAAAA"),
		Oracle:            common.HexToAddress("0xBBBB"),
		QuestionID:        common.HexToHash("0xCCCC"),
		PayoutDenominator: newUint256(10),
		PayoutNumerators:  []uint256.Int{newUint256(3), newUint256(7)},
	}
	block.AppendConditionalTokensConditionResolution(meta2, res)

	// Sequence should reflect insertion order
	if len(block.Sequence) != 2 {
		t.Fatalf("expected 2 Sequence entries, got %d", len(block.Sequence))
	}
	if block.Sequence[0] != uint8(generated.EventTypeConditionalTokensConditionPreparation) {
		t.Error("Sequence[0] should be ConditionPreparation")
	}
	if block.Sequence[1] != uint8(generated.EventTypeConditionalTokensConditionResolution) {
		t.Error("Sequence[1] should be ConditionResolution")
	}

	// Iterate events
	ch := block.EventsIter()
	if ch == nil {
		t.Fatal("EventsIter returned nil")
	}
	var events []generated.DecodedLog
	for e := range ch {
		events = append(events, e)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events from iterator, got %d", len(events))
	}

	// First event should be ConditionPreparation
	firstEv, ok := events[0].Value.(*generated.ConditionalTokensConditionPreparation)
	if !ok {
		t.Errorf("expected first event to be *ConditionalTokensConditionPreparation, got %T", events[0].Value)
	} else {
		if firstEv.ConditionID != cp.ConditionID {
			t.Errorf("ConditionID mismatch in iterated event")
		}
	}

	// Second event should be ConditionResolution
	secondEv, ok := events[1].Value.(*generated.ConditionalTokensConditionResolution)
	if !ok {
		t.Errorf("expected second event to be *ConditionalTokensConditionResolution, got %T", events[1].Value)
	} else {
		if len(secondEv.PayoutNumerators) != 2 {
			t.Errorf("expected 2 payout numerators, got %d", len(secondEv.PayoutNumerators))
		}
	}
}

func TestProtoEventBlockEventsIterNil(t *testing.T) {
	var nilBlock *generated.ProtoEventBlock
	ch := nilBlock.EventsIter()
	if ch != nil {
		t.Error("EventsIter on nil block should return nil")
	}
}

// =============================================================================
// 5. All 18 Event Types Proto Append + Readback
// =============================================================================

func TestAll18EventTypesAppendAndReadback(t *testing.T) {
	block := generated.NewProtoEventBlock()
	meta := newTestMeta(42000000, 0)

	// 1. ConditionalTokensConditionPreparation (EventType=1)
	block.AppendConditionalTokensConditionPreparation(meta, &generated.ConditionalTokensConditionPreparation{
		EventMeta: meta, ConditionID: common.HexToHash("0xaaaa"), Oracle: common.HexToAddress("0xbbbb"),
		QuestionID: common.HexToHash("0xcccc"), OutcomeSlotCount: newUint256(2),
	})
	if block.ConditionalTokensConditionPreparation_meta_index.Rows() != 1 {
		t.Error("ConditionPreparation not appended")
	}

	// 2. ConditionalTokensConditionResolution (EventType=2)
	block.AppendConditionalTokensConditionResolution(meta, &generated.ConditionalTokensConditionResolution{
		EventMeta: meta, ConditionID: common.HexToHash("0xaaaa"), Oracle: common.HexToAddress("0xbbbb"),
		QuestionID: common.HexToHash("0xcccc"), PayoutDenominator: newUint256(100),
		PayoutNumerators: []uint256.Int{newUint256(40), newUint256(60)},
	})
	if block.ConditionalTokensConditionResolution_meta_index.Rows() != 1 {
		t.Error("ConditionResolution not appended")
	}

	// 3. ConditionalTokensPositionSplit (EventType=3)
	block.AppendConditionalTokensPositionSplit(meta, &generated.ConditionalTokensPositionSplit{
		EventMeta: meta, Stakeholder: common.HexToAddress("0xeeee"), CollateralToken: common.HexToAddress("0xffff"),
		ParentCollectionID: common.HexToHash("0xa1"), ConditionID: common.HexToHash("0xa2"),
		Partition: []uint256.Int{newUint256(1), newUint256(2)}, Amount: newUint256(1000),
	})
	if block.ConditionalTokensPositionSplit_meta_index.Rows() != 1 {
		t.Error("PositionSplit not appended")
	}

	// 4. ConditionalTokensPositionsMerge (EventType=4)
	block.AppendConditionalTokensPositionsMerge(meta, &generated.ConditionalTokensPositionsMerge{
		EventMeta: meta, Stakeholder: common.HexToAddress("0xeeee"), CollateralToken: common.HexToAddress("0xffff"),
		ParentCollectionID: common.HexToHash("0xb1"), ConditionID: common.HexToHash("0xb2"),
		Partition: []uint256.Int{newUint256(1), newUint256(2)}, Amount: newUint256(500),
	})
	if block.ConditionalTokensPositionsMerge_meta_index.Rows() != 1 {
		t.Error("PositionsMerge not appended")
	}

	// 5. ConditionalTokensPayoutRedemption (EventType=5)
	block.AppendConditionalTokensPayoutRedemption(meta, &generated.ConditionalTokensPayoutRedemption{
		EventMeta: meta, Redeemer: common.HexToAddress("0x1111"), CollateralToken: common.HexToAddress("0x2222"),
		ParentCollectionID: common.HexToHash("0xc1"), ConditionID: common.HexToHash("0xc2"),
		IndexSets: []uint256.Int{newUint256(0)}, Payout: newUint256(500),
	})
	if block.ConditionalTokensPayoutRedemption_meta_index.Rows() != 1 {
		t.Error("PayoutRedemption not appended")
	}

	// 6. ExchangeOrderFilled (EventType=6)
	block.AppendExchangeOrderFilled(meta, &generated.ExchangeOrderFilled{
		EventMeta: meta, Maker: common.HexToAddress("0x3333"), Taker: common.HexToAddress("0x4444"),
		MakerAssetID: newUint256(1), TakerAssetID: newUint256(2),
		MakerAmountFilled: newUint256(100), TakerAmountFilled: newUint256(50), Fee: newUint256(1),
	})
	if block.ExchangeOrderFilled_meta_index.Rows() != 1 {
		t.Error("ExchangeOrderFilled not appended")
	}

	// 7. NegRiskExchangeOrderFilled (EventType=7)
	block.AppendNegRiskExchangeOrderFilled(meta, &generated.NegRiskExchangeOrderFilled{
		EventMeta: meta, Maker: common.HexToAddress("0x5555"), Taker: common.HexToAddress("0x6666"),
		MakerAssetID: newUint256(1), TakerAssetID: newUint256(2),
		MakerAmountFilled: newUint256(200), TakerAmountFilled: newUint256(100), Fee: newUint256(2),
	})
	if block.NegRiskExchangeOrderFilled_meta_index.Rows() != 1 {
		t.Error("NegRiskExchangeOrderFilled not appended")
	}

	// 8. NegRiskAdapterMarketPrepared (EventType=8)
	block.AppendNegRiskAdapterMarketPrepared(meta, &generated.NegRiskAdapterMarketPrepared{
		EventMeta: meta, MarketID: common.HexToHash("0xd1"), Creator: common.HexToAddress("0x7777"),
		FeeBips: newUint256(30), Data: []byte{0xde, 0xad, 0xbe, 0xef},
	})
	if block.NegRiskAdapterMarketPrepared_meta_index.Rows() != 1 {
		t.Error("MarketPrepared not appended")
	}

	// 9. NegRiskAdapterQuestionPrepared (EventType=9)
	block.AppendNegRiskAdapterQuestionPrepared(meta, &generated.NegRiskAdapterQuestionPrepared{
		EventMeta: meta, MarketID: common.HexToHash("0xe1"), QuestionID: common.HexToHash("0xe2"),
		Index: newUint256(0), Data: []byte{0x01, 0x02, 0x03},
	})
	if block.NegRiskAdapterQuestionPrepared_meta_index.Rows() != 1 {
		t.Error("QuestionPrepared not appended")
	}

	// 10. NegRiskAdapterPositionSplit (EventType=10)
	block.AppendNegRiskAdapterPositionSplit(meta, &generated.NegRiskAdapterPositionSplit{
		EventMeta: meta, Stakeholder: common.HexToAddress("0x8888"),
		ConditionID: common.HexToHash("0xf1"), Amount: newUint256(3000),
	})
	if block.NegRiskAdapterPositionSplit_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPositionSplit not appended")
	}

	// 11. NegRiskAdapterPositionsMerge (EventType=11)
	block.AppendNegRiskAdapterPositionsMerge(meta, &generated.NegRiskAdapterPositionsMerge{
		EventMeta: meta, Stakeholder: common.HexToAddress("0x9999"),
		ConditionID: common.HexToHash("0xf2"), Amount: newUint256(1500),
	})
	if block.NegRiskAdapterPositionsMerge_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPositionsMerge not appended")
	}

	// 12. NegRiskAdapterPositionsConverted (EventType=12)
	block.AppendNegRiskAdapterPositionsConverted(meta, &generated.NegRiskAdapterPositionsConverted{
		EventMeta: meta, Stakeholder: common.HexToAddress("0xaaaa"),
		MarketID: common.HexToHash("0xf3"), IndexSet: newUint256(1), Amount: newUint256(200),
	})
	if block.NegRiskAdapterPositionsConverted_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPositionsConverted not appended")
	}

	// 13. NegRiskAdapterPayoutRedemption (EventType=13)
	block.AppendNegRiskAdapterPayoutRedemption(meta, &generated.NegRiskAdapterPayoutRedemption{
		EventMeta: meta, Redeemer: common.HexToAddress("0xbbbb"),
		ConditionID: common.HexToHash("0xf4"), Amounts: []uint256.Int{newUint256(100)},
		Payout: newUint256(100),
	})
	if block.NegRiskAdapterPayoutRedemption_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPayoutRedemption not appended")
	}

	// 14. FixedProductMarketMakerFactoryFixedProductMarketMakerCreation (EventType=14)
	block.AppendFixedProductMarketMakerFactoryFixedProductMarketMakerCreation(meta,
		&generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation{
			EventMeta: meta, Creator: common.HexToAddress("0xcccc"),
			FixedProductMarketMaker: common.HexToAddress("0xdddd"),
			ConditionalTokens:       common.HexToAddress("0xeeee"),
			CollateralToken:         common.HexToAddress("0xffff"),
			ConditionIds:            []common.Hash{common.HexToHash("0xaabb")},
			Fee:                     newUint256(5),
		})
	if block.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation_meta_index.Rows() != 1 {
		t.Error("FactoryCreation not appended")
	}

	// 15. FixedProductMarketMakerFPMMBuy (EventType=15)
	block.AppendFixedProductMarketMakerFPMMBuy(meta, &generated.FixedProductMarketMakerFPMMBuy{
		EventMeta: meta, Buyer: common.HexToAddress("0x1111"), InvestmentAmount: newUint256(100000),
		FeeAmount: newUint256(100), OutcomeIndex: newUint256(0), OutcomeTokensBought: newUint256(50000),
	})
	if block.FixedProductMarketMakerFPMMBuy_meta_index.Rows() != 1 {
		t.Error("FPMMBuy not appended")
	}

	// 16. FixedProductMarketMakerFPMMSell (EventType=16)
	block.AppendFixedProductMarketMakerFPMMSell(meta, &generated.FixedProductMarketMakerFPMMSell{
		EventMeta: meta, Seller: common.HexToAddress("0x2222"), ReturnAmount: newUint256(90000),
		FeeAmount: newUint256(90), OutcomeIndex: newUint256(0), OutcomeTokensSold: newUint256(45000),
	})
	if block.FixedProductMarketMakerFPMMSell_meta_index.Rows() != 1 {
		t.Error("FPMMSell not appended")
	}

	// 17. FixedProductMarketMakerFPMMFundingAdded (EventType=17)
	block.AppendFixedProductMarketMakerFPMMFundingAdded(meta, &generated.FixedProductMarketMakerFPMMFundingAdded{
		EventMeta: meta, Funder: common.HexToAddress("0x3333"),
		AmountsAdded: []uint256.Int{newUint256(10000), newUint256(10000)}, SharesMinted: newUint256(20000),
	})
	if block.FixedProductMarketMakerFPMMFundingAdded_meta_index.Rows() != 1 {
		t.Error("FundingAdded not appended")
	}

	// 18. FixedProductMarketMakerFPMMFundingRemoved (EventType=18)
	block.AppendFixedProductMarketMakerFPMMFundingRemoved(meta, &generated.FixedProductMarketMakerFPMMFundingRemoved{
		EventMeta: meta, Funder: common.HexToAddress("0x4444"),
		AmountsRemoved: []uint256.Int{newUint256(5000)}, CollateralRemovedFromFeePool: newUint256(100),
		SharesBurnt: newUint256(5000),
	})
	if block.FixedProductMarketMakerFPMMFundingRemoved_meta_index.Rows() != 1 {
		t.Error("FundingRemoved not appended")
	}

	// Verify total event count in Sequence
	if len(block.Sequence) != 18 {
		t.Errorf("expected 18 Sequence entries, got %d", len(block.Sequence))
	}

	// Verify Sequence values
	expectedTypes := []uint8{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}
	for i, expected := range expectedTypes {
		if block.Sequence[i] != expected {
			t.Errorf("Sequence[%d]: got %d, want %d", i, block.Sequence[i], expected)
		}
	}

	// Readback via ProtoAt: verify a few key fields
	// Event 1 (ConditionPreparation)
	v1 := block.ConditionalTokensConditionPreparationProtoAt(0)
	if v1.OutcomeSlotCount() != newUint256(2) {
		t.Errorf("ConditionPreparation readback failed: OutcomeSlotCount")
	}

	// Event 2 (ConditionResolution)
	v2 := block.ConditionalTokensConditionResolutionProtoAt(0)
	nums := v2.PayoutNumerators()
	if len(nums) != 2 {
		t.Errorf("ConditionResolution readback: expected 2 numerators, got %d", len(nums))
	}

	// Event 6 (ExchangeOrderFilled)
	v6 := block.ExchangeOrderFilledProtoAt(0)
	if v6.Fee() != newUint256(1) {
		t.Errorf("ExchangeOrderFilled readback: Fee mismatch")
	}

	// Event 8 (MarketPrepared)
	v8 := block.NegRiskAdapterMarketPreparedProtoAt(0)
	if len(v8.Data()) != 4 {
		t.Errorf("MarketPrepared readback: Data length mismatch, got %d", len(v8.Data()))
	}

	// Event 14 (FactoryCreation)
	v14 := block.FixedProductMarketMakerFactoryFixedProductMarketMakerCreationProtoAt(0)
	condIds := v14.ConditionIds()
	if len(condIds) != 1 {
		t.Errorf("FactoryCreation readback: expected 1 ConditionId, got %d", len(condIds))
	}

	// Event 17 (FundingAdded)
	v17 := block.FixedProductMarketMakerFPMMFundingAddedProtoAt(0)
	added := v17.AmountsAdded()
	if len(added) != 2 {
		t.Errorf("FundingAdded readback: expected 2 amounts, got %d", len(added))
	}

	// All query counts verify exactly 1 row per type
	if block.ConditionalTokensConditionPreparation_meta_index.Rows() != 1 {
		t.Error("ConditionalTokensConditionPreparation count != 1")
	}
	if block.ConditionalTokensConditionResolution_meta_index.Rows() != 1 {
		t.Error("ConditionalTokensConditionResolution count != 1")
	}
	if block.ConditionalTokensPositionSplit_meta_index.Rows() != 1 {
		t.Error("ConditionalTokensPositionSplit count != 1")
	}
	if block.ConditionalTokensPositionsMerge_meta_index.Rows() != 1 {
		t.Error("ConditionalTokensPositionsMerge count != 1")
	}
	if block.ConditionalTokensPayoutRedemption_meta_index.Rows() != 1 {
		t.Error("ConditionalTokensPayoutRedemption count != 1")
	}
	if block.ExchangeOrderFilled_meta_index.Rows() != 1 {
		t.Error("ExchangeOrderFilled count != 1")
	}
	if block.NegRiskExchangeOrderFilled_meta_index.Rows() != 1 {
		t.Error("NegRiskExchangeOrderFilled count != 1")
	}
	if block.NegRiskAdapterMarketPrepared_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterMarketPrepared count != 1")
	}
	if block.NegRiskAdapterQuestionPrepared_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterQuestionPrepared count != 1")
	}
	if block.NegRiskAdapterPositionSplit_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPositionSplit count != 1")
	}
	if block.NegRiskAdapterPositionsMerge_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPositionsMerge count != 1")
	}
	if block.NegRiskAdapterPositionsConverted_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPositionsConverted count != 1")
	}
	if block.NegRiskAdapterPayoutRedemption_meta_index.Rows() != 1 {
		t.Error("NegRiskAdapterPayoutRedemption count != 1")
	}
	if block.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation_meta_index.Rows() != 1 {
		t.Error("FixedProductMarketMakerFactoryFixedProductMarketMakerCreation count != 1")
	}
	if block.FixedProductMarketMakerFPMMBuy_meta_index.Rows() != 1 {
		t.Error("FixedProductMarketMakerFPMMBuy count != 1")
	}
	if block.FixedProductMarketMakerFPMMSell_meta_index.Rows() != 1 {
		t.Error("FixedProductMarketMakerFPMMSell count != 1")
	}
	if block.FixedProductMarketMakerFPMMFundingAdded_meta_index.Rows() != 1 {
		t.Error("FixedProductMarketMakerFPMMFundingAdded count != 1")
	}
	if block.FixedProductMarketMakerFPMMFundingRemoved_meta_index.Rows() != 1 {
		t.Error("FixedProductMarketMakerFPMMFundingRemoved count != 1")
	}
}

func TestProtoEventBlockAppendNil(t *testing.T) {
	block := generated.NewProtoEventBlock()
	meta := newTestMeta(100, 0)

	// Append with nil event should be no-op (not panic)
	block.AppendConditionalTokensConditionPreparation(meta, nil)
	if len(block.Sequence) != 0 {
		t.Error("Append with nil event should not add to Sequence")
	}

	block.AppendExchangeOrderFilled(meta, nil)
	if len(block.Sequence) != 0 {
		t.Error("AppendExchangeOrderFilled nil should be no-op")
	}
}

// =============================================================================
// 6. ProtoSlot Reuse Across Blocks
// =============================================================================

func TestProtoSlotReuseAcrossBlocks(t *testing.T) {
	rb, err := generated.NewProtoRingBuffer(4)
	if err != nil {
		t.Fatal(err)
	}

	// first block writes into slot 0
	slot0 := rb.NextProtoSlot(100, "0xab")
	meta := newTestMeta(100, 0)
	slot0.AppendConditionalTokensConditionPreparation(meta, &generated.ConditionalTokensConditionPreparation{
		EventMeta: meta, ConditionID: common.HexToHash("0x01"), Oracle: common.HexToAddress("0x02"),
		QuestionID: common.HexToHash("0x03"), OutcomeSlotCount: newUint256(2),
	})

	// fill remaining slots
	for bn := uint64(101); bn <= 103; bn++ {
		rb.NextProtoSlot(bn, fmt.Sprintf("0x%064x", bn))
	}

	// now next slot wraps back to index 0
	wrappedSlot := rb.NextProtoSlot(200, "0xcd")
	if wrappedSlot == nil {
		t.Fatal("wrapped slot is nil")
	}

	// the wrapped slot should be completely fresh (previous data cleared by Reset)
	if len(wrappedSlot.Sequence) != 0 {
		t.Errorf("wrapped slot should have empty Sequence, got %d", len(wrappedSlot.Sequence))
	}
	if wrappedSlot.HeaderBlockNumber != 200 {
		t.Errorf("wrapped slot HeaderBlockNumber: got %d, want 200", wrappedSlot.HeaderBlockNumber)
	}
	if wrappedSlot.HeaderBlockHash != "0xcd" {
		t.Errorf("wrapped slot HeaderBlockHash: got %s, want 0xcd", wrappedSlot.HeaderBlockHash)
	}

	// old block 100 should be evicted by now
	_, ok := rb.GetProtoEventBlock(100)
	if ok {
		t.Error("block 100 should be evicted after wrap")
	}

	// new block 200 should be findable
	slot, ok := rb.GetProtoEventBlock(200)
	if !ok {
		t.Fatal("block 200 should be findable")
	}
	if slot.HeaderBlockNumber != 200 {
		t.Errorf("retrieved block 200 number: %d", slot.HeaderBlockNumber)
	}

	// append events to wrapped slot
	meta2 := newTestMeta(200, 0)
	wrappedSlot.AppendExchangeOrderFilled(meta2, &generated.ExchangeOrderFilled{
		EventMeta: meta2, Maker: common.HexToAddress("0x3333"), Taker: common.HexToAddress("0x4444"),
		MakerAssetID: newUint256(1), TakerAssetID: newUint256(2),
		MakerAmountFilled: newUint256(100), TakerAmountFilled: newUint256(50), Fee: newUint256(1),
	})

	if len(wrappedSlot.Sequence) != 1 {
		t.Errorf("wrapped slot should have 1 event, got %d", len(wrappedSlot.Sequence))
	}
	if wrappedSlot.ExchangeOrderFilled_meta_index.Rows() != 1 {
		t.Errorf("wrapped slot should have 1 ExchangeOrderFilled row, got %d", wrappedSlot.ExchangeOrderFilled_meta_index.Rows())
	}
}

// =============================================================================
// 7. Batch Insertion Column Counts and Reset
// =============================================================================

func TestBatchInsertionColumnCounts(t *testing.T) {
	batch := generated.NewConditionalTokensConditionPreparationBatch()
	if batch == nil {
		t.Fatal("NewConditionalTokensConditionPreparationBatch returned nil")
	}

	if batch.Rows() != 0 {
		t.Errorf("empty batch should have 0 rows, got %d", batch.Rows())
	}

	meta := newTestMeta(100, 0)
	ev := &generated.ConditionalTokensConditionPreparation{
		EventMeta:        meta,
		ConditionID:      common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
		Oracle:           common.HexToAddress("0x2222222222222222222222222222222222222222"),
		QuestionID:       common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333"),
		OutcomeSlotCount: newUint256(2),
	}

	ok := batch.Append(meta, ev)
	if !ok {
		t.Fatal("Append returned false")
	}

	if batch.Rows() != 1 {
		t.Errorf("batch should have 1 row, got %d", batch.Rows())
	}

	// Append more rows
	for i := 0; i < 9; i++ {
		metaI := newTestMeta(100, uint64(i+1))
		evI := &generated.ConditionalTokensConditionPreparation{
			EventMeta:        metaI,
			ConditionID:      common.HexToHash(fmt.Sprintf("0x%064x", i+1)),
			Oracle:           common.HexToAddress("0x2222222222222222222222222222222222222222"),
			QuestionID:       common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333"),
			OutcomeSlotCount: newUint256(uint64(i + 2)),
		}
		batch.Append(metaI, evI)
	}

	if batch.Rows() != 10 {
		t.Errorf("batch should have 10 rows after 10 appends, got %d", batch.Rows())
	}

	// Verify ColumnNames
	names := batch.ColumnNames()
	if len(names) < 4 {
		t.Errorf("expected at least 4 column names (common + event), got %d: %v", len(names), names)
	}

	// Verify Inputs
	inputs := batch.Inputs()
	if len(inputs) < 4 {
		t.Errorf("expected at least 4 input columns, got %d", len(inputs))
	}

	// Reset
	batch.Reset()
	if batch.Rows() != 0 {
		t.Errorf("batch should have 0 rows after Reset, got %d", batch.Rows())
	}

	// Can re-append after Reset
	if !batch.Append(meta, ev) {
		t.Error("Append after Reset returned false")
	}
	if batch.Rows() != 1 {
		t.Errorf("batch should have 1 row after Reset+re-append, got %d", batch.Rows())
	}
}

func TestAllBatchTypesColumnCounts(t *testing.T) {
	meta := newTestMeta(100, 0)

	tests := []struct {
		name    string
		batch   generated.ClickHouseEventBatch
		ev      any
		minCols int
		table   string
	}{
		{
			"ConditionPreparation",
			generated.NewConditionalTokensConditionPreparationBatch(),
			&generated.ConditionalTokensConditionPreparation{EventMeta: meta, ConditionID: common.HexToHash("0x01"), Oracle: common.HexToAddress("0x02"), QuestionID: common.HexToHash("0x03"), OutcomeSlotCount: newUint256(2)},
			8, "conditional_tokens_condition_preparation_events",
		},
		{
			"ConditionResolution",
			generated.NewConditionalTokensConditionResolutionBatch(),
			&generated.ConditionalTokensConditionResolution{EventMeta: meta, ConditionID: common.HexToHash("0x01"), Oracle: common.HexToAddress("0x02"), QuestionID: common.HexToHash("0x03"), PayoutDenominator: newUint256(10), PayoutNumerators: []uint256.Int{newUint256(3), newUint256(7)}},
			9, "conditional_tokens_condition_resolution_events",
		},
		{
			"PositionSplit",
			generated.NewConditionalTokensPositionSplitBatch(),
			&generated.ConditionalTokensPositionSplit{EventMeta: meta, Stakeholder: common.HexToAddress("0xaa"), CollateralToken: common.HexToAddress("0xbb"), ParentCollectionID: common.HexToHash("0xcc"), ConditionID: common.HexToHash("0xdd"), Partition: []uint256.Int{newUint256(1)}, Amount: newUint256(100)},
			10, "conditional_tokens_position_split_events",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.batch == nil {
				t.Fatal("batch is nil")
			}
			if tc.batch.TableName() != tc.table {
				t.Errorf("TableName: got %s, want %s", tc.batch.TableName(), tc.table)
			}
			if !tc.batch.Append(meta, tc.ev) {
				t.Error("Append returned false")
			}
			if tc.batch.Rows() != 1 {
				t.Errorf("Rows: want 1, got %d", tc.batch.Rows())
			}
			cols := tc.batch.ColumnNames()
			if len(cols) < tc.minCols {
				t.Errorf("ColumnNames: want at least %d, got %d: %v", tc.minCols, len(cols), cols)
			}
			inputs := tc.batch.Inputs()
			if len(inputs) < tc.minCols {
				t.Errorf("Inputs: want at least %d, got %d", tc.minCols, len(inputs))
			}

			// Reset and verify
			tc.batch.Reset()
			if tc.batch.Rows() != 0 {
				t.Error("Rows after Reset should be 0")
			}
			// Re-append
			if !tc.batch.Append(meta, tc.ev) {
				t.Error("Append after Reset returned false")
			}
			if tc.batch.Rows() != 1 {
				t.Errorf("Rows after Reset+re-append: want 1, got %d", tc.batch.Rows())
			}
		})
	}
}

func TestBatchAppendWrongType(t *testing.T) {
	batch := generated.NewConditionalTokensConditionPreparationBatch()
	meta := newTestMeta(100, 0)

	// Try appending a wrong event type
	wrongEv := &generated.ExchangeOrderFilled{
		EventMeta: meta, Maker: common.HexToAddress("0x3333"), Taker: common.HexToAddress("0x4444"),
		MakerAssetID: newUint256(1), TakerAssetID: newUint256(2),
		MakerAmountFilled: newUint256(100), TakerAmountFilled: newUint256(50), Fee: newUint256(1),
	}
	if batch.Append(meta, wrongEv) {
		t.Error("Append with wrong type should return false")
	}
	if batch.Rows() != 0 {
		t.Errorf("Rows should be 0 after wrong-type append, got %d", batch.Rows())
	}
}

// =============================================================================
// Edge cases
// =============================================================================

func TestProtoEventBlockSequenceInterleavedTypes(t *testing.T) {
	block := generated.NewProtoEventBlock()

	// Interleave two types: PositionSplit and PositionsMerge
	meta := newTestMeta(100, 0)
	split := &generated.ConditionalTokensPositionSplit{
		EventMeta: meta, Stakeholder: common.HexToAddress("0xa1"), CollateralToken: common.HexToAddress("0xb1"),
		ParentCollectionID: common.HexToHash("0xc1"), ConditionID: common.HexToHash("0xd1"),
		Partition: []uint256.Int{newUint256(2)}, Amount: newUint256(100),
	}
	merge := &generated.ConditionalTokensPositionsMerge{
		EventMeta: meta, Stakeholder: common.HexToAddress("0xa2"), CollateralToken: common.HexToAddress("0xb2"),
		ParentCollectionID: common.HexToHash("0xc2"), ConditionID: common.HexToHash("0xd2"),
		Partition: []uint256.Int{newUint256(2)}, Amount: newUint256(50),
	}

	block.AppendConditionalTokensPositionSplit(meta, split)
	block.AppendConditionalTokensPositionsMerge(meta, merge)
	block.AppendConditionalTokensPositionSplit(meta, split)
	block.AppendConditionalTokensPositionsMerge(meta, merge)

	if len(block.Sequence) != 4 {
		t.Fatalf("expected 4 Sequence entries, got %d", len(block.Sequence))
	}
	expectedSequence := []uint8{
		uint8(generated.EventTypeConditionalTokensPositionSplit),
		uint8(generated.EventTypeConditionalTokensPositionsMerge),
		uint8(generated.EventTypeConditionalTokensPositionSplit),
		uint8(generated.EventTypeConditionalTokensPositionsMerge),
	}
	for i, exp := range expectedSequence {
		if block.Sequence[i] != exp {
			t.Errorf("Sequence[%d] = %d, want %d", i, block.Sequence[i], exp)
		}
	}

	// Each type should have 2 rows
	if block.ConditionalTokensPositionSplit_meta_index.Rows() != 2 {
		t.Errorf("PositionSplit rows: got %d, want 2", block.ConditionalTokensPositionSplit_meta_index.Rows())
	}
	if block.ConditionalTokensPositionsMerge_meta_index.Rows() != 2 {
		t.Errorf("PositionsMerge rows: got %d, want 2", block.ConditionalTokensPositionsMerge_meta_index.Rows())
	}

	// Sequence walk should produce correct events
	parsed := block.ToParsedBlock()
	if len(parsed.ConditionalTokensPositionSplits) != 2 {
		t.Errorf("ToParsedBlock PositionSplits: got %d, want 2", len(parsed.ConditionalTokensPositionSplits))
	}
	if len(parsed.ConditionalTokensPositionsMerges) != 2 {
		t.Errorf("ToParsedBlock PositionsMerges: got %d, want 2", len(parsed.ConditionalTokensPositionsMerges))
	}
}

func TestProtoRingBufferManyBlocks(t *testing.T) {
	rb, err := generated.NewProtoRingBuffer(32)
	if err != nil {
		t.Fatal(err)
	}

	// Push 100 blocks
	for blockNum := uint64(1); blockNum <= 100; blockNum++ {
		slot := rb.NextProtoSlot(blockNum, fmt.Sprintf("0x%064x", blockNum))
		if slot == nil {
			t.Fatalf("NextProtoSlot(%d) returned nil", blockNum)
		}
		if slot.HeaderBlockNumber != blockNum {
			t.Errorf("HeaderBlockNumber mismatch at block %d", blockNum)
		}
	}

	// Buffer caps at 32
	if rb.Len() != 32 {
		t.Errorf("Len after 100 pushes: want 32, got %d", rb.Len())
	}

	// Old blocks 1-68 should be evicted
	_, ok := rb.GetProtoEventBlock(50)
	if ok {
		t.Error("block 50 should be evicted")
	}

	// Recent blocks should be present
	_, ok = rb.GetProtoEventBlock(69)
	if !ok {
		t.Error("block 69 should be present")
	}
	_, ok = rb.GetProtoEventBlock(100)
	if !ok {
		t.Error("block 100 should be present")
	}
}
