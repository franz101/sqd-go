package subgraph

import (
	"encoding/binary"
	"fmt"
	"log"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	bn256 "github.com/ethereum/go-ethereum/crypto/bn256/cloudflare"
	"github.com/holiman/uint256"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/shopspring/decimal"
)

const (
	negRiskYesOutcomeIndex uint8 = 0
	negRiskNoOutcomeIndex  uint8 = 1
)

var (
	// cfg holds the active configuration. Initialize with InitConfig or SetConfig.
	cfg *Config

	// EC Math constants for Conditional Tokens
	ctP            = uint256FromDecimal("21888242871839275222246405745257275088696311157297823662689037894645226208583")
	ctB            = big.NewInt(3)
	ctOne          = big.NewInt(1)
	ctParityBit    = new(big.Int).Lsh(big.NewInt(1), 254)
	ctLow254Mask   = new(big.Int).Sub(new(big.Int).Set(ctParityBit), ctOne)
	ctSqrtExponent = new(big.Int).Rsh(new(big.Int).Add(new(big.Int).Set(ctP), ctOne), 2)

	// FiftyCents represents 0.5 as a decimal for position split/merge pricing
	fiftyCents = decimal.NewFromFloat(0.5)

	subgraphLogMu    sync.Mutex
	subgraphLogCount       = make(map[string]uint64)
	subgraphEventLog int32 = 1

	// Caches for expensive crypto operations
	collectionCache    = xsync.NewMapOf[collectionKey, common.Hash]()
	collectionCacheLen int32
	positionCache      = xsync.NewMapOf[positionKey, *uint256.Int]()
	positionCacheLen   int32
)

const maxCacheLen = 65536

type collectionKey struct {
	parent    common.Hash
	condition common.Hash
	index     [32]byte
}

type positionKey struct {
	collateral common.Address
	collection common.Hash
}

// SetEventLoggingEnabled toggles diagnostic subgraph event logs.
// Call this during startup before processing begins.
func SetEventLoggingEnabled(enabled bool) {
	if enabled {
		atomic.StoreInt32(&subgraphEventLog, 1)
		return
	}
	atomic.StoreInt32(&subgraphEventLog, 0)
}

func logSubgraphLimited(key, format string, args ...any) {
	if atomic.LoadInt32(&subgraphEventLog) == 0 {
		return
	}

	subgraphLogMu.Lock()
	subgraphLogCount[key]++
	count := subgraphLogCount[key]
	subgraphLogMu.Unlock()

	// Avoid unbounded log volume while keeping recurring failures visible.
	if count > 5 && count%1000 != 0 {
		return
	}

	log.Printf("[event=%s count=%d] "+format, append([]any{key, count}, args...)...)
}

// InitConfig loads configuration from networks.yaml. Must be called before handlers.
func InitConfig(networksPath, network string) error {
	var err error
	cfg, err = LoadConfig(networksPath, network)
	if err != nil {
		return fmt.Errorf("failed to initialize subgraph config: %w", err)
	}
	return nil
}

// SetConfig sets the configuration directly. Use for testing.
func SetConfig(c *Config) {
	cfg = c
}

// getConfig returns the active config, initializing with defaults if needed.
func getConfig() *Config {
	if cfg == nil {
		panic("subgraph config not initialized - call InitConfig first")
	}
	return cfg
}

// Address accessors - use these instead of direct variable access
func getNegRiskAdapterAddress() common.Address    { return getConfig().NegRiskAdapter }
func getNegRiskExchangeAddress() common.Address   { return getConfig().NegRiskExchange }
func getExchangeAddress() common.Address          { return getConfig().Exchange }
func getNegRiskWrappedCollateral() common.Address { return getConfig().negRiskWrappedCollateral }

// Store represents a simplified interface to your persistence layer
type Store interface {
	IsSkipParsing() bool

	GetLastProcessedBlock() uint64
	SaveLastProcessedBlock(blockNumber uint64) error
	SetLastProcessedBlock(blockNumber uint64)
	OnBlockComplete(blockNumber uint64)
	Flush() error

	GetCondition(id common.Hash) *Condition
	SaveCondition(cond *Condition)
	CreateCondition(conditionID common.Hash, oracle common.Address, questionID common.Hash, isNegRisk bool) *Condition
	ResolveCondition(conditionID common.Hash, oracle common.Address, questionID common.Hash, outcomeSlotCount int, payouts []*uint256.Int) *Condition

	GetUserPosition(key UserPositionKey) *UserPosition
	SaveUserPosition(up *UserPosition)

	GetNegRiskQuestionCount(marketID common.Hash) (uint32, bool)
	GetNegRiskEvent(marketID common.Hash) *NegRiskEvent
	SaveNegRiskEvent(ev *NegRiskEvent)
	CreateNegRiskEvent(marketID common.Hash)
	IncrementNegRiskQuestionCount(marketID common.Hash)

	// GetAllPositions intentionally returns only currently hot/in-memory positions
	// for bounded-memory implementations (e.g. HybridStore).
	GetAllPositions() map[UserPositionKey]*UserPosition

	// PrintPnLSummary prints a summary of PnL for users.
	PrintPnLSummary(blockNumber uint64, numRandomSamples int)

	// GetConditionCount returns the number of conditions tracked.
	GetConditionCount() int
	// GetNegRiskMarketCount returns the number of neg risk markets tracked.
	GetNegRiskMarketCount() int
}

// PrintPnLSummaryHelper is a shared helper for Store implementations to print PnL summaries.
type pnlSummaryUser struct {
	pnl          decimal.Decimal
	openAmount   decimal.Decimal
	posCount     int
	openPosCount int
}

type pnlSummarySample struct {
	addr  common.Address
	score uint64
	valid bool
}

type pnlSummaryTop struct {
	addr     common.Address
	absPnL   decimal.Decimal
	pnl      decimal.Decimal
	posCount int
	valid    bool
}

func PrintPnLSummaryHelper(s Store, blockNumber uint64, numRandomSamples int) {
	positions := s.GetAllPositions()

	if len(positions) == 0 {
		fmt.Printf("[Block %d] Positions: 0 | Conditions: %d | NegRiskMarkets: %d\n",
			blockNumber, s.GetConditionCount(), s.GetNegRiskMarketCount())
		return
	}

	userStats := make(map[common.Address]pnlSummaryUser, len(positions)/2+1)

	for key, pos := range positions {
		st := userStats[key.User]
		st.posCount++
		st.pnl = st.pnl.Add(pos.RealizedPnL)
		if !pos.Amount.IsZero() {
			st.openPosCount++
			st.openAmount = st.openAmount.Add(pos.Amount)
		}
		userStats[key.User] = st
	}

	nonEmptyCount := 0
	withPnLCount := 0
	var interestingSamples []pnlSummarySample
	var fallbackSamples []pnlSummarySample
	if numRandomSamples > 0 {
		interestingSamples = make([]pnlSummarySample, numRandomSamples)
		fallbackSamples = make([]pnlSummarySample, numRandomSamples)
	}
	topPnL := [3]pnlSummaryTop{}
	sampleEpoch := blockNumber / 512

	for addr, st := range userStats {
		if st.openPosCount > 0 {
			nonEmptyCount++
			if numRandomSamples > 0 {
				score := pnlSampleScore(addr, sampleEpoch)
				interesting := !st.pnl.IsZero() || st.posCount > 1 || st.openPosCount > 1
				if interesting {
					insertPnLSampleTyped(interestingSamples, addr, score)
				}
				insertPnLSampleTyped(fallbackSamples, addr, score)
			}
		}
		if !st.pnl.IsZero() {
			withPnLCount++
			insertTopPnLTyped(topPnL[:], addr, st.pnl, st.posCount)
		}
	}

	fmt.Printf("[Block %d] Positions: %d | Conditions: %d | NegRiskMarkets: %d | Users: %d | NonEmpty: %d | WithPnL: %d\n",
		blockNumber, len(positions), s.GetConditionCount(), s.GetNegRiskMarketCount(), len(userStats), nonEmptyCount, withPnLCount)

	if numRandomSamples > 0 && nonEmptyCount > 0 {
		printed := 0
		printSample := func(e pnlSummarySample) {
			if !e.valid {
				return
			}
			st, ok := userStats[e.addr]
			if !ok || st.openPosCount == 0 {
				return
			}
			fmt.Printf("  Sample account: %s | OpenPositions: %d | OpenAmount: %s | RealizedPnL: %s\n",
				e.addr.Hex(), st.openPosCount, st.openAmount.String(), st.pnl.String())
			printed++
		}
		for _, e := range interestingSamples {
			printSample(e)
			if printed >= numRandomSamples {
				break
			}
		}
		if printed < numRandomSamples {
			for _, e := range fallbackSamples {
				if !e.valid {
					continue
				}
				dup := false
				for _, ie := range interestingSamples {
					if ie.valid && ie.addr == e.addr {
						dup = true
						break
					}
				}
				if dup {
					continue
				}
				printSample(e)
				if printed >= numRandomSamples {
					break
				}
			}
		}
	}

	if withPnLCount > 0 && numRandomSamples > 0 {
		fmt.Printf("  Top PnL users:\n")
		for _, entry := range topPnL {
			if !entry.valid {
				continue
			}
			fmt.Printf("    %s: %d positions, Realized PnL: %s USDC\n",
				entry.addr.Hex(), entry.posCount, entry.pnl.String())
		}
	}
}

func pnlSampleScore(addr common.Address, epoch uint64) uint64 {
	b := addr.Bytes()
	var x uint64 = 0x9e3779b97f4a7c15 ^ epoch
	for _, v := range b {
		x ^= uint64(v) + 0x9e3779b97f4a7c15 + (x << 6) + (x >> 2)
	}
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

func insertPnLSampleTyped(dst []pnlSummarySample, addr common.Address, score uint64) {
	if len(dst) == 0 {
		return
	}
	for _, cur := range dst {
		if cur.valid && cur.addr == addr {
			return
		}
	}
	insertAt := -1
	for i := range dst {
		if !dst[i].valid || score < dst[i].score {
			insertAt = i
			break
		}
	}
	if insertAt < 0 {
		return
	}
	for i := len(dst) - 1; i > insertAt; i-- {
		dst[i] = dst[i-1]
	}
	dst[insertAt] = pnlSummarySample{addr: addr, score: score, valid: true}
}

func insertTopPnLTyped(dst []pnlSummaryTop, addr common.Address, pnl decimal.Decimal, posCount int) {
	if len(dst) == 0 {
		return
	}
	absPnL := pnl.Abs()
	insertAt := -1
	for i := range dst {
		if !dst[i].valid || absPnL.Cmp(dst[i].absPnL) > 0 {
			insertAt = i
			break
		}
	}
	if insertAt < 0 {
		return
	}
	for i := len(dst) - 1; i > insertAt; i-- {
		dst[i] = dst[i-1]
	}
	dst[insertAt] = pnlSummaryTop{addr: addr, absPnL: absPnL, pnl: pnl, posCount: posCount, valid: true}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// HandleOrderFilledEvent handles order filled events from any exchange contract.
func HandleOrderFilledEvent(store Store, ev OrderEvent) {
	processOrder(store, ev.GetMaker(), ev.GetMakerAssetID(), ev.GetTakerAssetID(), ev.GetMakerAmountFilled(), ev.GetTakerAmountFilled())
}

// HandleOrderFilled handles CTF Exchange order filled events.
func HandleOrderFilled(store Store, ev *ExchangeOrderFilled) {
	HandleOrderFilledEvent(store, ev)
}

// HandleNegRiskOrderFilled handles NegRisk Exchange order filled events.
func HandleNegRiskOrderFilled(store Store, ev *NegriskexchangeOrderFilled) {
	HandleOrderFilledEvent(store, ev)
}

func processOrder(store Store, maker common.Address, makerAsset, takerAsset, makerAmt, takerAmt *big.Int) {
	var tokenID *uint256.Int
	var baseAmount, quoteAmount decimal.Decimal
	var isBuy bool

	zero := uint256.NewInt(0)
	makerAssetID := bigToUint256(makerAsset)
	makerFilled := BigIntToDecimal(makerAmt)
	takerFilled := BigIntToDecimal(takerAmt)

	if makerAssetID.Eq(zero) { // BUY
		isBuy = true
		tokenID = bigToUint256(takerAsset)
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
		updateUserPositionWithBuy(store, maker, tokenID, price, baseAmount, decimal.Zero)
	} else {
		updateUserPositionWithSell(store, maker, tokenID, price, baseAmount)
	}
}

func HandlePositionSplit(store Store, ev *ConditionaltokensPositionSplit) {
	if isIgnoredStakeholder(ev.Stakeholder) {
		return
	}
	if store.GetCondition(ev.ConditionID) == nil {
		// Match TS parity: ignore unknown conditions instead of creating placeholders.
		return
	}

	amount := BigIntToDecimal(ev.Amount)
	parentID := ev.ParentCollectionID
	if amount.IsZero() {
		return
	}

	if parentID != (common.Hash{}) {
		parentPosID := getPositionID(ev.CollateralToken, parentID)
		parentPrice := decimal.NewFromInt(1)
		if up := store.GetUserPosition(userPositionKeyFor(ev.Stakeholder, parentPosID)); up != nil {
			parentPrice = up.AvgPrice
		}
		updateUserPositionWithSell(store, ev.Stakeholder, parentPosID, parentPrice, amount)
	}

	for _, part := range ev.Partition {
		if part == nil || part.Sign() <= 0 {
			continue
		}
		posID := getPositionID(ev.CollateralToken, getCollectionID(parentID, ev.ConditionID, part))
		updateUserPositionWithBuy(store, ev.Stakeholder, posID, fiftyCents, amount, decimal.Zero)
	}
}

func HandlePositionsMerge(store Store, ev *ConditionaltokensPositionsMerge) {
	if isIgnoredStakeholder(ev.Stakeholder) {
		return
	}
	if store.GetCondition(ev.ConditionID) == nil {
		// Match TS parity: ignore unknown conditions instead of creating placeholders.
		return
	}

	amount := BigIntToDecimal(ev.Amount)
	parentID := ev.ParentCollectionID
	if amount.IsZero() {
		return
	}

	for _, part := range ev.Partition {
		if part == nil || part.Sign() <= 0 {
			continue
		}
		posID := getPositionID(ev.CollateralToken, getCollectionID(parentID, ev.ConditionID, part))
		updateUserPositionWithSell(store, ev.Stakeholder, posID, fiftyCents, amount)
	}

	if parentID != (common.Hash{}) {
		parentPosID := getPositionID(ev.CollateralToken, parentID)
		parentPrice := decimal.NewFromInt(1)
		if up := store.GetUserPosition(userPositionKeyFor(ev.Stakeholder, parentPosID)); up != nil {
			parentPrice = up.AvgPrice
		}
		updateUserPositionWithBuy(store, ev.Stakeholder, parentPosID, parentPrice, amount, decimal.Zero)
	}
}

func HandleNegRiskPositionSplit(store Store, ev *NegriskadapterPositionSplit) {
	if ev.Stakeholder == getNegRiskExchangeAddress() {
		return
	}

	// Create condition lazily if it doesn't exist
	if store.GetCondition(ev.ConditionID) == nil {
		store.CreateCondition(ev.ConditionID, getNegRiskAdapterAddress(), common.Hash{}, true)
	}

	condID := ev.ConditionID
	amount := BigIntToDecimal(ev.Amount)

	posIDYes := getNegRiskPositionIDByCondition(condID, negRiskYesOutcomeIndex)
	updateUserPositionWithBuy(store, ev.Stakeholder, posIDYes, fiftyCents, amount, decimal.Zero)

	posIDNo := getNegRiskPositionIDByCondition(condID, negRiskNoOutcomeIndex)
	updateUserPositionWithBuy(store, ev.Stakeholder, posIDNo, fiftyCents, amount, decimal.Zero)
}

func HandleNegRiskPositionsMerge(store Store, ev *NegriskadapterPositionsMerge) {
	if ev.Stakeholder == getNegRiskExchangeAddress() {
		return
	}

	// Create condition lazily if it doesn't exist
	if store.GetCondition(ev.ConditionID) == nil {
		store.CreateCondition(ev.ConditionID, getNegRiskAdapterAddress(), common.Hash{}, true)
	}

	condID := ev.ConditionID
	amount := BigIntToDecimal(ev.Amount)

	posIDYes := getNegRiskPositionIDByCondition(condID, negRiskYesOutcomeIndex)
	updateUserPositionWithSell(store, ev.Stakeholder, posIDYes, fiftyCents, amount)

	posIDNo := getNegRiskPositionIDByCondition(condID, negRiskNoOutcomeIndex)
	updateUserPositionWithSell(store, ev.Stakeholder, posIDNo, fiftyCents, amount)
}

func HandlePositionsConverted(store Store, ev *NegriskadapterPositionsConverted) {
	marketID := ev.MarketID
	nr := store.GetNegRiskEvent(marketID)
	if nr == nil || nr.QuestionCount == 0 {
		logSubgraphLimited("negrisk_positions_converted_missing_questions",
			"[NEGRISK] PositionsConverted skipped: market=%s has no tracked questions",
			marketID.Hex(),
		)
		return
	}
	questionCount := nr.QuestionCount

	type noSell struct {
		questionIndex uint32
		positionID    *uint256.Int
		price         decimal.Decimal
	}

	useQuestionIDs := false
	for _, qid := range nr.QuestionIDs {
		if qid != (common.Hash{}) {
			useQuestionIDs = true
			break
		}
	}

	resolvePositionID := func(questionIndex uint32, outcomeIndex uint8) (*uint256.Int, bool) {
		if useQuestionIDs {
			if questionIndex >= uint32(len(nr.QuestionIDs)) {
				return nil, false
			}
			questionID := nr.QuestionIDs[questionIndex]
			if questionID == (common.Hash{}) {
				return nil, false
			}
			conditionID := getConditionID(getNegRiskAdapterAddress(), questionID)
			return getNegRiskPositionIDByCondition(conditionID, outcomeIndex), true
		}
		return getNegRiskPositionID(marketID, questionIndex, outcomeIndex), true
	}

	noSells := make([]noSell, 0)
	yesBuys := make([]*uint256.Int, 0)
	sumPrice := decimal.Zero
	amount := BigIntToDecimal(ev.Amount)

	for questionIndex := uint32(0); questionIndex < questionCount; questionIndex++ {
		selected := indexSetContains(ev.IndexSet, questionIndex)

		positionID, ok := resolvePositionID(questionIndex, negRiskYesOutcomeIndex)
		if !selected {
			if !ok {
				if useQuestionIDs {
					logSubgraphLimited("negrisk_positions_converted_missing_question_mapping",
						"[NEGRISK] PositionsConverted skipped: market=%s missing question mapping for index=%d",
						marketID.Hex(), questionIndex,
					)
					return
				}
				continue
			}
			yesBuys = append(yesBuys, positionID)
			continue
		}

		positionID, ok = resolvePositionID(questionIndex, negRiskNoOutcomeIndex)
		if !ok {
			logSubgraphLimited("negrisk_positions_converted_missing_question_mapping",
				"[NEGRISK] PositionsConverted skipped: market=%s missing question mapping for index=%d",
				marketID.Hex(), questionIndex,
			)
			return
		}

		userPosition := store.GetUserPosition(userPositionKeyFor(ev.Stakeholder, positionID))
		if userPosition == nil || userPosition.Amount.LessThan(amount) {
			logSubgraphLimited("negrisk_positions_converted_missing_no_position",
				"[NEGRISK] PositionsConverted skipped: market=%s stakeholder=%s missing/insufficient NO position at index=%d",
				marketID.Hex(), ev.Stakeholder.Hex(), questionIndex,
			)
			return
		}
		noSells = append(noSells, noSell{
			questionIndex: questionIndex,
			positionID:    positionID,
			price:         userPosition.AvgPrice,
		})
		sumPrice = sumPrice.Add(userPosition.AvgPrice)
	}

	noCount := uint32(len(noSells))
	if noCount == 0 {
		logSubgraphLimited("negrisk_positions_converted_no_no_outcomes",
			"[NEGRISK] PositionsConverted skipped: market=%s stakeholder=%s indexSet=%s matchedNoOutcomes=0",
			marketID.Hex(), ev.Stakeholder.Hex(), indexSetToString(ev.IndexSet),
		)
		return
	}

	for _, sell := range noSells {
		updateUserPositionWithSell(store, ev.Stakeholder, sell.positionID, sell.price, amount)
	}

	avgPrice := sumPrice.Div(decimal.NewFromInt(int64(noCount)))
	if len(yesBuys) == 0 || questionCount == noCount {
		logSubgraphLimited("negrisk_positions_converted_all_no",
			"[NEGRISK] PositionsConverted all-NO path: market=%s stakeholder=%s questionCount=%d",
			marketID.Hex(), ev.Stakeholder.Hex(), questionCount,
		)
		return
	}

	yesPrice := computeNegRiskYesPriceDecimal(avgPrice, noCount, questionCount)
	pnlAdjustment := decimal.Zero

	if yesPrice.IsNegative() {
		totalAdjustment := amount.Mul(yesPrice.Abs()).Neg()
		pnlAdjustment = totalAdjustment.Div(decimal.NewFromInt(int64(len(yesBuys))))
		logSubgraphLimited("negrisk_positions_converted_negative_yes_price",
			"[NEGRISK] Negative YES price clamped: market=%s stakeholder=%s yesPrice=%s pnlAdjustment=%s yesBuys=%d",
			marketID.Hex(), ev.Stakeholder.Hex(), yesPrice.String(), pnlAdjustment.String(),
			len(yesBuys),
		)
		yesPrice = decimal.Zero
	}

	for _, positionID := range yesBuys {
		updateUserPositionWithBuy(store, ev.Stakeholder, positionID, yesPrice, amount, pnlAdjustment)
	}

	logSubgraphLimited("negrisk_positions_converted_processed",
		"[NEGRISK] PositionsConverted processed: market=%s stakeholder=%s questionCount=%d noCount=%d yesPrice=%s amount=%s",
		marketID.Hex(), ev.Stakeholder.Hex(), questionCount, noCount, yesPrice.String(), amount.String(),
	)
}

func HandleMarketPrepared(store Store, ev *NegriskadapterMarketPrepared) {
	if _, exists := store.GetNegRiskQuestionCount(ev.MarketID); exists {
		logSubgraphLimited("negrisk_market_prepared_duplicate",
			"[NEGRISK] MarketPrepared duplicate: market=%s resetting tracked state",
			ev.MarketID.Hex(),
		)
	}
	store.CreateNegRiskEvent(ev.MarketID)
	logSubgraphLimited("negrisk_market_prepared",
		"[NEGRISK] MarketPrepared: market=%s initialized",
		ev.MarketID.Hex(),
	)
}

func HandleQuestionPrepared(store Store, ev *NegriskadapterQuestionPrepared) {
	nr := store.GetNegRiskEvent(ev.MarketID)
	if nr == nil {
		logSubgraphLimited("negrisk_question_prepared_missing_market",
			"[NEGRISK] QuestionPrepared ignored without prior MarketPrepared: market=%s",
			ev.MarketID.Hex(),
		)
		return
	}

	questionIndex, ok := questionIndexFromPreparedIndex(ev.Index)
	if !ok {
		logSubgraphLimited("negrisk_question_prepared_invalid_index",
			"[NEGRISK] QuestionPrepared ignored invalid indexSet: market=%s question=%s index=%s",
			ev.MarketID.Hex(), ev.QuestionID.Hex(), indexSetToString(ev.Index),
		)
		return
	}

	if questionIndex >= uint32(len(nr.QuestionIDs)) {
		expanded := make([]common.Hash, questionIndex+1)
		copy(expanded, nr.QuestionIDs)
		nr.QuestionIDs = expanded
	}
	nr.QuestionIDs[questionIndex] = ev.QuestionID
	if nr.QuestionCount < questionIndex+1 {
		nr.QuestionCount = questionIndex + 1
	}
	store.SaveNegRiskEvent(nr)

	logSubgraphLimited("negrisk_question_prepared",
		"[NEGRISK] QuestionPrepared: market=%s question=%s index=%s totalQuestions=%d",
		ev.MarketID.Hex(), ev.QuestionID.Hex(), indexSetToString(ev.Index), nr.QuestionCount,
	)
}

func HandleConditionPreparation(store Store, ev *ConditionaltokensConditionPreparation) {
	outcomes := int(ev.OutcomeSlotCount.Int64())
	if outcomes <= 0 {
		logSubgraphLimited("condition_preparation_invalid_outcomes",
			"[CONDITION] Skipping invalid condition: condition=%s oracle=%s outcomes=%d",
			ev.ConditionID.Hex(), ev.Oracle.Hex(), outcomes,
		)
		return
	}
	if cond := store.GetCondition(ev.ConditionID); cond != nil {
		cond.Oracle = ev.Oracle
		cond.QuestionID = ev.QuestionID
		cond.OutcomeSlotCount = outcomes
		store.SaveCondition(cond)
		logSubgraphLimited("condition_preparation_duplicate",
			"[CONDITION] Prepared duplicate/update: condition=%s oracle=%s question=%s outcomes=%d",
			ev.ConditionID.Hex(), ev.Oracle.Hex(), ev.QuestionID.Hex(), outcomes,
		)
		return
	}
	store.SaveCondition(&Condition{
		ID:               ev.ConditionID,
		Oracle:           ev.Oracle,
		QuestionID:       ev.QuestionID,
		OutcomeSlotCount: outcomes,
		Resolved:         false,
	})
	logSubgraphLimited("condition_preparation",
		"[CONDITION] Prepared: condition=%s oracle=%s question=%s outcomes=%d",
		ev.ConditionID.Hex(), ev.Oracle.Hex(), ev.QuestionID.Hex(), outcomes,
	)
}

func HandleConditionResolution(store Store, ev *ConditionaltokensConditionResolution) {
	if store.GetCondition(ev.ConditionID) == nil {
		logSubgraphLimited("condition_resolution_unknown_condition",
			"[CONDITION] ConditionResolution ignored unknown condition: condition=%s oracle=%s question=%s",
			ev.ConditionID.Hex(), ev.Oracle.Hex(), ev.QuestionID.Hex(),
		)
		return
	}
	payouts := make([]*uint256.Int, len(ev.PayoutNumerators))
	for i, p := range ev.PayoutNumerators {
		payouts[i] = bigToUint256(p)
	}
	store.ResolveCondition(
		ev.ConditionID,
		ev.Oracle,
		ev.QuestionID,
		int(ev.OutcomeSlotCount.Int64()),
		payouts,
	)
	logSubgraphLimited("condition_resolution",
		"[CONDITION] Resolved: condition=%s oracle=%s question=%s outcomes=%d payouts=%d",
		ev.ConditionID.Hex(), ev.Oracle.Hex(), ev.QuestionID.Hex(), ev.OutcomeSlotCount.Int64(), len(ev.PayoutNumerators),
	)
}

func HandlePayoutRedemptionCTF(store Store, ev *ConditionaltokensPayoutRedemption) {
	if ev.Redeemer == getNegRiskAdapterAddress() {
		logSubgraphLimited("condition_redemption_ctf_ignored_adapter",
			"[CONDITION] PayoutRedemptionCTF ignored adapter redemption: redeemer=%s condition=%s",
			ev.Redeemer.Hex(), ev.ConditionID.Hex(),
		)
		return
	}

	cond := store.GetCondition(ev.ConditionID)
	if cond == nil || !cond.Resolved {
		logSubgraphLimited("condition_redemption_ctf_unresolved",
			"[CONDITION] PayoutRedemptionCTF skipped unresolved condition: condition=%s redeemer=%s",
			ev.ConditionID.Hex(), ev.Redeemer.Hex(),
		)
		return
	}

	denomDec, ok := calculatePayoutDenominator(cond)
	if !ok {
		logSubgraphLimited("condition_redemption_ctf_zero_denom",
			"[CONDITION] PayoutRedemptionCTF skipped zero payout denominator: condition=%s",
			ev.ConditionID.Hex(),
		)
		return
	}

	for i := range cond.Payouts {
		if len(cond.Payouts) > i {
			indexSet := new(big.Int).Lsh(big.NewInt(1), uint(i))
			collID := getCollectionID(ev.ParentCollectionID, ev.ConditionID, indexSet)
			posID := getPositionID(ev.CollateralToken, collID)

			price := Uint256ToDecimal(cond.Payouts[i]).Div(denomDec)

			up := loadOrCreateUserPosition(store, ev.Redeemer, posID)
			if !up.Amount.IsZero() {
				updateUserPositionWithSell(store, ev.Redeemer, posID, price, up.Amount)
			}
		}
	}
}

func HandlePayoutRedemptionNR(store Store, ev *NegriskadapterPayoutRedemption) {
	cond := store.GetCondition(ev.ConditionID)
	if cond == nil || !cond.Resolved {
		logSubgraphLimited("negrisk_redemption_unresolved",
			"[NEGRISK] PayoutRedemption skipped unresolved condition: condition=%s redeemer=%s",
			ev.ConditionID.Hex(), ev.Redeemer.Hex(),
		)
		return
	}

	denomDec, ok := calculatePayoutDenominator(cond)
	if !ok {
		logSubgraphLimited("negrisk_redemption_zero_denom",
			"[NEGRISK] PayoutRedemption skipped zero payout denominator: condition=%s",
			ev.ConditionID.Hex(),
		)
		return
	}

	for i := range uint8(2) {
		if len(ev.Amounts) > int(i) && len(cond.Payouts) > int(i) {
			posID := getNegRiskPositionIDByCondition(ev.ConditionID, i)
			amount := BigIntToDecimal(ev.Amounts[i])
			price := Uint256ToDecimal(cond.Payouts[i]).Div(denomDec)
			updateUserPositionWithSell(store, ev.Redeemer, posID, price, amount)
		}
	}

	logSubgraphLimited("negrisk_redemption_processed",
		"[NEGRISK] PayoutRedemption processed: condition=%s redeemer=%s amounts=%d",
		ev.ConditionID.Hex(), ev.Redeemer.Hex(), len(ev.Amounts),
	)
}

// ---------------------------------------------------------------------------
// Position Updates & Math
// ---------------------------------------------------------------------------

// calculatePayoutDenominator computes the total payout denominator for a resolved condition.
// Returns the denominator as a decimal and false if zero.
func calculatePayoutDenominator(cond *Condition) (decimal.Decimal, bool) {
	denom := new(uint256.Int)
	for _, p := range cond.Payouts {
		denom.Add(denom, p)
	}
	if denom.IsZero() {
		return decimal.Zero, false
	}
	return Uint256ToDecimal(denom), true
}

type userPositionFastUpdater interface {
	applyUserPositionBuy(user common.Address, tokenID *uint256.Int, price, amount, pnlAdj decimal.Decimal)
	applyUserPositionSell(user common.Address, tokenID *uint256.Int, price, amount decimal.Decimal)
}

func userPositionKeyFor(user common.Address, tokenID *uint256.Int) UserPositionKey {
	var h common.Hash
	if tokenID != nil {
		tokenID.WriteToSlice(h[:])
	}
	return UserPositionKey{User: user, TokenID: h}
}

func newUserPositionForKey(key UserPositionKey, tokenID *uint256.Int) *UserPosition {
	return &UserPosition{ID: key, TokenID: *tokenID}
}

func applyUserPositionBuyState(up *UserPosition, price, amount, pnlAdj decimal.Decimal) bool {
	if up == nil || amount.IsZero() {
		return false
	}
	if !pnlAdj.IsZero() {
		up.RealizedPnL = up.RealizedPnL.Add(pnlAdj)
	}
	up.AvgPrice = updateAvgPriceDecimal(up.AvgPrice, up.Amount, price, amount)

	up.Amount = up.Amount.Add(amount)
	up.TotalBought = up.TotalBought.Add(amount)
	return true
}

func applyUserPositionSellState(up *UserPosition, price, amount decimal.Decimal) bool {
	if up == nil {
		return false
	}
	adjAmt := amount
	if adjAmt.GreaterThan(up.Amount) {
		adjAmt = up.Amount
	}
	if adjAmt.IsZero() {
		return false
	}
	pnl := adjAmt.Mul(price.Sub(up.AvgPrice))
	up.RealizedPnL = up.RealizedPnL.Add(pnl)
	up.Amount = up.Amount.Sub(adjAmt)
	return true
}

func updateUserPositionWithBuy(store Store, user common.Address, tokenID *uint256.Int, price, amount, pnlAdj decimal.Decimal) {
	if amount.IsZero() {
		return
	}
	if fast, ok := store.(userPositionFastUpdater); ok {
		fast.applyUserPositionBuy(user, tokenID, price, amount, pnlAdj)
		return
	}
	up := loadOrCreateUserPosition(store, user, tokenID)
	if applyUserPositionBuyState(up, price, amount, pnlAdj) {
		store.SaveUserPosition(up)
	}
}

func updateUserPositionWithSell(store Store, user common.Address, tokenID *uint256.Int, price, amount decimal.Decimal) {
	if fast, ok := store.(userPositionFastUpdater); ok {
		fast.applyUserPositionSell(user, tokenID, price, amount)
		return
	}
	up := loadOrCreateUserPosition(store, user, tokenID)
	if applyUserPositionSellState(up, price, amount) {
		store.SaveUserPosition(up)
	}
}

func loadOrCreateUserPosition(store Store, user common.Address, tokenID *uint256.Int) *UserPosition {
	key := userPositionKeyFor(user, tokenID)
	if up := store.GetUserPosition(key); up != nil {
		return up
	}
	return newUserPositionForKey(key, tokenID)
}

func updateAvgPriceDecimal(currentAvg, currentAmt, newPrice, newAmt decimal.Decimal) decimal.Decimal {
	if newAmt.IsZero() {
		return currentAvg
	}
	denom := currentAmt.Add(newAmt)
	if denom.IsZero() {
		return currentAvg
	}
	// Preserve decimal scale/sign exactly; uint256 conversion loses exponent and overflows.
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

// ---------------------------------------------------------------------------
// Helpers & Cryptography (CTHelpers)
// ---------------------------------------------------------------------------

func getPositionID(collateral common.Address, collection common.Hash) *uint256.Int {
	key := positionKey{collateral: collateral, collection: collection}
	if val, ok := positionCache.Load(key); ok {
		return val
	}

	var buf [52]byte
	copy(buf[0:20], collateral[:])
	copy(buf[20:52], collection[:])
	hash := crypto.Keccak256(buf[:])
	val := new(uint256.Int).SetBytes(hash)

	if atomic.AddInt32(&positionCacheLen, 1) > maxCacheLen {
		positionCache = xsync.NewMapOf[positionKey, *uint256.Int]()
		atomic.StoreInt32(&positionCacheLen, 1)
	}
	positionCache.Store(key, val)
	return val
}

func getNegRiskPositionIDByCondition(conditionID common.Hash, outcome uint8) *uint256.Int {
	indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcome))
	collID := getCollectionID(common.Hash{}, conditionID, indexSet)
	return getPositionID(getNegRiskWrappedCollateral(), collID)
}

func getNegRiskFallbackQuestionID(marketID common.Hash, questionIndex uint32) common.Hash {
	// Preserve legacy IDs for the common <=255 path while avoiding collisions
	// for larger question indexes in fallback mode.
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

func getNegRiskPositionID(marketID common.Hash, questionIndex uint32, outcomeIndex uint8) *uint256.Int {
	questionID := getNegRiskFallbackQuestionID(marketID, questionIndex)
	conditionID := getConditionID(getNegRiskAdapterAddress(), questionID)
	return getNegRiskPositionIDByCondition(conditionID, outcomeIndex)
}

func getConditionID(oracle common.Address, questionID common.Hash) common.Hash {
	payload := make([]byte, 84)
	copy(payload[:20], oracle.Bytes())
	copy(payload[20:52], questionID.Bytes())
	payload[83] = 0x02
	return crypto.Keccak256Hash(payload)
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

	var (
		y1 *big.Int
		yy *big.Int
	)
	for {
		x1 = addMod(x1, ctOne, ctP)
		yy = addMod(mulMod(x1, mulMod(x1, x1, ctP), ctP), ctB, ctP)
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

		yy = addMod(mulMod(x2, mulMod(x2, x2, ctP), ctP), ctB, ctP)
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

	res := bigIntToHash(x1)
	if atomic.AddInt32(&collectionCacheLen, 1) > maxCacheLen {
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
		panic("invalid uint256 decimal")
	}
	return n
}

func bigIntToHash(v *big.Int) common.Hash {
	var out common.Hash
	if v == nil {
		return out
	}
	b := v.Bytes()
	copy(out[32-len(b):], b)
	return out
}

func bigToUint256(i *big.Int) *uint256.Int {
	if i == nil {
		return uint256.NewInt(0)
	}
	z, _ := uint256.FromBig(i)
	return z
}

func indexSetToString(v *big.Int) string {
	if v == nil {
		return "<nil>"
	}
	return v.String()
}

func indexSetContains(indexSet *big.Int, index uint32) bool {
	if indexSet == nil {
		return false
	}
	return indexSet.Bit(int(index)) == 1
}

func questionIndexFromPreparedIndex(indexSet *big.Int) (uint32, bool) {
	if indexSet == nil || indexSet.Sign() <= 0 {
		return 0, false
	}
	// QuestionPrepared index must be one-hot.
	if indexSet.BitLen() <= 0 {
		return 0, false
	}
	one := big.NewInt(1)
	tmp := new(big.Int).Sub(new(big.Int).Set(indexSet), one)
	if new(big.Int).And(indexSet, tmp).Sign() != 0 {
		return 0, false
	}
	bit := indexSet.BitLen() - 1
	if bit < 0 {
		return 0, false
	}
	return uint32(bit), true
}

func isIgnoredStakeholder(stakeholder common.Address) bool {
	return stakeholder == getNegRiskAdapterAddress() || stakeholder == getExchangeAddress()
}
