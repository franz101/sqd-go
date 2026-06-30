//go:build e2e
// +build e2e

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
	"github.com/shopspring/decimal"
)

// TestWallet0x10f5b9bd_PnL validates the polymarket pipeline PnL for wallet
// 0x10f5b9bd80fc212b718c5dced42f0cff57a6c701 against the Polymarket data-api.
//
// It processes the 149 blocks where this wallet had activity
// (debugger/data/wallet_0x10f5b9bd_full/) and asserts the computed PnL
// matches the data-api reference: -$66.59.
//
// The reference is computed by scripts/polymarket_pnl.py which aggregates:
//   - closed-positions: sum of all realizedPnl (trade-level, no dedup)
//   - open positions: sum of cashPnl (unrealized) + realizedPnl
//   - total = closed_realized + open_cash + open_realized
//
// To fetch block data (full history from config start to head):
//
//	go run debugger/fetchUntil.go -endpoint https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream -start 33605403 -end 40206663 -out debugger/data/wallet_0x10f5b9bd_full
//
// Run test:
//
//	go test ./examples/polymarket/... -run TestWallet0x10f5b9bd_PnL -v
func TestWallet0x10f5b9bd_PnL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	wallet := common.HexToAddress("0x10f5b9bd80fc212b718c5dced42f0cff57a6c701")

	// Find project root and data directory
	projectRoot := findProjectRoot()
	dataDir := filepath.Join(projectRoot, "debugger/data/wallet_0x10f5b9bd_full")

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Skipf("data directory not found: %s (run: go run debugger/fetchBlocks.go -blocks debugger/data/wallet_0x10f5b9bd_blocks.txt -out debugger/data/wallet_0x10f5b9bd_full)", dataDir)
	}

	// Set up processor in proto mode
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	ctx := context.Background()
	store := setupTestClickHouse(t, ctx, projectRoot, "polymarket_wallet_0x10f5b9bd_e2e")
	defer store.Close()
	// Wire the durable store so the authoritative cold path (PositionState.GetValue)
	// can resolve positions that spill out of the bounded hot/cold tiers from
	// ClickHouse; without it every evicted-then-retouched position resets to zero.
	proc.State.Store = store

	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := proc.EnableColdCache(coldDir, true); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	defer func() {
		if err := proc.CloseColdCache(); err != nil {
			t.Logf("close cold cache: %v", err)
		}
	}()

	proc.State.SetSnapshotsEnabled(false)

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

	t.Logf("Processed %d blocks (%d events) from block %d to %d",
		stats.blocks, stats.events, stats.firstBlock, stats.lastBlock)

	// Collect positions for the target wallet from the hot state
	positions := collectPositionsForWallet(t, proc.State, wallet)

	t.Logf("Found %d positions for wallet %s", len(positions), wallet.Hex())
	for i, pos := range positions {
		// Find the resolved price for this position
		resolvedPrice := decimal.Zero
		hasResolved := false
		if price, ok := resolvedPositionPayoutPrice(proc.State, pos.tokenID); ok {
			resolvedPrice = price
			hasResolved = true
		}
		t.Logf("  [%d] TokenID: %s, Amount: %s, AvgPrice: %s, RealizedPnL: %s, TotalBought: %s, ResolvedPrice: %s (hasResolved: %v)",
			i+1, shortTokenID(pos.tokenID), pos.amount.String(), pos.avgPrice.String(),
			pos.realizedPnL.String(), pos.totalBought.String(), resolvedPrice.String(), hasResolved)
	}

	// Calculate totals
	// PnL = realized_pnl (all positions) + cashPnl (unrealized: totalBought * (payout - avgPrice))
	totalRealized := decimal.Zero
	totalHoldings := decimal.Zero
	for _, pos := range positions {
		totalRealized = totalRealized.Add(pos.realizedPnL)
		totalHoldings = totalHoldings.Add(positionHoldingValue(proc.State, pos))
	}
	totalPnL := totalRealized.Add(totalHoldings)

	t.Logf("Final State - Realized: $%s, Holdings: $%s, Total PnL: $%s",
		totalRealized.String(), totalHoldings.String(), totalPnL.String())

	// Expected from data-api (scripts/polymarket_pnl.py):
	// Closed positions realized: -$25.56
	// Open positions cashPnl: -$37.28
	// Open positions realizedPnl: -$3.75
	// Total PnL: -$66.59
	expectedPnL := decimal.NewFromFloat(-66.59)

	// Tolerance: 5% (data-api uses trade-level granularity, CH aggregates per position;
	// resolved payout prices vs avg_price differ slightly)
	const epsilon = 0.05

	if !withinTolerance(totalPnL, expectedPnL, epsilon) {
		t.Errorf("PnL mismatch: got %s, want %s (data-api, tolerance: %.1f%%)",
			totalPnL.String(), expectedPnL.String(), epsilon*100)
	}

	// Sanity checks
	if len(positions) == 0 {
		t.Error("no positions found")
	}
	if stats.blocks == 0 {
		t.Error("no blocks were processed")
	}
	if stats.events == 0 {
		t.Error("no events were found")
	}
}

func setupTestClickHouse(t *testing.T, ctx context.Context, projectRoot, dbPrefix string) *database.Store {
	t.Helper()

	host := envOrDefault("CLICKHOUSE_HOST", "127.0.0.1")
	port := envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9003)
	password := envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("%s_%d", dbPrefix, time.Now().UnixNano())

	if err := database.DropClickHouseDatabase(ctx, host, port, "default", password, db); err != nil {
		t.Skipf("ClickHouse test database unavailable at %s:%d: %v", host, port, err)
	}
	store, err := database.NewClickHouse(ctx, host, port, "default", password, db)
	if err != nil {
		t.Skipf("ClickHouse test database unavailable at %s:%d: %v", host, port, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = database.DropClickHouseDatabase(context.Background(), host, port, "default", password, db)
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
	t.Setenv("CLICKHOUSE_PASSWORD", password)
	t.Setenv("CLICKHOUSE_DATABASE", db)
	t.Setenv("CLICKHOUSE_PRUNE_INTERVAL", "999999999999")

	return store
}
