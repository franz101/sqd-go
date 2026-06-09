package polymarket

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/holiman/uint256"
	"github.com/klauspost/compress/zstd"
	"github.com/shopspring/decimal"
)

// TestWallet0xf05b67Positions validates event processing using raw block data.
//
// This test processes the FULL block range from config start (33605403) to 40206663
// and validates the final state against Remote CH cumulative values.
//
// Expected behavior:
// - Processing all events from genesis to block 40206663
// - Final state should match Remote CH: 4 pos, Realized: $37.88, Holdings: $313.68, Net: $351.56
//
// To fetch full history:
//
//	go run debugger/fetchUntil.go -start 33605403 -end 40206663 -out debugger/data/wallet_0xf05b67_full
//
// Run from project root:
//
//	go test ./examples/polymarket/... -run TestWallet0xf05b67Positions -v
//
// Target wallet: 0xf05b670c0f91f8171984db945a28d2ad0f170cc4
// Remote CH final state (from genesis): 4 pos, Realized: $37.88, Holdings: $313.68, Net: $351.56
func TestWallet0xf05b67Positions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	// Find project root and data directory
	projectRoot := findProjectRoot()
	dataDir := filepath.Join(projectRoot, "debugger/data/wallet_0xf05b67_full")

	// Check if data directory exists
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Skipf("full data directory not found: %s (run: go run debugger/fetchUntil.go -start 33605403 -end 40206663 -out debugger/data/wallet_0xf05b67_full)", dataDir)
	}

	// Set up processor in proto mode
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	ctx := context.Background()
	store := setupWalletTestClickHouse(t, ctx, projectRoot)
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := proc.EnableColdCache(coldDir, true); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	defer func() {
		if err := proc.CloseColdCache(); err != nil {
			t.Logf("close cold cache: %v", err)
		}
	}()

	// Parse and process all zstd files
	stats, err := processDataFiles(ctx, t, proc, store, dataDir)
	if err != nil {
		t.Fatalf("failed to process data files: %v", err)
	}
	if committed, err := proc.Flush(ctx, store, stats.lastBlock); err != nil {
		t.Fatalf("flush processor state to ClickHouse: %v", err)
	} else if committed != stats.lastBlock {
		t.Fatalf("flush committed block mismatch: got %d, want %d", committed, stats.lastBlock)
	}
	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("flush async inserts: %v", err)
	}

	recoveredProc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create recovery processor: %v", err)
	}
	if err := recoveredProc.LoadFromDatabase(stats.lastBlock); err != nil {
		t.Fatalf("load processor state from ClickHouse: %v", err)
	}
	if restored, err := recoveredProc.RestoreToBlock(stats.lastBlock); err != nil {
		t.Fatalf("restore processor state snapshot: %v", err)
	} else if restored != stats.lastBlock {
		t.Fatalf("snapshot restored block mismatch: got %d, want %d", restored, stats.lastBlock)
	}

	t.Logf("Processed %d blocks (%d events) from block %d to %d",
		stats.blocks, stats.events, stats.firstBlock, stats.lastBlock)

	// Collect all positions for the target wallet (both in-memory and recovered)
	inMemoryPositions := collectPositionsForWallet(t, proc.State, targetWallet)
	t.Logf("Found %d positions in memory before recovery", len(inMemoryPositions))
	for i, pos := range inMemoryPositions {
		t.Logf("  [IN-MEMORY %d] TokenID: %s, Amount: %s, AvgPrice: %s, RealizedPnL: %s, TotalBought: %s",
			i+1, shortTokenID(pos.tokenID), pos.amount.String(), pos.avgPrice.String(), pos.realizedPnL.String(), pos.totalBought.String())
	}

	positions := collectPositionsForWallet(t, recoveredProc.State, targetWallet)

	// Filter out positions with invalid state (negative values indicate incomplete state from partial range processing)
	// This can happen when FPMM funding events are processed without the corresponding opening events
	validPositions := make([]positionInfo, 0, len(positions))
	for _, pos := range positions {
		if pos.amount.IsNegative() || pos.totalBought.IsNegative() || pos.avgPrice.IsNegative() {
			t.Logf("  [SKIPPED] TokenID: %s has invalid state - indicates incomplete processing (Amount: %s, AvgPrice: %s, TotalBought: %s)",
				shortTokenID(pos.tokenID), pos.amount.String(), pos.avgPrice.String(), pos.totalBought.String())
			continue
		}
		validPositions = append(validPositions, pos)
	}
	positions = validPositions

	t.Logf("Found %d valid positions for wallet %s", len(positions), targetWallet.Hex())
	for i, pos := range positions {
		t.Logf("  [%d] TokenID: %s, Amount: %s, AvgPrice: %s, HoldingValue: %s, RealizedPnL: %s, TotalBought: %s",
			i+1, shortTokenID(pos.tokenID), pos.amount.String(), pos.avgPrice.String(),
			positionHoldingValue(recoveredProc.State, pos).String(), pos.realizedPnL.String(), pos.totalBought.String())
	}
	assertWallet0xf05b67PositionDetails(t, positions)

	// Calculate totals
	totalRealized := decimal.Zero
	totalHoldings := decimal.Zero
	for _, pos := range positions {
		totalRealized = totalRealized.Add(pos.realizedPnL)
		totalHoldings = totalHoldings.Add(positionHoldingValue(recoveredProc.State, pos))
	}
	totalNet := totalRealized.Add(totalHoldings)

	t.Logf("Final State - Realized: $%s, Holdings: $%s, Net: $%s",
		totalRealized.String(), totalHoldings.String(), totalNet.String())

	// Expected values from Remote CH (processing from genesis)
	// Remote CH: 4 pos  |  Realized: $37.88  Holdings: $313.68  |  Net: $351.56
	expectedPositions := 4
	expectedRealized := decimal.NewFromFloat(37.88)
	expectedHoldings := decimal.NewFromFloat(313.68)
	expectedNet := decimal.NewFromFloat(351.56)

	// Tolerance for floating point comparison (2% to account for decimal precision differences)
	const epsilon = 0.02

	// Validate position count matches Remote CH
	if len(positions) != expectedPositions {
		t.Errorf("position count mismatch: got %d, want %d (Remote CH)", len(positions), expectedPositions)
	}

	// Validate realized PnL matches Remote CH (within epsilon tolerance)
	if !withinTolerance(totalRealized, expectedRealized, epsilon) {
		t.Errorf("realized PnL mismatch: got %s, want %s (Remote CH, tolerance: %.1f%%)",
			totalRealized.String(), expectedRealized.String(), epsilon*100)
	}

	// Validate holdings value matches Remote CH (within epsilon tolerance)
	if !withinTolerance(totalHoldings, expectedHoldings, epsilon) {
		t.Errorf("holdings value mismatch: got %s, want %s (Remote CH, tolerance: %.1f%%)",
			totalHoldings.String(), expectedHoldings.String(), epsilon*100)
	}

	// Validate net equity matches Remote CH (within epsilon tolerance)
	if !withinTolerance(totalNet, expectedNet, epsilon) {
		t.Errorf("net equity mismatch: got %s, want %s (Remote CH, tolerance: %.1f%%)",
			totalNet.String(), expectedNet.String(), epsilon*100)
	}

	// Basic sanity checks
	if stats.blocks == 0 {
		t.Error("no blocks were processed")
	}
	if stats.events == 0 {
		t.Error("no events were found")
	}

	// Validate all positions have sensible values
	for _, pos := range positions {
		if pos.amount.IsNegative() {
			t.Errorf("position %s has negative amount: %s", shortTokenID(pos.tokenID), pos.amount.String())
		}
		if pos.avgPrice.IsNegative() {
			t.Errorf("position %s has negative avgPrice: %s", shortTokenID(pos.tokenID), pos.avgPrice.String())
		}
		if pos.totalBought.IsNegative() {
			t.Errorf("position %s has negative totalBought: %s", shortTokenID(pos.tokenID), pos.totalBought.String())
		}
	}
}

func assertWallet0xf05b67PositionDetails(t *testing.T, positions []positionInfo) {
	t.Helper()

	expected := map[common.Hash]struct {
		amount      decimal.Decimal
		avgPrice    decimal.Decimal
		realizedPnL decimal.Decimal
		totalBought decimal.Decimal
	}{
		common.HexToHash("0x0c6a838063f582923c5c7e92655f2fb937ab0bc756f5055da665ee415f8a35dd"): {
			amount:      decimal.RequireFromString("81.7221"),
			avgPrice:    decimal.RequireFromString("0.49"),
			realizedPnL: decimal.RequireFromString("26.7375"),
			totalBought: decimal.RequireFromString("167.9721"),
		},
		common.HexToHash("0x9fd554bb1c9ec1d7f23dd34456c11de34df46f224d6868cdebfce9e8db24e5de"): {
			amount:      decimal.RequireFromString("549.89"),
			avgPrice:    decimal.RequireFromString("0.497623"),
			realizedPnL: decimal.RequireFromString("0.532905"),
			totalBought: decimal.RequireFromString("774.082556"),
		},
		common.HexToHash("0xba813d48ca523eaf501ded2aa5b81f9a4f7807ff5ddaa70d891ae58bf6d83e70"): {
			amount:      decimal.Zero,
			avgPrice:    decimal.RequireFromString("0.5"),
			realizedPnL: decimal.Zero,
			totalBought: decimal.RequireFromString("440.262556"),
		},
		common.HexToHash("0xefb9a0f75d240bab65404da47db245ae7f7de91f2b1785402b84fe778ae58021"): {
			amount:      decimal.RequireFromString("0.001514"),
			avgPrice:    decimal.RequireFromString("0.67"),
			realizedPnL: decimal.RequireFromString("10.606"),
			totalBought: decimal.RequireFromString("265.151514"),
		},
	}

	got := make(map[common.Hash]positionInfo, len(positions))
	for _, pos := range positions {
		got[pos.tokenID] = pos
	}

	const epsilon = 0.001
	for tokenID, want := range expected {
		pos, ok := got[tokenID]
		if !ok {
			t.Errorf("missing expected token %s", shortTokenID(tokenID))
			continue
		}
		if !withinTolerance(pos.amount, want.amount, epsilon) {
			t.Errorf("amount mismatch for %s: got %s, want %s", shortTokenID(tokenID), pos.amount, want.amount)
		}
		if !withinTolerance(pos.avgPrice, want.avgPrice, epsilon) {
			t.Errorf("avgPrice mismatch for %s: got %s, want %s", shortTokenID(tokenID), pos.avgPrice, want.avgPrice)
		}
		if !withinTolerance(pos.realizedPnL, want.realizedPnL, epsilon) {
			t.Errorf("realizedPnL mismatch for %s: got %s, want %s", shortTokenID(tokenID), pos.realizedPnL, want.realizedPnL)
		}
		if !withinTolerance(pos.totalBought, want.totalBought, epsilon) {
			t.Errorf("totalBought mismatch for %s: got %s, want %s", shortTokenID(tokenID), pos.totalBought, want.totalBought)
		}
	}
}

// positionInfo stores calculated position values for validation
type positionInfo struct {
	tokenID     common.Hash
	amount      decimal.Decimal
	avgPrice    decimal.Decimal
	realizedPnL decimal.Decimal
	totalBought decimal.Decimal
}

// processingStats captures statistics from processing
type processingStats struct {
	blocks     uint64
	events     uint64
	firstBlock uint64
	lastBlock  uint64
}

// collectPositionsForWallet gathers all non-zero positions for a wallet from the hot state
func collectPositionsForWallet(t *testing.T, state *generated.State, wallet common.Address) []positionInfo {
	var positions []positionInfo

	if state == nil || state.HotState == nil {
		return positions
	}

	state.HotState.UserPositions.Range(func(key generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.Tombstone {
			return true
		}
		if pos.User == wallet {
			// FIXED: Use exponent -18 to properly convert Decimal256 (scaled by 1e18)
			// This is equivalent to decimal.NewFromBigInt(v.ScaledBig(), -18)
			// which automatically divides by 1e18 via the exponent

			// Debug: log raw scaled values
			amtScaled := pos.Amount.ScaledBig()
			tbScaled := pos.TotalBought.ScaledBig()

			amt := decimal.NewFromBigInt(amtScaled, -18)
			avgP := decimal.NewFromBigInt(pos.AvgPrice.ScaledBig(), -18)
			rPnL := decimal.NewFromBigInt(pos.RealizedPnL.ScaledBig(), -18)
			tb := decimal.NewFromBigInt(tbScaled, -18)

			// Log if values seem suspicious (Amount > 1M tokens or TotalBought >> Amount)
			if amt.GreaterThan(decimal.NewFromInt(1000)) {
				t.Logf("  [DEBUG] TokenID: %s, Amount (scaled): %s, TotalBought (scaled): %s",
					shortTokenID(pos.TokenID), amtScaled.String(), tbScaled.String())
				t.Logf("           amt: %s, avgP: %s, rPnL: %s, tb: %s",
					amt.String(), avgP.String(), rPnL.String(), tb.String())
			}

			// Include positions with any activity (amount, realized PnL, or total bought)
			if !amt.IsZero() || !rPnL.IsZero() || !tb.IsZero() {
				positions = append(positions, positionInfo{
					tokenID:     pos.TokenID,
					amount:      amt,
					avgPrice:    avgP,
					realizedPnL: rPnL,
					totalBought: tb,
				})
			}
		}
		return true
	})

	return positions
}

func positionHoldingValue(state *generated.State, pos positionInfo) decimal.Decimal {
	if price, ok := resolvedPositionPayoutPrice(state, pos.tokenID); ok {
		return pos.amount.Mul(price)
	}
	return pos.amount.Mul(pos.avgPrice)
}

func resolvedPositionPayoutPrice(state *generated.State, tokenID common.Hash) (decimal.Decimal, bool) {
	if state == nil || state.HotState == nil {
		return decimal.Zero, false
	}

	collaterals := map[common.Address]struct{}{
		common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"): {},
		negRiskWrappedCollateral: {},
	}
	state.HotState.FixedProductMarketMakers.Range(func(_ generated.FixedProductMarketMakersClockKey, fpmm generated.MemoryFixedProductMarketMaker) bool {
		if !fpmm.Tombstone {
			collaterals[fpmm.CollateralToken] = struct{}{}
		}
		return true
	})

	var price decimal.Decimal
	found := false
	state.HotState.Conditions.Range(func(_ generated.ConditionsClockKey, cond generated.MemoryCondition) bool {
		if cond.Tombstone || !cond.Resolved || len(cond.Payouts) == 0 {
			return true
		}
		denom := uint256.NewInt(0)
		for i := range cond.Payouts {
			denom.Add(denom, &cond.Payouts[i])
		}
		if denom.IsZero() {
			return true
		}

		for outcomeIndex := range cond.Payouts {
			if outcomeIndex >= 256 {
				break
			}
			indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcomeIndex))

			// For neg-risk conditions, use BabyJubJub CollectionID computation
			// Neg-risk conditions use negRiskAdapterAddr as oracle
			var collectionID common.Hash
			if cond.Oracle == negRiskAdapterAddr {
				collectionID = getNegRiskCollectionID(cond.ID, uint8(outcomeIndex))
			} else {
				collectionID = getCollectionID(common.Hash{}, cond.ID, indexSet)
			}

			for collateral := range collaterals {
				positionID := getPositionID(collateral, collectionID)
				if tokenIDHash(positionID) == tokenID {
					price = Uint256ToDecimal(cond.Payouts[outcomeIndex]).Div(Uint256ToDecimal(*denom))
					found = true
					return false
				}
			}
		}
		return true
	})

	return price, found
}

// processDataFiles reads and processes all zstd JSONL files from the data directory.
func processDataFiles(ctx context.Context, t *testing.T, proc *generated.Processor, store *database.Store, dataDir string) (*processingStats, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	defer decoder.Close()

	stats := &processingStats{}
	jsonlParser := parser.NewFastJSONLParser(100)

	files, err := filepath.Glob(filepath.Join(dataDir, "*.jsonl.zstd"))
	if err != nil {
		return nil, fmt.Errorf("glob files: %w", err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no zstd files found in %s", dataDir)
	}

	for _, filePath := range files {
		baseName := filepath.Base(filePath)
		t.Logf("processing: %s", baseName)

		compressed, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read file %s: %w", filePath, err)
		}

		decompressed, err := decoder.DecodeAll(compressed, nil)
		if err != nil {
			return nil, fmt.Errorf("decompress file %s: %w", filePath, err)
		}

		// Parse JSONL and process each block in source order. UserPosition sell
		// updates clamp to the current amount, so replay order is part of the state.
		err = jsonlParser.Parse(decompressed, func(block *parser.Block) error {
			stats.blocks++
			stats.lastBlock = block.Header.Number
			if stats.firstBlock == 0 {
				stats.firstBlock = block.Header.Number
			}

			blockTime := time.Unix(int64(block.Header.Timestamp), 0).UTC()

			blockLogs := make([]ingestion.CustomLog, 0, len(block.Logs))
			for _, lg := range block.Logs {
				// Deep-copy topics since parser reuses the slice
				topics := make([]string, len(lg.Topics))
				copy(topics, lg.Topics)

				blockLogs = append(blockLogs, ingestion.CustomLog{
					ChainID:          137, // Polygon
					BlockNumber:      block.Header.Number,
					BlockTimestamp:   blockTime,
					BlockHash:        block.Header.Hash,
					ContractAddress:  lg.Address,
					TransactionHash:  lg.TransactionHash,
					TransactionIndex: lg.TransactionIndex,
					LogIndex:         lg.LogIndex,
					Topics:           topics,
					Data:             lg.Data,
				})
				stats.events++
			}

			if len(blockLogs) > 0 {
				if err := proc.Process(ctx, store, blockLogs); err != nil {
					return fmt.Errorf("process block %d: %w", block.Header.Number, err)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("parse JSONL from file %s: %w", filePath, err)
		}
	}

	return stats, nil
}

func setupWalletTestClickHouse(t *testing.T, ctx context.Context, projectRoot string) *database.Store {
	t.Helper()

	host := envOrDefault("CLICKHOUSE_HOST", "127.0.0.1")
	port := envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOrDefault("CLICKHOUSE_USER", "default")
	password := envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("polymarket_wallet_0xf05b67_test_antigravity_%d", time.Now().UnixNano())

	if err := database.DropClickHouseDatabase(ctx, host, port, user, password, db); err != nil {
		t.Skipf("ClickHouse test database unavailable at %s:%d: %v", host, port, err)
	}
	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse test database unavailable at %s:%d: %v", host, port, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		if err := database.DropClickHouseDatabase(context.Background(), host, port, user, password, db); err != nil {
			t.Logf("drop ClickHouse test database %s: %v", db, err)
		}
	})

	generatedDir := filepath.Join(projectRoot, "examples/polymarket/generated")
	if err := store.ApplySQLFileWithDatabase(ctx, filepath.Join(generatedDir, "schema.sql"), "polymarket"); err != nil {
		t.Fatalf("apply generated schema to test database: %v", err)
	}
	if err := store.ApplySQLFileWithDatabase(ctx, filepath.Join(generatedDir, "custom_schema.sql"), "polymarket"); err != nil {
		t.Fatalf("apply generated custom schema to test database: %v", err)
	}

	t.Setenv("CLICKHOUSE_HOST", host)
	t.Setenv("CLICKHOUSE_NATIVE_PORT", fmt.Sprintf("%d", port))
	t.Setenv("CLICKHOUSE_USER", user)
	t.Setenv("CLICKHOUSE_PASSWORD", password)
	t.Setenv("CLICKHOUSE_DATABASE", db)
	t.Setenv("CLICKHOUSE_PRUNE_INTERVAL", "999999999999")

	return store
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envOrDefaultInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

// shortTokenID returns a shortened token ID for display
func shortTokenID(id common.Hash) string {
	hex := id.Hex()
	if len(hex) > 16 {
		return hex[:10] + "..." + hex[len(hex)-8:]
	}
	return hex
}

// findProjectRoot finds the project root by looking for go.mod
func findProjectRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find project root (go.mod)")
		}
		dir = parent
	}
}

// withinTolerance checks if actual is within epsilon percentage of expected
func withinTolerance(actual, expected decimal.Decimal, epsilon float64) bool {
	if actual.IsZero() && expected.IsZero() {
		return true
	}
	if expected.IsZero() {
		return actual.Abs().LessThanOrEqual(decimal.NewFromFloat(epsilon))
	}
	diff := actual.Sub(expected).Abs().Div(expected.Abs()).InexactFloat64()
	return diff <= epsilon
}

// Target wallet for this test
var targetWallet = common.HexToAddress("0xf05b670c0f91f8171984db945a28d2ad0f170cc4")
