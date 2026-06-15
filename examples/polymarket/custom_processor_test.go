package polymarket

import (
	"context"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

type mockStore struct{}

func (s *mockStore) Conn() *ch.Client { return nil }
func (s *mockStore) DB() string       { return "polymarket" }

type MemoryStore struct {
	state *generated.State
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{state: generated.NewState()}
}

func (s *MemoryStore) GetUserPosition(user common.Address, tokenID uint256.Int) *generated.InternalPositionState {
	return s.state.GetUserPosition(user, tokenID)
}

func (s *MemoryStore) GetCondition(conditionID common.Hash) (*generated.Condition, bool) {
	return s.state.Condition.Get(conditionID)
}

func TestUpdateAvgPriceDecimal(t *testing.T) {
	tests := []struct {
		name       string
		currentAvg string
		currentAmt string
		newPrice   string
		newAmt     string
		want       string
	}{
		{
			name:       "equal weights",
			currentAvg: "0.5",
			currentAmt: "10",
			newPrice:   "0.6",
			newAmt:     "10",
			want:       "0.55",
		},
		{
			name:       "different weights",
			currentAvg: "0.4",
			currentAmt: "100",
			newPrice:   "0.8",
			newAmt:     "20",
			want:       "0.4666666666666667",
		},
		{
			name:       "zero current amount",
			currentAvg: "0",
			currentAmt: "0",
			newPrice:   "0.5",
			newAmt:     "10",
			want:       "0.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := updateAvgPriceDecimal(decimal.RequireFromString(tt.currentAvg), decimal.RequireFromString(tt.currentAmt), decimal.RequireFromString(tt.newPrice), decimal.RequireFromString(tt.newAmt))
			want := decimal.RequireFromString(tt.want)
			if !got.Equal(want) {
				if got.Sub(want).Abs().GreaterThan(decimal.NewFromFloat(1e-10)) {
					t.Errorf("got %s, want %s", got, want)
				}
			}
		})
	}
}

func TestHandleOrderFilled(t *testing.T) {
	s := NewMemoryStore()
	maker := common.HexToAddress("0x1")
	taker := common.HexToAddress("0x2")
	tokenID := *uint256.NewInt(123)

	ev := &generated.ExchangeOrderFilled{
		Maker:             maker,
		Taker:             taker,
		MakerAssetID:      *uint256.NewInt(0),
		TakerAssetID:      tokenID,
		MakerAmountFilled: *uint256.NewInt(50 * 1e6),
		TakerAmountFilled: *uint256.NewInt(100 * 1e6),
	}

	handleOrderFilled(s.state, ev)

	up := s.GetUserPosition(maker, tokenID)
	if up == nil {
		t.Fatal("expected user position to be created")
	}

	expectedAmount := decimal.NewFromInt(100 * 1e6)
	if !up.Amount.Equal(expectedAmount) {
		t.Errorf("expected amount %s, got %s", expectedAmount, up.Amount)
	}

	expectedPrice := decimal.NewFromFloat(0.5)
	if !up.AvgPrice.Equal(expectedPrice) {
		t.Errorf("expected avg price %s, got %s", expectedPrice, up.AvgPrice)
	}
}

func TestCustomProcessingIntegration(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	block := &generated.ParsedBlock{
		BlockNumber: 100,
		Sequence:    []uint8{uint8(generated.EventTypeConditionalTokensConditionPreparation)},
		ConditionalTokensConditionPreparations: []generated.ConditionalTokensConditionPreparation{
			{
				ConditionID:      common.HexToHash("0x123"),
				Oracle:           common.HexToAddress("0x456"),
				OutcomeSlotCount: *uint256.NewInt(2),
			},
		},
	}

	err := CustomProcessing(ctx, generated.Store(nil), s.state, block)
	if err != nil {
		t.Fatalf("CustomProcessing failed: %v", err)
	}

	if _, ok := s.GetCondition(common.HexToHash("0x123")); !ok {
		t.Errorf("expected condition 0x123 to be stored")
	}
}

func TestCustomProcessingProtoParityWithParsedBlock(t *testing.T) {
	ctx := context.Background()
	parsedState := generated.NewState()
	protoState := generated.NewState()
	parsedBlock, protoBlock := buildParityBlocks()

	if err := CustomProcessing(ctx, generated.Store(nil), parsedState, parsedBlock); err != nil {
		t.Fatalf("parsed CustomProcessing failed: %v", err)
	}
	if err := CustomProcessingProto(ctx, generated.Store(nil), protoState, protoBlock); err != nil {
		t.Fatalf("proto CustomProcessing failed: %v", err)
	}

	assertHotStateParity(t, parsedState, protoState)
}

func TestCustomProcessingProtoUsesProtoCallback(t *testing.T) {
	oldParsed := generated.CustomProcessFn
	oldProto := generated.CustomProcessProtoFn
	defer func() {
		generated.CustomProcessFn = oldParsed
		generated.CustomProcessProtoFn = oldProto
	}()

	var parsedCalls, protoCalls int
	generated.CustomProcessFn = func(state *generated.State, block *generated.ParsedBlock) error {
		parsedCalls++
		return nil
	}
	generated.CustomProcessProtoFn = func(state *generated.State, block *generated.ProtoEventBlock) error {
		protoCalls++
		return nil
	}

	_, protoBlock := buildParityBlocks()
	if err := CustomProcessingProto(context.Background(), generated.Store(nil), generated.NewState(), protoBlock); err != nil {
		t.Fatalf("CustomProcessingProto failed: %v", err)
	}
	if protoCalls != 1 {
		t.Fatalf("proto callback calls = %d, want 1", protoCalls)
	}
	if parsedCalls != 0 {
		t.Fatalf("parsed callback calls = %d, want 0", parsedCalls)
	}
}

func BenchmarkCustomProcessingParsedVsProto(b *testing.B) {
	ctx := context.Background()
	parsedBlock, protoBlock := buildParityBlocks()

	b.Run("parsed", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			state := generated.NewState()
			if err := CustomProcessing(ctx, generated.Store(nil), state, parsedBlock); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("proto", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			state := generated.NewState()
			if err := CustomProcessingProto(ctx, generated.Store(nil), state, protoBlock); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkProcessParsedVsProto(b *testing.B) {
	parsedBlock, protoBlock := buildParityBlocks()

	b.Run("parsed", func(b *testing.B) {
		state := generated.NewState()
		for i := 0; i < b.N; i++ {
			if err := Process(state, parsedBlock); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("proto", func(b *testing.B) {
		state := generated.NewState()
		for i := 0; i < b.N; i++ {
			if err := ProcessProto(state, protoBlock); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func buildParityBlocks() (*generated.ParsedBlock, *generated.ProtoEventBlock) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	blockNumber := uint64(12345)
	blockHash := common.HexToHash("0x12345")
	user := common.HexToAddress("0x0000000000000000000000000000000000000101")
	taker := common.HexToAddress("0x0000000000000000000000000000000000000202")
	collateral := common.HexToAddress("0x0000000000000000000000000000000000000303")
	conditionID := common.HexToHash("0xabcdef")
	questionID := common.HexToHash("0xbeef")
	tokenID := *uint256.NewInt(777)
	fpmm := common.HexToAddress("0x0000000000000000000000000000000000000404")

	meta := func(logIndex uint64, contract common.Address) generated.EventMeta {
		return generated.EventMeta{
			BlockNumber:      blockNumber,
			BlockTimestamp:   ts,
			BlockHash:        blockHash,
			ContractAddress:  contract,
			TransactionHash:  common.BigToHash(new(big.Int).SetUint64(logIndex + 1)),
			TransactionIndex: 1,
			LogIndex:         logIndex,
		}
	}

	conditionPrep := generated.ConditionalTokensConditionPreparation{
		EventMeta:        meta(1, common.HexToAddress(generated.ConditionalTokensConditionPreparationAddress)),
		ConditionID:      conditionID,
		Oracle:           common.HexToAddress("0x0000000000000000000000000000000000000505"),
		QuestionID:       questionID,
		OutcomeSlotCount: *uint256.NewInt(2),
	}
	order := generated.ExchangeOrderFilled{
		EventMeta:         meta(2, common.HexToAddress(generated.ExchangeOrderFilledAddress)),
		Maker:             user,
		Taker:             taker,
		MakerAssetID:      *uint256.NewInt(0),
		TakerAssetID:      tokenID,
		MakerAmountFilled: *uint256.NewInt(5_000_000),
		TakerAmountFilled: *uint256.NewInt(10_000_000),
	}
	split := generated.ConditionalTokensPositionSplit{
		EventMeta:          meta(3, common.HexToAddress(generated.ConditionalTokensPositionSplitAddress)),
		Stakeholder:        user,
		CollateralToken:    collateral,
		ParentCollectionID: common.Hash{},
		ConditionID:        conditionID,
		Partition:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
		Amount:             *uint256.NewInt(2_000_000),
	}
	merge := generated.ConditionalTokensPositionsMerge{
		EventMeta:          meta(4, common.HexToAddress(generated.ConditionalTokensPositionsMergeAddress)),
		Stakeholder:        user,
		CollateralToken:    collateral,
		ParentCollectionID: common.Hash{},
		ConditionID:        conditionID,
		Partition:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
		Amount:             *uint256.NewInt(500_000),
	}
	fpmmCreate := generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation{
		EventMeta:               meta(5, common.HexToAddress(generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreationAddress)),
		Creator:                 user,
		FixedProductMarketMaker: fpmm,
		ConditionalTokens:       common.HexToAddress(generated.ConditionalTokensConditionPreparationAddress),
		CollateralToken:         collateral,
		ConditionIds:            []common.Hash{conditionID},
		Fee:                     *uint256.NewInt(0),
	}
	fpmmBuy := generated.FixedProductMarketMakerFPMMBuy{
		EventMeta:           meta(6, fpmm),
		Buyer:               user,
		InvestmentAmount:    *uint256.NewInt(600_000),
		FeeAmount:           *uint256.NewInt(0),
		OutcomeIndex:        *uint256.NewInt(0),
		OutcomeTokensBought: *uint256.NewInt(1_000_000),
	}

	parsedBlock := &generated.ParsedBlock{
		BlockNumber: blockNumber,
		BlockHash:   blockHash.Hex(),
		Sequence: []uint8{
			uint8(generated.EventTypeConditionalTokensConditionPreparation),
			uint8(generated.EventTypeExchangeOrderFilled),
			uint8(generated.EventTypeConditionalTokensPositionSplit),
			uint8(generated.EventTypeConditionalTokensPositionsMerge),
			uint8(generated.EventTypeFixedProductMarketMakerFactoryFixedProductMarketMakerCreation),
			uint8(generated.EventTypeFixedProductMarketMakerFPMMBuy),
		},
		ConditionalTokensConditionPreparations:                         []generated.ConditionalTokensConditionPreparation{conditionPrep},
		ExchangeOrderFilleds:                                           []generated.ExchangeOrderFilled{order},
		ConditionalTokensPositionSplits:                                []generated.ConditionalTokensPositionSplit{split},
		ConditionalTokensPositionsMerges:                               []generated.ConditionalTokensPositionsMerge{merge},
		FixedProductMarketMakerFactoryFixedProductMarketMakerCreations: []generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation{fpmmCreate},
		FixedProductMarketMakerFPMMBuys:                                []generated.FixedProductMarketMakerFPMMBuy{fpmmBuy},
	}

	protoBlock := generated.NewProtoEventBlock()
	protoBlock.HeaderBlockNumber = blockNumber
	protoBlock.HeaderBlockHash = blockHash.Hex()
	protoBlock.AppendConditionalTokensConditionPreparation(conditionPrep.EventMeta, &conditionPrep)
	protoBlock.AppendExchangeOrderFilled(order.EventMeta, &order)
	protoBlock.AppendConditionalTokensPositionSplit(split.EventMeta, &split)
	protoBlock.AppendConditionalTokensPositionsMerge(merge.EventMeta, &merge)
	protoBlock.AppendFixedProductMarketMakerFactoryFixedProductMarketMakerCreation(fpmmCreate.EventMeta, &fpmmCreate)
	protoBlock.AppendFixedProductMarketMakerFPMMBuy(fpmmBuy.EventMeta, &fpmmBuy)

	return parsedBlock, protoBlock
}

func assertHotStateParity(t *testing.T, parsedState, protoState *generated.State) {
	t.Helper()
	if !reflect.DeepEqual(collectConditions(parsedState), collectConditions(protoState)) {
		t.Fatalf("condition hot state mismatch\nparsed=%#v\nproto=%#v", collectConditions(parsedState), collectConditions(protoState))
	}
	if !reflect.DeepEqual(collectPositions(parsedState), collectPositions(protoState)) {
		t.Fatalf("position hot state mismatch\nparsed=%#v\nproto=%#v", collectPositions(parsedState), collectPositions(protoState))
	}
	if !reflect.DeepEqual(collectFPMMs(parsedState), collectFPMMs(protoState)) {
		t.Fatalf("fpmm hot state mismatch\nparsed=%#v\nproto=%#v", collectFPMMs(parsedState), collectFPMMs(protoState))
	}
	if !reflect.DeepEqual(collectNegRiskEvents(parsedState), collectNegRiskEvents(protoState)) {
		t.Fatalf("neg-risk hot state mismatch\nparsed=%#v\nproto=%#v", collectNegRiskEvents(parsedState), collectNegRiskEvents(protoState))
	}
}

func collectConditions(state *generated.State) map[generated.ConditionsClockKey]generated.MemoryCondition {
	out := make(map[generated.ConditionsClockKey]generated.MemoryCondition)
	state.HotState.Conditions.Range(func(k generated.ConditionsClockKey, v generated.MemoryCondition) bool {
		out[k] = v
		return true
	})
	return out
}

func collectPositions(state *generated.State) map[generated.UserPositionsClockKey]generated.MemoryUserPosition {
	out := make(map[generated.UserPositionsClockKey]generated.MemoryUserPosition)
	state.HotState.UserPositions.Range(func(k generated.UserPositionsClockKey, v generated.MemoryUserPosition) bool {
		out[k] = v
		return true
	})
	return out
}

func collectFPMMs(state *generated.State) map[generated.FixedProductMarketMakersClockKey]generated.MemoryFixedProductMarketMaker {
	out := make(map[generated.FixedProductMarketMakersClockKey]generated.MemoryFixedProductMarketMaker)
	state.HotState.FixedProductMarketMakers.Range(func(k generated.FixedProductMarketMakersClockKey, v generated.MemoryFixedProductMarketMaker) bool {
		out[k] = v
		return true
	})
	return out
}

func collectNegRiskEvents(state *generated.State) map[generated.NegRiskEventsClockKey]generated.MemoryNegRiskEvent {
	out := make(map[generated.NegRiskEventsClockKey]generated.MemoryNegRiskEvent)
	state.HotState.NegRiskEvents.Range(func(k generated.NegRiskEventsClockKey, v generated.MemoryNegRiskEvent) bool {
		out[k] = v
		return true
	})
	return out
}

func TestBug1And4_PositionsConverted(t *testing.T) {
	s := NewMemoryStore()
	user := common.HexToAddress("0x123")
	marketID := common.HexToHash("0xabc")

	// Create NegRiskEvent
	nr := &generated.NegRiskEvent{
		ID:            marketID,
		QuestionCount: 3,
		QuestionIDs:   []common.Hash{common.HexToHash("0x1"), common.HexToHash("0x2"), common.HexToHash("0x3")},
	}
	s.state.NegRiskEvent.Save(nr, generated.EventMeta{})

	// Setup user NO position for question 0
	condID0 := generated.GetConditionID(negRiskAdapterAddr, common.HexToHash("0x1"))
	posID0 := s.state.GetNegRiskPositionIDByCondition(condID0, 1)
	var h0 common.Hash
	posID0.WriteToSlice(h0[:])
	up0 := &generated.InternalPositionState{
		ID:          generated.UserPositionKey{User: user, TokenID: h0},
		TokenID:     posID0,
		Amount:      decimal.NewFromInt(10 * 1e6),
		AvgPrice:    decimal.NewFromFloat(0.4), // 40 cents average price
		RealizedPnL: decimal.Zero,
		TotalBought: decimal.NewFromInt(10 * 1e6),
	}
	s.state.SaveUserPosition(up0, generated.EventMeta{})

	// Setup user NO position for question 1
	condID1 := generated.GetConditionID(negRiskAdapterAddr, common.HexToHash("0x2"))
	posID1 := s.state.GetNegRiskPositionIDByCondition(condID1, 1)
	var h1 common.Hash
	posID1.WriteToSlice(h1[:])
	up1 := &generated.InternalPositionState{
		ID:          generated.UserPositionKey{User: user, TokenID: h1},
		TokenID:     posID1,
		Amount:      decimal.NewFromInt(10 * 1e6),
		AvgPrice:    decimal.NewFromFloat(0.4), // 40 cents average price
		RealizedPnL: decimal.Zero,
		TotalBought: decimal.NewFromInt(10 * 1e6),
	}
	s.state.SaveUserPosition(up1, generated.EventMeta{})

	// Prepare event: conversion amount 15 * 1e6, indexSet 3 (question 0 & 1 selected)
	ev := &generated.NegRiskAdapterPositionsConverted{
		Stakeholder: user,
		MarketID:    marketID,
		IndexSet:    *uint256.NewInt(3), // question 0 & 1 selected (NO sold), question 2 bought (YES bought)
		Amount:      *uint256.NewInt(15 * 1e6),
	}

	condID2 := generated.GetConditionID(negRiskAdapterAddr, common.HexToHash("0x3"))
	yesPosID2 := s.state.GetNegRiskPositionIDByCondition(condID2, 0)

	handlePositionsConverted(s.state, ev)

	// Verify NO positions are now 0 amount
	noUp0 := s.GetUserPosition(user, posID0)
	if noUp0 == nil || !noUp0.Amount.IsZero() {
		t.Errorf("expected NO position 0 amount to be 0, got %v", noUp0)
	}
	noUp1 := s.GetUserPosition(user, posID1)
	if noUp1 == nil || !noUp1.Amount.IsZero() {
		t.Errorf("expected NO position 1 amount to be 0, got %v", noUp1)
	}

	// Verify YES position 2 is updated with yesPrice = -0.2
	yesUp2 := s.GetUserPosition(user, yesPosID2)
	if yesUp2 == nil {
		t.Fatal("expected YES position 2 to exist")
	}
	expectedYesAmt := decimal.NewFromInt(15 * 1e6)
	if !yesUp2.Amount.Equal(expectedYesAmt) {
		t.Errorf("expected YES amount %s, got %s", expectedYesAmt, yesUp2.Amount)
	}
	expectedYesPrice := decimal.NewFromFloat(-0.2)
	if !yesUp2.AvgPrice.Equal(expectedYesPrice) {
		t.Errorf("expected YES avg price %s, got %s", expectedYesPrice, yesUp2.AvgPrice)
	}
	if !yesUp2.RealizedPnL.IsZero() {
		t.Errorf("expected YES realized pnl to remain zero, got %s", yesUp2.RealizedPnL)
	}
}

func TestBug1_PositionsConvertedMissingNoDoesNotAbortYesBuy(t *testing.T) {
	s := NewMemoryStore()
	user := common.HexToAddress("0x123")
	marketID := common.HexToHash("0xabc")
	questionIDs := []common.Hash{common.HexToHash("0x1"), common.HexToHash("0x2"), common.HexToHash("0x3")}
	s.state.NegRiskEvent.Save(&generated.NegRiskEvent{
		ID:            marketID,
		QuestionCount: 3,
		QuestionIDs:   questionIDs,
	}, generated.EventMeta{})

	condID0 := generated.GetConditionID(negRiskAdapterAddr, questionIDs[0])
	noPosID0 := s.state.GetNegRiskPositionIDByCondition(condID0, 1)
	saveTestPosition(s.state, user, noPosID0, "10000000", "0.6")

	condID1 := generated.GetConditionID(negRiskAdapterAddr, questionIDs[1])
	missingNoPosID1 := s.state.GetNegRiskPositionIDByCondition(condID1, 1)

	condID2 := generated.GetConditionID(negRiskAdapterAddr, questionIDs[2])
	yesPosID2 := s.state.GetNegRiskPositionIDByCondition(condID2, 0)

	handlePositionsConverted(s.state, &generated.NegRiskAdapterPositionsConverted{
		Stakeholder: user,
		MarketID:    marketID,
		IndexSet:    *uint256.NewInt(3),
		Amount:      *uint256.NewInt(5 * 1e6),
	})

	noUp0 := s.GetUserPosition(user, noPosID0)
	if noUp0 == nil || !noUp0.Amount.Equal(decimal.NewFromInt(5*1e6)) {
		t.Fatalf("expected first NO position to sell down to 5000000, got %#v", noUp0)
	}
	if noUp1 := s.GetUserPosition(user, missingNoPosID1); noUp1 != nil {
		t.Fatalf("expected missing NO position to remain absent, got %#v", noUp1)
	}

	yesUp2 := s.GetUserPosition(user, yesPosID2)
	if yesUp2 == nil {
		t.Fatal("expected YES position to be bought even when one selected NO position is missing")
	}
	if want := decimal.NewFromInt(5 * 1e6); !yesUp2.Amount.Equal(want) {
		t.Fatalf("expected YES amount %s, got %s", want, yesUp2.Amount)
	}
	if want := decimal.RequireFromString("-0.4"); !yesUp2.AvgPrice.Equal(want) {
		t.Fatalf("expected unclamped YES avg price %s, got %s", want, yesUp2.AvgPrice)
	}
}

func TestBug2_PositionSplitAndMergeParentCollection(t *testing.T) {
	s := NewMemoryStore()
	user := common.HexToAddress("0x123")
	conditionID := common.HexToHash("0xabc")
	parentID := common.HexToHash("0xdef")
	collateral := common.HexToAddress("0x888")

	// Save condition
	cond := &generated.Condition{
		ID:               conditionID,
		Oracle:           common.HexToAddress("0x456"),
		QuestionID:       common.HexToHash("0x789"),
		OutcomeSlotCount: 2,
	}
	s.state.SaveCondition(cond, generated.EventMeta{})

	// Setup parent position
	parentPosID := s.state.GetPositionID(collateral, parentID)
	var parentH common.Hash
	parentPosID.WriteToSlice(parentH[:])
	parentUp := &generated.InternalPositionState{
		ID:          generated.UserPositionKey{User: user, TokenID: parentH},
		TokenID:     parentPosID,
		Amount:      decimal.NewFromInt(100 * 1e6),
		AvgPrice:    decimal.NewFromFloat(1.0),
		RealizedPnL: decimal.Zero,
		TotalBought: decimal.NewFromInt(100 * 1e6),
	}
	s.state.SaveUserPosition(parentUp, generated.EventMeta{})

	// Split event
	splitEv := &generated.ConditionalTokensPositionSplit{
		Stakeholder:        user,
		CollateralToken:    collateral,
		ParentCollectionID: parentID,
		ConditionID:        conditionID,
		Partition:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
		Amount:             *uint256.NewInt(10 * 1e6),
	}

	handlePositionSplit(s.state, splitEv)

	// Parent position should NOT be changed in the reference subgraph logic
	gotParentUp := s.GetUserPosition(user, parentPosID)
	if !gotParentUp.Amount.Equal(decimal.NewFromInt(100 * 1e6)) {
		t.Errorf("expected parent amount to remain 100, got %s", gotParentUp.Amount)
	}

	basePosID0 := ctfPositionID(s.state, collateral, common.Hash{}, conditionID, 0)
	basePosID1 := ctfPositionID(s.state, collateral, common.Hash{}, conditionID, 1)
	parentDerivedPosID0 := ctfPositionID(s.state, collateral, parentID, conditionID, 0)
	parentDerivedPosID1 := ctfPositionID(s.state, collateral, parentID, conditionID, 1)

	baseUp0 := s.GetUserPosition(user, basePosID0)
	baseUp1 := s.GetUserPosition(user, basePosID1)
	if baseUp0 == nil || !baseUp0.Amount.Equal(decimal.NewFromInt(10*1e6)) {
		t.Fatalf("expected base outcome 0 position amount 10000000 after split, got %#v", baseUp0)
	}
	if baseUp1 == nil || !baseUp1.Amount.Equal(decimal.NewFromInt(10*1e6)) {
		t.Fatalf("expected base outcome 1 position amount 10000000 after split, got %#v", baseUp1)
	}
	if got := s.GetUserPosition(user, parentDerivedPosID0); got != nil {
		t.Fatalf("expected parent-derived outcome 0 position to stay untouched, got %#v", got)
	}
	if got := s.GetUserPosition(user, parentDerivedPosID1); got != nil {
		t.Fatalf("expected parent-derived outcome 1 position to stay untouched, got %#v", got)
	}

	// Merge event
	mergeEv := &generated.ConditionalTokensPositionsMerge{
		Stakeholder:        user,
		CollateralToken:    collateral,
		ParentCollectionID: parentID,
		ConditionID:        conditionID,
		Partition:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
		Amount:             *uint256.NewInt(10 * 1e6),
	}

	handlePositionsMerge(s.state, mergeEv)

	// Parent position should still be unchanged
	gotParentUp = s.GetUserPosition(user, parentPosID)
	if !gotParentUp.Amount.Equal(decimal.NewFromInt(100 * 1e6)) {
		t.Errorf("expected parent amount to remain 100, got %s", gotParentUp.Amount)
	}
	baseUp0 = s.GetUserPosition(user, basePosID0)
	baseUp1 = s.GetUserPosition(user, basePosID1)
	if baseUp0 == nil || !baseUp0.Amount.IsZero() {
		t.Fatalf("expected base outcome 0 position to be zero after merge, got %#v", baseUp0)
	}
	if baseUp1 == nil || !baseUp1.Amount.IsZero() {
		t.Fatalf("expected base outcome 1 position to be zero after merge, got %#v", baseUp1)
	}
}

func TestBug5_PayoutRedemptionUsesBaseOutcomePositions(t *testing.T) {
	s := NewMemoryStore()
	user := common.HexToAddress("0x123")
	conditionID := common.HexToHash("0xabc")
	parentID := common.HexToHash("0xdef")
	collateral := common.HexToAddress("0x888")

	s.state.SaveCondition(&generated.Condition{
		ID:               conditionID,
		Oracle:           common.HexToAddress("0x456"),
		QuestionID:       common.HexToHash("0x789"),
		OutcomeSlotCount: 2,
		Resolved:         true,
		Payouts:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(0)},
	}, generated.EventMeta{})

	basePosID0 := ctfPositionID(s.state, collateral, common.Hash{}, conditionID, 0)
	parentDerivedPosID0 := ctfPositionID(s.state, collateral, parentID, conditionID, 0)
	saveTestPosition(s.state, user, basePosID0, "10000000", "0.25")
	saveTestPosition(s.state, user, parentDerivedPosID0, "7000000", "0.25")

	handlePayoutRedemptionCTF(s.state, &generated.ConditionalTokensPayoutRedemption{
		Redeemer:           user,
		CollateralToken:    collateral,
		ParentCollectionID: parentID,
		ConditionID:        conditionID,
		IndexSets:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
		Payout:             *uint256.NewInt(10 * 1e6),
	})

	baseUp0 := s.GetUserPosition(user, basePosID0)
	if baseUp0 == nil || !baseUp0.Amount.IsZero() {
		t.Fatalf("expected base outcome position to be fully redeemed, got %#v", baseUp0)
	}
	if want := decimal.RequireFromString("7500000"); !baseUp0.RealizedPnL.Equal(want) {
		t.Fatalf("expected base outcome realized pnl %s, got %s", want, baseUp0.RealizedPnL)
	}
	parentUp0 := s.GetUserPosition(user, parentDerivedPosID0)
	if parentUp0 == nil || !parentUp0.Amount.Equal(decimal.NewFromInt(7*1e6)) {
		t.Fatalf("expected parent-derived outcome position to remain untouched, got %#v", parentUp0)
	}
}

func TestBug3_ConditionPreparationNonBinary(t *testing.T) {
	s := NewMemoryStore()
	condID := common.HexToHash("0xabc")

	ev := &generated.ConditionalTokensConditionPreparation{
		ConditionID:      condID,
		Oracle:           common.HexToAddress("0x456"),
		QuestionID:       common.HexToHash("0x789"),
		OutcomeSlotCount: *uint256.NewInt(3), // non-binary
	}

	handleConditionPreparation(s.state, ev)

	_, ok := s.GetCondition(condID)
	if ok {
		t.Errorf("expected condition to be ignored for outcomeSlotCount != 2")
	}
}

func TestMathHelpersAndPositionAccounting(t *testing.T) {
	if got, ok := outcomeIndexUint8(*uint256.NewInt(255)); !ok || got != 255 {
		t.Fatalf("outcomeIndexUint8(255) = %d/%t, want 255/true", got, ok)
	}
	tooLarge := new(uint256.Int).Lsh(uint256.NewInt(1), 8)
	if got, ok := outcomeIndexUint8(*tooLarge); ok || got != 0 {
		t.Fatalf("outcomeIndexUint8(256) = %d/%t, want 0/false", got, ok)
	}

	if !uint256PairSumZero(nil) {
		t.Fatal("expected nil uint256 pair sum to be treated as zero")
	}
	if !uint256PairSumZero([]uint256.Int{*uint256.NewInt(0), *uint256.NewInt(0)}) {
		t.Fatal("expected zero pair sum")
	}
	if uint256PairSumZero([]uint256.Int{*uint256.NewInt(1), *uint256.NewInt(0)}) {
		t.Fatal("expected non-zero pair sum")
	}

	if got := computeFpmmPriceDecimal(nil, 0); !got.IsZero() {
		t.Fatalf("expected empty fpmm price to be zero, got %s", got)
	}
	if got := computeFpmmPriceDecimal([]uint256.Int{*uint256.NewInt(0), *uint256.NewInt(0)}, 0); !got.IsZero() {
		t.Fatalf("expected zero-denominator fpmm price to be zero, got %s", got)
	}
	if got := computeFpmmPriceDecimal([]uint256.Int{*uint256.NewInt(100), *uint256.NewInt(300)}, 1); !got.Equal(decimal.RequireFromString("0.25")) {
		t.Fatalf("fpmm price mismatch: got %s want 0.25", got)
	}
	if got := computeFpmmPriceDecimal([]uint256.Int{*uint256.NewInt(100), *uint256.NewInt(300)}, 2); !got.IsZero() {
		t.Fatalf("out-of-range fpmm price = %s, want 0", got)
	}

	addr := common.HexToAddress("0x00000000000000000000000000000000000000ff")
	if got := uint256FromAddress(addr); got.String() != "255" {
		t.Fatalf("uint256FromAddress mismatch: got %s want 255", got.String())
	}
	if got := Uint256ToDecimal(*uint256.NewInt(123456)); !got.Equal(decimal.NewFromInt(123456)) {
		t.Fatalf("Uint256ToDecimal mismatch: got %s", got)
	}
	if got := computeNegRiskYesPriceDecimal(decimal.RequireFromString("0.5"), 0, 3); !got.IsZero() {
		t.Fatalf("zero noCount yes price = %s, want 0", got)
	}
	if got := computeNegRiskYesPriceDecimal(decimal.RequireFromString("0.5"), 3, 3); !got.IsZero() {
		t.Fatalf("all-NO yes price = %s, want 0", got)
	}

	denom, ok := calculatePayoutDenominator(&generated.Condition{Payouts: []uint256.Int{*uint256.NewInt(3), *uint256.NewInt(7)}})
	if !ok || !denom.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("payout denominator = %s/%t, want 10/true", denom, ok)
	}
	if denom, ok := calculatePayoutDenominator(&generated.Condition{Payouts: []uint256.Int{*uint256.NewInt(0), *uint256.NewInt(0)}}); ok || !denom.IsZero() {
		t.Fatalf("zero payout denominator = %s/%t, want 0/false", denom, ok)
	}

	if !isOneHot(uint256.NewInt(8)) || isOneHot(uint256.NewInt(0)) || isOneHot(uint256.NewInt(3)) {
		t.Fatal("isOneHot returned unexpected values")
	}
	bits := &uint256.Int{0, 0, 0, 1 << 63}
	if getBit(bits, 255) != 1 || getBit(bits, 256) != 0 || getBit(bits, -1) != 0 {
		t.Fatal("getBit boundary behavior mismatch")
	}

	s := NewMemoryStore()
	user := common.HexToAddress("0x456")
	tokenID := *uint256.NewInt(999)
	updateUserPositionWithBuy(s.state, user, tokenID, decimal.RequireFromString("0.25"), decimal.NewFromInt(100), decimal.RequireFromString("-2"), generated.EventMeta{})
	updateUserPositionWithBuy(s.state, user, tokenID, decimal.RequireFromString("0.75"), decimal.NewFromInt(100), decimal.Zero, generated.EventMeta{})
	up := s.GetUserPosition(user, tokenID)
	if up == nil {
		t.Fatal("expected position after buys")
	}
	if !up.Amount.Equal(decimal.NewFromInt(200)) || !up.TotalBought.Equal(decimal.NewFromInt(200)) || !up.AvgPrice.Equal(decimal.RequireFromString("0.5")) || !up.RealizedPnL.Equal(decimal.RequireFromString("-2")) {
		t.Fatalf("buy accounting mismatch: %#v", up)
	}
	updateUserPositionWithSell(s.state, user, tokenID, decimal.RequireFromString("0.8"), decimal.NewFromInt(250), generated.EventMeta{})
	up = s.GetUserPosition(user, tokenID)
	if !up.Amount.IsZero() {
		t.Fatalf("expected sell to cap at current amount and zero position, got %s", up.Amount)
	}
	if want := decimal.RequireFromString("58"); !up.RealizedPnL.Equal(want) {
		t.Fatalf("sell realized pnl mismatch: got %s want %s", up.RealizedPnL, want)
	}
}

func saveTestPosition(state *generated.State, user common.Address, tokenID uint256.Int, amount string, avgPrice string) {
	var h common.Hash
	tokenID.WriteToSlice(h[:])
	amountDec := decimal.RequireFromString(amount)
	state.SaveUserPosition(&generated.InternalPositionState{
		ID:          generated.UserPositionKey{User: user, TokenID: h},
		TokenID:     tokenID,
		Amount:      amountDec,
		AvgPrice:    decimal.RequireFromString(avgPrice),
		RealizedPnL: decimal.Zero,
		TotalBought: amountDec,
	}, generated.EventMeta{})
}

func ctfPositionID(state *generated.State, collateral common.Address, parentID, conditionID common.Hash, outcomeIndex uint8) uint256.Int {
	indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcomeIndex))
	collID := state.GetCollectionID(parentID, conditionID, *indexSet)
	return state.GetPositionID(collateral, collID)
}

func TestPruningIntegration_2000StepMark(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	// 1. Initialize starting block at 78000000
	s.state.LastSyncBlock = 78000000
	s.state.SaveSnapshot(78000000)

	// 2. Simulate person X having their last trade/position updated at block 78000053
	user := common.HexToAddress("0xabcd")
	tokenID := *uint256.NewInt(555)

	var h common.Hash
	tokenID.WriteToSlice(h[:])
	amountDec := decimal.RequireFromString("1000")
	s.state.SaveUserPosition(&generated.InternalPositionState{
		ID:          generated.UserPositionKey{User: user, TokenID: h},
		TokenID:     tokenID,
		Amount:      amountDec,
		AvgPrice:    decimal.RequireFromString("0.5"),
		RealizedPnL: decimal.Zero,
		TotalBought: amountDec,
	}, generated.EventMeta{
		BlockNumber: 78000053,
	})

	s.state.SaveSnapshot(78000053)

	// 3. Process blocks step-by-step up to block 78002000 to verify pruning trigger behavior
	// We will process blocks: 78000001, 78000999, 78001000, 78001001, 78001999, 78002000
	blocksToProcess := []uint64{78000001, 78000999, 78001000, 78001001, 78001999, 78002000}

	for _, bNum := range blocksToProcess {
		block := &generated.ParsedBlock{
			BlockNumber: bNum,
		}

		err := CustomProcessing(ctx, generated.Store(nil), s.state, block)
		if err != nil {
			t.Fatalf("CustomProcessing failed at block %d: %v", bNum, err)
		}

		// Check triggers
		if bNum < 78001000 {
			// Pruning should NOT have triggered yet, so LastSyncBlock remains 78000000
			if s.state.LastSyncBlock != 78000000 {
				t.Errorf("block %d: expected LastSyncBlock to be 78000000, got %d", bNum, s.state.LastSyncBlock)
			}
		} else if bNum >= 78001000 && bNum < 78002000 {
			// Pruning should have triggered at 78001000, updating LastSyncBlock to 78001000
			if s.state.LastSyncBlock != 78001000 {
				t.Errorf("block %d: expected LastSyncBlock to be 78001000, got %d", bNum, s.state.LastSyncBlock)
			}
		} else if bNum == 78002000 {
			// Pruning should have triggered again at 78002000, updating LastSyncBlock to 78002000
			if s.state.LastSyncBlock != 78002000 {
				t.Errorf("block %d: expected LastSyncBlock to be 78002000, got %d", bNum, s.state.LastSyncBlock)
			}
		}
	}

	// 4. Verify that the position of person X is still present and NOT deleted/lost
	up := s.GetUserPosition(user, tokenID)
	if up == nil {
		t.Fatal("expected position of person X to be preserved and not pruned")
	}
	if !up.Amount.Equal(amountDec) {
		t.Errorf("expected position amount %s, got %s", amountDec, up.Amount)
	}
}

func TestProcessorDatabaseRecovery(t *testing.T) {
	// Create mock ClickHouse HTTP server that returns valid JSONEachRow data
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		query := string(bodyBytes)
		t.Logf("Mock server received r.Method=%s query: %q", r.Method, query)
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(query, "memory_conditions") {
			w.Write([]byte(`{"id":"0x0000000000000000000000000000000000000000000000000000000000000123","oracle":"0x0000000000000000000000000000000000000456","question_id":"0x0000000000000000000000000000000000000000000000000000000000000789","outcome_slot_count":2,"resolved":1,"payouts":["1","0"]}` + "\n"))
		} else if strings.Contains(query, "memory_user_positions") {
			w.Write([]byte(`{"user":"0x000000000000000000000000000000000000abcd","token_id":"0x000000000000000000000000000000000000000000000000000000000000022b","amount":"1000","avg_price":"0.5","realized_pn_l":"10","total_bought":"1000"}` + "\n"))
		} else if strings.Contains(query, "memory_neg_risk_events") {
			w.Write([]byte(`{"id":"0x0000000000000000000000000000000000000000000000000000000000000aaa","question_count":2,"question_ids":["0x0000000000000000000000000000000000000000000000000000000000000111","0x0000000000000000000000000000000000000000000000000000000000000222"]}` + "\n"))
		} else if strings.Contains(query, "memory_markets") {
			w.Write([]byte(`{"id":"0x0000000000000000000000000000000000000000000000000000000000000bbb","question_count":1,"question_ids":["0x0000000000000000000000000000000000000000000000000000000000000333"]}` + "\n"))
		} else if strings.Contains(query, "memory_fixed_product_market_makers") {
			w.Write([]byte(`{"id":"0x0000000000000000000000000000000000000fff","condition_id":"0x0000000000000000000000000000000000000000000000000000000000000ccc","collateral_token":"0x0000000000000000000000000000000000000eee"}` + "\n"))
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockServer.Close()

	u, err := url.Parse(mockServer.URL)
	if err != nil {
		t.Fatalf("failed to parse mock server URL: %v", err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("failed to split host/port: %v", err)
	}

	// Set CLI env variable to target the mock server port
	os.Setenv("CLICKHOUSE_HTTP_PORT", portStr)
	defer os.Unsetenv("CLICKHOUSE_HTTP_PORT")

	proc, err := NewProcessor()
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	// Call recovery/LoadFromDatabase
	err = proc.LoadFromDatabase(context.Background(), 78000100)
	if err != nil {
		t.Fatalf("LoadFromDatabase failed: %v", err)
	}

	// Verify loaded state values
	if proc.State.LastSyncBlock != 78000100 {
		t.Errorf("expected LastSyncBlock 78000100, got %d", proc.State.LastSyncBlock)
	}

	// Check position
	user := common.HexToAddress("0x000000000000000000000000000000000000abcd")
	tokenID := *uint256.NewInt(555)
	up := proc.State.GetUserPosition(user, tokenID)
	if up == nil {
		t.Fatalf("expected recovered position to exist")
	}
	if !up.Amount.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("expected position amount 1000, got %s", up.Amount)
	}
	if !up.RealizedPnL.Equal(decimal.NewFromInt(10)) {
		t.Errorf("expected realized pnl 10, got %s", up.RealizedPnL)
	}

	// Check condition
	condID := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000123")
	cond, ok := proc.State.GetCondition(condID)
	if !ok {
		t.Fatalf("expected recovered condition to exist")
	}
	if cond.Oracle != common.HexToAddress("0x0000000000000000000000000000000000000456") {
		t.Errorf("unexpected oracle address: %s", cond.Oracle)
	}
}

func TestProcessorCrashAndMemoryRecovery(t *testing.T) {
	proc, err := NewProcessor()
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	// Simulate processing at block 78000100 and saving a clean snapshot
	proc.State.LastSyncBlock = 78000100

	user := common.HexToAddress("0xcafe")
	tokenID := *uint256.NewInt(777)

	// Save initial position and save single clean snapshot for block 78000100
	saveTestPosition(proc.State, user, tokenID, "500", "0.4")
	proc.State.SaveSnapshot(78000100)

	// User does a transaction at block 78000101
	saveTestPosition(proc.State, user, tokenID, "800", "0.45")
	proc.State.SaveSnapshot(78000101)

	// User does another transaction at block 78000102
	saveTestPosition(proc.State, user, tokenID, "1200", "0.47")
	proc.State.SaveSnapshot(78000102)

	// Verify the position in state reflects block 78000102
	pos := proc.State.GetUserPosition(user, tokenID)
	if pos == nil || !pos.Amount.Equal(decimal.NewFromInt(1200)) {
		t.Fatalf("expected amount 1200 at block 78000102, got %v", pos)
	}

	// Now we simulate a CRASH/FAILURE at block 78000103.
	// Since block 78000103 processing crashed/aborted, we must ROLLBACK to the last stable snapshot at block 78000100.
	_, err = proc.RestoreToBlock(78000100)
	if err != nil {
		t.Fatalf("RestoreToBlock failed: %v", err)
	}

	// Verify that the state was successfully rolled back to block 78000100!
	pos = proc.State.GetUserPosition(user, tokenID)
	if pos == nil {
		t.Fatalf("expected position to be preserved on rollback")
	}
	if !pos.Amount.Equal(decimal.NewFromInt(500)) {
		t.Errorf("expected position to roll back to amount 500, got %s", pos.Amount)
	}
	if !pos.AvgPrice.Equal(decimal.RequireFromString("0.4")) {
		t.Errorf("expected position average price to roll back to 0.4, got %s", pos.AvgPrice)
	}
	if proc.State.LastSyncBlock != 78000100 {
		t.Errorf("expected LastSyncBlock to roll back to 78000100, got %d", proc.State.LastSyncBlock)
	}
}

func TestConfigurablePruningInterval(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	// Initialize LastSyncBlock and LastPruneBlock
	s.state.LastSyncBlock = 100000
	s.state.LastPruneBlock = 100000

	// Set custom environment variable
	t.Setenv("CLICKHOUSE_PRUNE_INTERVAL", "5000")

	// Call CustomProcessing at block 101000 (which is +1000 from LastSyncBlock, so it triggers commit,
	// but is only +1000 from LastPruneBlock, which is < 5000, so it should NOT trigger pruning update)
	block := &generated.ParsedBlock{
		BlockNumber: 101000,
	}
	err := CustomProcessing(ctx, generated.Store(nil), s.state, block)
	if err != nil {
		t.Fatalf("CustomProcessing failed: %v", err)
	}

	if s.state.LastSyncBlock != 101000 {
		t.Errorf("expected LastSyncBlock to be 101000, got %d", s.state.LastSyncBlock)
	}
	if s.state.LastPruneBlock != 100000 {
		t.Errorf("expected LastPruneBlock to remain 100000, got %d", s.state.LastPruneBlock)
	}

	// Now call CustomProcessing at block 106000 (which is +5000 from LastPruneBlock, so it SHOULD trigger pruning update)
	block2 := &generated.ParsedBlock{
		BlockNumber: 106000,
	}
	err = CustomProcessing(ctx, &mockStore{}, s.state, block2)
	if err != nil {
		t.Fatalf("CustomProcessing failed: %v", err)
	}

	if s.state.LastSyncBlock != 106000 {
		t.Errorf("expected LastSyncBlock to be 106000, got %d", s.state.LastSyncBlock)
	}
	if s.state.LastPruneBlock != 106000 {
		t.Errorf("expected LastPruneBlock to update to 106000, got %d", s.state.LastPruneBlock)
	}
}

// TestFromDecimalEdgeCases verifies the fromDecimal conversion handles:
//   - float-origin decimals (exp != -18, pow10 scaling path)
//   - roundtrip identity (exp == -18 fast path)
//   - overflow at uint256 boundary (2^256-1 vs 2^255-1)
//   - negative values
//   - zero
//   - division results (non-normalized exponents like exp=-16)
func TestFromDecimalEdgeCases(t *testing.T) {
	scale18 := protomath.Decimal256Scale18

	// 1. Shopspring exponent behavior — verify our assumptions
	t.Run("exponent_behavior", func(t *testing.T) {
		cases := []struct {
			name string
			d    decimal.Decimal
			wantExp int
		}{
			{"NewFromFloat(0.5)", decimal.NewFromFloat(0.5), -1},
			{"NewFromFloat(0.1)", decimal.NewFromFloat(0.1), -1},
			{"NewFromFloat(42.0)", decimal.NewFromFloat(42.0), 0},
			{"division 1.0/3.0", decimal.NewFromFloat(1.0).Div(decimal.NewFromFloat(3.0)), -16},
			{"addition 0.1+0.2", decimal.NewFromFloat(0.1).Add(decimal.NewFromFloat(0.2)), -1},
			{"multiplication 0.5*2.0", decimal.NewFromFloat(0.5).Mul(decimal.NewFromFloat(2.0)), -1},
		}
		for _, c := range cases {
			if got := int(c.d.Exponent()); got != c.wantExp {
				t.Errorf("%s: exp=%d want=%d (shopspring behavior changed — review fromDecimal paths)", c.name, got, c.wantExp)
			}
		}
	})

	// 2. Roundtrip identity — toDecimal → fromDecimal must be lossless
	t.Run("roundtrip_identity", func(t *testing.T) {
		values := []string{"0", "0.5", "1.0", "123.456", "99999999999999999999.999999999999999999"}
		for _, s := range values {
			v, err := protomath.ParseDecimal256(s, scale18)
			if err != nil {
				t.Fatalf("ParseDecimal256(%q): %v", s, err)
			}
			d := decimal.NewFromBigInt(v.ScaledBig(), -18) // simulate toDecimal
			back := fromDecimal(d)
			if !back.Eq(v) {
				t.Errorf("roundtrip %q: got %s want %s", s, back.String(scale18), v.String(scale18))
			}
		}
	})

	// 3. Float-origin decimals — exp != -18, exercises the pow10 scaling path
	t.Run("float_origin", func(t *testing.T) {
		cases := []struct {
			d    decimal.Decimal
			want string
		}{
			{decimal.NewFromFloat(0.5), "0.500000000000000000"},
			{decimal.NewFromFloat(0.1), "0.100000000000000000"},
			{decimal.NewFromFloat(0.123456789), "0.123456789000000000"},
			{decimal.NewFromFloat(42.0), "42.000000000000000000"},
			{decimal.NewFromFloat(0.000000001), "0.000000001000000000"},
			{decimal.RequireFromString("0.000000000000000001"), "0.000000000000000001"},
			// Division result — non-normalized exponent
			{decimal.NewFromFloat(1.0).Div(decimal.NewFromFloat(3.0)), "0.333333333333333300"},
		}
		for _, c := range cases {
			got := fromDecimal(c.d).String(scale18)
			if got != c.want {
				t.Errorf("fromDecimal(coeff=%s,exp=%d) = %s want %s",
					c.d.Coefficient().String(), c.d.Exponent(), got, c.want)
			}
		}
	})

	// 4. Negative values
	t.Run("negative", func(t *testing.T) {
		cases := []decimal.Decimal{
			decimal.NewFromFloat(-0.5),
			decimal.NewFromFloat(-100.0),
			decimal.RequireFromString("-0.000000000000000001"),
		}
		for _, d := range cases {
			v := fromDecimal(d)
			if !v.IsNegative() {
				t.Errorf("fromDecimal(%s) should be negative", d.String())
			}
			if v.IsZero() {
				t.Errorf("fromDecimal(%s) should not be zero", d.String())
			}
		}
	})

	// 5. Zero
	t.Run("zero", func(t *testing.T) {
		v := fromDecimal(decimal.Zero)
		if !v.IsZero() {
			t.Errorf("fromDecimal(0) = %s want zero", v.String(scale18))
		}
	})

	// 6. Overflow at uint256 boundary
	t.Run("overflow", func(t *testing.T) {
		// 2^256 - 1 does NOT fit in signed Decimal256 (max positive is 2^255 - 1)
		maxU256 := new(big.Int).Lsh(big.NewInt(1), 256)
		maxU256.Sub(maxU256, big.NewInt(1))
		v := fromDecimal(decimal.NewFromBigInt(maxU256, -18))
		if !v.IsZero() {
			t.Errorf("fromDecimal(2^256-1) should overflow to zero, got %s", v.String(scale18))
		}

		// 2^255 - 1 DOES fit (max positive)
		maxSigned := new(big.Int).Lsh(big.NewInt(1), 255)
		maxSigned.Sub(maxSigned, big.NewInt(1))
		v = fromDecimal(decimal.NewFromBigInt(maxSigned, -18))
		if v.IsZero() {
			t.Errorf("fromDecimal(2^255-1, exp=-18) should NOT overflow, got zero")
		}

		// 10^78 overflows (needs ~260 bits > uint256)
		huge, _ := new(big.Int).SetString("1000000000000000000000000000000000000000000000000000000000000000000000000000000", 10)
		v = fromDecimal(decimal.NewFromBigInt(huge, -18))
		if !v.IsZero() {
			t.Errorf("fromDecimal(10^78) should overflow to zero, got %s", v.String(scale18))
		}
	})
}
