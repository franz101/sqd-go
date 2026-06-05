//go:build ignore

package main

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

func TestFPMMCreationBackfillsConditionForResolutionAndRedemption(t *testing.T) {
	state := generated.NewState()
	user := common.HexToAddress("0x6de391f369a4d7f2e93553cbd8939b270269668a")
	fpmmAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	collateral := common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174")
	conditionID := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	handleFixedProductMarketMakerCreation(state, &generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation{
		FixedProductMarketMaker: fpmmAddr,
		CollateralToken:         collateral,
		ConditionIds:            []common.Hash{conditionID},
	})

	if cond, ok := state.Condition.Get(conditionID); !ok {
		t.Fatalf("expected FPMM creation to backfill missing condition")
	} else if cond.OutcomeSlotCount != 2 {
		t.Fatalf("expected binary condition, got outcome slot count %d", cond.OutcomeSlotCount)
	}

	handleFPMMBuy(state, &generated.FixedProductMarketMakerFPMMBuy{
		Buyer:               user,
		InvestmentAmount:    *uint256.NewInt(40_000_000),
		OutcomeIndex:        *uint256.NewInt(0),
		OutcomeTokensBought: *uint256.NewInt(100_000_000),
	}, fpmmAddr)

	handleConditionResolution(state, &generated.ConditionalTokensConditionResolution{
		ConditionID:       conditionID,
		PayoutNumerators: []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(0)},
	})

	handlePayoutRedemptionCTF(state, &generated.ConditionalTokensPayoutRedemption{
		Redeemer:        user,
		CollateralToken: collateral,
		ConditionID:     conditionID,
	})

	posID := getFixedProductMarketMakerPositionID(&generated.FixedProductMarketMaker{
		ID:              fpmmAddr,
		ConditionID:     conditionID,
		CollateralToken: collateral,
	}, 0)
	pos := getUserPosition(state, user, posID)
	if pos == nil {
		t.Fatalf("expected user position")
	}
	if !pos.Amount.IsZero() {
		t.Fatalf("expected redeemed position amount to be zero, got %s", pos.Amount)
	}
	wantPnL := decimal.NewFromInt(60_000_000)
	if !pos.RealizedPnL.Equal(wantPnL) {
		t.Fatalf("realized pnl mismatch: got %s want %s", pos.RealizedPnL, wantPnL)
	}
}

func TestFPMMMappingCanBeLearnedFromConditionalTokenSplit(t *testing.T) {
	state := generated.NewState()
	user := common.HexToAddress("0x6de391f369a4d7f2e93553cbd8939b270269668a")
	fpmmAddr := common.HexToAddress("0x61234ab9b1fe92f5e397fc0429d60128d11e2d1a")
	collateral := common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174")
	conditionID := common.HexToHash("0x4aeb08df75e1fc83da8163bfa90e54ad9c164f416614000450300fabe1e9d532")

	handlePositionSplit(state, &generated.ConditionalTokensPositionSplit{
		Stakeholder:     fpmmAddr,
		CollateralToken: collateral,
		ConditionID:     conditionID,
		Amount:          *uint256.NewInt(100_000_000),
	})

	if fpmm, ok := state.FixedProductMarketMaker.Get(fpmmAddr); !ok {
		t.Fatalf("expected FPMM mapping learned from ConditionalTokens split")
	} else if fpmm.ConditionID != conditionID || fpmm.CollateralToken != collateral {
		t.Fatalf("bad learned FPMM mapping: condition=%s collateral=%s", fpmm.ConditionID, fpmm.CollateralToken)
	}

	handleFPMMBuy(state, &generated.FixedProductMarketMakerFPMMBuy{
		Buyer:               user,
		InvestmentAmount:    *uint256.NewInt(25_000_000),
		OutcomeIndex:        *uint256.NewInt(0),
		OutcomeTokensBought: *uint256.NewInt(50_000_000),
	}, fpmmAddr)

	posID := getFixedProductMarketMakerPositionID(&generated.FixedProductMarketMaker{
		ID:              fpmmAddr,
		ConditionID:     conditionID,
		CollateralToken: collateral,
	}, 0)
	pos := getUserPosition(state, user, posID)
	if pos == nil {
		t.Fatalf("expected user FPMM buy position")
	}
	if !pos.Amount.Equal(decimal.NewFromInt(50_000_000)) {
		t.Fatalf("amount mismatch: got %s", pos.Amount)
	}
	if !pos.AvgPrice.Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("avg price mismatch: got %s", pos.AvgPrice)
	}
}
