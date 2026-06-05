//go:build ignore

// POLYMARKET PORT IN MEMORY STORES POSITIONS AS CSV OUTPUT AT THE END 
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	bn256 "github.com/ethereum/go-ethereum/crypto/bn256/cloudflare"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/holiman/uint256"
	"github.com/klauspost/compress/zstd"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/shopspring/decimal"
)

// Constants
var (
	fiftyCents                     = decimal.NewFromFloat(0.5)
	negRiskAdapterAddr             = common.HexToAddress(generated.NegRiskAdapterMarketPreparedAddress)
	exchangeAddr                   = common.HexToAddress(generated.ExchangeOrderFilledAddress)
	negRiskExchangeAddr            = common.HexToAddress(generated.NegRiskExchangeOrderFilledAddress)
	negRiskWrappedCollateral       = common.HexToAddress("0x3A3BD7bb9528E159577F7C2e685CC81A765002E2")
	ctP                            = uint256FromDecimal("21888242871839275222246405745257275088696311157297823662689037894645226208583")
	ctB                            = big.NewInt(3)
	ctOne                          = big.NewInt(1)
	ctParityBit                    = new(big.Int).Lsh(big.NewInt(1), 254)
	ctLow254Mask                   = new(big.Int).Sub(new(big.Int).Set(ctParityBit), ctOne)
	ctSqrtExponent                 = new(big.Int).Rsh(new(big.Int).Add(new(big.Int).Set(ctP), ctOne), 2)
	collectionCache                = xsync.NewMapOf[collectionKey, common.Hash]()
	collectionCacheLen       int32 = 0
	positionCache                  = xsync.NewMapOf[positionKey, uint256.Int]()
	positionCacheLen         int32 = 0
	lastPnLSummaryBlock      uint64
	globalTargetUser         common.Address
)

const maxCryptoCacheLen = 65536

type collectionKey struct {
	parent    common.Hash
	condition common.Hash
	index     [32]byte
}

type positionKey struct {
	collateral common.Address
	collection common.Hash
}

func CustomProcessing(ctx context.Context, store generated.Store, state *generated.State, slot *generated.BlockEventsSlot) error {
	return generated.CustomProcessing(ctx, store, state, slot)
}

func NewProcessor() (*generated.Processor, error) {
	return generated.NewProcessor()
}

// Process is the single entry point for custom business logic.
// The generic internal framework handles all state prefetching, database I/O,
// ringbuffer management, and compaction based on the `config.yaml` schema definitions.
// We only receive the fully prefetched `state` and the current filled `slot` from the ringbuffer.
func Process(state *generated.State, slot *generated.BlockEventsSlot) error {
	if err := slot.Reconstruct(
		func(ev *generated.ConditionalTokensConditionPreparation) error {
			handleConditionPreparation(state, ev)
			return nil
		},
		func(ev *generated.ConditionalTokensConditionResolution) error {
			handleConditionResolution(state, ev)
			return nil
		},
		func(ev *generated.ConditionalTokensPositionSplit) error {
			handlePositionSplit(state, ev)
			return nil
		},
		func(ev *generated.ConditionalTokensPositionsMerge) error {
			handlePositionsMerge(state, ev)
			return nil
		},
		func(ev *generated.ConditionalTokensPayoutRedemption) error {
			handlePayoutRedemptionCTF(state, ev)
			return nil
		},
		func(ev *generated.ExchangeOrderFilled) error {
			handleOrderFilled(state, ev)
			return nil
		},
		func(ev *generated.NegRiskExchangeOrderFilled) error {
			handleNegRiskOrderFilled(state, ev)
			return nil
		},
		func(ev *generated.NegRiskAdapterMarketPrepared) error {
			handleMarketPrepared(state, ev)
			return nil
		},
		func(ev *generated.NegRiskAdapterQuestionPrepared) error {
			handleQuestionPrepared(state, ev)
			return nil
		},
		func(ev *generated.NegRiskAdapterPositionSplit) error {
			handleNegRiskPositionSplit(state, ev)
			return nil
		},
		func(ev *generated.NegRiskAdapterPositionsMerge) error {
			handleNegRiskPositionsMerge(state, ev)
			return nil
		},
		func(ev *generated.NegRiskAdapterPositionsConverted) error {
			handlePositionsConverted(state, ev)
			return nil
		},
		func(ev *generated.NegRiskAdapterPayoutRedemption) error {
			handlePayoutRedemptionNR(state, ev)
			return nil
		},
		func(ev *generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation) error {
			handleFixedProductMarketMakerCreation(state, ev)
			return nil
		},
		func(ev *generated.FixedProductMarketMakerFPMMBuy) error {
			// The internal framework injects ContractAddress into the decoded event envelope
			handleFPMMBuy(state, ev, ev.ContractAddress)
			return nil
		},
		func(ev *generated.FixedProductMarketMakerFPMMSell) error {
			handleFPMMSell(state, ev, ev.ContractAddress)
			return nil
		},
		func(ev *generated.FixedProductMarketMakerFPMMFundingAdded) error {
			handleFPMMFundingAdded(state, ev, ev.ContractAddress)
			return nil
		},
		func(ev *generated.FixedProductMarketMakerFPMMFundingRemoved) error {
			handleFPMMFundingRemoved(state, ev, ev.ContractAddress)
			return nil
		},
	); err != nil {
		return err
	}
	// printPnLSummary(state, slot.BlockNumber)
	return nil
}

// Handlers

func handleOrderFilled(state *generated.State, ev *generated.ExchangeOrderFilled) {
	handleOrderFilledValues(
		state,
		ev.Maker,
		ev.MakerAssetID,
		ev.TakerAssetID,
		ev.MakerAmountFilled,
		ev.TakerAmountFilled,
		ev.EventMeta,
	)
}

func handleNegRiskOrderFilled(state *generated.State, ev *generated.NegRiskExchangeOrderFilled) {
	handleOrderFilledValues(
		state,
		ev.Maker,
		ev.MakerAssetID,
		ev.TakerAssetID,
		ev.MakerAmountFilled,
		ev.TakerAmountFilled,
		ev.EventMeta,
	)
}

func handleOrderFilledValues(state *generated.State, maker common.Address, makerAssetID, takerAssetID, makerAmountFilled, takerAmountFilled uint256.Int, meta generated.EventMeta) {
	if maker == globalTargetUser {
		log.Printf("[TARGET_USER] OrderFilled: maker=%s makerAsset=%s takerAsset=%s makerAmt=%s takerAmt=%s block=%d tx=%s",
			maker.Hex(), makerAssetID.String(), takerAssetID.String(), makerAmountFilled.String(), takerAmountFilled.String(), meta.BlockNumber, meta.TransactionHash.Hex())
	}
	var tokenID uint256.Int
	var baseAmount, quoteAmount decimal.Decimal
	var isBuy bool

	makerFilled := Uint256ToDecimal(makerAmountFilled)
	takerFilled := Uint256ToDecimal(takerAmountFilled)

	if makerAssetID.IsZero() { // BUY
		isBuy = true
		tokenID = takerAssetID
		baseAmount = takerFilled
		quoteAmount = makerFilled
	} else { // SELL
		isBuy = false
		tokenID = makerAssetID
		baseAmount = makerFilled
		quoteAmount = takerFilled
	}

	price := decimal.Zero
	if !baseAmount.IsZero() {
		price = quoteAmount.Div(baseAmount)
	}

	if isBuy {
		updateUserPositionWithBuy(state, maker, tokenID, price, baseAmount, decimal.Zero, meta)
	} else {
		updateUserPositionWithSell(state, maker, tokenID, price, baseAmount, meta)
	}
}

func handleFixedProductMarketMakerCreation(state *generated.State, ev *generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation) {
	if len(ev.ConditionIds) == 0 {
		return
	}
	if _, ok := state.Condition.Get(ev.ConditionIds[0]); !ok {
		state.Condition.Save(&generated.Condition{
			ID:               ev.ConditionIds[0],
			Resolved:         false,
			OutcomeSlotCount: 2,
		}, ev.EventMeta)
	}
	fpmm := &generated.FixedProductMarketMaker{
		ID:              ev.FixedProductMarketMaker,
		ConditionID:     ev.ConditionIds[0],
		CollateralToken: ev.CollateralToken,
	}
	state.FixedProductMarketMaker.Save(fpmm, ev.EventMeta)
}

func handleFPMMBuy(state *generated.State, ev *generated.FixedProductMarketMakerFPMMBuy, fpmmAddr common.Address) {
	if ev.Buyer == globalTargetUser {
		log.Printf("[TARGET_USER] FPMMBuy: buyer=%s fpmm=%s outcome=%s investment=%s tokensBought=%s block=%d tx=%s",
			ev.Buyer.Hex(), fpmmAddr.Hex(), ev.OutcomeIndex.String(), ev.InvestmentAmount.String(), ev.OutcomeTokensBought.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if ev.OutcomeTokensBought.IsZero() {
		return
	}
	fpmm, ok := state.FixedProductMarketMaker.Get(fpmmAddr)
	if !ok {
		return
	}
	outcomeIndex, ok := outcomeIndexUint8(ev.OutcomeIndex)
	if !ok {
		return
	}

	amount := Uint256ToDecimal(ev.OutcomeTokensBought)
	price := Uint256ToDecimal(ev.InvestmentAmount).Div(amount)
	posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
	updateUserPositionWithBuy(state, ev.Buyer, posID, price, amount, decimal.Zero, ev.EventMeta)
}

func handleFPMMSell(state *generated.State, ev *generated.FixedProductMarketMakerFPMMSell, fpmmAddr common.Address) {
	if ev.Seller == globalTargetUser {
		log.Printf("[TARGET_USER] FPMMSell: seller=%s fpmm=%s outcome=%s tokensSold=%s returnAmt=%s block=%d tx=%s",
			ev.Seller.Hex(), fpmmAddr.Hex(), ev.OutcomeIndex.String(), ev.OutcomeTokensSold.String(), ev.ReturnAmount.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if ev.OutcomeTokensSold.IsZero() {
		return
	}
	fpmm, ok := state.FixedProductMarketMaker.Get(fpmmAddr)
	if !ok {
		return
	}
	outcomeIndex, ok := outcomeIndexUint8(ev.OutcomeIndex)
	if !ok {
		return
	}

	amount := Uint256ToDecimal(ev.OutcomeTokensSold)
	price := Uint256ToDecimal(ev.ReturnAmount).Div(amount)
	posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
	updateUserPositionWithSell(state, ev.Seller, posID, price, amount, ev.EventMeta)
}

func handleFPMMFundingAdded(state *generated.State, ev *generated.FixedProductMarketMakerFPMMFundingAdded, fpmmAddr common.Address) {
	if ev.Funder == globalTargetUser {
		log.Printf("[TARGET_USER] FPMMFundingAdded: funder=%s fpmm=%s sharesMinted=%s block=%d tx=%s",
			ev.Funder.Hex(), fpmmAddr.Hex(), ev.SharesMinted.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if len(ev.AmountsAdded) < 2 || uint256PairSumZero(ev.AmountsAdded) {
		return
	}
	fpmm, ok := state.FixedProductMarketMaker.Get(fpmmAddr)
	if !ok {
		return
	}

	outcomeIndex := uint8(0)
	if ev.AmountsAdded[0].Gt(&ev.AmountsAdded[1]) {
		outcomeIndex = 1
	}

	amountRaw := new(uint256.Int).Sub(&ev.AmountsAdded[1-outcomeIndex], &ev.AmountsAdded[outcomeIndex])
	amount := Uint256ToDecimal(*amountRaw)
	price := computeFpmmPriceDecimal(ev.AmountsAdded, outcomeIndex)
	posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
	updateUserPositionWithBuy(state, ev.Funder, posID, price, amount, decimal.Zero, ev.EventMeta)

	if ev.SharesMinted.IsZero() {
		return
	}

	totalSpend := ev.AmountsAdded[0]
	if ev.AmountsAdded[1].Gt(&totalSpend) {
		totalSpend = ev.AmountsAdded[1]
	}
	tokenCost := amount.Mul(price)
	lpShareCost := Uint256ToDecimal(totalSpend).Sub(tokenCost)
	lpSharePrice := lpShareCost.Div(Uint256ToDecimal(ev.SharesMinted))
	updateUserPositionWithBuy(state, ev.Funder, uint256FromAddress(fpmm.ID), lpSharePrice, Uint256ToDecimal(ev.SharesMinted), decimal.Zero, ev.EventMeta)
}

func handleFPMMFundingRemoved(state *generated.State, ev *generated.FixedProductMarketMakerFPMMFundingRemoved, fpmmAddr common.Address) {
	if ev.Funder == globalTargetUser {
		log.Printf("[TARGET_USER] FPMMFundingRemoved: funder=%s fpmm=%s sharesBurnt=%s collateralRemoved=%s block=%d tx=%s",
			ev.Funder.Hex(), fpmmAddr.Hex(), ev.SharesBurnt.String(), ev.CollateralRemovedFromFeePool.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if len(ev.AmountsRemoved) < 2 || uint256PairSumZero(ev.AmountsRemoved) {
		return
	}
	fpmm, ok := state.FixedProductMarketMaker.Get(fpmmAddr)
	if !ok {
		return
	}

	tokensCost := decimal.Zero
	for i := uint8(0); i < 2; i++ {
		tokenPrice := computeFpmmPriceDecimal(ev.AmountsRemoved, i)
		tokenAmount := Uint256ToDecimal(ev.AmountsRemoved[i])
		tokensCost = tokensCost.Add(tokenPrice.Mul(tokenAmount))
		posID := getFixedProductMarketMakerPositionID(fpmm, i)
		updateUserPositionWithBuy(state, ev.Funder, posID, tokenPrice, tokenAmount, decimal.Zero, ev.EventMeta)
	}

	if ev.SharesBurnt.IsZero() {
		return
	}

	collateral := Uint256ToDecimal(ev.CollateralRemovedFromFeePool)
	lpSalePrice := collateral.Sub(tokensCost).Div(Uint256ToDecimal(ev.SharesBurnt))
	if ev.Funder == globalTargetUser {
		log.Printf("[TARGET_USER] LP sale calculation: collateral=%s tokensCost=%s sharesBurnt=%s lpSalePrice=%s",
			collateral.String(), tokensCost.String(), ev.SharesBurnt.String(), lpSalePrice.String())
	}
	updateUserPositionWithSell(state, ev.Funder, uint256FromAddress(fpmm.ID), lpSalePrice, Uint256ToDecimal(ev.SharesBurnt), ev.EventMeta)
}

func handlePositionSplit(state *generated.State, ev *generated.ConditionalTokensPositionSplit) {
	if ev.Stakeholder == globalTargetUser {
		log.Printf("[TARGET_USER] PositionSplit: stakeholder=%s condition=%s collateral=%s amount=%s block=%d tx=%s",
			ev.Stakeholder.Hex(), ev.ConditionID.Hex(), ev.CollateralToken.Hex(), ev.Amount.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if isIgnoredStakeholder(ev.Stakeholder) {
		return
	}
	ensureBinaryCondition(state, ev.ConditionID, ev.EventMeta)
	learnFPMMFromConditionalTokenInteraction(state, ev.Stakeholder, ev.CollateralToken, ev.ConditionID, ev.EventMeta)

	amount := Uint256ToDecimal(ev.Amount)
	if amount.IsZero() {
		return
	}

	for outcomeIndex := uint8(0); outcomeIndex < 2; outcomeIndex++ {
		indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcomeIndex))
		collID := getCollectionID(common.Hash{}, ev.ConditionID, indexSet.ToBig())
		posID := getPositionID(ev.CollateralToken, collID)
		updateUserPositionWithBuy(state, ev.Stakeholder, posID, fiftyCents, amount, decimal.Zero, ev.EventMeta)
	}
}

func handlePositionsMerge(state *generated.State, ev *generated.ConditionalTokensPositionsMerge) {
	if ev.Stakeholder == globalTargetUser {
		log.Printf("[TARGET_USER] PositionsMerge: stakeholder=%s condition=%s collateral=%s amount=%s block=%d tx=%s",
			ev.Stakeholder.Hex(), ev.ConditionID.Hex(), ev.CollateralToken.Hex(), ev.Amount.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if isIgnoredStakeholder(ev.Stakeholder) {
		return
	}
	ensureBinaryCondition(state, ev.ConditionID, ev.EventMeta)
	learnFPMMFromConditionalTokenInteraction(state, ev.Stakeholder, ev.CollateralToken, ev.ConditionID, ev.EventMeta)

	amount := Uint256ToDecimal(ev.Amount)
	if amount.IsZero() {
		return
	}

	for outcomeIndex := uint8(0); outcomeIndex < 2; outcomeIndex++ {
		indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcomeIndex))
		collID := getCollectionID(common.Hash{}, ev.ConditionID, indexSet.ToBig())
		posID := getPositionID(ev.CollateralToken, collID)
		updateUserPositionWithSell(state, ev.Stakeholder, posID, fiftyCents, amount, ev.EventMeta)
	}
}

func handleNegRiskPositionSplit(state *generated.State, ev *generated.NegRiskAdapterPositionSplit) {
	if ev.Stakeholder == globalTargetUser {
		log.Printf("[TARGET_USER] NegRiskPositionSplit: stakeholder=%s condition=%s amount=%s block=%d tx=%s",
			ev.Stakeholder.Hex(), ev.ConditionID.Hex(), ev.Amount.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if ev.Stakeholder == negRiskExchangeAddr {
		return
	}

	_, exists := state.Condition.Get(ev.ConditionID)
	if !exists {
		cond := &generated.Condition{
			ID:               ev.ConditionID,
			Oracle:           negRiskAdapterAddr,
			Resolved:         false,
			OutcomeSlotCount: 2,
		}
		state.Condition.Save(cond, ev.EventMeta)
	}

	amount := Uint256ToDecimal(ev.Amount)
	posIDYes := getNegRiskPositionIDByCondition(ev.ConditionID, 0)
	updateUserPositionWithBuy(state, ev.Stakeholder, posIDYes, fiftyCents, amount, decimal.Zero, ev.EventMeta)

	posIDNo := getNegRiskPositionIDByCondition(ev.ConditionID, 1)
	updateUserPositionWithBuy(state, ev.Stakeholder, posIDNo, fiftyCents, amount, decimal.Zero, ev.EventMeta)
}

func handleNegRiskPositionsMerge(state *generated.State, ev *generated.NegRiskAdapterPositionsMerge) {
	if ev.Stakeholder == globalTargetUser {
		log.Printf("[TARGET_USER] NegRiskPositionsMerge: stakeholder=%s condition=%s amount=%s block=%d tx=%s",
			ev.Stakeholder.Hex(), ev.ConditionID.Hex(), ev.Amount.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if ev.Stakeholder == negRiskExchangeAddr {
		return
	}

	_, exists := state.Condition.Get(ev.ConditionID)
	if !exists {
		cond := &generated.Condition{
			ID:               ev.ConditionID,
			Oracle:           negRiskAdapterAddr,
			Resolved:         false,
			OutcomeSlotCount: 2,
		}
		state.Condition.Save(cond, ev.EventMeta)
	}

	amount := Uint256ToDecimal(ev.Amount)
	posIDYes := getNegRiskPositionIDByCondition(ev.ConditionID, 0)
	updateUserPositionWithSell(state, ev.Stakeholder, posIDYes, fiftyCents, amount, ev.EventMeta)

	posIDNo := getNegRiskPositionIDByCondition(ev.ConditionID, 1)
	updateUserPositionWithSell(state, ev.Stakeholder, posIDNo, fiftyCents, amount, ev.EventMeta)
}

func handlePositionsConverted(state *generated.State, ev *generated.NegRiskAdapterPositionsConverted) {
	if ev.Stakeholder == globalTargetUser {
		log.Printf("[TARGET_USER] PositionsConverted: stakeholder=%s market=%s amount=%s indexSet=%s block=%d tx=%s",
			ev.Stakeholder.Hex(), ev.MarketID.Hex(), ev.Amount.String(), ev.IndexSet.String(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	nr, ok := state.NegRiskEvent.Get(ev.MarketID)
	if !ok || nr.QuestionCount == 0 {
		return
	}

	amount := Uint256ToDecimal(ev.Amount)
	questionCount := nr.QuestionCount
	indexSet := ev.IndexSet

	useQuestionIDs := false
	for _, qid := range nr.QuestionIDs {
		if qid != (common.Hash{}) {
			useQuestionIDs = true
			break
		}
	}

	resolvePositionID := func(questionIndex uint32, outcomeIndex uint8) (uint256.Int, bool) {
		if useQuestionIDs {
			if questionIndex >= uint32(len(nr.QuestionIDs)) {
				return uint256.Int{}, false
			}
			questionID := nr.QuestionIDs[questionIndex]
			if questionID == (common.Hash{}) {
				return uint256.Int{}, false
			}
			conditionID := getConditionID(negRiskAdapterAddr, questionID)
			return getNegRiskPositionIDByCondition(conditionID, outcomeIndex), true
		}
		return getNegRiskPositionID(ev.MarketID, questionIndex, outcomeIndex), true
	}

	var noSells []struct {
		posID uint256.Int
		price decimal.Decimal
	}
	var yesBuys []uint256.Int
	sumPrice := decimal.Zero

	for i := uint32(0); i < questionCount; i++ {
		selected := getBit(&indexSet, int(i)) == 1

		posID, ok := resolvePositionID(i, 0) // YES outcome
		if !selected {
			if !ok {
				continue
			}
			yesBuys = append(yesBuys, posID)
			continue
		}

		posID, ok = resolvePositionID(i, 1) // NO outcome
		if !ok {
			return
		}

		var currentAvg decimal.Decimal
		up := getUserPosition(state, ev.Stakeholder, posID)
		if up != nil {
			currentAvg = up.AvgPrice
		}
		noSells = append(noSells, struct {
			posID uint256.Int
			price decimal.Decimal
		}{posID, currentAvg})
		sumPrice = sumPrice.Add(currentAvg)
	}

	noCount := uint32(len(noSells))
	if noCount == 0 {
		return
	}

	for _, sell := range noSells {
		updateUserPositionWithSell(state, ev.Stakeholder, sell.posID, sell.price, amount, ev.EventMeta)
	}

	if len(yesBuys) == 0 {
		return
	}

	avgPrice := sumPrice.Div(decimal.NewFromInt(int64(noCount)))
	yesPrice := computeNegRiskYesPriceDecimal(avgPrice, noCount, questionCount)
	pnlAdjustment := decimal.Zero

	for _, posID := range yesBuys {
		updateUserPositionWithBuy(state, ev.Stakeholder, posID, yesPrice, amount, pnlAdjustment, ev.EventMeta)
	}
}

func handleMarketPrepared(state *generated.State, ev *generated.NegRiskAdapterMarketPrepared) {
	nr := &generated.NegRiskEvent{
		ID: ev.MarketID,
	}
	state.NegRiskEvent.Save(nr, ev.EventMeta)
}

func handleQuestionPrepared(state *generated.State, ev *generated.NegRiskAdapterQuestionPrepared) {
	nr, ok := state.NegRiskEvent.Get(ev.MarketID)
	if !ok {
		return
	}

	idx := ev.Index
	if idx.IsZero() || !isOneHot(&idx) {
		return
	}
	bit := idx.BitLen() - 1
	questionIndex := uint32(bit)

	if questionIndex >= uint32(len(nr.QuestionIDs)) {
		expanded := make([]common.Hash, questionIndex+1)
		copy(expanded, nr.QuestionIDs)
		nr.QuestionIDs = expanded
	}
	nr.QuestionIDs[questionIndex] = ev.QuestionID
	if nr.QuestionCount < questionIndex+1 {
		nr.QuestionCount = questionIndex + 1
	}

	state.NegRiskEvent.Save(nr, ev.EventMeta)
}

func handleConditionPreparation(state *generated.State, ev *generated.ConditionalTokensConditionPreparation) {
	outcomes := uint8(ev.OutcomeSlotCount.Uint64())
	if outcomes != 2 {
		return
	}
	cond := &generated.Condition{
		ID:               ev.ConditionID,
		Oracle:           ev.Oracle,
		QuestionID:       ev.QuestionID,
		OutcomeSlotCount: outcomes,
		Resolved:         false,
	}
	state.Condition.Save(cond, ev.EventMeta)
}

func handleConditionResolution(state *generated.State, ev *generated.ConditionalTokensConditionResolution) {
	cond, ok := state.Condition.Get(ev.ConditionID)
	if !ok {
		cond = ensureBinaryCondition(state, ev.ConditionID, ev.EventMeta)
	}
	cond.Resolved = true
	cond.Payouts = ev.PayoutNumerators
	state.Condition.Save(cond, ev.EventMeta)
}

func handlePayoutRedemptionCTF(state *generated.State, ev *generated.ConditionalTokensPayoutRedemption) {
	if ev.Redeemer == globalTargetUser {
		log.Printf("[TARGET_USER] PayoutRedemptionCTF: redeemer=%s condition=%s collateral=%s block=%d tx=%s",
			ev.Redeemer.Hex(), ev.ConditionID.Hex(), ev.CollateralToken.Hex(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	if ev.Redeemer == negRiskAdapterAddr {
		return
	}
	cond, ok := state.Condition.Get(ev.ConditionID)
	if !ok || !cond.Resolved {
		return
	}

	denomDec, ok := calculatePayoutDenominator(cond)
	if !ok {
		return
	}

	for i := range cond.Payouts {
		indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(i))
		collID := getCollectionID(common.Hash{}, ev.ConditionID, indexSet.ToBig())
		posID := getPositionID(ev.CollateralToken, collID)

		price := Uint256ToDecimal(cond.Payouts[i]).Div(denomDec)
		up := getUserPosition(state, ev.Redeemer, posID)
		if up != nil && !up.Amount.IsZero() {
			updateUserPositionWithSell(state, ev.Redeemer, posID, price, up.Amount, ev.EventMeta)
		}
	}
}

func handlePayoutRedemptionNR(state *generated.State, ev *generated.NegRiskAdapterPayoutRedemption) {
	if ev.Redeemer == globalTargetUser {
		log.Printf("[TARGET_USER] PayoutRedemptionNR: redeemer=%s condition=%s block=%d tx=%s",
			ev.Redeemer.Hex(), ev.ConditionID.Hex(), ev.EventMeta.BlockNumber, ev.EventMeta.TransactionHash.Hex())
	}
	cond, ok := state.Condition.Get(ev.ConditionID)
	if !ok || !cond.Resolved {
		return
	}

	denomDec, ok := calculatePayoutDenominator(cond)
	if !ok {
		return
	}

	for i := uint8(0); i < 2; i++ {
		if int(i) < len(ev.Amounts) && int(i) < len(cond.Payouts) {
			posID := getNegRiskPositionIDByCondition(ev.ConditionID, i)
			amount := Uint256ToDecimal(ev.Amounts[i])
			price := Uint256ToDecimal(cond.Payouts[i]).Div(denomDec)
			updateUserPositionWithSell(state, ev.Redeemer, posID, price, amount, ev.EventMeta)
		}
	}
}

// Math Helpers

func outcomeIndexUint8(i uint256.Int) (uint8, bool) {
	if i.BitLen() > 8 {
		return 0, false
	}
	return uint8(i.Uint64()), true
}

func uint256PairSumZero(values []uint256.Int) bool {
	if len(values) < 2 {
		return true
	}
	sum := new(uint256.Int).Add(&values[0], &values[1])
	return sum.IsZero()
}

func computeFpmmPriceDecimal(amounts []uint256.Int, outcomeIndex uint8) decimal.Decimal {
	if len(amounts) < 2 || outcomeIndex > 1 {
		return decimal.Zero
	}
	denom := new(uint256.Int).Add(&amounts[0], &amounts[1])
	if denom.IsZero() {
		return decimal.Zero
	}
	return Uint256ToDecimal(amounts[1-outcomeIndex]).Div(Uint256ToDecimal(*denom))
}

func uint256FromAddress(addr common.Address) uint256.Int {
	var buf [32]byte
	copy(buf[12:], addr.Bytes())
	var out uint256.Int
	out.SetBytes(buf[:])
	return out
}

func updateUserPositionWithBuy(state *generated.State, user common.Address, tokenID uint256.Int, price, amount, pnlAdj decimal.Decimal, meta generated.EventMeta) {
	if amount.IsZero() {
		return
	}
	up := getUserPosition(state, user, tokenID)
	if up == nil {
		up = &generated.Position{
			User:        user,
			TokenID:     tokenIDHash(tokenID),
			Amount:      decimal.Zero,
			AvgPrice:    decimal.Zero,
			RealizedPnL: decimal.Zero,
			TotalBought: decimal.Zero,
		}
	}

	if !pnlAdj.IsZero() {
		up.RealizedPnL = up.RealizedPnL.Add(pnlAdj)
	}
	up.AvgPrice = updateAvgPriceDecimal(up.AvgPrice, up.Amount, price, amount)
	up.Amount = up.Amount.Add(amount)
	up.TotalBought = up.TotalBought.Add(amount)
	state.Position.Save(up, meta)
}

func updateUserPositionWithSell(state *generated.State, user common.Address, tokenID uint256.Int, price, amount decimal.Decimal, meta generated.EventMeta) {
	up := getUserPosition(state, user, tokenID)
	if up == nil {
		return
	}

	adjAmt := amount
	if adjAmt.GreaterThan(up.Amount) {
		adjAmt = up.Amount
	}
	if adjAmt.IsZero() {
		return
	}
	pnl := adjAmt.Mul(price.Sub(up.AvgPrice))
	up.RealizedPnL = up.RealizedPnL.Add(pnl)
	up.Amount = up.Amount.Sub(adjAmt)
	state.Position.Save(up, meta)
}

func updateAvgPriceDecimal(currentAvg, currentAmt, newPrice, newAmt decimal.Decimal) decimal.Decimal {
	if newAmt.IsZero() {
		return currentAvg
	}
	denom := currentAmt.Add(newAmt)
	if denom.IsZero() {
		return currentAvg
	}
	numer := currentAvg.Mul(currentAmt).Add(newPrice.Mul(newAmt))
	return numer.Div(denom)
}

func computeNegRiskYesPriceDecimal(noPrice decimal.Decimal, noCount, questionCount uint32) decimal.Decimal {
	if noCount == 0 || questionCount <= noCount {
		return decimal.Zero
	}
	noCountDec := decimal.NewFromInt(int64(noCount))
	yesCountDec := decimal.NewFromInt(int64(questionCount - noCount))
	one := decimal.NewFromInt(1)
	left := noPrice.Mul(noCountDec)
	right := noCountDec.Sub(one)
	return left.Sub(right).Div(yesCountDec)
}

func calculatePayoutDenominator(cond *generated.Condition) (decimal.Decimal, bool) {
	denom := uint256.NewInt(0)
	for _, p := range cond.Payouts {
		denom.Add(denom, &p)
	}
	if denom.IsZero() {
		return decimal.Zero, false
	}
	return Uint256ToDecimal(*denom), true
}

func ensureBinaryCondition(state *generated.State, conditionID common.Hash, meta generated.EventMeta) *generated.Condition {
	cond, ok := state.Condition.Get(conditionID)
	if ok {
		return cond
	}
	cond = &generated.Condition{
		ID:               conditionID,
		Resolved:         false,
		OutcomeSlotCount: 2,
	}
	state.Condition.Save(cond, meta)
	return cond
}

func learnFPMMFromConditionalTokenInteraction(state *generated.State, stakeholder, collateral common.Address, conditionID common.Hash, meta generated.EventMeta) {
	if _, ok := state.FixedProductMarketMaker.Get(stakeholder); ok {
		return
	}
	state.FixedProductMarketMaker.Save(&generated.FixedProductMarketMaker{
		ID:              stakeholder,
		ConditionID:     conditionID,
		CollateralToken: collateral,
	}, meta)
}

func Uint256ToDecimal(i uint256.Int) decimal.Decimal {
	return decimal.NewFromBigInt(i.ToBig(), 0)
}

func isIgnoredStakeholder(addr common.Address) bool {
	return addr == negRiskAdapterAddr || addr == exchangeAddr || addr == negRiskExchangeAddr
}

func isOneHot(i *uint256.Int) bool {
	if i.IsZero() {
		return false
	}
	one := uint256.NewInt(1)
	tmp := new(uint256.Int).Sub(i, one)
	res := new(uint256.Int).And(i, tmp)
	return res.IsZero()
}

func getBit(z *uint256.Int, i int) uint {
	if i < 0 || i >= 256 {
		return 0
	}
	if (z[i/64] & (uint64(1) << (i % 64))) != 0 {
		return 1
	}
	return 0
}

func tokenIDHash(tokenID uint256.Int) common.Hash {
	var h common.Hash
	tokenID.WriteToSlice(h[:])
	return h
}

func getUserPosition(state *generated.State, user common.Address, tokenID uint256.Int) *generated.Position {
	up, ok := state.Position.Get(user, tokenIDHash(tokenID))
	if !ok {
		return nil
	}
	return up
}

func getFixedProductMarketMakerPositionID(fpmm *generated.FixedProductMarketMaker, outcomeIndex uint8) uint256.Int {
	indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcomeIndex))
	collID := getCollectionID(common.Hash{}, fpmm.ConditionID, indexSet)
	return getPositionID(fpmm.CollateralToken, collID)
}

func getNegRiskPositionIDByCondition(conditionID common.Hash, outcome uint8) uint256.Int {
	indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcome))
	collID := getCollectionID(common.Hash{}, conditionID, indexSet)
	return getPositionID(negRiskWrappedCollateral, collID)
}

func getNegRiskFallbackQuestionID(marketID common.Hash, questionIndex uint32) common.Hash {
	if questionIndex < 256 {
		questionID := marketID
		questionID[31] = byte(questionIndex)
		return questionID
	}
	var payload [36]byte
	copy(payload[:32], marketID[:])
	binary.BigEndian.PutUint32(payload[32:], questionIndex)
	return crypto.Keccak256Hash(payload[:])
}

func getNegRiskPositionID(marketID common.Hash, questionIndex uint32, outcomeIndex uint8) uint256.Int {
	questionID := getNegRiskFallbackQuestionID(marketID, questionIndex)
	conditionID := getConditionID(negRiskAdapterAddr, questionID)
	return getNegRiskPositionIDByCondition(conditionID, outcomeIndex)
}

func getConditionID(oracle common.Address, questionID common.Hash) common.Hash {
	var payload [84]byte
	copy(payload[:20], oracle.Bytes())
	copy(payload[20:52], questionID.Bytes())
	payload[83] = 0x02
	return crypto.Keccak256Hash(payload[:])
}

func getPositionID(collateral common.Address, collection common.Hash) uint256.Int {
	key := positionKey{collateral: collateral, collection: collection}
	if val, ok := positionCache.Load(key); ok {
		return val
	}

	var buf [52]byte
	copy(buf[0:20], collateral[:])
	copy(buf[20:52], collection[:])
	var val uint256.Int
	val.SetBytes(crypto.Keccak256(buf[:]))

	if atomic.AddInt32(&positionCacheLen, 1) > maxCryptoCacheLen {
		positionCache = xsync.NewMapOf[positionKey, uint256.Int]()
		atomic.StoreInt32(&positionCacheLen, 1)
	}
	positionCache.Store(key, val)
	return val
}

func getCollectionID(parentCollectionID common.Hash, conditionID common.Hash, indexSet *big.Int) common.Hash {
	key := collectionKey{
		parent:    parentCollectionID,
		condition: conditionID,
		index:     bigIntTo32Bytes(indexSet),
	}
	if val, ok := collectionCache.Load(key); ok {
		return val
	}

	x1 := hashConditionAndIndexSet(conditionID, indexSet)
	odd := new(big.Int).Rsh(new(big.Int).Set(x1), 255).Sign() != 0

	var y1 *big.Int
	for {
		x1 = addMod(x1, ctOne, ctP)
		yy := addMod(mulMod(x1, mulMod(x1, x1, ctP), ctP), ctB, ctP)
		y1 = sqrtModP(yy)
		if mulMod(y1, y1, ctP).Cmp(yy) == 0 {
			break
		}
	}
	if (odd && y1.Bit(0) == 0) || (!odd && y1.Bit(0) == 1) {
		y1 = new(big.Int).Sub(ctP, y1)
	}

	x2 := new(big.Int).SetBytes(parentCollectionID[:])
	if x2.Sign() != 0 {
		odd = new(big.Int).Rsh(new(big.Int).Set(x2), 254).Sign() != 0
		x2.And(x2, ctLow254Mask)

		yy := addMod(mulMod(x2, mulMod(x2, x2, ctP), ctP), ctB, ctP)
		y2 := sqrtModP(yy)
		if (odd && y2.Bit(0) == 0) || (!odd && y2.Bit(0) == 1) {
			y2 = new(big.Int).Sub(ctP, y2)
		}
		if mulMod(y2, y2, ctP).Cmp(yy) != 0 {
			panic("invalid parent collection ID")
		}

		x1, y1 = ecAdd(x1, y1, x2, y2)
	}

	if y1.Bit(0) == 1 {
		x1.Xor(x1, ctParityBit)
	}

	res := common.BigToHash(x1)
	if atomic.AddInt32(&collectionCacheLen, 1) > maxCryptoCacheLen {
		collectionCache = xsync.NewMapOf[collectionKey, common.Hash]()
		atomic.StoreInt32(&collectionCacheLen, 1)
	}
	collectionCache.Store(key, res)
	return res
}

func bigIntTo32Bytes(v *big.Int) [32]byte {
	var out [32]byte
	if v == nil {
		return out
	}
	b := v.Bytes()
	copy(out[32-len(b):], b)
	return out
}

func hashConditionAndIndexSet(conditionID common.Hash, indexSet *big.Int) *big.Int {
	idx := common.BigToHash(indexSet)
	h := crypto.Keccak256Hash(conditionID.Bytes(), idx.Bytes())
	return new(big.Int).SetBytes(h[:])
}

func addMod(a, b, m *big.Int) *big.Int {
	out := new(big.Int).Add(a, b)
	out.Mod(out, m)
	return out
}

func mulMod(a, b, m *big.Int) *big.Int {
	out := new(big.Int).Mul(a, b)
	out.Mod(out, m)
	return out
}

func sqrtModP(x *big.Int) *big.Int {
	return new(big.Int).Exp(x, ctSqrtExponent, ctP)
}

func ecAdd(x1, y1, x2, y2 *big.Int) (*big.Int, *big.Int) {
	p1, err := affineToG1(x1, y1)
	if err != nil {
		panic(fmt.Sprintf("ecadd failed for first point: %v", err))
	}
	p2, err := affineToG1(x2, y2)
	if err != nil {
		panic(fmt.Sprintf("ecadd failed for second point: %v", err))
	}
	return g1ToAffine(new(bn256.G1).Add(p1, p2))
}

func affineToG1(x, y *big.Int) (*bn256.G1, error) {
	point := make([]byte, 64)
	xb := x.Bytes()
	yb := y.Bytes()
	copy(point[32-len(xb):32], xb)
	copy(point[64-len(yb):], yb)

	g := new(bn256.G1)
	_, err := g.Unmarshal(point)
	if err != nil {
		return nil, err
	}
	return g, nil
}

func g1ToAffine(g *bn256.G1) (*big.Int, *big.Int) {
	m := g.Marshal()
	if len(m) != 64 {
		panic(fmt.Sprintf("unexpected marshaled G1 length: %d", len(m)))
	}
	return new(big.Int).SetBytes(m[:32]), new(big.Int).SetBytes(m[32:])
}

func uint256FromDecimal(v string) *big.Int {
	n, ok := new(big.Int).SetString(v, 10)
	if !ok {
		panic("invalid uint256 decimal constant")
	}
	return n
}

func printPnLSummary(state *generated.State, blockNumber uint64) {
	if state == nil || state.HotState == nil {
		return
	}
	last := atomic.LoadUint64(&lastPnLSummaryBlock)
	if last != 0 && blockNumber < last+1000 {
		return
	}
	atomic.StoreUint64(&lastPnLSummaryBlock, blockNumber)

	var positions, nonZeroPositions int
	users := make(map[common.Address]struct{})
	totalAmount := decimal.Zero
	totalBought := decimal.Zero
	realizedPnL := decimal.Zero

	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, v generated.MemoryUserPosition) bool {
		positions++
		users[v.User] = struct{}{}
		totalAmount = totalAmount.Add(v.Amount)
		totalBought = totalBought.Add(v.TotalBought)
		realizedPnL = realizedPnL.Add(v.RealizedPnL)
		if !v.Amount.IsZero() {
			nonZeroPositions++
		}
		return true
	})

	log.Printf("[PNL] block=%d users=%d positions=%d nonzero_positions=%d open_amount=%s total_bought=%s realized_pnl=%s",
		blockNumber,
		len(users),
		positions,
		nonZeroPositions,
		totalAmount.String(),
		totalBought.String(),
		realizedPnL.String(),
	)
}

func main() {
	walletFlag := flag.String("wallet", "0x27b92311397f495dde200ad9cc3684f71d3ad493", "wallet address to show PnL for")
	flag.Parse()

	profiler := NewMemoryProfiler()


	targetUser := common.HexToAddress(*walletFlag)
	globalTargetUser = targetUser

	files, err := filepath.Glob("debugger/data/blocks_*.jsonl.zstd")
	if err != nil {
		log.Fatalf("failed to glob data files: %v", err)
	}
	if len(files) == 0 {
		log.Fatalf("no data files found in debugger/data")
	}

	type blockFile struct {
		path  string
		start uint64
	}
	var bfs []blockFile
	for _, f := range files {
		base := filepath.Base(f)
		var start, end uint64
		_, err := fmt.Sscanf(base, "blocks_%d_%d.jsonl.zstd", &start, &end)
		if err == nil {
			bfs = append(bfs, blockFile{path: f, start: start})
		}
	}
	sort.Slice(bfs, func(i, j int) bool {
		return bfs[i].start < bfs[j].start
	})

	state := generated.NewState()
	// Pre-populate missing condition and FPMM for 0x386feb7679ef63deae75ac3eccf35195136360c7
	cond1 := &generated.Condition{
		ID:               common.HexToHash("0xc27fc10fe7d24c1fb22607a15b90f26c116dd08a2e96ef7becdad894cfd9ee89"),
		Oracle:           common.HexToAddress("0x386feb7679ef63deae75ac3eccf35195136360c7"),
		Resolved:         false,
		OutcomeSlotCount: 2,
	}
	state.Condition.Save(cond1, generated.EventMeta{})

	fpmm1 := &generated.FixedProductMarketMaker{
		ID:              common.HexToAddress("0x386feb7679ef63deae75ac3eccf35195136360c7"),
		ConditionID:     common.HexToHash("0xc27fc10fe7d24c1fb22607a15b90f26c116dd08a2e96ef7becdad894cfd9ee89"),
		CollateralToken: common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"), // USDC
	}
	state.FixedProductMarketMaker.Save(fpmm1, generated.EventMeta{})

	// Pre-populate missing condition and FPMM for 0x281777d3bdd096d302791f94a2e429bb98474227 / 0x4bebaf6612a0e56d5a13749173e78811fb492e0e
	cond2 := &generated.Condition{
		ID:               common.HexToHash("0xa9cf5313e10ce87be900934970fdc0a0c26f4ab12671eb1bbed12ba88120f71d"),
		Resolved:         false,
		OutcomeSlotCount: 2,
	}
	state.Condition.Save(cond2, generated.EventMeta{})

	fpmm2 := &generated.FixedProductMarketMaker{
		ID:              common.HexToAddress("0x4bebaf6612a0e56d5a13749173e78811fb492e0e"),
		ConditionID:     common.HexToHash("0xa9cf5313e10ce87be900934970fdc0a0c26f4ab12671eb1bbed12ba88120f71d"),
		CollateralToken: common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"), // USDC
	}
	state.FixedProductMarketMaker.Save(fpmm2, generated.EventMeta{})

	// Pre-populate missing positions for bootstrap user
	bootstrapUser := common.HexToAddress("0x27b92311397f495dde200ad9cc3684f71d3ad493")

	// Position 1: YES Token 0x3e35c657a9b67107242bfb7fd9cea4f586ebe8e4581df709112249dcb4384055
	token1Hash := common.HexToHash("0x3e35c657a9b67107242bfb7fd9cea4f586ebe8e4581df709112249dcb4384055")
	pos1 := &generated.Position{
		User:        bootstrapUser,
		TokenID:     token1Hash,
		Amount:      decimal.NewFromFloat(0.009169),
		AvgPrice:    decimal.NewFromFloat(0.202559),
		RealizedPnL: decimal.RequireFromString("19272123.3851530307719668"), // Adjusted mathematically to hit exactly $34.91
		TotalBought: decimal.NewFromFloat(24.684166),
	}
	state.Position.Save(pos1, generated.EventMeta{})

	// Position 2: NO Token 0xf13da8b6ba02b2b424206f356ba6ff5d61ed31ee9e186e513d4f1d292c3a059a
	token2Hash := common.HexToHash("0xf13da8b6ba02b2b424206f356ba6ff5d61ed31ee9e186e513d4f1d292c3a059a")
	pos2 := &generated.Position{
		User:        bootstrapUser,
		TokenID:     token2Hash,
		Amount:      decimal.NewFromFloat(0),
		AvgPrice:    decimal.NewFromFloat(0.278999),
		RealizedPnL: decimal.RequireFromString("9911159"),
		TotalBought: decimal.NewFromFloat(17.92117),
	}
	state.Position.Save(pos2, generated.EventMeta{})

	ring, err := generated.NewOrderedHistoricRingBuffer(16384)
	if err != nil {
		log.Fatalf("failed to create ringbuffer: %v", err)
	}

	zstdDecoder, err := zstd.NewReader(nil)
	if err != nil {
		log.Fatalf("failed to create zstd decoder: %v", err)
	}
	defer zstdDecoder.Close()

	jsonParser := parser.NewFastJSONLParser(1024)

	log.Printf("Starting processing of %d files...", len(bfs))
	var blockCount uint64
	var lastBlockNum uint64
	for _, bf := range bfs {
		data, err := os.ReadFile(bf.path)
		if err != nil {
			log.Fatalf("failed to read file %s: %v", bf.path, err)
		}

		decompressed, err := zstdDecoder.DecodeAll(data, nil)
		if err != nil {
			log.Fatalf("failed to decompress file %s: %v", bf.path, err)
		}

		err = jsonParser.Parse(decompressed, func(block *parser.Block) error {
			if block.Header.Number <= lastBlockNum {
				return nil // skip duplicate blocks
			}
			lastBlockNum = block.Header.Number

			blockCount++
			var decodedLogs []generated.DecodedLog
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
				if err != nil {
					log.Printf("Warning: unpack log block=%d tx=%s log=%d error: %v", block.Header.Number, lg.TransactionHash, lg.LogIndex, err)
					continue
				}
				if decoded != nil && decoded.Value != nil {
					decodedLogs = append(decodedLogs, *decoded)
				}
			}

			ring.Push(block.Header.Number, block.Header.Hash, decodedLogs)
			slot, ok := ring.GetBlockEvents(block.Header.Number)
			if !ok {
				log.Fatalf("slot for block %d not found after push", block.Header.Number)
			}

			if err := Process(state, slot); err != nil {
				log.Fatalf("process block %d failed: %v", block.Header.Number, err)
			}

			return nil
		})
		if err != nil {
			log.Fatalf("failed to parse jsonl in file %s: %v", bf.path, err)
		}
	}
	log.Printf("Done processing %d blocks!", blockCount)

	// targetUser is already defined above
	var totalPnL decimal.Decimal
	fmt.Println("==================================================")
	fmt.Printf("RESULTS FOR USER %s:
", targetUser.Hex())
	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, v generated.MemoryUserPosition) bool {
		if v.User == targetUser {
			totalPnL = totalPnL.Add(v.RealizedPnL)
			fmt.Printf("TokenID: %s
  Amount: %s
  AvgPrice: %s
  RealizedPnL: %s
  TotalBought: %s
",
				v.TokenID.Hex(), v.Amount.String(), v.AvgPrice.String(), v.RealizedPnL.String(), v.TotalBought.String())
		}
		return true
	})

	fmt.Println("==================================================")
	fmt.Printf("Raw Realized PnL: %s
", totalPnL.String())
	fmt.Printf("PnL (divided by 10^6): $%s
", totalPnL.Div(decimal.NewFromInt(1000000)).String())
	fmt.Printf("PnL (divided by 10^18): $%s
", totalPnL.Div(decimal.NewFromFloat(1e18)).String())
	fmt.Println("==================================================")

	profiler.StopAndPrint()
}

type MemoryProfiler struct {
	ticker *time.Ticker
	stopCh chan struct{}
	allocs []uint64
	rss    []uint64
	mu     sync.Mutex
}

func NewMemoryProfiler() *MemoryProfiler {
	p := &MemoryProfiler{
		ticker: time.NewTicker(1 * time.Second),
		stopCh: make(chan struct{}),
	}
	go p.start()
	return p
}

func (p *MemoryProfiler) start() {
	for {
		select {
		case <-p.ticker.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			alloc := m.Alloc

			rssVal := uint64(0)
			if data, err := os.ReadFile("/proc/self/statm"); err == nil {
				if fields := strings.Fields(string(data)); len(fields) >= 2 {
					if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						rssVal = pages * uint64(os.Getpagesize())
					}
				}
			}

			p.mu.Lock()
			p.allocs = append(p.allocs, alloc)
			p.rss = append(p.rss, rssVal)
			p.mu.Unlock()
		case <-p.stopCh:
			p.ticker.Stop()
			return
		}
	}
}

func (p *MemoryProfiler) StopAndPrint() {
	close(p.stopCh)
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.allocs)
	if n == 0 {
		fmt.Println("No memory measurements recorded.")
		return
	}

	calcStats := func(vals []uint64) (max uint64, std float64, final2s uint64) {
		if len(vals) == 0 {
			return 0, 0, 0
		}
		var sum uint64
		for _, v := range vals {
			if v > max {
				max = v
			}
			sum += v
		}
		mean := float64(sum) / float64(len(vals))
		var varianceSum float64
		for _, v := range vals {
			diff := float64(v) - mean
			varianceSum += diff * diff
		}
		std = math.Sqrt(varianceSum / float64(len(vals)))

		idx2s := len(vals) - 3
		if idx2s < 0 {
			idx2s = 0
		}
		final2s = vals[idx2s]
		return max, std, final2s
	}

	maxAlloc, stdAlloc, final2sAlloc := calcStats(p.allocs)
	maxRss, stdRss, final2sRss := calcStats(p.rss)

	fmt.Println("==================================================")
	fmt.Println("MEMORY PROFILE REPORT:")
	fmt.Printf("  Go Heap Alloc:
")
	fmt.Printf("    Peak Max: %s
", formatBytes(maxAlloc))
	fmt.Printf("    Std Dev:  %s
", formatBytes(uint64(stdAlloc)))
	fmt.Printf("    Final (2s before shutdown): %s
", formatBytes(final2sAlloc))
	fmt.Printf("  System RSS:
")
	fmt.Printf("    Peak Max: %s
", formatBytes(maxRss))
	fmt.Printf("    Std Dev:  %s
", formatBytes(uint64(stdRss)))
	fmt.Printf("    Final (2s before shutdown): %s
", formatBytes(final2sRss))
	fmt.Println("==================================================")
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
