package polymarket

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	bn256 "github.com/ethereum/go-ethereum/crypto/bn256/cloudflare"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/cli"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// pow10[0]=1, pow10[1]=10, pow10[2]=100, ... up to 10^38
var pow10 [39]*big.Int

func init() {
	pow10[0] = big.NewInt(1)
	for i := 1; i < len(pow10); i++ {
		pow10[i] = new(big.Int).Mul(pow10[i-1], big.NewInt(10))
	}
}

func toDecimal(v protomath.Decimal256) decimal.Decimal {
	return decimal.NewFromBigInt(v.ScaledBig(), -18)
}

func fromDecimal(v decimal.Decimal) protomath.Decimal256 {
	if v.IsZero() {
		return protomath.Decimal256{}
	}
	if v.Exponent() < -18 {
		v = v.Round(18)
	}
	coeff := v.Coefficient()
	exp := int(v.Exponent())

	// Fast path: shopspring Decimal is already at scale -18 (most common — roundtrip from toDecimal).
	// In that case the coefficient IS the scaled value and needs no multiplication.
	if exp == -18 {
		res, ok := protomath.FromDecimal256ScaledBigInt(coeff)
		if ok {
			return res
		}
		// Overflow — fall through to string fallback below.
	} else if shift := exp + 18; shift > 0 {
		// Need to multiply coefficient by 10^shift.
		if shift < len(pow10) {
			scaled := new(big.Int).Mul(coeff, pow10[shift])
			res, ok := protomath.FromDecimal256ScaledBigInt(scaled)
			if ok {
				return res
			}
		} else {
			scaled := new(big.Int).Mul(coeff, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(shift)), nil))
			res, ok := protomath.FromDecimal256ScaledBigInt(scaled)
			if ok {
				return res
			}
		}
	}
	// Fallback: value already rounded to 18dp above, so ParseDecimal256 won't reject it.
	res, _ := protomath.ParseDecimal256(v.StringFixed(18), protomath.Decimal256Scale18)
	return res
}

func init() {
	generated.CustomProcessFn = Process
	generated.CustomProcessProtoFn = ProcessProto
	cli.RegisterProcessorV2(generated.ProjectName, func(protoMode bool) (ingestion.Processor, error) {
		return generated.NewProcessor(protoMode)
	})
}

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
	// CLOCK (second-chance) caches for the pure ID derivations. Each entry is a
	// memoized result of an expensive keccak / elliptic-curve computation; the
	// CLOCK eviction keeps the hot working set across the cap instead of flushing
	// the whole table (see clockcache.go).
	collectionCache = newClockCache[collectionKey, common.Hash](maxCryptoCacheLen, 64, hashCollectionKey)
	positionCache   = newClockCache[positionKey, uint256.Int](maxCryptoCacheLen, 64, hashPositionKey)
	// negRiskPosCache memoizes the neg-risk position ID keyed by (conditionID,
	// outcome). Its miss path runs computeCollectionId, whose Legendre-symbol
	// loop is a 256-bit modular exponentiation — the single most expensive
	// primitive in the processor — so this is the highest-value cache of the three.
	negRiskPosCache = newClockCache[negRiskKey, uint256.Int](maxCryptoCacheLen, 64, hashNegRiskKey)
	lastPnLSummaryBlock uint64
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

type negRiskKey struct {
	condition common.Hash
	outcome   uint8
}

// Shard hashes read 8 bytes from a field that is itself a keccak output, so the
// low bits are already uniformly distributed across shards.
func hashCollectionKey(k collectionKey) uint64 {
	return binary.LittleEndian.Uint64(k.condition[:8]) ^ binary.LittleEndian.Uint64(k.index[:8])
}

func hashPositionKey(k positionKey) uint64 {
	return binary.LittleEndian.Uint64(k.collection[:8])
}

func hashNegRiskKey(k negRiskKey) uint64 {
	return binary.LittleEndian.Uint64(k.condition[:8]) + uint64(k.outcome)
}

func ensureConditionsLoaded(state *generated.State, conditionIDs []common.Hash) {
	for _, condID := range conditionIDs {
		if _, ok := state.Condition.Get(condID); !ok {
			if prep, ok := state.ConditionPreparation.Get(condID); ok {
				cond := &generated.Condition{
					ID:               prep.ConditionID,
					Oracle:           prep.Oracle,
					QuestionID:       prep.QuestionID,
					OutcomeSlotCount: uint8(prep.OutcomeSlotCount.Uint64()),
					Resolved:         false,
				}
				state.Condition.Save(cond, generated.EventMeta{
					BlockNumber:      prep.BlockNumber,
					BlockTimestamp:   prep.BlockTimestamp,
					TransactionIndex: prep.TransactionIndex,
					LogIndex:         prep.LogIndex,
				})
			}
		}
	}
}

// fpmmResolveNanos / fpmmResolveRoundTrips are diagnostic counters (single-goroutine processor).
var fpmmResolveNanos int64
var fpmmResolveRoundTrips int64

func ensureFPMMMarketsLoaded(state *generated.State, fpmmAddrs []common.Address) {
	if state.Store == nil || state.Store.Conn() == nil {
		return
	}
	queued := 0
	for _, fpmmAddr := range fpmmAddrs {
		// Check if already loaded in hot state (or tombstoned from a prior miss).
		key := generated.FixedProductMarketMakersClockKey{ID: fpmmAddr}
		if _, ok := state.HotState.FixedProductMarketMakers.Get(key); ok {
			continue
		}
		state.HotState.FixedProductMarketMakersResolver.Queue(key)
		queued++
	}
	// One batched round-trip per block instead of one per missing address: the
	// resolver issues WHERE id IN (...) and tombstones not-found keys, so the
	// queued set and the consumed results are identical to the per-address path.
	if queued > 0 {
		t0 := time.Now()
		fpmmResolveRoundTrips++
		if err := state.HotState.FixedProductMarketMakersResolver.Resolve(context.Background(), state.Store.Conn(), state.Store.DB()); err != nil {
			// Some markets might not exist yet; tombstone handles the negative case.
		}
		fpmmResolveNanos += time.Since(t0).Nanoseconds()
	}
}

// Process is the single entry point for custom business logic in Parsed (V1) Mode.
func Process(state *generated.State, block *generated.ParsedBlock) error {
	var conditionIDs []common.Hash
	for _, ev := range block.ConditionalTokensPositionSplits {
		conditionIDs = append(conditionIDs, ev.ConditionID)
	}
	for _, ev := range block.ConditionalTokensPositionsMerges {
		conditionIDs = append(conditionIDs, ev.ConditionID)
	}
	for _, ev := range block.ConditionalTokensPayoutRedemptions {
		conditionIDs = append(conditionIDs, ev.ConditionID)
	}
	for _, ev := range block.NegRiskAdapterPositionSplits {
		conditionIDs = append(conditionIDs, ev.ConditionID)
	}
	for _, ev := range block.NegRiskAdapterPositionsMerges {
		conditionIDs = append(conditionIDs, ev.ConditionID)
	}
	for _, ev := range block.NegRiskAdapterPayoutRedemptions {
		conditionIDs = append(conditionIDs, ev.ConditionID)
	}
	ensureConditionsLoaded(state, conditionIDs)

	var fpmmAddrs []common.Address
	for i := range block.FixedProductMarketMakerFPMMBuys {
		fpmmAddrs = append(fpmmAddrs, block.FixedProductMarketMakerFPMMBuys[i].ContractAddress)
	}
	for i := range block.FixedProductMarketMakerFPMMSells {
		fpmmAddrs = append(fpmmAddrs, block.FixedProductMarketMakerFPMMSells[i].ContractAddress)
	}
	for i := range block.FixedProductMarketMakerFPMMFundingAddeds {
		fpmmAddrs = append(fpmmAddrs, block.FixedProductMarketMakerFPMMFundingAddeds[i].ContractAddress)
	}
	for i := range block.FixedProductMarketMakerFPMMFundingRemoveds {
		fpmmAddrs = append(fpmmAddrs, block.FixedProductMarketMakerFPMMFundingRemoveds[i].ContractAddress)
	}
	ensureFPMMMarketsLoaded(state, fpmmAddrs)

	for ev := range block.EventsIter() {
		switch e := ev.(type) {
		case *generated.ConditionalTokensConditionPreparation:
			handleConditionPreparation(state, e)
		case *generated.ConditionalTokensConditionResolution:
			handleConditionResolution(state, e)
		case *generated.ConditionalTokensPositionSplit:
			handlePositionSplit(state, e)
		case *generated.ConditionalTokensPositionsMerge:
			handlePositionsMerge(state, e)
		case *generated.ConditionalTokensPayoutRedemption:
			handlePayoutRedemptionCTF(state, e)
		case *generated.ExchangeOrderFilled:
			handleOrderFilled(state, e)
		case *generated.NegRiskExchangeOrderFilled:
			handleNegRiskOrderFilled(state, e)
		case *generated.NegRiskAdapterMarketPrepared:
			handleMarketPrepared(state, e)
		case *generated.NegRiskAdapterQuestionPrepared:
			handleQuestionPrepared(state, e)
		case *generated.NegRiskAdapterPositionSplit:
			handleNegRiskPositionSplit(state, e)
		case *generated.NegRiskAdapterPositionsMerge:
			handleNegRiskPositionsMerge(state, e)
		case *generated.NegRiskAdapterPositionsConverted:
			handlePositionsConverted(state, e)
		case *generated.NegRiskAdapterPayoutRedemption:
			handlePayoutRedemptionNR(state, e)
		case *generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation:
			handleFixedProductMarketMakerCreation(state, e)
		case *generated.FixedProductMarketMakerFPMMBuy:
			handleFPMMBuy(state, e, e.ContractAddress)
		case *generated.FixedProductMarketMakerFPMMSell:
			handleFPMMSell(state, e, e.ContractAddress)
		case *generated.FixedProductMarketMakerFPMMFundingAdded:
			handleFPMMFundingAdded(state, e, e.ContractAddress)
		case *generated.FixedProductMarketMakerFPMMFundingRemoved:
			handleFPMMFundingRemoved(state, e, e.ContractAddress)
		}
	}
	return nil
}

// ProcessProto is the single entry point for custom business logic in Proto (V2) Mode.
func ProcessProto(state *generated.State, block *generated.ProtoEventBlock) error {
	if state == nil || block == nil {
		return nil
	}
	var conditionIDs []common.Hash
	block.QueryConditionalTokensPositionSplit().Map(func(ev generated.ConditionalTokensPositionSplitProtoView) {
		conditionIDs = append(conditionIDs, ev.ConditionID())
	})
	block.QueryConditionalTokensPositionsMerge().Map(func(ev generated.ConditionalTokensPositionsMergeProtoView) {
		conditionIDs = append(conditionIDs, ev.ConditionID())
	})
	block.QueryConditionalTokensPayoutRedemption().Map(func(ev generated.ConditionalTokensPayoutRedemptionProtoView) {
		conditionIDs = append(conditionIDs, ev.ConditionID())
	})
	block.QueryNegRiskAdapterPositionSplit().Map(func(ev generated.NegRiskAdapterPositionSplitProtoView) {
		conditionIDs = append(conditionIDs, ev.ConditionID())
	})
	block.QueryNegRiskAdapterPositionsMerge().Map(func(ev generated.NegRiskAdapterPositionsMergeProtoView) {
		conditionIDs = append(conditionIDs, ev.ConditionID())
	})
	block.QueryNegRiskAdapterPayoutRedemption().Map(func(ev generated.NegRiskAdapterPayoutRedemptionProtoView) {
		conditionIDs = append(conditionIDs, ev.ConditionID())
	})
	ensureConditionsLoaded(state, conditionIDs)

	// Prefetch FPMM markets referenced in buy/sell/funding events
	var fpmmAddrs []common.Address
	block.QueryFixedProductMarketMakerFPMMBuy().Map(func(ev generated.FixedProductMarketMakerFPMMBuyProtoView) {
		fpmmAddrs = append(fpmmAddrs, ev.Meta().ContractAddress)
	})
	block.QueryFixedProductMarketMakerFPMMSell().Map(func(ev generated.FixedProductMarketMakerFPMMSellProtoView) {
		fpmmAddrs = append(fpmmAddrs, ev.Meta().ContractAddress)
	})
	block.QueryFixedProductMarketMakerFPMMFundingAdded().Map(func(ev generated.FixedProductMarketMakerFPMMFundingAddedProtoView) {
		fpmmAddrs = append(fpmmAddrs, ev.Meta().ContractAddress)
	})
	block.QueryFixedProductMarketMakerFPMMFundingRemoved().Map(func(ev generated.FixedProductMarketMakerFPMMFundingRemovedProtoView) {
		fpmmAddrs = append(fpmmAddrs, ev.Meta().ContractAddress)
	})
	ensureFPMMMarketsLoaded(state, fpmmAddrs)

	var conditionPreparationIdx int
	var conditionResolutionIdx int
	var positionSplitIdx int
	var positionsMergeIdx int
	var payoutRedemptionIdx int
	var orderFilledIdx int
	var negRiskOrderFilledIdx int
	var marketPreparedIdx int
	var questionPreparedIdx int
	var negRiskPositionSplitIdx int
	var negRiskPositionsMergeIdx int
	var positionsConvertedIdx int
	var negRiskPayoutRedemptionIdx int
	var fpmmCreationIdx int
	var fpmmBuyIdx int
	var fpmmSellIdx int
	var fpmmFundingAddedIdx int
	var fpmmFundingRemovedIdx int

	var (
		evCondPrep       generated.ConditionalTokensConditionPreparation
		evCondRes        generated.ConditionalTokensConditionResolution
		evPosSplit       generated.ConditionalTokensPositionSplit
		evPosMerge       generated.ConditionalTokensPositionsMerge
		evPayoutCTF      generated.ConditionalTokensPayoutRedemption
		evMarketPrep     generated.NegRiskAdapterMarketPrepared
		evQuestionPrep   generated.NegRiskAdapterQuestionPrepared
		evNRPosSplit     generated.NegRiskAdapterPositionSplit
		evNRPosMerge     generated.NegRiskAdapterPositionsMerge
		evPosConverted   generated.NegRiskAdapterPositionsConverted
		evNRPayout       generated.NegRiskAdapterPayoutRedemption
		evFpmmCreation   generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation
		evFpmmBuy        generated.FixedProductMarketMakerFPMMBuy
		evFpmmSell       generated.FixedProductMarketMakerFPMMSell
		evFpmmFundingAdd generated.FixedProductMarketMakerFPMMFundingAdded
		evFpmmFundingRem generated.FixedProductMarketMakerFPMMFundingRemoved
	)

	for _, typ := range block.Sequence {
		switch generated.EventType(typ) {
		case generated.EventTypeConditionalTokensConditionPreparation:
			ev := block.ConditionalTokensConditionPreparationProtoAt(conditionPreparationIdx)
			conditionPreparationIdx++
			evCondPrep.EventMeta = ev.Meta()
			evCondPrep.ConditionID = ev.ConditionID()
			evCondPrep.Oracle = ev.Oracle()
			evCondPrep.QuestionID = ev.QuestionID()
			evCondPrep.OutcomeSlotCount = ev.OutcomeSlotCount()
			handleConditionPreparation(state, &evCondPrep)
		case generated.EventTypeConditionalTokensConditionResolution:
			ev := block.ConditionalTokensConditionResolutionProtoAt(conditionResolutionIdx)
			conditionResolutionIdx++
			evCondRes.EventMeta = ev.Meta()
			evCondRes.ConditionID = ev.ConditionID()
			evCondRes.Oracle = ev.Oracle()
			evCondRes.QuestionID = ev.QuestionID()
			evCondRes.PayoutDenominator = ev.PayoutDenominator()
			evCondRes.PayoutNumerators = ev.PayoutNumerators()
			handleConditionResolution(state, &evCondRes)
		case generated.EventTypeConditionalTokensPositionSplit:
			ev := block.ConditionalTokensPositionSplitProtoAt(positionSplitIdx)
			positionSplitIdx++
			evPosSplit.EventMeta = ev.Meta()
			evPosSplit.Stakeholder = ev.Stakeholder()
			evPosSplit.CollateralToken = ev.CollateralToken()
			evPosSplit.ParentCollectionID = ev.ParentCollectionID()
			evPosSplit.ConditionID = ev.ConditionID()
			evPosSplit.Partition = ev.Partition()
			evPosSplit.Amount = ev.Amount()
			handlePositionSplit(state, &evPosSplit)
		case generated.EventTypeConditionalTokensPositionsMerge:
			ev := block.ConditionalTokensPositionsMergeProtoAt(positionsMergeIdx)
			positionsMergeIdx++
			evPosMerge.EventMeta = ev.Meta()
			evPosMerge.Stakeholder = ev.Stakeholder()
			evPosMerge.CollateralToken = ev.CollateralToken()
			evPosMerge.ParentCollectionID = ev.ParentCollectionID()
			evPosMerge.ConditionID = ev.ConditionID()
			evPosMerge.Partition = ev.Partition()
			evPosMerge.Amount = ev.Amount()
			handlePositionsMerge(state, &evPosMerge)
		case generated.EventTypeConditionalTokensPayoutRedemption:
			ev := block.ConditionalTokensPayoutRedemptionProtoAt(payoutRedemptionIdx)
			payoutRedemptionIdx++
			evPayoutCTF.EventMeta = ev.Meta()
			evPayoutCTF.Redeemer = ev.Redeemer()
			evPayoutCTF.CollateralToken = ev.CollateralToken()
			evPayoutCTF.ParentCollectionID = ev.ParentCollectionID()
			evPayoutCTF.ConditionID = ev.ConditionID()
			evPayoutCTF.IndexSets = ev.IndexSets()
			evPayoutCTF.Payout = ev.Payout()
			handlePayoutRedemptionCTF(state, &evPayoutCTF)
		case generated.EventTypeExchangeOrderFilled:
			ev := block.ExchangeOrderFilledProtoAt(orderFilledIdx)
			orderFilledIdx++
			handleOrderFilledValues(state, ev.Maker(), ev.MakerAssetID(), ev.TakerAssetID(), ev.MakerAmountFilled(), ev.TakerAmountFilled(), ev.Meta())
		case generated.EventTypeNegRiskExchangeOrderFilled:
			ev := block.NegRiskExchangeOrderFilledProtoAt(negRiskOrderFilledIdx)
			negRiskOrderFilledIdx++
			handleOrderFilledValues(state, ev.Maker(), ev.MakerAssetID(), ev.TakerAssetID(), ev.MakerAmountFilled(), ev.TakerAmountFilled(), ev.Meta())
		case generated.EventTypeNegRiskAdapterMarketPrepared:
			ev := block.NegRiskAdapterMarketPreparedProtoAt(marketPreparedIdx)
			marketPreparedIdx++
			evMarketPrep.EventMeta = ev.Meta()
			evMarketPrep.MarketID = ev.MarketID()
			evMarketPrep.Creator = ev.Creator()
			evMarketPrep.FeeBips = ev.FeeBips()
			evMarketPrep.Data = ev.Data()
			handleMarketPrepared(state, &evMarketPrep)
		case generated.EventTypeNegRiskAdapterQuestionPrepared:
			ev := block.NegRiskAdapterQuestionPreparedProtoAt(questionPreparedIdx)
			questionPreparedIdx++
			evQuestionPrep.EventMeta = ev.Meta()
			evQuestionPrep.MarketID = ev.MarketID()
			evQuestionPrep.QuestionID = ev.QuestionID()
			evQuestionPrep.Index = ev.Index()
			evQuestionPrep.Data = ev.Data()
			handleQuestionPrepared(state, &evQuestionPrep)
		case generated.EventTypeNegRiskAdapterPositionSplit:
			ev := block.NegRiskAdapterPositionSplitProtoAt(negRiskPositionSplitIdx)
			negRiskPositionSplitIdx++
			evNRPosSplit.EventMeta = ev.Meta()
			evNRPosSplit.Stakeholder = ev.Stakeholder()
			evNRPosSplit.ConditionID = ev.ConditionID()
			evNRPosSplit.Amount = ev.Amount()
			handleNegRiskPositionSplit(state, &evNRPosSplit)
		case generated.EventTypeNegRiskAdapterPositionsMerge:
			ev := block.NegRiskAdapterPositionsMergeProtoAt(negRiskPositionsMergeIdx)
			negRiskPositionsMergeIdx++
			evNRPosMerge.EventMeta = ev.Meta()
			evNRPosMerge.Stakeholder = ev.Stakeholder()
			evNRPosMerge.ConditionID = ev.ConditionID()
			evNRPosMerge.Amount = ev.Amount()
			handleNegRiskPositionsMerge(state, &evNRPosMerge)
		case generated.EventTypeNegRiskAdapterPositionsConverted:
			ev := block.NegRiskAdapterPositionsConvertedProtoAt(positionsConvertedIdx)
			positionsConvertedIdx++
			evPosConverted.EventMeta = ev.Meta()
			evPosConverted.Stakeholder = ev.Stakeholder()
			evPosConverted.MarketID = ev.MarketID()
			evPosConverted.IndexSet = ev.IndexSet()
			evPosConverted.Amount = ev.Amount()
			handlePositionsConverted(state, &evPosConverted)
		case generated.EventTypeNegRiskAdapterPayoutRedemption:
			ev := block.NegRiskAdapterPayoutRedemptionProtoAt(negRiskPayoutRedemptionIdx)
			negRiskPayoutRedemptionIdx++
			evNRPayout.EventMeta = ev.Meta()
			evNRPayout.Redeemer = ev.Redeemer()
			evNRPayout.ConditionID = ev.ConditionID()
			evNRPayout.Amounts = ev.Amounts()
			evNRPayout.Payout = ev.Payout()
			handlePayoutRedemptionNR(state, &evNRPayout)
		case generated.EventTypeFixedProductMarketMakerFactoryFixedProductMarketMakerCreation:
			ev := block.FixedProductMarketMakerFactoryFixedProductMarketMakerCreationProtoAt(fpmmCreationIdx)
			fpmmCreationIdx++
			evFpmmCreation.EventMeta = ev.Meta()
			evFpmmCreation.Creator = ev.Creator()
			evFpmmCreation.FixedProductMarketMaker = ev.FixedProductMarketMaker()
			evFpmmCreation.ConditionalTokens = ev.ConditionalTokens()
			evFpmmCreation.CollateralToken = ev.CollateralToken()
			evFpmmCreation.ConditionIds = ev.ConditionIds()
			evFpmmCreation.Fee = ev.Fee()
			handleFixedProductMarketMakerCreation(state, &evFpmmCreation)
		case generated.EventTypeFixedProductMarketMakerFPMMBuy:
			ev := block.FixedProductMarketMakerFPMMBuyProtoAt(fpmmBuyIdx)
			fpmmBuyIdx++
			evFpmmBuy.EventMeta = ev.Meta()
			evFpmmBuy.Buyer = ev.Buyer()
			evFpmmBuy.InvestmentAmount = ev.InvestmentAmount()
			evFpmmBuy.FeeAmount = ev.FeeAmount()
			evFpmmBuy.OutcomeIndex = ev.OutcomeIndex()
			evFpmmBuy.OutcomeTokensBought = ev.OutcomeTokensBought()
			handleFPMMBuy(state, &evFpmmBuy, ev.Meta().ContractAddress)
		case generated.EventTypeFixedProductMarketMakerFPMMSell:
			ev := block.FixedProductMarketMakerFPMMSellProtoAt(fpmmSellIdx)
			fpmmSellIdx++
			evFpmmSell.EventMeta = ev.Meta()
			evFpmmSell.Seller = ev.Seller()
			evFpmmSell.ReturnAmount = ev.ReturnAmount()
			evFpmmSell.FeeAmount = ev.FeeAmount()
			evFpmmSell.OutcomeIndex = ev.OutcomeIndex()
			evFpmmSell.OutcomeTokensSold = ev.OutcomeTokensSold()
			handleFPMMSell(state, &evFpmmSell, ev.Meta().ContractAddress)
		case generated.EventTypeFixedProductMarketMakerFPMMFundingAdded:
			ev := block.FixedProductMarketMakerFPMMFundingAddedProtoAt(fpmmFundingAddedIdx)
			fpmmFundingAddedIdx++
			evFpmmFundingAdd.EventMeta = ev.Meta()
			evFpmmFundingAdd.Funder = ev.Funder()
			evFpmmFundingAdd.AmountsAdded = ev.AmountsAdded()
			evFpmmFundingAdd.SharesMinted = ev.SharesMinted()
			handleFPMMFundingAdded(state, &evFpmmFundingAdd, ev.Meta().ContractAddress)
		case generated.EventTypeFixedProductMarketMakerFPMMFundingRemoved:
			ev := block.FixedProductMarketMakerFPMMFundingRemovedProtoAt(fpmmFundingRemovedIdx)
			fpmmFundingRemovedIdx++
			evFpmmFundingRem.EventMeta = ev.Meta()
			evFpmmFundingRem.Funder = ev.Funder()
			evFpmmFundingRem.AmountsRemoved = ev.AmountsRemoved()
			evFpmmFundingRem.CollateralRemovedFromFeePool = ev.CollateralRemovedFromFeePool()
			evFpmmFundingRem.SharesBurnt = ev.SharesBurnt()
			handleFPMMFundingRemoved(state, &evFpmmFundingRem, ev.Meta().ContractAddress)
		}
	}
	return nil
}

// Handlers (Parsed Mode / V1)

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

	// Exchange amounts are in raw outcome tokens - divide by 1e6 to get stake units
	makerFilled := Uint256ToDecimal(makerAmountFilled).Div(decimal.NewFromInt(1e6))
	takerFilled := Uint256ToDecimal(takerAmountFilled).Div(decimal.NewFromInt(1e6))

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

	// Price = quote / base (both in stake units now, quote in USDC, base in stake units)
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

	// Divide by 1e6 to convert raw outcome tokens to "full stake" units
	amountRaw := Uint256ToDecimal(ev.OutcomeTokensBought)
	amount := amountRaw.Div(decimal.NewFromInt(1e6))
	// Price calculation: investmentAmount / amount (in stake units) gives USDC per stake
	price := CollateralToDecimal(ev.InvestmentAmount).Div(amount)
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

	// Divide by 1e6 to convert raw outcome tokens to "full stake" units
	amountRaw := Uint256ToDecimal(ev.OutcomeTokensSold)
	amount := amountRaw.Div(decimal.NewFromInt(1e6))
	// Price calculation: returnAmount / amount (in stake units) gives USDC per stake
	price := CollateralToDecimal(ev.ReturnAmount).Div(amount)
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

	// The FPMM sends back the smaller outcome. The user effectively buys the
	// difference between the larger and smaller outcome balances.
	sendbackOutcomeIndex := uint8(0)
	if ev.AmountsAdded[0].Gt(&ev.AmountsAdded[1]) {
		sendbackOutcomeIndex = 1
	}
	largerOutcomeIndex := 1 - sendbackOutcomeIndex

	amountRaw := new(uint256.Int).Sub(&ev.AmountsAdded[largerOutcomeIndex], &ev.AmountsAdded[sendbackOutcomeIndex])
	// Divide by 1e6 to convert to "full stake" units
	amount := Uint256ToDecimal(*amountRaw).Div(decimal.NewFromInt(1e6))
	price := computeFpmmPriceDecimal(ev.AmountsAdded, sendbackOutcomeIndex)
	posID := getFixedProductMarketMakerPositionID(fpmm, sendbackOutcomeIndex)
	updateUserPositionWithBuy(state, ev.Funder, posID, price, amount, decimal.Zero, ev.EventMeta)

	if ev.SharesMinted.IsZero() {
		return
	}

	// totalSpend is max(amountsAdded) in USDC's 1e6 collateral scale.
	totalSpendWei := ev.AmountsAdded[0]
	if ev.AmountsAdded[1].Gt(&totalSpendWei) {
		totalSpendWei = ev.AmountsAdded[1]
	}
	totalSpend := CollateralToDecimal(totalSpendWei)
	tokenCost := amount.Mul(price)
	lpShareCost := totalSpend.Sub(tokenCost)
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
		// Divide by 1e6 to convert to "full stake" units
		tokenAmount := Uint256ToDecimal(ev.AmountsRemoved[i]).Div(decimal.NewFromInt(1e6))
		tokensCost = tokensCost.Add(tokenPrice.Mul(tokenAmount))
		posID := getFixedProductMarketMakerPositionID(fpmm, i)
		updateUserPositionWithBuy(state, ev.Funder, posID, tokenPrice, tokenAmount, decimal.Zero, ev.EventMeta)
	}

	if ev.SharesBurnt.IsZero() {
		return
	}

	lpSalePrice := CollateralToDecimal(ev.CollateralRemovedFromFeePool).Sub(tokensCost).Div(Uint256ToDecimal(ev.SharesBurnt))
	lpPosID := uint256FromAddress(fpmm.ID)
	// Skip LP share sell if position doesn't exist in hot state
	up := getUserPosition(state, ev.Funder, lpPosID)
	if up != nil {
		updateUserPositionWithSell(state, ev.Funder, lpPosID, lpSalePrice, Uint256ToDecimal(ev.SharesBurnt), ev.EventMeta)
	}
}

func handlePositionSplit(state *generated.State, ev *generated.ConditionalTokensPositionSplit) {
	if isIgnoredStakeholder(ev.Stakeholder) {
		return
	}
	_, ok := state.Condition.Get(ev.ConditionID)
	if !ok {
		return
	}

	// Divide by 1e6 to convert to "full stake" units
	amount := Uint256ToDecimal(ev.Amount).Div(decimal.NewFromInt(1e6))
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

	// Divide by 1e6 to convert to "full stake" units
	amount := Uint256ToDecimal(ev.Amount).Div(decimal.NewFromInt(1e6))
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

	// Divide by 1e6 to convert to "full stake" units
	amount := Uint256ToDecimal(ev.Amount).Div(decimal.NewFromInt(1e6))
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

	// Divide by 1e6 to convert to "full stake" units
	amount := Uint256ToDecimal(ev.Amount).Div(decimal.NewFromInt(1e6))
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

	// Divide by 1e6 to convert to "full stake" units
	amount := Uint256ToDecimal(ev.Amount).Div(decimal.NewFromInt(1e6))
	questionCount := nr.QuestionCount
	indexSet := ev.IndexSet

	resolvePositionID := func(questionIndex uint32, outcomeIndex uint8) (uint256.Int, bool) {
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
			currentAvg = toDecimal(up.AvgPrice)
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
		// Sell NO tokens at fiftyCents (0.5), not at user's avgPrice
		// This generates PnL when avgPrice differs from 0.5
		updateUserPositionWithSell(state, ev.Stakeholder, sell.posID, fiftyCents, amount, ev.EventMeta)
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
	// Only create NegRiskEvent if it doesn't exist (QuestionPrepared might have created it first)
	_, ok := state.NegRiskEvent.Get(ev.MarketID)
	if !ok {
		nr := &generated.NegRiskEvent{
			ID: ev.MarketID,
		}
		state.NegRiskEvent.Save(nr, ev.EventMeta)
	}
}

func handleQuestionPrepared(state *generated.State, ev *generated.NegRiskAdapterQuestionPrepared) {
	nr, ok := state.NegRiskEvent.Get(ev.MarketID)
	if !ok {
		// Create NegRiskEvent if it doesn't exist yet (QuestionPrepared can fire before MarketPrepared)
		nr = &generated.NegRiskEvent{
			ID: ev.MarketID,
		}
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
		outcomes := len(ev.PayoutNumerators)
		if outcomes != 2 {
			return
		}
		cond = &generated.Condition{
			ID:               ev.ConditionID,
			Oracle:           ev.Oracle,
			QuestionID:       ev.QuestionID,
			OutcomeSlotCount: uint8(outcomes),
		}
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
		if up != nil && !toDecimal(up.Amount).IsZero() {
			updateUserPositionWithSell(state, ev.Redeemer, posID, price, toDecimal(up.Amount), ev.EventMeta)
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
			amount := Uint256ToDecimal(ev.Amounts[i]).Div(decimal.NewFromInt(1e6))
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
	// Price is calculated as ratio of amounts (both in raw outcome tokens)
	// Since we divide amounts by 1e6 elsewhere, the ratio stays the same
	return Uint256ToDecimal(amounts[1-outcomeIndex]).DivRound(Uint256ToDecimal(*denom), 28)
}

func uint256FromAddress(addr common.Address) uint256.Int {
	// Match graph-ts BigInt.fromByteArray(Address): the address bytes are
	// interpreted as signed little-endian when converted into the LP share
	// token id, so high-bit addresses must be sign-extended.
	src := addr.Bytes()
	var buf [32]byte
	if len(src) > 0 && src[len(src)-1]&0x80 != 0 {
		for i := 0; i < len(buf)-len(src); i++ {
			buf[i] = 0xff
		}
	}
	for i := 0; i < len(src); i++ {
		buf[len(buf)-1-i] = src[i]
	}
	var out uint256.Int
	out.SetBytes(buf[:])
	return out
}

func updateUserPositionWithBuy(state *generated.State, user common.Address, tokenID uint256.Int, price, amount, pnlAdj decimal.Decimal, meta generated.EventMeta) {
	if amount.IsZero() {
		return
	}
	if user.Hex() == "0xf05B670C0F91F8171984db945A28D2Ad0F170cC4" || user.Hex() == "0xf05b670c0f91f8171984db945a28d2ad0f170cc4" {
		fmt.Printf("[DEBUG BUY] block=%d token=%s amount=%s price=%s pnlAdj=%s\n", meta.BlockNumber, tokenIDHash(tokenID).Hex(), amount.String(), price.String(), pnlAdj.String())
	}
	up := getUserPosition(state, user, tokenID)
	if up == nil {
		up = &generated.Position{
			User:        user,
			TokenID:     tokenIDHash(tokenID),
			Amount:      fromDecimal(decimal.Zero),
			AvgPrice:    fromDecimal(decimal.Zero),
			RealizedPnL: fromDecimal(decimal.Zero),
			TotalBought: fromDecimal(decimal.Zero),
		}
	}

	if !pnlAdj.IsZero() {
		up.RealizedPnL = fromDecimal(toDecimal(up.RealizedPnL).Add(pnlAdj))
	}
	// Amount and price are both in "human-readable" units now:
	// - amount: in "full stake" units (divided by 1e6)
	// - price: in USDC (0-1 range)
	// So amount * price = USDC value directly
	// Note: prices are stored in Decimal256 (1e18 scale), so the average price
	// calculation works correctly
	up.AvgPrice = fromDecimal(updateAvgPriceDecimal(toDecimal(up.AvgPrice), toDecimal(up.Amount), price, amount))
	up.Amount = fromDecimal(toDecimal(up.Amount).Add(amount))
	up.TotalBought = fromDecimal(toDecimal(up.TotalBought).Add(amount))
	state.Position.Save(up, meta)
}

func updateUserPositionWithSell(state *generated.State, user common.Address, tokenID uint256.Int, price, amount decimal.Decimal, meta generated.EventMeta) {
	isTargetUser := user.Hex() == "0xf05B670C0F91F8171984db945A28D2Ad0F170cC4" || user.Hex() == "0xf05b670c0f91f8171984db945a28d2ad0f170cc4"
	if isTargetUser {
		fmt.Printf("[DEBUG SELL START] block=%d token=%s amount=%s price=%s\n", meta.BlockNumber, tokenIDHash(tokenID).Hex(), amount.String(), price.String())
	}
	up := getUserPosition(state, user, tokenID)
	if up == nil {
		if isTargetUser {
			fmt.Printf("[DEBUG SELL] position not found\n")
		}
		return
	}

	if isTargetUser {
		fmt.Printf("[DEBUG SELL BEFORE] up.Amount=%s up.AvgPrice=%s up.RealizedPnL=%s\n",
			toDecimal(up.Amount).String(), toDecimal(up.AvgPrice).String(), toDecimal(up.RealizedPnL).String())
	}

	adjAmt := amount
	if adjAmt.GreaterThan(toDecimal(up.Amount)) {
		adjAmt = toDecimal(up.Amount)
	}
	if adjAmt.IsZero() {
		if isTargetUser {
			fmt.Printf("[DEBUG SELL] adjAmt is zero\n")
		}
		return
	}
	// PnL = amount * (price - avgPrice)
	// toDecimal handles the 1e18 scaling, so prices are in correct USDC range
	pnl := adjAmt.Mul(price.Sub(toDecimal(up.AvgPrice)))
	newRealizedPnL := toDecimal(up.RealizedPnL).Add(pnl)
	up.RealizedPnL = fromDecimal(newRealizedPnL)
	up.Amount = fromDecimal(toDecimal(up.Amount).Sub(adjAmt))

	if isTargetUser {
		fmt.Printf("[DEBUG SELL AFTER] pnl=%s newRealizedPnL=%s up.Amount=%s up.RealizedPnL=%s (scaledBig=%s)\n",
			pnl.String(), newRealizedPnL.String(), toDecimal(up.Amount).String(), toDecimal(up.RealizedPnL).String(), up.RealizedPnL.ScaledBig().String())
	}

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
	// Convert uint256 (raw from Solidity) to decimal for arithmetic
	// Uses exponent 0 (raw value) - caller responsible for handling scaling
	return decimal.NewFromBigInt(i.ToBig(), 0)
}

// WeiToDecimal converts uint256 (wei, 1e18 scale) to decimal by dividing by 1e18
// Use this for USDC and other wei-scaled investment/collateral amounts
func WeiToDecimal(i uint256.Int) decimal.Decimal {
	return decimal.NewFromBigInt(i.ToBig(), -18)
}

// CollateralToDecimal converts Polymarket's USDC-scaled integer values to a
// human decimal. The original PnL subgraph uses COLLATERAL_SCALE = 1e6.
func CollateralToDecimal(i uint256.Int) decimal.Decimal {
	return decimal.NewFromBigInt(i.ToBig(), -6)
}

// CTFOutcomeToDecimal converts CTF outcome token amounts to decimal (raw value)
// CTF events use "outcome tokens" where 1 full stake = 1e6 outcome tokens
// This returns the raw value - scaling is applied in updateUserPositionWithBuy
func CTFOutcomeToDecimal(i uint256.Int) decimal.Decimal {
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
	key := negRiskKey{condition: conditionID, outcome: outcome}
	if val, ok := negRiskPosCache.Load(key); ok {
		return val
	}
	// Use BabyJubJub curve computation for collection ID (correct neg-risk token ID generation).
	// computeCollectionId's Legendre-symbol loop is a 256-bit modexp, so memoizing
	// the whole (conditionID, outcome) -> positionID chain elides the processor's
	// most expensive primitive on the hot neg-risk event paths.
	collID := getNegRiskCollectionID(conditionID, outcome)
	val := getPositionID(negRiskWrappedCollateral, collID)
	negRiskPosCache.Store(key, val)
	return val
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
	copy(point[64-len(yb):64], yb)

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

func CustomProcessingProto(ctx context.Context, store generated.Store, state *generated.State, slot *generated.ProtoEventBlock) error {
	return generated.CustomProcessingProto(ctx, store, state, slot)
}

func CustomProcessing(ctx context.Context, store generated.Store, state *generated.State, slot *generated.ParsedBlock) error {
	return generated.CustomProcessing(ctx, store, state, slot)
}

func NewProcessor() (*generated.Processor, error) {
	return generated.NewProcessor(false)
}
