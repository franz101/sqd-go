package experiment

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// ============================================================================
// Self-contained PnL replay for wallet 0xa0932d9 using SQD JSONL.
//
// All event decoding and business logic is inlined here (matches
// examples/polymarket/custom_processor.go + generated/state.go).
// No imports from examples/ or internal/.
//
// Reference: Polymarket API $142.19 (all-time, includes events outside range).
// ============================================================================

var (
	wallet          = common.HexToAddress("0xa0932d9aa1ca003376d1237c799efacb302a1198")
	million         = decimal.NewFromInt(1_000_000)
	fiftyCents      = decimal.NewFromFloat(0.5)
	usdc            = common.HexToAddress("0x2791bca1f2de4661ed88a30c99a7a9449aa84174")
	negRiskAdapter  = common.HexToAddress("0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296")
	negRiskExchange = common.HexToAddress("0xC5d563A36AE78145C45a50134d48A1215220f80a")
	exchangeAddr    = common.HexToAddress("0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E")

	// BN254 curve constants for GetCollectionID
	ctP, _            = new(big.Int).SetString("21888242871839275222246405745257275088696311157297823662689037894645226208583", 10)
	ctB               = big.NewInt(3)
	ctSqrtExponent, _ = new(big.Int).SetString("5472060717959818805561601436314318772174077789324455915672259473661306552146", 10)
)

// ── Condition ────────────────────────────────────────────────────
type condition struct {
	id               common.Hash
	oracle           common.Address
	questionID       common.Hash
	outcomeSlotCount int
	resolved         bool
	payouts          []uint256.Int
}

// ── Position ─────────────────────────────────────────────────────
type userPosition struct {
	amount      decimal.Decimal
	avgPrice    decimal.Decimal
	realizedPnL decimal.Decimal
	totalBought decimal.Decimal
}

// ── Keccak-256 helpers ───────────────────────────────────────────
func keccak(data []byte) []byte { return crypto.Keccak256(data) }

func hashConditionAndIndexSet(conditionID common.Hash, indexSet uint256.Int) *big.Int {
	var buf [64]byte
	copy(buf[0:32], conditionID[:])
	indexSet.WriteToSlice(buf[32:64])
	h := new(big.Int).SetBytes(keccak(buf[:]))
	// low254Mask = 2^254 - 1
	mask := new(big.Int).Lsh(big.NewInt(1), 254)
	mask.Sub(mask, big.NewInt(1))
	h.And(h, mask)
	return h
}

func getBit(z *big.Int, i int) uint {
	if i < 0 || i >= 256 {
		return 0
	}
	return uint(z.Bit(i))
}

func bigToUint256(b *big.Int) uint256.Int {
	var u uint256.Int
	u.SetFromBig(b)
	return u
}

func uint256ToBig(i *uint256.Int) *big.Int {
	b := new(big.Int)
	raw := i.Bytes()
	if len(raw) == 0 {
		return b
	}
	b.SetBytes(raw)
	return b
}

func uint256ToDec(i uint256.Int) decimal.Decimal {
	return decimal.NewFromBigInt(i.ToBig(), 0)
}

// ── GetCollectionID (BN254 elliptic curve) ───────────────────────
func getCollectionID(parentCollectionID common.Hash, conditionID common.Hash, indexSet uint256.Int) common.Hash {
	x1 := hashConditionAndIndexSet(conditionID, indexSet)
	odd := getBit(x1, 255) == 1

	p := ctP
	one := big.NewInt(1)
	b := ctB

	// Find y1 such that y1^2 = x1^3 + b (mod p)
	var y1 *big.Int
	for {
		x1.Add(x1, one)
		x1.Mod(x1, p)
		x1Sq := new(big.Int).Mul(x1, x1)
		x1Sq.Mod(x1Sq, p)
		x1Cu := new(big.Int).Mul(x1Sq, x1)
		x1Cu.Mod(x1Cu, p)
		yy := new(big.Int).Add(x1Cu, b)
		yy.Mod(yy, p)
		y1 = new(big.Int).Exp(yy, ctSqrtExponent, p)
		y1Sq := new(big.Int).Mul(y1, y1)
		y1Sq.Mod(y1Sq, p)
		if y1Sq.Cmp(yy) == 0 {
			break
		}
	}

	if (odd && getBit(y1, 0) == 0) || (!odd && getBit(y1, 0) == 1) {
		y1.Sub(p, y1)
	}

	x2 := new(big.Int).SetBytes(parentCollectionID[:])
	if x2.Sign() != 0 {
		odd = getBit(x2, 254) == 1
		mask := new(big.Int).Lsh(big.NewInt(1), 254)
		mask.Sub(mask, big.NewInt(1))
		x2.And(x2, mask)

		x2Sq := new(big.Int).Mul(x2, x2)
		x2Sq.Mod(x2Sq, p)
		x2Cu := new(big.Int).Mul(x2Sq, x2)
		x2Cu.Mod(x2Cu, p)
		yy := new(big.Int).Add(x2Cu, b)
		yy.Mod(yy, p)
		y2 := new(big.Int).Exp(yy, ctSqrtExponent, p)
		if (odd && getBit(y2, 0) == 0) || (!odd && getBit(y2, 0) == 1) {
			y2.Sub(p, y2)
		}
		y2Sq := new(big.Int).Mul(y2, y2)
		y2Sq.Mod(y2Sq, p)
		if y2Sq.Cmp(yy) != 0 {
			panic("invalid parent collection ID")
		}
		x1, y1 = ecAddBig(x1, y1, x2, y2)
	}

	if getBit(y1, 0) == 1 {
		parity := new(big.Int).Lsh(big.NewInt(1), 254)
		x1.Xor(x1, parity)
	}

	var res common.Hash
	x1.FillBytes(res[:])
	return res
}

func ecAddBig(x1, y1, x2, y2 *big.Int) (*big.Int, *big.Int) {
	// Standard BN curve addition: y^2 = x^3 + 3
	p := ctP
	lambda := new(big.Int).Sub(y2, y1)
	lambda.Mod(lambda, p)
	dx := new(big.Int).Sub(x2, x1)
	dx.Mod(dx, p)
	dxInv := new(big.Int).ModInverse(dx, p)
	lambda.Mul(lambda, dxInv)
	lambda.Mod(lambda, p)

	x3 := new(big.Int).Mul(lambda, lambda)
	x3.Sub(x3, x1)
	x3.Sub(x3, x2)
	x3.Mod(x3, p)

	y3 := new(big.Int).Sub(x1, x3)
	y3.Mul(y3, lambda)
	y3.Sub(y3, y1)
	y3.Mod(y3, p)

	return x3, y3
}

// ── GetPositionID ────────────────────────────────────────────────
func getPositionID(collateral common.Address, collection common.Hash) uint256.Int {
	var buf [52]byte
	copy(buf[0:20], collateral[:])
	copy(buf[20:52], collection[:])
	h := keccak(buf[:])
	return *new(uint256.Int).SetBytes(h)
}

func getPositionIDKey(collateral common.Address, conditionID common.Hash, outcome uint8) string {
	indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcome))
	collID := getCollectionID(common.Hash{}, conditionID, *indexSet)
	posID := getPositionID(collateral, collID)
	return posID.String()
}

// ── Condition state ──────────────────────────────────────────────
var conditions = make(map[common.Hash]*condition)

func getCondition(id common.Hash) *condition {
	// id is already a common.Hash from UnpackLog
	c, ok := conditions[id]
	if !ok {
		return nil
	}
	return c
}

func saveCondition(cond *condition) {
	conditions[cond.id] = cond
}

// ── Position state ───────────────────────────────────────────────
var positions = make(map[string]*userPosition) // key = posID.String()

func getOrCreatePos(key string) *userPosition {
	if up, ok := positions[key]; ok {
		return up
	}
	up := &userPosition{}
	positions[key] = up
	return up
}

func updateBuy(key string, price, amount decimal.Decimal) {
	if amount.IsZero() {
		return
	}
	up := getOrCreatePos(key)
	denom := up.amount.Add(amount)
	if !denom.IsZero() {
		numer := up.avgPrice.Mul(up.amount).Add(price.Mul(amount))
		up.avgPrice = numer.Div(denom)
	}
	up.amount = up.amount.Add(amount)
	up.totalBought = up.totalBought.Add(amount)
}

func updateSell(key string, price, amount decimal.Decimal) {
	up, ok := positions[key]
	if !ok {
		return
	}
	adjAmt := amount
	if adjAmt.GreaterThan(up.amount) {
		adjAmt = up.amount
	}
	if adjAmt.IsZero() {
		return
	}
	pnl := adjAmt.Mul(price.Sub(up.avgPrice))
	up.realizedPnL = up.realizedPnL.Add(pnl)
	up.amount = up.amount.Sub(adjAmt)
}

// ── Event handlers (matching custom_processor.go) ────────────────
func handleOrderFilled(maker common.Address, makerAssetID, takerAssetID,
	makerAmountFilled, takerAmountFilled uint256.Int) {
	if maker != wallet {
		return
	}
	makerFilled := uint256ToDec(makerAmountFilled)
	takerFilled := uint256ToDec(takerAmountFilled)

	var posKey string
	var baseAmount, quoteAmount decimal.Decimal
	var isBuy bool

	if makerAssetID.IsZero() {
		isBuy = true
		posKey = takerAssetID.String()
		baseAmount = takerFilled
		quoteAmount = makerFilled
	} else {
		isBuy = false
		posKey = makerAssetID.String()
		baseAmount = makerFilled
		quoteAmount = takerFilled
	}

	price := decimal.Zero
	if !baseAmount.IsZero() {
		price = quoteAmount.Div(baseAmount)
	}

	if isBuy {
		updateBuy(posKey, price, baseAmount)
	} else {
		updateSell(posKey, price, baseAmount)
	}
}

func handlePositionSplit(stakeholder common.Address, collateralToken common.Address,
	conditionID common.Hash, amountRaw uint256.Int) {
	if stakeholder != wallet {
		return
	}
	c := getCondition(conditionID)
	if c == nil {
		return
	}
	amount := uint256ToDec(amountRaw)
	if amount.IsZero() {
		return
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		key := getPositionIDKey(collateralToken, conditionID, outcome)
		updateBuy(key, fiftyCents, amount)
	}
}

func handlePositionsMerge(stakeholder common.Address, collateralToken common.Address,
	conditionID common.Hash, amountRaw uint256.Int) {
	if stakeholder != wallet {
		return
	}
	c := getCondition(conditionID)
	if c == nil {
		return
	}
	amount := uint256ToDec(amountRaw)
	if amount.IsZero() {
		return
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		key := getPositionIDKey(collateralToken, conditionID, outcome)
		updateSell(key, fiftyCents, amount)
	}
}

func handleConditionPreparation(conditionID common.Hash, oracle common.Address,
	questionID common.Hash, outcomeSlotCount uint256.Int) {
	if outcomeSlotCount.Uint64() != 2 {
		return
	}
	saveCondition(&condition{
		id:               conditionID,
		oracle:           oracle,
		questionID:       questionID,
		outcomeSlotCount: 2,
	})
}

func handleConditionResolution(conditionID common.Hash, payoutNumerators []uint256.Int) {
	c := getCondition(conditionID)
	if c == nil {
		return
	}
	c.resolved = true
	c.payouts = payoutNumerators
	saveCondition(c)
}

func handlePayoutRedemption(redeemer common.Address, collateralToken common.Address,
	conditionID common.Hash, indexSets []uint256.Int) {
	if redeemer == negRiskAdapter || redeemer == exchangeAddr {
		return
	}
	c := getCondition(conditionID)
	if c == nil || !c.resolved || len(c.payouts) == 0 {
		return
	}
	denom := uint256.NewInt(0)
	for _, p := range c.payouts {
		denom.Add(denom, &p)
	}
	if denom.IsZero() {
		return
	}
	denomDec := uint256ToDec(*denom)

	for i := range c.payouts {
		indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(i))
		key := getPositionIDKey(collateralToken, conditionID, uint8(i))
		up, ok := positions[key]
		if !ok || up.amount.IsZero() {
			continue
		}
		price := uint256ToDec(c.payouts[i]).Div(denomDec)
		updateSell(key, price, up.amount)
		_ = indexSet
	}
}

func padAddress(addr common.Address) string {
	return "0x" + common.Bytes2Hex(addr.Bytes())
}

// ── ABI decoders (inlined to avoid importing generated/events.go) ─
func decodeAddress(data string, word int) common.Address {
	off := 2 + word*64
	raw := common.FromHex(data[off : off+64])
	// Address is last 20 bytes of 32-byte word
	return common.BytesToAddress(raw[12:32])
}

func decodeUint256(data string, word int) uint256.Int {
	off := 2 + word*64
	raw := common.FromHex(data[off : off+64])
	return *new(uint256.Int).SetBytes(raw)
}

// ── SQD JSONL types ──────────────────────────────────────────────
type sqdBlock struct {
	Header struct {
		Number    uint64 `json:"number"`
		Hash      string `json:"hash"`
		Timestamp uint64 `json:"timestamp"`
	} `json:"header"`
	Logs []sqdLog `json:"logs"`
}

type sqdLog struct {
	Address          string   `json:"address"`
	Topics           []string `json:"topics"`
	Data             string   `json:"data"`
	TransactionHash  string   `json:"transactionHash"`
	TransactionIndex uint64   `json:"transactionIndex"`
	LogIndex         uint64   `json:"logIndex"`
}

func loadBlocks(t *testing.T, path string) []sqdBlock {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return parseBlocks(data)
}

func loadBlocksOptional(t *testing.T, path string) []sqdBlock {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseBlocks(data)
}

func parseBlocks(data []byte) []sqdBlock {
	var blocks []sqdBlock
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var b sqdBlock
		if err := json.Unmarshal([]byte(line), &b); err != nil {
			panic(fmt.Errorf("parse block: %w", err))
		}
		blocks = append(blocks, b)
	}
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].Header.Number < blocks[j].Header.Number
	})
	return blocks
}

// Known topic0 hashes
const (
	topicOrderFilled    = "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"
	topicConditionPrep  = "0xab3760c3bd2bb38b5bcf54dc79802ed67338b4cf29f3054ded67ed24661e4177"
	topicPositionSplit  = "0x2e6bb91f8cbcda0c93623c54d0403a43514fabc40084ec96b6d5379a74786298"
	topicPositionsMerge = "0x6f13ca62553fcc2bcd2372180a43949c1e4cebba603901ede2f4e14f36b282ca"
	topicResolution     = "0xb44d84d3289691f71497564b85d4233648d9dbae8cbdbb4329f301c3a0185894"
	topicPayoutCTF      = "0x8fdc1c4e5fb4ee3336c8bb7141e39c236a4c58065815af064ed0bbcfeeb43be3"
)

func topicIs(topics []string, want string) bool {
	return len(topics) > 0 && strings.EqualFold(topics[0], want)
}

// ── Test ─────────────────────────────────────────────────────────
func TestPolymarketCustomWalletPnL(t *testing.T) {
	orderfilled := loadBlocks(t, "../tests/wallet_0xa0932d9_orderfilled.jsonl")
	splitmerge := loadBlocks(t, "../tests/wallet_0xa0932d9_split_merge.jsonl")
	condprep := loadBlocks(t, "../tests/wallet_0xa0932d9_condition_prep.jsonl")
	redemption := loadBlocksOptional(t, "../tests/wallet_0xa0932d9_redemption.jsonl")

	seen := make(map[uint64]bool)
	var all []sqdBlock
	for _, blocks := range [][]sqdBlock{condprep, splitmerge, orderfilled, redemption} {
		for _, b := range blocks {
			if !seen[b.Header.Number] {
				seen[b.Header.Number] = true
				all = append(all, b)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Header.Number < all[j].Header.Number
	})

	t.Logf("Blocks: %d (cond=%d sm=%d of=%d)", len(all), len(condprep), len(splitmerge), len(orderfilled))

	var (
		countCondPrep  int
		countSplit     int
		countMerge     int
		countOrderFill int
		countResolve   int
		countRedeem    int
	)

	for _, b := range all {
		for _, lg := range b.Logs {
			addr := common.HexToAddress(lg.Address)
			topics := lg.Topics
			data := lg.Data

			switch {
			case addr == common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045") && topicIs(topics, topicConditionPrep):
				// ConditionPreparation(bytes32 indexed conditionId, address indexed oracle, bytes32 indexed questionId, uint256 outcomeSlotCount)
				conditionID := common.HexToHash(topics[1])
				oracle := common.BytesToAddress(common.HexToHash(topics[2]).Bytes()[12:])
				questionID := common.HexToHash(topics[3])
				outcomeSlotCount := decodeUint256(data, 0)
				handleConditionPreparation(conditionID, oracle, questionID, outcomeSlotCount)
				countCondPrep++

			case addr == exchangeAddr && topicIs(topics, topicOrderFilled):
				// OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint256 makerAssetId, uint256 takerAssetId, uint256 makerAmountFilled, uint256 takerAmountFilled, uint256 fee)
				maker := common.BytesToAddress(common.HexToHash(topics[2]).Bytes()[12:])
				if maker != wallet {
					continue
				}
				// Data: orderHash(32) + makerAssetId(32) + takerAssetId(32) + makerAmountFilled(32) + takerAmountFilled(32) + fee(32)
				makerAssetID := decodeUint256(data, 1)
				takerAssetID := decodeUint256(data, 2)
				makerAmt := decodeUint256(data, 3)
				takerAmt := decodeUint256(data, 4)
				handleOrderFilled(maker, makerAssetID, takerAssetID, makerAmt, takerAmt)
				countOrderFill++

			case addr == common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045") && topicIs(topics, topicPositionSplit):
				// PositionSplit(address indexed stakeholder, address collateralToken, bytes32 indexed parentCollectionId, bytes32 indexed conditionId, uint256[] partition, uint256 amount)
				stakeholder := common.BytesToAddress(common.HexToHash(topics[1]).Bytes()[12:])
				if stakeholder != wallet {
					continue
				}
				collateralToken := decodeAddress(data, 0)
				conditionID := common.HexToHash(topics[3])
				amount := decodeUint256(data, 2)
				handlePositionSplit(stakeholder, collateralToken, conditionID, amount)
				countSplit++

			case addr == common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045") && topicIs(topics, topicPositionsMerge):
				// PositionsMerge(address indexed stakeholder, address collateralToken, bytes32 indexed parentCollectionId, bytes32 indexed conditionId, uint256[] partition, uint256 amount)
				stakeholder := common.BytesToAddress(common.HexToHash(topics[1]).Bytes()[12:])
				if stakeholder != wallet {
					continue
				}
				collateralToken := decodeAddress(data, 0)
				conditionID := common.HexToHash(topics[3])
				amount := decodeUint256(data, 2)
				handlePositionsMerge(stakeholder, collateralToken, conditionID, amount)
				countMerge++

			case addr == common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045") && topicIs(topics, topicResolution):
				// ConditionResolution(bytes32 indexed conditionId, address indexed oracle, bytes32 indexed questionId, uint256 payoutDenominator, uint256[] payoutNumerators)
				conditionID := common.HexToHash(topics[1])
				// payoutDenominator at data word 0, then payoutNumerators offset at word 1
				payoutOffset := decodeUint256(data, 1)
				numPayouts := int(payoutOffset.Uint64())
				payouts := make([]uint256.Int, numPayouts)
				for i := 0; i < numPayouts; i++ {
					payouts[i] = decodeUint256(data, 2+i)
				}
				handleConditionResolution(conditionID, payouts)
				countResolve++

			case addr == common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045") && topicIs(topics, topicPayoutCTF):
				// PayoutRedemption(address indexed redeemer, address indexed collateralToken, bytes32 indexed parentCollectionId, bytes32 conditionId, uint256[] indexSets, uint256 payout)
				redeemer := common.BytesToAddress(common.HexToHash(topics[1]).Bytes()[12:])
				collateralToken := common.BytesToAddress(common.HexToHash(topics[2]).Bytes()[12:])
				conditionID := common.HexToHash(topics[4]) // non-indexed conditionId is at data word 0 after 3 indexed topics
				// Actually conditionId is NOT indexed — check the event sig:
				// PayoutRedemption(address indexed redeemer, address indexed collateralToken, bytes32 indexed parentCollectionId, bytes32 conditionId, uint256[] indexSets, uint256 payout)
				// So indexed: redeemer(t1), collateralToken(t2), parentCollectionId(t3)
				// Data: conditionId(w0), indexSets offset(w1), payout(w2), indexSets data...
				conditionID = common.HexToHash(data[2:66])
				isOffset := decodeUint256(data, 1)
				numIndexSets := int(isOffset.Uint64())
				indexSets := make([]uint256.Int, numIndexSets)
				for i := 0; i < numIndexSets; i++ {
					indexSets[i] = decodeUint256(data, 2+i)
				}
				handlePayoutRedemption(redeemer, collateralToken, conditionID, indexSets)
				countRedeem++
			}
		}
	}

	// ── Results ──────────────────────────────────────────────────
	totalPnLRaw := decimal.Zero
	openCount := 0
	closedCount := 0

	for key, up := range positions {
		pnlUSD := up.realizedPnL.Div(million)
		totalPnLRaw = totalPnLRaw.Add(up.realizedPnL)

		if up.amount.IsZero() {
			closedCount++
		} else {
			openCount++
			t.Logf("  OPEN  pos=%s amt=$%s price=$%s pnl=$%s bought=$%s",
				key[:16],
				up.amount.Div(million).StringFixed(2),
				up.avgPrice.Div(million).StringFixed(6),
				pnlUSD.StringFixed(2),
				up.totalBought.Div(million).StringFixed(2),
			)
		}
	}

	totalPnLUSD := totalPnLRaw.Div(million)

	t.Logf("")
	t.Logf("─────────────── Results ───────────────")
	t.Logf("Events: cond=%d split=%d merge=%d order=%d resolve=%d redeem=%d total=%d",
		countCondPrep, countSplit, countMerge, countOrderFill, countResolve, countRedeem,
		countCondPrep+countSplit+countMerge+countOrderFill+countResolve+countRedeem)
	t.Logf("Positions: %d (closed=%d open=%d)", len(positions), closedCount, openCount)
	t.Logf("Realized PnL: $%s", totalPnLUSD.StringFixed(2))
	t.Logf("Polymarket API: $142.19 (all-time)")
	t.Logf("Conditions: %d", len(conditions))

	// Verify counts
	if countCondPrep != 6 {
		t.Errorf("cond prep: want 6, got %d", countCondPrep)
	}
	if countSplit != 1 {
		t.Errorf("split: want 1, got %d", countSplit)
	}
	if countMerge != 9 {
		t.Errorf("merge: want 9, got %d", countMerge)
	}
	if countOrderFill != 13 {
		t.Errorf("orderfilled: want 13, got %d", countOrderFill)
	}

	t.Logf("")
	t.Logf("Prices: Split/Merge always @ $0.50 per YES/NO")
	t.Logf("OrderFilled prices vary — see output above for open positions")
	_ = fmt.Sprintf // suppress unused import
}
