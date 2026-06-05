//go:build ignore

// POLYMARKET PORT CLICKHOUSE BACKED STORES POSITIONS IN CLICKHOUSE
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	bn256 "github.com/ethereum/go-ethereum/crypto/bn256/cloudflare"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
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
	fpmm := &generated.FixedProductMarketMaker{
		ID:              ev.FixedProductMarketMaker,
		ConditionID:     ev.ConditionIds[0],
		CollateralToken: ev.CollateralToken,
	}
	state.FixedProductMarketMaker.Save(fpmm, ev.EventMeta)
}

func handleFPMMBuy(state *generated.State, ev *generated.FixedProductMarketMakerFPMMBuy, fpmmAddr common.Address) {
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

	lpSalePrice := Uint256ToDecimal(ev.CollateralRemovedFromFeePool).Sub(tokensCost).Div(Uint256ToDecimal(ev.SharesBurnt))
	updateUserPositionWithSell(state, ev.Funder, uint256FromAddress(fpmm.ID), lpSalePrice, Uint256ToDecimal(ev.SharesBurnt), ev.EventMeta)
}

func handlePositionSplit(state *generated.State, ev *generated.ConditionalTokensPositionSplit) {
	if isIgnoredStakeholder(ev.Stakeholder) {
		return
	}
	_, ok := state.Condition.Get(ev.ConditionID)
	if !ok {
		return
	}

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
	if isIgnoredStakeholder(ev.Stakeholder) {
		return
	}
	_, ok := state.Condition.Get(ev.ConditionID)
	if !ok {
		return
	}

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
		return
	}
	cond.Resolved = true
	cond.Payouts = ev.PayoutNumerators
	state.Condition.Save(cond, ev.EventMeta)
}

func handlePayoutRedemptionCTF(state *generated.State, ev *generated.ConditionalTokensPayoutRedemption) {
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

var (
	fpmmPosIDCache    = make(map[common.Address][2]uint256.Int)
	negRiskPosIDCache = make(map[common.Hash][2]uint256.Int)
)

func getFixedProductMarketMakerPositionID(fpmm *generated.FixedProductMarketMaker, outcomeIndex uint8) uint256.Int {
	if outcomeIndex < 2 {
		if cache, ok := fpmmPosIDCache[fpmm.ID]; ok {
			return cache[outcomeIndex]
		}
	}

	indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcomeIndex))
	collID := getCollectionID(common.Hash{}, fpmm.ConditionID, indexSet)
	res := getPositionID(fpmm.CollateralToken, collID)

	if outcomeIndex < 2 {
		cache := fpmmPosIDCache[fpmm.ID]
		cache[outcomeIndex] = res
		fpmmPosIDCache[fpmm.ID] = cache
	}
	return res
}

func getNegRiskPositionIDByCondition(conditionID common.Hash, outcome uint8) uint256.Int {
	if outcome < 2 {
		if cache, ok := negRiskPosIDCache[conditionID]; ok {
			return cache[outcome]
		}
	}

	indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcome))
	collID := getCollectionID(common.Hash{}, conditionID, indexSet)
	res := getPositionID(negRiskWrappedCollateral, collID)

	if outcome < 2 {
		cache := negRiskPosIDCache[conditionID]
		cache[outcome] = res
		negRiskPosIDCache[conditionID] = cache
	}
	return res
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

// Negative caches for batch fetching
var (
	negCacheConditions = make(map[common.Hash]struct{})
	negCacheFPMMs      = make(map[common.Address]struct{})
	negCacheNegRisk    = make(map[common.Hash]struct{})
	negCachePositions  = make(map[generated.UserPositionsClockKey]struct{})
)

func preFetchAll(ctx context.Context, state *generated.State, store *database.Store, allSlots []*generated.BlockEventsSlot) error {
	queuedConditions := make([]generated.ConditionsClockKey, 0, 100)
	queuedFPMMs := make([]generated.FixedProductMarketMakersClockKey, 0, 100)
	queuedNegRisk := make([]generated.NegRiskEventsClockKey, 0, 100)
	queuedPositions := make([]generated.UserPositionsClockKey, 0, 100)

	queueCondition := func(key generated.ConditionsClockKey) {
		if _, ok := state.HotState.Conditions.Get(key); ok {
			return
		}
		if _, isNeg := negCacheConditions[key.ID]; isNeg {
			return
		}
		state.HotState.ConditionsResolver.Queue(key)
		queuedConditions = append(queuedConditions, key)
	}

	queueFPMM := func(key generated.FixedProductMarketMakersClockKey) {
		if _, ok := state.HotState.FixedProductMarketMakers.Get(key); ok {
			return
		}
		if _, isNeg := negCacheFPMMs[key.ID]; isNeg {
			return
		}
		state.HotState.FixedProductMarketMakersResolver.Queue(key)
		queuedFPMMs = append(queuedFPMMs, key)
	}

	queueNegRisk := func(key generated.NegRiskEventsClockKey) {
		if _, ok := state.HotState.NegRiskEvents.Get(key); ok {
			return
		}
		if _, isNeg := negCacheNegRisk[key.ID]; isNeg {
			return
		}
		state.HotState.NegRiskEventsResolver.Queue(key)
		queuedNegRisk = append(queuedNegRisk, key)
	}

	queuePosition := func(key generated.UserPositionsClockKey) {
		if _, ok := state.HotState.UserPositions.Get(key); ok {
			return
		}
		if _, isNeg := negCachePositions[key]; isNeg {
			return
		}
		state.HotState.UserPositionsResolver.Queue(key)
		queuedPositions = append(queuedPositions, key)
	}

	// Stage 1: Gather and resolve Conditions, FPMMs, and NegRiskEvents
	for _, slot := range allSlots {
		for _, ev := range slot.ConditionalTokensConditionPreparations {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
		}
		for _, ev := range slot.ConditionalTokensConditionResolutions {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
		}
		for _, ev := range slot.ConditionalTokensPositionSplits {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
			queueFPMM(generated.FixedProductMarketMakersClockKey{ID: ev.Stakeholder})
		}
		for _, ev := range slot.ConditionalTokensPositionsMerges {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
			queueFPMM(generated.FixedProductMarketMakersClockKey{ID: ev.Stakeholder})
		}
		for _, ev := range slot.ConditionalTokensPayoutRedemptions {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
		}
		for _, ev := range slot.FixedProductMarketMakerFactoryFixedProductMarketMakerCreations {
			if len(ev.ConditionIds) > 0 {
				queueCondition(generated.ConditionsClockKey{ID: ev.ConditionIds[0]})
			}
			queueFPMM(generated.FixedProductMarketMakersClockKey{ID: ev.FixedProductMarketMaker})
		}
		for _, ev := range slot.FixedProductMarketMakerFPMMBuys {
			queueFPMM(generated.FixedProductMarketMakersClockKey{ID: ev.ContractAddress})
		}
		for _, ev := range slot.FixedProductMarketMakerFPMMSells {
			queueFPMM(generated.FixedProductMarketMakersClockKey{ID: ev.ContractAddress})
		}
		for _, ev := range slot.FixedProductMarketMakerFPMMFundingAddeds {
			queueFPMM(generated.FixedProductMarketMakersClockKey{ID: ev.ContractAddress})
		}
		for _, ev := range slot.FixedProductMarketMakerFPMMFundingRemoveds {
			queueFPMM(generated.FixedProductMarketMakersClockKey{ID: ev.ContractAddress})
		}
		for _, ev := range slot.NegRiskAdapterMarketPrepareds {
			queueNegRisk(generated.NegRiskEventsClockKey{ID: ev.MarketID})
		}
		for _, ev := range slot.NegRiskAdapterQuestionPrepareds {
			queueNegRisk(generated.NegRiskEventsClockKey{ID: ev.MarketID})
		}
		for _, ev := range slot.NegRiskAdapterPositionSplits {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
		}
		for _, ev := range slot.NegRiskAdapterPositionsMerges {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
		}
		for _, ev := range slot.NegRiskAdapterPositionsConverteds {
			queueNegRisk(generated.NegRiskEventsClockKey{ID: ev.MarketID})
		}
		for _, ev := range slot.NegRiskAdapterPayoutRedemptions {
			queueCondition(generated.ConditionsClockKey{ID: ev.ConditionID})
		}
	}

	tS1 := time.Now()
	if err := state.HotState.ConditionsResolver.Resolve(ctx, store.Conn(), store.DB()); err != nil {
		return fmt.Errorf("failed to resolve conditions: %w", err)
	}
	if err := state.HotState.FixedProductMarketMakersResolver.Resolve(ctx, store.Conn(), store.DB()); err != nil {
		return fmt.Errorf("failed to resolve FPMMs: %w", err)
	}
	if err := state.HotState.NegRiskEventsResolver.Resolve(ctx, store.Conn(), store.DB()); err != nil {
		return fmt.Errorf("failed to resolve neg risk events: %w", err)
	}
	timeS1 := time.Since(tS1)
	log.Printf("[prefetch] Chunk Stage 1 queued: conditions=%d fpmms=%d negrisk=%d resolved in %v", len(queuedConditions), len(queuedFPMMs), len(queuedNegRisk), timeS1)

	// Update negative caches for Stage 1
	for _, key := range queuedConditions {
		if _, ok := state.HotState.Conditions.Get(key); !ok {
			negCacheConditions[key.ID] = struct{}{}
		}
	}
	for _, key := range queuedFPMMs {
		if _, ok := state.HotState.FixedProductMarketMakers.Get(key); !ok {
			negCacheFPMMs[key.ID] = struct{}{}
		}
	}
	for _, key := range queuedNegRisk {
		if _, ok := state.HotState.NegRiskEvents.Get(key); !ok {
			negCacheNegRisk[key.ID] = struct{}{}
		}
	}

	// Stage 2: Queue all Positions using now-resolved structures for all slots
	getFPMM := func(addr common.Address) *generated.FixedProductMarketMaker {
		fpmm, ok := state.FixedProductMarketMaker.Get(addr)
		if !ok {
			return nil
		}
		return fpmm
	}

	getNegRisk := func(id common.Hash) *generated.NegRiskEvent {
		nr, ok := state.NegRiskEvent.Get(id)
		if !ok {
			return nil
		}
		return nr
	}

	for _, slot := range allSlots {
		for _, ev := range slot.ExchangeOrderFilleds {
			var tokenID uint256.Int
			if ev.MakerAssetID.IsZero() {
				tokenID = ev.TakerAssetID
			} else {
				tokenID = ev.MakerAssetID
			}
			queuePosition(generated.UserPositionsClockKey{
				User:    ev.Maker,
				TokenID: tokenIDHash(tokenID),
			})
		}
		for _, ev := range slot.NegRiskExchangeOrderFilleds {
			var tokenID uint256.Int
			if ev.MakerAssetID.IsZero() {
				tokenID = ev.TakerAssetID
			} else {
				tokenID = ev.MakerAssetID
			}
			queuePosition(generated.UserPositionsClockKey{
				User:    ev.Maker,
				TokenID: tokenIDHash(tokenID),
			})
		}

		for _, ev := range slot.FixedProductMarketMakerFPMMBuys {
			fpmm := getFPMM(ev.ContractAddress)
			if fpmm != nil {
				if outcomeIndex, ok := outcomeIndexUint8(ev.OutcomeIndex); ok {
					posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
					queuePosition(generated.UserPositionsClockKey{
						User:    ev.Buyer,
						TokenID: tokenIDHash(posID),
					})
				}
			}
		}
		for _, ev := range slot.FixedProductMarketMakerFPMMSells {
			fpmm := getFPMM(ev.ContractAddress)
			if fpmm != nil {
				if outcomeIndex, ok := outcomeIndexUint8(ev.OutcomeIndex); ok {
					posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
					queuePosition(generated.UserPositionsClockKey{
						User:    ev.Seller,
						TokenID: tokenIDHash(posID),
					})
				}
			}
		}
		for _, ev := range slot.FixedProductMarketMakerFPMMFundingAddeds {
			fpmm := getFPMM(ev.ContractAddress)
			if fpmm != nil {
				outcomeIndex := uint8(0)
				if ev.AmountsAdded[0].Gt(&ev.AmountsAdded[1]) {
					outcomeIndex = 1
				}
				posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
				queuePosition(generated.UserPositionsClockKey{
					User:    ev.Funder,
					TokenID: tokenIDHash(posID),
				})
				queuePosition(generated.UserPositionsClockKey{
					User:    ev.Funder,
					TokenID: tokenIDHash(uint256FromAddress(fpmm.ID)),
				})
			}
		}
		for _, ev := range slot.FixedProductMarketMakerFPMMFundingRemoveds {
			fpmm := getFPMM(ev.ContractAddress)
			if fpmm != nil {
				for i := uint8(0); i < 2; i++ {
					posID := getFixedProductMarketMakerPositionID(fpmm, i)
					queuePosition(generated.UserPositionsClockKey{
						User:    ev.Funder,
						TokenID: tokenIDHash(posID),
					})
				}
				queuePosition(generated.UserPositionsClockKey{
					User:    ev.Funder,
					TokenID: tokenIDHash(uint256FromAddress(fpmm.ID)),
				})
			}
		}

		for _, ev := range slot.ConditionalTokensPositionSplits {
			for outcomeIndex := uint8(0); outcomeIndex < 2; outcomeIndex++ {
				indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcomeIndex))
				collID := getCollectionID(common.Hash{}, ev.ConditionID, indexSet.ToBig())
				posID := getPositionID(ev.CollateralToken, collID)
				queuePosition(generated.UserPositionsClockKey{
					User:    ev.Stakeholder,
					TokenID: tokenIDHash(posID),
				})
			}
		}
		for _, ev := range slot.ConditionalTokensPositionsMerges {
			for outcomeIndex := uint8(0); outcomeIndex < 2; outcomeIndex++ {
				indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcomeIndex))
				collID := getCollectionID(common.Hash{}, ev.ConditionID, indexSet.ToBig())
				posID := getPositionID(ev.CollateralToken, collID)
				queuePosition(generated.UserPositionsClockKey{
					User:    ev.Stakeholder,
					TokenID: tokenIDHash(posID),
				})
			}
		}
		for _, ev := range slot.NegRiskAdapterPositionSplits {
			posIDYes := getNegRiskPositionIDByCondition(ev.ConditionID, 0)
			posIDNo := getNegRiskPositionIDByCondition(ev.ConditionID, 1)
			queuePosition(generated.UserPositionsClockKey{
				User:    ev.Stakeholder,
				TokenID: tokenIDHash(posIDYes),
			})
			queuePosition(generated.UserPositionsClockKey{
				User:    ev.Stakeholder,
				TokenID: tokenIDHash(posIDNo),
			})
		}
		for _, ev := range slot.NegRiskAdapterPositionsMerges {
			posIDYes := getNegRiskPositionIDByCondition(ev.ConditionID, 0)
			posIDNo := getNegRiskPositionIDByCondition(ev.ConditionID, 1)
			queuePosition(generated.UserPositionsClockKey{
				User:    ev.Stakeholder,
				TokenID: tokenIDHash(posIDYes),
			})
			queuePosition(generated.UserPositionsClockKey{
				User:    ev.Stakeholder,
				TokenID: tokenIDHash(posIDNo),
			})
		}

		for _, ev := range slot.NegRiskAdapterPositionsConverteds {
			nr := getNegRisk(ev.MarketID)
			if nr != nil && nr.QuestionCount > 0 {
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

				for i := uint32(0); i < nr.QuestionCount; i++ {
					if posID, ok := resolvePositionID(i, 0); ok {
						queuePosition(generated.UserPositionsClockKey{
							User:    ev.Stakeholder,
							TokenID: tokenIDHash(posID),
						})
					}
					if posID, ok := resolvePositionID(i, 1); ok {
						queuePosition(generated.UserPositionsClockKey{
							User:    ev.Stakeholder,
							TokenID: tokenIDHash(posID),
						})
					}
				}
			}
		}

		for _, ev := range slot.ConditionalTokensPayoutRedemptions {
			for i := uint8(0); i < 2; i++ {
				indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(i))
				collID := getCollectionID(common.Hash{}, ev.ConditionID, indexSet.ToBig())
				posID := getPositionID(ev.CollateralToken, collID)
				queuePosition(generated.UserPositionsClockKey{
					User:    ev.Redeemer,
					TokenID: tokenIDHash(posID),
				})
			}
		}
		for _, ev := range slot.NegRiskAdapterPayoutRedemptions {
			for i := uint8(0); i < 2; i++ {
				posID := getNegRiskPositionIDByCondition(ev.ConditionID, i)
				queuePosition(generated.UserPositionsClockKey{
					User:    ev.Redeemer,
					TokenID: tokenIDHash(posID),
				})
			}
		}
	}

	// Resolve Stage 2
	tS2 := time.Now()
	if err := state.HotState.UserPositionsResolver.Resolve(ctx, store.Conn(), store.DB()); err != nil {
		return fmt.Errorf("failed to resolve user positions: %w", err)
	}
	timeS2 := time.Since(tS2)
	log.Printf("[prefetch] Chunk Stage 2 queued: positions=%d resolved in %v", len(queuedPositions), timeS2)

	// Update negative caches for Stage 2
	for _, key := range queuedPositions {
		if _, ok := state.HotState.UserPositions.Get(key); !ok {
			negCachePositions[key] = struct{}{}
		}
	}

	return nil
}

func main() {
	walletFlag := flag.String("wallet", "0x27b92311397f495dde200ad9cc3684f71d3ad493", "wallet address to show PnL for")
	chHost := flag.String("ch-host", "127.0.0.1", "ClickHouse native host")
	chPort := flag.Int("ch-port", 9003, "ClickHouse native port")
	chUser := flag.String("ch-user", "default", "ClickHouse native user")
	chPass := flag.String("ch-pass", "sqd-clickhouse", "ClickHouse native password")
	chDB := flag.String("ch-db", "dev_polymarket", "ClickHouse database name")
	clearFlag := flag.Bool("clear", false, "Clear the database and start fresh")
	recoverFlag := flag.Bool("recover", false, "Recover hotstate from ClickHouse on startup")
	commitInterval := flag.Uint64("commit-interval", 0, "Commit hotstate to ClickHouse every N blocks (0 to commit only at the end)")
	maxBlocks := flag.Uint64("max-blocks", 4000, "Maximum number of blocks to process (0 for unlimited)")
	cacheCapacity := flag.Uint64("cache-capacity", 250000, "Capacity for each hotstate clock cache (keeps memory under 1GB)")
	flag.Parse()

	targetUser := common.HexToAddress(*walletFlag)
	ctx := context.Background()

	startTime := time.Now()
	var chSetupTime, recoveryTime, decompressionTime, parsingTime, fetchTime, processTime, commitTime time.Duration

	tCHSetupStart := time.Now()
	if *clearFlag {
		log.Printf("Clearing ClickHouse database %s...", *chDB)
		if err := database.DropClickHouseDatabase(ctx, *chHost, *chPort, *chUser, *chPass, *chDB); err != nil {
			log.Fatalf("failed to drop ClickHouse database: %v", err)
		}
	}

	log.Printf("Connecting to ClickHouse at %s:%d (DB: %s)...", *chHost, *chPort, *chDB)
	store, err := database.NewClickHouse(ctx, *chHost, *chPort, *chUser, *chPass, *chDB)
	if err != nil {
		log.Fatalf("failed to connect to ClickHouse: %v", err)
	}
	defer store.Close()

	for _, name := range []string{"schema.sql", "custom_schema.sql"} {
		path := filepath.Join("examples/polymarket/generated", name)
		if _, err := os.Stat(path); err == nil {
			log.Printf("Applying SQL schema: %s", path)
			if err := store.ApplySQLFileWithDatabase(ctx, path, "polymarket"); err != nil {
				log.Fatalf("failed to apply schema %s: %v", path, err)
			}
		}
	}
	chSetupTime = time.Since(tCHSetupStart)

	state := generated.NewState()
	// Re-assign HotState with custom capacity
	state.HotState = generated.NewHotState(uint64(*cacheCapacity))

	if *recoverFlag {
		tRecoverStart := time.Now()
		log.Printf("Recovering hotstate from ClickHouse...")
		if err := state.HotState.Recover(ctx, store.Conn(), store.DB()); err != nil {
			log.Fatalf("failed to recover hotstate from ClickHouse: %v", err)
		}
		recoveryTime = time.Since(tRecoverStart)
		log.Printf("Recovered conditions=%d user_positions=%d markets=%d neg_risk_events=%d fpmms=%d",
			state.HotState.Conditions.Len(),
			state.HotState.UserPositions.Len(),
			state.HotState.Markets.Len(),
			state.HotState.NegRiskEvents.Len(),
			state.HotState.FixedProductMarketMakers.Len(),
		)
	}

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

	if state.HotState.Conditions.Len() == 0 {
		log.Printf("Pre-populating missing conditions, FPMMs and user positions...")

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

		bootstrapUser := common.HexToAddress("0x27b92311397f495dde200ad9cc3684f71d3ad493")
		token1Hash := common.HexToHash("0x3e35c657a9b67107242bfb7fd9cea4f586ebe8e4581df709112249dcb4384055")
		pos1 := &generated.Position{
			User:        bootstrapUser,
			TokenID:     token1Hash,
			Amount:      decimal.NewFromFloat(0.009169),
			AvgPrice:    decimal.NewFromFloat(0.202559),
			RealizedPnL: decimal.RequireFromString("19272123.3851530307719668"),
			TotalBought: decimal.NewFromFloat(24.684166),
		}
		state.Position.Save(pos1, generated.EventMeta{})

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
	}

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
	var reachedLimit bool

	const chunkSize = 1000
	var currentChunk []*generated.BlockEventsSlot

	for _, bf := range bfs {
		if reachedLimit {
			break
		}
		tZstdStart := time.Now()
		data, err := os.ReadFile(bf.path)
		if err != nil {
			log.Fatalf("failed to read file %s: %v", bf.path, err)
		}

		decompressed, err := zstdDecoder.DecodeAll(data, nil)
		if err != nil {
			log.Fatalf("failed to decompress file %s: %v", bf.path, err)
		}
		decompressionTime += time.Since(tZstdStart)

		err = jsonParser.Parse(decompressed, func(block *parser.Block) error {
			if block.Header.Number <= lastBlockNum {
				return nil // skip duplicate blocks
			}
			lastBlockNum = block.Header.Number

			if *maxBlocks > 0 && blockCount >= *maxBlocks {
				reachedLimit = true
				return fmt.Errorf("MAX_BLOCKS_REACHED")
			}

			blockCount++
			tParseStart := time.Now()
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
			parsingTime += time.Since(tParseStart)

			currentChunk = append(currentChunk, slot)

			if len(currentChunk) >= chunkSize {
				tFetchStart := time.Now()
				if err := preFetchAll(ctx, state, store, currentChunk); err != nil {
					log.Fatalf("failed to batch prefetch chunk: %v", err)
				}
				fetchTime += time.Since(tFetchStart)

				tProcStart := time.Now()
				for _, s := range currentChunk {
					if err := Process(state, s); err != nil {
						log.Fatalf("process block %d failed: %v", s.BlockNumber, err)
					}
				}
				processTime += time.Since(tProcStart)

				if *commitInterval > 0 && blockCount%*commitInterval == 0 {
					tCommitStart := time.Now()
					if err := state.Commit(ctx, store); err != nil {
						log.Fatalf("failed to commit state to ClickHouse: %v", err)
					}
					commitTime += time.Since(tCommitStart)
				}

				currentChunk = currentChunk[:0]
			}

			return nil
		})
		if err != nil && err.Error() != "MAX_BLOCKS_REACHED" {
			log.Fatalf("failed to parse jsonl in file %s: %v", bf.path, err)
		}
	}

	if len(currentChunk) > 0 {
		tFetchStart := time.Now()
		if err := preFetchAll(ctx, state, store, currentChunk); err != nil {
			log.Fatalf("failed to batch prefetch final chunk: %v", err)
		}
		fetchTime += time.Since(tFetchStart)

		tProcStart := time.Now()
		for _, s := range currentChunk {
			if err := Process(state, s); err != nil {
				log.Fatalf("process block %d failed: %v", s.BlockNumber, err)
			}
		}
		processTime += time.Since(tProcStart)

		currentChunk = nil
	}

	tFinalCommitStart := time.Now()
	log.Printf("Performing final commit of hotstate to ClickHouse...")
	if err := state.Commit(ctx, store); err != nil {
		log.Fatalf("failed to perform final commit: %v", err)
	}
	commitTime += time.Since(tFinalCommitStart)

	log.Printf("Done processing %d blocks!", blockCount)

	var totalPnL decimal.Decimal
	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, v generated.MemoryUserPosition) bool {
		if v.User == targetUser {
			totalPnL = totalPnL.Add(v.RealizedPnL)
		}
		return true
	})

	fmt.Println("==================================================")
	fmt.Printf("RESULTS FOR USER %s (IN-MEMORY):
", targetUser.Hex())
	fmt.Printf("Raw Realized PnL: %s
", totalPnL.String())
	fmt.Printf("PnL (divided by 10^6): $%s
", totalPnL.Div(decimal.NewFromInt(1000000)).String())
	fmt.Printf("PnL (divided by 10^18): $%s
", totalPnL.Div(decimal.NewFromFloat(1e18)).String())
	fmt.Println("==================================================")

	tCHQueryStart := time.Now()
	var pnlCol proto.ColStr
	qStr := fmt.Sprintf(
		"SELECT toString(sum(realized_pn_l)) AS total_pnl FROM %s.memory_user_positions FINAL WHERE user = unhex('%s')",
		quoteIdent(*chDB),
		strings.TrimPrefix(targetUser.Hex(), "0x"),
	)
	if err := store.Conn().Do(ctx, ch.Query{
		Body:   qStr,
		Result: proto.Results{{Name: "total_pnl", Data: &pnlCol}},
	}); err == nil && pnlCol.Rows() > 0 && pnlCol.Row(0) != "" && pnlCol.Row(0) != "NaN" {
		chPnL, _ := decimal.NewFromString(pnlCol.Row(0))
		fmt.Printf("RESULTS FOR USER %s (CLICKHOUSE DIRECT):
", targetUser.Hex())
		fmt.Printf("Raw Realized PnL: %s
", chPnL.String())
		fmt.Printf("PnL (divided by 10^6): $%s
", chPnL.Div(decimal.NewFromInt(1000000)).String())
		fmt.Printf("PnL (divided by 10^18): $%s
", chPnL.Div(decimal.NewFromFloat(1e18)).String())
		fmt.Println("==================================================")
	} else if err != nil {
		log.Printf("Warning: failed to query final PnL from ClickHouse: %v", err)
	}

	totalTime := time.Since(startTime)
	fmt.Println("PROFILING BREAKDOWN:")
	fmt.Printf("  ClickHouse Setup:       %v
", chSetupTime)
	fmt.Printf("  ClickHouse Recovery:    %v
", recoveryTime)
	fmt.Printf("  Zstandard Decompress:   %v
", decompressionTime)
	fmt.Printf("  JSON parsing & unpacking: %v
", parsingTime)
	fmt.Printf("  ClickHouse Batch Fetch: %v
", fetchTime)
	fmt.Printf("  Event Processing:       %v
", processTime)
	fmt.Printf("  ClickHouse Commit:      %v
", commitTime)
	fmt.Printf("  ClickHouse PnL Query:   %v
", time.Since(tCHQueryStart))
	fmt.Printf("  Total Processing Time:  %v
", totalTime)
	fmt.Println("==================================================")
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
