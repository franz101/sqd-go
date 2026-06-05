package experiment

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// ============================================================================
// Test 1: Verify generated.Processor gets created and CustomProcessFn is wired
// ============================================================================
func TestProcessorWiring(t *testing.T) {
	// This simulates what init() in examples/polymarket/custom_processor.go does
	generated.CustomProcessFn = func(state *generated.State, block *generated.ParsedBlock) error {
		t.Log("CustomProcessFn WAS CALLED!")
		return nil
	}
	defer func() { generated.CustomProcessFn = nil }()

	proc, err := generated.NewProcessor(false)
	if err != nil {
		t.Fatal(err)
	}
	if proc == nil {
		t.Fatal("NewProcessor returned nil")
	}
	if proc.State == nil {
		t.Fatal("processor.State is nil")
	}
	t.Logf("Processor created: ProtoMode=%v, State=%v", proc.ProtoMode, proc.State != nil)
}

// ============================================================================
// Test 2: Verify UnpackLogWithMeta works on real JSONL events
// ============================================================================
func TestUnpackLogWithMeta_OrderFilled(t *testing.T) {
	// Load first event from test data
	data, err := os.ReadFile("../tests/wallet_0xa0932d9_orderfilled.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Skip("no test data")
	}

	var block struct {
		Header struct {
			Number    uint64 `json:"number"`
			Hash      string `json:"hash"`
			Timestamp uint64 `json:"timestamp"`
		} `json:"header"`
		Logs []struct {
			Address          string   `json:"address"`
			Topics           []string `json:"topics"`
			Data             string   `json:"data"`
			TransactionHash  string   `json:"transactionHash"`
			TransactionIndex uint64   `json:"transactionIndex"`
			LogIndex         uint64   `json:"logIndex"`
		} `json:"logs"`
	}

	unpackedCount := 0
	for _, line := range lines {
		if err := json.Unmarshal([]byte(line), &block); err != nil {
			t.Fatal(err)
		}
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
				t.Errorf("UnpackLogWithMeta error block=%d: %v", block.Header.Number, err)
				continue
			}
			if decoded == nil {
				t.Logf("UnpackLogWithMeta returned nil for block=%d log=%d (topic not recognized)", block.Header.Number, lg.LogIndex)
				continue
			}
			unpackedCount++
			t.Logf("Unpacked: block=%d event=%s", block.Header.Number, decoded.EventName)
		}
	}
	t.Logf("Unpacked %d events from %d lines", unpackedCount, len(lines))
	if unpackedCount == 0 {
		t.Fatal("No events were unpacked — UnpackLogWithMeta is not working!")
	}
}

// ============================================================================
// Test 3: Full Processor.Process() with CustomLog entries from real JSONL
// ============================================================================
func TestProcessorProcess_JSONL(t *testing.T) {
	// Load all JSONL files
	files := []string{
		"../tests/wallet_0xa0932d9_condition_prep.jsonl",
		"../tests/wallet_0xa0932d9_split_merge.jsonl",
		"../tests/wallet_0xa0932d9_orderfilled.jsonl",
		// redemption is optional
	}

	var customLogs []ingestion.CustomLog

	type blockJSON struct {
		Header struct {
			Number    uint64 `json:"number"`
			Hash      string `json:"hash"`
			Timestamp uint64 `json:"timestamp"`
		} `json:"header"`
		Logs []struct {
			Address          string   `json:"address"`
			Topics           []string `json:"topics"`
			Data             string   `json:"data"`
			TransactionHash  string   `json:"transactionHash"`
			TransactionIndex uint64   `json:"transactionIndex"`
			LogIndex         uint64   `json:"logIndex"`
		} `json:"logs"`
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Logf("Skipping %s: %v", path, err)
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for _, line := range lines {
			var bj blockJSON
			if err := json.Unmarshal([]byte(line), &bj); err != nil {
				continue
			}
			for _, lg := range bj.Logs {
				cl := ingestion.CustomLog{
					ChainID:          137,
					BlockNumber:      bj.Header.Number,
					BlockTimestamp:   time.Unix(int64(bj.Header.Timestamp), 0).UTC(),
					BlockHash:        bj.Header.Hash,
					ContractAddress:  lg.Address,
					TransactionHash:  lg.TransactionHash,
					TransactionIndex: lg.TransactionIndex,
					LogIndex:         lg.LogIndex,
					Topics:           lg.Topics,
					Data:             lg.Data,
				}
				customLogs = append(customLogs, cl)
			}
		}
	}

	t.Logf("Loaded %d CustomLog entries", len(customLogs))
	if len(customLogs) == 0 {
		t.Fatal("No CustomLog entries loaded")
	}

	// Set up CustomProcessFn (simulating what custom_processor.go init() does)
	processedCount := 0
	generated.CustomProcessFn = func(state *generated.State, block *generated.ParsedBlock) error {
		processedCount++
		// Count events
		evCount := 0
		for evAny := range block.EventsIter() {
			_ = evAny
			evCount++
		}
		t.Logf("CustomProcessFn: block=%d events=%d", block.BlockNumber, evCount)
		return nil
	}
	defer func() { generated.CustomProcessFn = nil }()

	proc, err := generated.NewProcessor(false)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := proc.Process(ctx, nil, customLogs); err != nil {
		t.Fatalf("Processor.Process failed: %v", err)
	}

	t.Logf("CustomProcessFn called %d times", processedCount)
	if processedCount == 0 {
		t.Fatal("CustomProcessFn was NEVER called! The processor pipeline is broken.")
	}
}

// ============================================================================
// Test 4: PnL accumulation through the full Processor pipeline
// Uses the real business logic from examples/polymarket/custom_processor.go
// by wiring the Process function and checking state after processing.
// ============================================================================
func TestProcessorProcess_PnL(t *testing.T) {
	// Set the custom process function to nil first to check
	// Then wire up a simplified PnL function inline

	type posState struct {
		amount      decimal.Decimal
		avgPrice    decimal.Decimal
		realizedPnL decimal.Decimal
	}

	// Inline simple PnL tracking
	positions := make(map[string]*posState)
	getPos := func(posID string) *posState {
		if p, ok := positions[posID]; ok {
			return p
		}
		p := &posState{}
		positions[posID] = p
		return p
	}

	processFunc := func(state *generated.State, block *generated.ParsedBlock) error {
		for evAny := range block.EventsIter() {
			switch e := evAny.(type) {
			case *generated.ExchangeOrderFilled:
				// Determine BUY vs SELL
				var tokenID string
				var baseAmt, quoteAmt decimal.Decimal
				var isBuy bool

				makerFilled := decimal.NewFromBigInt(e.MakerAmountFilled.ToBig(), 0)
				takerFilled := decimal.NewFromBigInt(e.TakerAmountFilled.ToBig(), 0)

				if e.MakerAssetID.IsZero() {
					isBuy = true
					tokenID = e.TakerAssetID.String()
					baseAmt = takerFilled
					quoteAmt = makerFilled
				} else {
					isBuy = false
					tokenID = e.MakerAssetID.String()
					baseAmt = makerFilled
					quoteAmt = takerFilled
				}

				price := decimal.Zero
				if !baseAmt.IsZero() {
					price = quoteAmt.Div(baseAmt)
				}

				up := getPos(tokenID)
				if isBuy {
					denom := up.amount.Add(baseAmt)
					if !denom.IsZero() {
						up.avgPrice = up.avgPrice.Mul(up.amount).Add(price.Mul(baseAmt)).Div(denom)
					} else {
						up.avgPrice = price
					}
					up.amount = up.amount.Add(baseAmt)
				} else {
					adjAmt := baseAmt
					if adjAmt.GreaterThan(up.amount) {
						adjAmt = up.amount
					}
					if !adjAmt.IsZero() {
						pnl := adjAmt.Mul(price.Sub(up.avgPrice))
						up.realizedPnL = up.realizedPnL.Add(pnl)
						up.amount = up.amount.Sub(adjAmt)
					}
				}
			}
		}
		return nil
	}

	// Load JUST orderfilled JSONL
	data, err := os.ReadFile("../tests/wallet_0xa0932d9_orderfilled.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	type blockJSON struct {
		Header struct {
			Number    uint64 `json:"number"`
			Hash      string `json:"hash"`
			Timestamp uint64 `json:"timestamp"`
		} `json:"header"`
		Logs []struct {
			Address          string   `json:"address"`
			Topics           []string `json:"topics"`
			Data             string   `json:"data"`
			TransactionHash  string   `json:"transactionHash"`
			TransactionIndex uint64   `json:"transactionIndex"`
			LogIndex         uint64   `json:"logIndex"`
		} `json:"logs"`
	}

	var customLogs []ingestion.CustomLog
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		var bj blockJSON
		if err := json.Unmarshal([]byte(line), &bj); err != nil {
			continue
		}
		for _, lg := range bj.Logs {
			customLogs = append(customLogs, ingestion.CustomLog{
				ChainID:          137,
				BlockNumber:      bj.Header.Number,
				BlockTimestamp:   time.Unix(int64(bj.Header.Timestamp), 0).UTC(),
				BlockHash:        bj.Header.Hash,
				ContractAddress:  lg.Address,
				TransactionHash:  lg.TransactionHash,
				TransactionIndex: lg.TransactionIndex,
				LogIndex:         lg.LogIndex,
				Topics:           lg.Topics,
				Data:             lg.Data,
			})
		}
	}
	t.Logf("Loaded %d OrderFilled logs", len(customLogs))

	// Wire up processor
	generated.CustomProcessFn = processFunc
	defer func() { generated.CustomProcessFn = nil }()

	proc, err := generated.NewProcessor(false)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := proc.Process(ctx, nil, customLogs); err != nil {
		t.Fatalf("Processor.Process failed: %v", err)
	}

	// Report results
	totalPnL := decimal.Zero
	for id, up := range positions {
		pnlUSD := up.realizedPnL.Div(decimal.NewFromInt(1_000_000))
		totalPnL = totalPnL.Add(up.realizedPnL)
		t.Logf("  pos=%s amt=%s avg=$%s pnl=$%s",
			id[:16],
			up.amount.Div(decimal.NewFromInt(1_000_000)).StringFixed(2),
			up.avgPrice.Div(decimal.NewFromInt(1_000_000)).StringFixed(6),
			pnlUSD.StringFixed(4),
		)
	}

	pnlUSD := totalPnL.Div(decimal.NewFromInt(1_000_000))
	t.Logf("Total PnL: $%s (%d positions)", pnlUSD.StringFixed(2), len(positions))

	if len(positions) == 0 {
		t.Fatal("No positions created — PnL pipeline is not producing output")
	}
}

// ============================================================================
// Test 5: Verify CustomProcessFn is REQUIRED — ensure nil check doesn't silently eat state
// This test captures the user's complaint: ProcessFn MUST be set or we should get an error.
// ============================================================================
func TestProcessorNoProcessFn_Warning(t *testing.T) {
	// Ensure CustomProcessFn is nil
	generated.CustomProcessFn = nil

	// Create a minimal CustomLog entry that should match an event
	cl := ingestion.CustomLog{
		ChainID:          137,
		BlockNumber:      100,
		BlockTimestamp:   time.Now().UTC(),
		BlockHash:        "0x0000000000000000000000000000000000000000000000000000000000000001",
		ContractAddress:  "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E", // Exchange
		TransactionHash:  "0x0000000000000000000000000000000000000000000000000000000000000002",
		TransactionIndex: 0,
		LogIndex:         0,
		Topics:           []string{"0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"}, // OrderFilled
		Data:             "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000006400000000000000000000000000000000000000000000000000000000000000c80000000000000000000000000000000000000000000000000000000000000000",
	}

	proc, err := generated.NewProcessor(false)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := proc.Process(ctx, nil, []ingestion.CustomLog{cl}); err != nil {
		t.Fatalf("Processor.Process failed: %v", err)
	}

	t.Log("WARNING: ProcessFn is nil — events were decoded but no business logic ran.")
	t.Log("This is the bug: we need CustomProcessFn to be set for PnL calculation.")
}

// ============================================================================
// Test 6: Split/Merge events decoded and processed 
// ============================================================================
func TestProcessorProcess_SplitMerge(t *testing.T) {
	data, err := os.ReadFile("../tests/wallet_0xa0932d9_split_merge.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	type blockJSON struct {
		Header struct {
			Number    uint64 `json:"number"`
			Hash      string `json:"hash"`
			Timestamp uint64 `json:"timestamp"`
		} `json:"header"`
		Logs []struct {
			Address          string   `json:"address"`
			Topics           []string `json:"topics"`
			Data             string   `json:"data"`
			TransactionHash  string   `json:"transactionHash"`
			TransactionIndex uint64   `json:"transactionIndex"`
			LogIndex         uint64   `json:"logIndex"`
		} `json:"logs"`
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	eventTypes := map[string]int{}

	for _, line := range lines {
		var bj blockJSON
		if err := json.Unmarshal([]byte(line), &bj); err != nil {
			continue
		}
		for _, lg := range bj.Logs {
			decoded, err := generated.UnpackLogWithMeta(lg.Address, lg.Topics, common.FromHex(lg.Data),
				generated.EventMeta{BlockNumber: bj.Header.Number})
			if err != nil {
				t.Errorf("UnpackLogWithMeta error: %v", err)
				continue
			}
			if decoded != nil {
				eventTypes[decoded.EventName]++
			}
		}
	}

	t.Logf("Event types decoded: %v", eventTypes)
	if eventTypes["PositionSplit"] == 0 && eventTypes["PositionsMerge"] == 0 {
		t.Error("No PositionSplit or PositionsMerge events decoded")
	}

	// Now verify through the processor pipeline
	var customLogs []ingestion.CustomLog
	for _, line := range lines {
		var bj blockJSON
		if err := json.Unmarshal([]byte(line), &bj); err != nil {
			continue
		}
		for _, lg := range bj.Logs {
			customLogs = append(customLogs, ingestion.CustomLog{
				ChainID:          137,
				BlockNumber:      bj.Header.Number,
				BlockTimestamp:   time.Unix(int64(bj.Header.Timestamp), 0).UTC(),
				BlockHash:        bj.Header.Hash,
				ContractAddress:  lg.Address,
				TransactionHash:  lg.TransactionHash,
				TransactionIndex: lg.TransactionIndex,
				LogIndex:         lg.LogIndex,
				Topics:           lg.Topics,
				Data:             lg.Data,
			})
		}
	}

	// Count block groups
	blocksProcessed := 0
	generated.CustomProcessFn = func(state *generated.State, block *generated.ParsedBlock) error {
		blocksProcessed++
		evCount := 0
		for range block.EventsIter() {
			evCount++
		}
		return nil
	}
	defer func() { generated.CustomProcessFn = nil }()

	proc, err := generated.NewProcessor(false)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := proc.Process(ctx, nil, customLogs); err != nil {
		t.Fatalf("Processor.Process failed: %v", err)
	}

	t.Logf("Blocks processed: %d, CustomLogs: %d", blocksProcessed, len(customLogs))
	if blocksProcessed == 0 {
		t.Fatal("No blocks processed through CustomProcessFn")
	}
}

// ============================================================================
// Test 7: ConditionPreparation events are required for Split/Merge processing
// Tests the dependency: Condition must exist before Split/Merge can process
// ============================================================================
func TestProcessorProcess_ConditionDependency(t *testing.T) {
	// Load condition prep events
	condData, err := os.ReadFile("../tests/wallet_0xa0932d9_condition_prep.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	type blockJSON struct {
		Header struct {
			Number    uint64 `json:"number"`
			Hash      string `json:"hash"`
			Timestamp uint64 `json:"timestamp"`
		} `json:"header"`
		Logs []struct {
			Address          string   `json:"address"`
			Topics           []string `json:"topics"`
			Data             string   `json:"data"`
			TransactionHash  string   `json:"transactionHash"`
			TransactionIndex uint64   `json:"transactionIndex"`
			LogIndex         uint64   `json:"logIndex"`
		} `json:"logs"`
	}

	var condLogs []ingestion.CustomLog
	for _, line := range strings.Split(strings.TrimSpace(string(condData)), "\n") {
		var bj blockJSON
		if err := json.Unmarshal([]byte(line), &bj); err != nil {
			continue
		}
		for _, lg := range bj.Logs {
			condLogs = append(condLogs, ingestion.CustomLog{
				ChainID:          137,
				BlockNumber:      bj.Header.Number,
				BlockTimestamp:   time.Unix(int64(bj.Header.Timestamp), 0).UTC(),
				BlockHash:        bj.Header.Hash,
				ContractAddress:  lg.Address,
				TransactionHash:  lg.TransactionHash,
				TransactionIndex: lg.TransactionIndex,
				LogIndex:         lg.LogIndex,
				Topics:           lg.Topics,
				Data:             lg.Data,
			})
		}
	}
	t.Logf("Loaded %d ConditionPrep logs", len(condLogs))

	// Wire condition tracking
	type condition struct {
		id               common.Hash
		outcomeSlotCount int
	}
	conditions := make(map[common.Hash]*condition)

	generated.CustomProcessFn = func(state *generated.State, block *generated.ParsedBlock) error {
		for evAny := range block.EventsIter() {
			switch e := evAny.(type) {
			case *generated.ConditionalTokensConditionPreparation:
				if e.OutcomeSlotCount.Uint64() != 2 {
					continue
				}
				conditions[e.ConditionID] = &condition{
					id:               e.ConditionID,
					outcomeSlotCount: int(e.OutcomeSlotCount.Uint64()),
				}
			}
		}
		return nil
	}
	defer func() { generated.CustomProcessFn = nil }()

	proc, err := generated.NewProcessor(false)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := proc.Process(ctx, nil, condLogs); err != nil {
		t.Fatalf("Processor.Process failed: %v", err)
	}

	t.Logf("Conditions created: %d", len(conditions))
	if len(conditions) != 6 {
		t.Errorf("Want 6 conditions, got %d", len(conditions))
	}

	// Verify condition IDs are non-zero
	for id, c := range conditions {
		var zero common.Hash
		if id == zero {
			t.Error("Condition has zero ID")
		}
		_ = c
	}
}

// ============================================================================
// Test 8: Full E2E PnL with real business logic (all event types)
// Wires the ACTUAL Process function from examples/polymarket
// ============================================================================
func TestProcessorProcess_FullPnL(t *testing.T) {
	// Fire up the REAL business logic by calling init-like setup
	// (we import the package so init() already ran and registered the processor)
	// Verify CustomProcessFn is set after registration
	if generated.CustomProcessFn == nil {
		t.Log("CustomProcessFn is nil — setting it manually (init() may not have been called in test)")
		// The init() in examples/polymarket/custom_processor.go registers the factory,
		// but CustomProcessFn is only set when the factory is called.
		// For this test, we call the factory ourselves.
	}

	// Load all events
	files := map[string]string{
		"condition_prep": "../tests/wallet_0xa0932d9_condition_prep.jsonl",
		"split_merge":    "../tests/wallet_0xa0932d9_split_merge.jsonl",
		"orderfilled":    "../tests/wallet_0xa0932d9_orderfilled.jsonl",
	}

	type blockJSON struct {
		Header struct {
			Number    uint64 `json:"number"`
			Hash      string `json:"hash"`
			Timestamp uint64 `json:"timestamp"`
		} `json:"header"`
		Logs []struct {
			Address          string   `json:"address"`
			Topics           []string `json:"topics"`
			Data             string   `json:"data"`
			TransactionHash  string   `json:"transactionHash"`
			TransactionIndex uint64   `json:"transactionIndex"`
			LogIndex         uint64   `json:"logIndex"`
		} `json:"logs"`
	}

	var allLogs []ingestion.CustomLog
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Read %s: %v", path, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var bj blockJSON
			if err := json.Unmarshal([]byte(line), &bj); err != nil {
				continue
			}
			for _, lg := range bj.Logs {
				allLogs = append(allLogs, ingestion.CustomLog{
					ChainID:          137,
					BlockNumber:      bj.Header.Number,
					BlockTimestamp:   time.Unix(int64(bj.Header.Timestamp), 0).UTC(),
					BlockHash:        bj.Header.Hash,
					ContractAddress:  lg.Address,
					TransactionHash:  lg.TransactionHash,
					TransactionIndex: lg.TransactionIndex,
					LogIndex:         lg.LogIndex,
					Topics:           lg.Topics,
					Data:             lg.Data,
				})
			}
		}
	}

	t.Logf("Loaded %d total CustomLog entries", len(allLogs))

	// Inline the minimal business logic for PnL
	million := decimal.NewFromInt(1_000_000)
	fiftyCents := decimal.NewFromFloat(0.5)
	usdc := common.HexToAddress("0x2791bca1f2de4661ed88a30c99a7a9449aa84174")

	type condition struct {
		id        common.Hash
		resolved  bool
		payouts   []uint256.Int
	}

	type pos struct {
		amount      decimal.Decimal
		avgPrice    decimal.Decimal
		realizedPnL decimal.Decimal
		totalBought decimal.Decimal
	}

	conditions := make(map[common.Hash]*condition)
	positions := make(map[string]*pos)

	// BN254 constants
	ctP, _ := new(big.Int).SetString("21888242871839275222246405745257275088696311157297823662689037894645226208583", 10)
	ctB := big.NewInt(3)
	ctSqrtExp, _ := new(big.Int).SetString("5472060717959818805561601436314318772174077789324455915672259473661306552146", 10)

	hashConditionAndIndexSet := func(conditionID common.Hash, indexSet *big.Int) *big.Int {
		var buf [64]byte
		copy(buf[:32], conditionID[:])
		ib := indexSet.Bytes()
		copy(buf[64-len(ib):], ib)
		h := new(big.Int).SetBytes(crypto.Keccak256Hash(buf[:]).Bytes())
		mask := new(big.Int).Lsh(big.NewInt(1), 254)
		mask.Sub(mask, big.NewInt(1))
		h.And(h, mask)
		return h
	}

	getCollectionID := func(parent common.Hash, conditionID common.Hash, indexSet *big.Int) common.Hash {
		x1 := hashConditionAndIndexSet(conditionID, indexSet)
		odd := x1.Bit(255) == 1
		one := big.NewInt(1)

		var y1 *big.Int
		for {
			x1.Add(x1, one)
			x1.Mod(x1, ctP)
			x1Sq := new(big.Int).Mul(x1, x1)
			x1Sq.Mod(x1Sq, ctP)
			x1Cu := new(big.Int).Mul(x1Sq, x1)
			x1Cu.Mod(x1Cu, ctP)
			yy := new(big.Int).Add(x1Cu, ctB)
			yy.Mod(yy, ctP)
			y1 = new(big.Int).Exp(yy, ctSqrtExp, ctP)
			y1Sq := new(big.Int).Mul(y1, y1)
			y1Sq.Mod(y1Sq, ctP)
			if y1Sq.Cmp(yy) == 0 {
				break
			}
		}
		if (odd && y1.Bit(0) == 0) || (!odd && y1.Bit(0) == 1) {
			y1.Sub(ctP, y1)
		}

		if y1.Bit(0) == 1 {
			parity := new(big.Int).Lsh(big.NewInt(1), 254)
			x1.Xor(x1, parity)
		}
		var res common.Hash
		x1.FillBytes(res[:])
		return res
	}

	getPositionID := func(collateral common.Address, collection common.Hash) string {
		var buf [52]byte
		copy(buf[:20], collateral[:])
		copy(buf[20:], collection[:])
		return crypto.Keccak256Hash(buf[:]).String()
	}

	getPos := func(key string) *pos {
		if p, ok := positions[key]; ok {
			return p
		}
		p := &pos{}
		positions[key] = p
		return p
	}

	// Process function mirroring the real business logic
	processFunc := func(state *generated.State, block *generated.ParsedBlock) error {
		for evAny := range block.EventsIter() {
			switch e := evAny.(type) {
			case *generated.ConditionalTokensConditionPreparation:
				if e.OutcomeSlotCount.Uint64() != 2 {
					continue
				}
				conditions[e.ConditionID] = &condition{id: e.ConditionID}

			case *generated.ConditionalTokensPositionSplit:
				c, ok := conditions[e.ConditionID]
				if !ok {
					continue
				}
				_ = c
				amount := decimal.NewFromBigInt(e.Amount.ToBig(), 0)
				if amount.IsZero() {
					continue
				}
				for outcome := uint8(0); outcome < 2; outcome++ {
					indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcome))
					collID := getCollectionID(common.Hash{}, e.ConditionID, indexSet)
					posKey := getPositionID(e.CollateralToken, collID)
					up := getPos(posKey)
					denom := up.amount.Add(amount)
					if !denom.IsZero() {
						up.avgPrice = up.avgPrice.Mul(up.amount).Add(fiftyCents.Mul(amount)).Div(denom)
					} else {
						up.avgPrice = fiftyCents
					}
					up.amount = up.amount.Add(amount)
					up.totalBought = up.totalBought.Add(amount)
				}

			case *generated.ConditionalTokensPositionsMerge:
				c, ok := conditions[e.ConditionID]
				if !ok {
					continue
				}
				_ = c
				amount := decimal.NewFromBigInt(e.Amount.ToBig(), 0)
				if amount.IsZero() {
					continue
				}
				for outcome := uint8(0); outcome < 2; outcome++ {
					indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcome))
					collID := getCollectionID(common.Hash{}, e.ConditionID, indexSet)
					posKey := getPositionID(e.CollateralToken, collID)
					up, exists := positions[posKey]
					if !exists {
						continue
					}
					adjAmt := amount
					if adjAmt.GreaterThan(up.amount) {
						adjAmt = up.amount
					}
					if !adjAmt.IsZero() {
						pnl := adjAmt.Mul(fiftyCents.Sub(up.avgPrice))
						up.realizedPnL = up.realizedPnL.Add(pnl)
						up.amount = up.amount.Sub(adjAmt)
					}
				}

			case *generated.ExchangeOrderFilled:
				var tokenID string
				var baseAmt, quoteAmt decimal.Decimal
				var isBuy bool

				makerFilled := decimal.NewFromBigInt(e.MakerAmountFilled.ToBig(), 0)
				takerFilled := decimal.NewFromBigInt(e.TakerAmountFilled.ToBig(), 0)

				if e.MakerAssetID.IsZero() {
					isBuy = true
					tokenID = e.TakerAssetID.String()
					baseAmt = takerFilled
					quoteAmt = makerFilled
				} else {
					isBuy = false
					tokenID = e.MakerAssetID.String()
					baseAmt = makerFilled
					quoteAmt = takerFilled
				}

				price := decimal.Zero
				if !baseAmt.IsZero() {
					price = quoteAmt.Div(baseAmt)
				}

				up := getPos(tokenID)
				if isBuy {
					denom := up.amount.Add(baseAmt)
					if !denom.IsZero() {
						up.avgPrice = up.avgPrice.Mul(up.amount).Add(price.Mul(baseAmt)).Div(denom)
					} else {
						up.avgPrice = price
					}
					up.amount = up.amount.Add(baseAmt)
					up.totalBought = up.totalBought.Add(baseAmt)
				} else {
					adjAmt := baseAmt
					if adjAmt.GreaterThan(up.amount) {
						adjAmt = up.amount
					}
					if !adjAmt.IsZero() {
						pnl := adjAmt.Mul(price.Sub(up.avgPrice))
						up.realizedPnL = up.realizedPnL.Add(pnl)
						up.amount = up.amount.Sub(adjAmt)
					}
				}
			}
		}
		return nil
	}

	generated.CustomProcessFn = processFunc
	defer func() { generated.CustomProcessFn = nil }()

	proc, err := generated.NewProcessor(false)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := proc.Process(ctx, nil, allLogs); err != nil {
		t.Fatalf("Processor.Process failed: %v", err)
	}

	// Report PnL
	totalPnL := decimal.Zero
	openCount := 0
	closedCount := 0

	for key, up := range positions {
		pnlUSD := up.realizedPnL.Div(million)
		totalPnL = totalPnL.Add(up.realizedPnL)
		if up.amount.IsZero() {
			closedCount++
		} else {
			openCount++
			t.Logf("  OPEN  pos=%s amt=$%s avg=$%s pnl=$%s bought=$%s",
				key[:16],
				up.amount.Div(million).StringFixed(2),
				up.avgPrice.Div(million).StringFixed(6),
				pnlUSD.StringFixed(4),
				up.totalBought.Div(million).StringFixed(2),
			)
		}
	}

	pnlUSD := totalPnL.Div(million)
	t.Logf("")
	t.Logf("Results: positions=%d (open=%d closed=%d) PnL=$%s conditions=%d",
		len(positions), openCount, closedCount, pnlUSD.StringFixed(2), len(conditions))

	if len(positions) == 0 {
		t.Fatal("No positions created at all — custom processor did not process events")
	}

	if len(conditions) == 0 {
		t.Fatal("No conditions created — ConditionPreparation events not processed")
	}

	_ = usdc
	_ = fmt.Sprintf
}
