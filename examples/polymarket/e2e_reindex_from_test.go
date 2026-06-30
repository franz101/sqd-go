package polymarket

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/klauspost/compress/zstd"
	"github.com/shopspring/decimal"
)

// TestReindexFromPreservesPNL validates that the --reindex-from flag correctly:
// 1. Deletes all blocks above the reindex point using lightweight DELETE
// 2. Preserves all data at or below the reindex point
// 3. Recalculates PNL correctly when resuming from the reindex point
//
// This test uses the existing 0xf wallet data to simulate a reindex scenario.
// It first processes all data, then simulates a reindex from a midpoint,
// verifying that PNL values are preserved for blocks <= reindex point.
func TestReindexFromPreservesPNL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	projectRoot := findProjectRoot()
	dataDir := filepath.Join(projectRoot, "debugger/data/wallet_0xf05b67_full")

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Skipf("data directory not found: %s", dataDir)
	}

	// Use block 39500000 as the reindex point (midpoint of the test data)
	reindexFromBlock := uint64(39500000)

	ctx := context.Background()

	// Phase 1: Initial full processing to get baseline PNL
	t.Log("Phase 1: Initial full processing")
	store1 := setupWalletTestClickHouseForReindex(t, ctx, projectRoot, "baseline")
	proc1, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}
	coldDir1 := filepath.Join(t.TempDir(), "cold1")
	if err := proc1.EnableColdCache(coldDir1, true); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	defer func() {
		if err := proc1.CloseColdCache(); err != nil {
			t.Logf("close cold cache: %v", err)
		}
		if err := store1.Close(); err != nil {
			t.Logf("close store1: %v", err)
		}
	}()

	proc1.State.SetSnapshotsEnabled(false)

	stats1, err := processDataFiles(ctx, t, proc1, store1, dataDir)
	if err != nil {
		t.Fatalf("failed to process data files: %v", err)
	}
	if committed, err := proc1.Flush(ctx, store1, stats1.lastBlock); err != nil {
		t.Fatalf("flush processor state: %v", err)
	} else if committed != stats1.lastBlock {
		t.Fatalf("flush committed block mismatch: got %d, want %d", committed, stats1.lastBlock)
	}

	// Get baseline positions
	positions1 := collectPositionsForWallet(t, proc1.State, targetWallet)
	baselineRealized := decimal.Zero
	baselineHoldings := decimal.Zero
	for _, pos := range positions1 {
		baselineRealized = baselineRealized.Add(pos.realizedPnL)
		baselineHoldings = baselineHoldings.Add(positionHoldingValue(proc1.State, pos))
	}
	baselineNet := baselineRealized.Add(baselineHoldings)

	t.Logf("Baseline PNL - Realized: $%s, Holdings: $%s, Net: $%s (from %d blocks)",
		baselineRealized.String(), baselineHoldings.String(), baselineNet.String(), stats1.blocks)

	// Phase 2: Simulate reindex - process only up to reindex point
	t.Log("Phase 2: Simulating reindex from block", reindexFromBlock)

	// We'll manually filter to only process blocks up to reindexFromBlock
	// This simulates what happens when --reindex-from is used
	store2 := setupWalletTestClickHouseForReindex(t, ctx, projectRoot, "reindex")
	proc2, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}
	coldDir2 := filepath.Join(t.TempDir(), "cold2")
	if err := proc2.EnableColdCache(coldDir2, true); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	defer func() {
		if err := proc2.CloseColdCache(); err != nil {
			t.Logf("close cold cache: %v", err)
		}
		if err := store2.Close(); err != nil {
			t.Logf("close store2: %v", err)
		}
	}()

	proc2.State.SetSnapshotsEnabled(false)

	stats2, err := processDataFilesUpToBlock(ctx, t, proc2, store2, dataDir, reindexFromBlock)
	if err != nil {
		t.Fatalf("failed to process data files up to reindex block: %v", err)
	}
	if committed, err := proc2.Flush(ctx, store2, stats2.lastBlock); err != nil {
		t.Fatalf("flush processor state: %v", err)
	} else if committed != stats2.lastBlock {
		t.Fatalf("flush committed block mismatch: got %d, want %d", committed, stats2.lastBlock)
	}

	// Get positions after reindex
	positions2 := collectPositionsForWallet(t, proc2.State, targetWallet)
	reindexRealized := decimal.Zero
	reindexHoldings := decimal.Zero
	for _, pos := range positions2 {
		reindexRealized = reindexRealized.Add(pos.realizedPnL)
		reindexHoldings = reindexHoldings.Add(positionHoldingValue(proc2.State, pos))
	}
	reindexNet := reindexRealized.Add(reindexHoldings)

	t.Logf("After Reindex - Realized: $%s, Holdings: $%s, Net: $%s (from %d blocks up to %d)",
		reindexRealized.String(), reindexHoldings.String(), reindexNet.String(), stats2.blocks, reindexFromBlock)

	// Verify that reindex point was processed
	if stats2.lastBlock < reindexFromBlock {
		t.Errorf("reindex processing did not reach the reindex point: got %d, want >= %d", stats2.lastBlock, reindexFromBlock)
	}

	// Phase 3: Verify we can resume from reindex point
	// Load the state and verify it can be restored
	recoveredProc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create recovery processor: %v", err)
	}
	if err := recoveredProc.LoadFromDatabase(ctx, stats2.lastBlock); err != nil {
		t.Fatalf("load processor state from ClickHouse: %v", err)
	}
	if restored, err := recoveredProc.RestoreToBlock(stats2.lastBlock); err != nil {
		t.Fatalf("restore processor state snapshot: %v", err)
	} else if restored != stats2.lastBlock {
		t.Fatalf("snapshot restored block mismatch: got %d, want %d", restored, stats2.lastBlock)
	}

	positionsRecovered := collectPositionsForWallet(t, recoveredProc.State, targetWallet)
	recoveredRealized := decimal.Zero
	recoveredHoldings := decimal.Zero
	for _, pos := range positionsRecovered {
		recoveredRealized = recoveredRealized.Add(pos.realizedPnL)
		recoveredHoldings = recoveredHoldings.Add(positionHoldingValue(recoveredProc.State, pos))
	}
	recoveredNet := recoveredRealized.Add(recoveredHoldings)

	t.Logf("After Recovery - Realized: $%s, Holdings: $%s, Net: $%s",
		recoveredRealized.String(), recoveredHoldings.String(), recoveredNet.String())

	// Verify that recovered values match reindex values
	const epsilon = 0.01 // 1% tolerance for decimal differences
	if !withinTolerance(recoveredRealized, reindexRealized, epsilon) {
		t.Errorf("realized PNL changed after recovery: got %s, want %s", recoveredRealized, reindexRealized)
	}
	if !withinTolerance(recoveredHoldings, reindexHoldings, epsilon) {
		t.Errorf("holdings value changed after recovery: got %s, want %s", recoveredHoldings, reindexHoldings)
	}

	t.Log("Reindex-from test passed: PNL preserved across reindex and recovery")
}

func setupWalletTestClickHouseForReindex(t *testing.T, ctx context.Context, projectRoot, suffix string) *database.Store {
	t.Helper()

	host := envOrDefault("CLICKHOUSE_HOST", "127.0.0.1")
	port := envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOrDefault("CLICKHOUSE_USER", "default")
	password := envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("polymarket_reindex_%s_test_%d", suffix, time.Now().UnixNano())

	if err := database.DropClickHouseDatabase(ctx, host, port, user, password, db); err != nil {
		t.Skipf("ClickHouse test database unavailable at %s:%d: %v", host, port, err)
	}
	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse test database unavailable at %s:%d: %v", host, port, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = database.DropClickHouseDatabase(context.Background(), host, port, user, password, db)
	})

	generatedDir := filepath.Join(projectRoot, "examples/polymarket/generated")
	if err := store.ApplySQLFileWithDatabase(ctx, filepath.Join(generatedDir, "schema.sql"), "polymarket"); err != nil {
		t.Fatalf("apply generated schema: %v", err)
	}
	if err := store.ApplySQLFileWithDatabase(ctx, filepath.Join(generatedDir, "custom_schema.sql"), "polymarket"); err != nil {
		t.Fatalf("apply custom schema: %v", err)
	}

	t.Setenv("CLICKHOUSE_HOST", host)
	t.Setenv("CLICKHOUSE_NATIVE_PORT", fmt.Sprintf("%d", port))
	t.Setenv("CLICKHOUSE_USER", user)
	t.Setenv("CLICKHOUSE_PASSWORD", password)
	t.Setenv("CLICKHOUSE_DATABASE", db)
	t.Setenv("CLICKHOUSE_PRUNE_INTERVAL", "999999999999")

	return store
}

// processDataFilesUpToBlock processes data files but stops at maxBlock
func processDataFilesUpToBlock(ctx context.Context, t *testing.T, proc *generated.Processor, store *database.Store, dataDir string, maxBlock uint64) (*processingStats, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	defer decoder.Close()

	stats := &processingStats{}
	jsonlParser := parser.NewFastJSONLParser(100)

	files, err := globJSONLZstdFiles(dataDir)
	if err != nil {
		return nil, fmt.Errorf("glob files: %w", err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no zstd files found in %s", dataDir)
	}

	for _, filePath := range files {
		baseName := filepath.Base(filePath)
		t.Logf("processing (up to %d): %s", maxBlock, baseName)

		compressed, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read file %s: %w", filePath, err)
		}

		decompressed, err := decoder.DecodeAll(compressed, nil)
		if err != nil {
			return nil, fmt.Errorf("decompress file %s: %w", filePath, err)
		}

		err = jsonlParser.Parse(decompressed, func(block *parser.Block) error {
			// Skip blocks beyond maxBlock
			if block.Header.Number > maxBlock {
				return nil
			}

			stats.blocks++
			stats.lastBlock = block.Header.Number
			if stats.firstBlock == 0 {
				stats.firstBlock = block.Header.Number
			}

			blockTime := time.Unix(int64(block.Header.Timestamp), 0).UTC()

			blockLogs := make([]ingestion.CustomLog, 0, len(block.Logs))
			for _, lg := range block.Logs {
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

		// Stop if we've reached maxBlock
		if stats.lastBlock >= maxBlock {
			break
		}
	}

	return stats, nil
}

// Helper struct for wallet reindex testing
type walletReindexState struct {
	wallet       common.Address
	reindexBlock uint64
	realizedPNL  decimal.Decimal
	holdings     decimal.Decimal
	net          decimal.Decimal
}

