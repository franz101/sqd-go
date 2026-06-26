package ingestion

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
)

// TestIntegrationReindexFrom tests the --reindex-from flag:
// 1. First ingests a range of blocks
// 2. Then reindexes from a midpoint, deleting blocks above that point
// 3. Verifies that blocks above are deleted and blocks at/below are preserved
//
// Requires a running ClickHouse (CI provides it via service container).
// Skipped when ClickHouse is not reachable.
func TestIntegrationReindexFrom(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}

	host, port, password := chEnv()
	dbName := fmt.Sprintf("integration_test_reindex_%d", time.Now().UnixNano())

	// Use a small range of USDC Transfer events on Ethereum
	// Blocks 22000000-22000200 (200 blocks total)
	endBlock := uint64(22000200)
	// Phase 2 reindexes from this midpoint: blocks > it are deleted then rebuilt,
	// blocks <= it are left untouched.
	reindexFromBlock := uint64(22000100)
	cfg := &config.Config{
		Name: dbName,
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: 22000000,
			EndBlock:   &endBlock,
			Contracts: []config.ChainContractConfig{{
				Name:    "USDC",
				Address: config.Address{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"},
				Events: []config.EventConfig{
					{Event: "Transfer(address indexed from, address indexed to, uint256 value)"},
				},
			}},
		}},
	}

	// Setup: Create database and tables
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer setupCancel()

	store, err := database.NewClickHouse(setupCtx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("setup ClickHouse: %v", err)
	}
	if err := store.EnsureTablesWithOptions(setupCtx, true, database.EnsureTablesOptions{}); err != nil {
		t.Fatalf("ensure base tables: %v", err)
	}

	createTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.usdc_transfer_events (
		block_number UInt64,
		block_timestamp DateTime64(3, 'UTC'),
		transaction_index UInt64,
		log_index UInt64,
		from FixedString(20),
		to FixedString(20),
		value UInt256
	) ENGINE = MergeTree()
	ORDER BY (block_number, transaction_index, log_index)`, quoteIdentForTest(dbName))

	if err := store.Conn().Do(setupCtx, ch.Query{Body: createTable}); err != nil {
		t.Fatalf("create typed table: %v", err)
	}
	store.Close()

	// Phase 1: Initial ingestion of all 200 blocks
	t.Log("Phase 1: Initial ingestion of blocks 22000000-22000200")
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel1()

	opts1 := Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     "default",
		ClickHousePassword: password,
		ClickHouseDatabase: dbName,
		Restart:            false,
		CursorMode:         false,
		PageSize:           0,
	}

	err = Run(ctx1, cfg, opts1)
	if err != nil && ctx1.Err() == nil {
		t.Fatalf("Phase 1 ingestion.Run: %v", err)
	}

	// Verify initial ingestion
	queryCtx1, queryCancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer queryCancel1()

	verifyStore1, err := database.NewClickHouse(queryCtx1, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("connect ClickHouse for verification: %v", err)
	}
	defer verifyStore1.Close()

	initialRows := countTransfers(t, queryCtx1, verifyStore1, dbName, "")
	t.Logf("Phase 1 complete: %d rows in usdc_transfer_events", initialRows)
	if initialRows == 0 {
		t.Fatal("expected USDC Transfer events in initial ingestion")
	}

	// Split the initial rows at the reindex midpoint. Phase 3 checks that the
	// below-or-equal partition is preserved untouched and the above partition is
	// deleted then rebuilt to the same count (a no-op delete would double it).
	initialBelow := countTransfers(t, queryCtx1, verifyStore1, dbName, fmt.Sprintf("block_number <= %d", reindexFromBlock))
	initialAbove := countTransfers(t, queryCtx1, verifyStore1, dbName, fmt.Sprintf("block_number > %d", reindexFromBlock))
	if initialAbove == 0 {
		t.Fatalf("expected USDC Transfer events above block %d in initial ingestion (test cannot verify reindex otherwise)", reindexFromBlock)
	}

	// Get max block_number after initial ingestion
	var maxBlock proto.ColUInt64
	if err := verifyStore1.Conn().Do(queryCtx1, ch.Query{
		Body: fmt.Sprintf("SELECT max(block_number) FROM %s.usdc_transfer_events", quoteIdentForTest(dbName)),
		Result: proto.Results{
			{Name: "max(block_number)", Data: &maxBlock},
		},
	}); err != nil {
		t.Fatalf("max block query: %v", err)
	}
	initialMaxBlock := maxBlock.Row(0)
	t.Logf("Initial: %d rows (<= %d: %d, > %d: %d), max block %d", initialRows, reindexFromBlock, initialBelow, reindexFromBlock, initialAbove, initialMaxBlock)

	// Phase 2: Reindex from the midpoint. This deletes all blocks > reindexFromBlock,
	// then re-indexes the range [reindexFromBlock, endBlock], rebuilding them.
	t.Logf("Phase 2: Reindexing from block %d (delete blocks > %d, then re-ingest the range)", reindexFromBlock, reindexFromBlock)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	opts2 := Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     "default",
		ClickHousePassword: password,
		ClickHouseDatabase: dbName,
		Restart:            false,
		CursorMode:         false,
		PageSize:           0,
		ReindexFrom:        reindexFromBlock,
	}

	// Update config to continue from reindex point
	cfgReindex := &config.Config{
		Name: dbName,
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: reindexFromBlock,
			EndBlock:   &endBlock,
			Contracts: []config.ChainContractConfig{{
				Name:    "USDC",
				Address: config.Address{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"},
				Events: []config.EventConfig{
					{Event: "Transfer(address indexed from, address indexed to, uint256 value)"},
				},
			}},
		}},
	}

	err = Run(ctx2, cfgReindex, opts2)
	if err != nil && ctx2.Err() == nil {
		t.Fatalf("Phase 2 reindex ingestion.Run: %v", err)
	}

	// Phase 3: Verify reindex results
	t.Log("Phase 3: Verifying reindex results")
	queryCtx3, queryCancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer queryCancel3()

	verifyStore3, err := database.NewClickHouse(queryCtx3, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("connect ClickHouse for final verification: %v", err)
	}
	defer verifyStore3.Close()

	// max block_number should be back at endBlock: reindex re-ingested the full
	// [reindexFromBlock, endBlock] range after deleting it.
	var newMaxBlock proto.ColUInt64
	if err := verifyStore3.Conn().Do(queryCtx3, ch.Query{
		Body: fmt.Sprintf("SELECT max(block_number) FROM %s.usdc_transfer_events", quoteIdentForTest(dbName)),
		Result: proto.Results{
			{Name: "max(block_number)", Data: &newMaxBlock},
		},
	}); err != nil {
		t.Fatalf("max block query after reindex: %v", err)
	}
	finalMaxBlock := newMaxBlock.Row(0)
	t.Logf("Final max block_number after reindex: %d", finalMaxBlock)
	if finalMaxBlock != endBlock {
		t.Errorf("Expected max block_number %d after reindex (full range rebuilt), got %d", endBlock, finalMaxBlock)
	}

	// Blocks at or below the reindex point must be untouched (the lightweight
	// DELETE only removes block_number > reindexFromBlock).
	finalBelow := countTransfers(t, queryCtx3, verifyStore3, dbName, fmt.Sprintf("block_number <= %d", reindexFromBlock))
	if finalBelow != initialBelow {
		t.Errorf("Expected %d rows with block_number <= %d preserved, found %d", initialBelow, reindexFromBlock, finalBelow)
	} else {
		t.Logf("Verified: %d rows with block_number <= %d (preserved untouched)", finalBelow, reindexFromBlock)
	}

	// Blocks above the reindex point were deleted then re-ingested. The count must
	// match the original exactly: if the delete had not run, re-inserting into the
	// MergeTree would have doubled these rows.
	finalAbove := countTransfers(t, queryCtx3, verifyStore3, dbName, fmt.Sprintf("block_number > %d", reindexFromBlock))
	if finalAbove != initialAbove {
		t.Errorf("Expected %d rows with block_number > %d after reindex (deleted then rebuilt), found %d", initialAbove, reindexFromBlock, finalAbove)
	} else {
		t.Logf("Verified: %d rows with block_number > %d (deleted then rebuilt, no duplication)", finalAbove, reindexFromBlock)
	}

	// Cleanup
	if err := database.DropClickHouseDatabase(queryCtx3, host, port, "default", password, dbName); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

// countTransfers runs a count() over usdc_transfer_events with the given WHERE
// clause (empty clause counts all rows) and fails the test on a query error.
func countTransfers(t *testing.T, ctx context.Context, store *database.Store, dbName, where string) uint64 {
	t.Helper()
	body := fmt.Sprintf("SELECT count() FROM %s.usdc_transfer_events", quoteIdentForTest(dbName))
	if where != "" {
		body += " WHERE " + where
	}
	var col proto.ColUInt64
	if err := store.Conn().Do(ctx, ch.Query{
		Body:   body,
		Result: proto.Results{{Name: "count()", Data: &col}},
	}); err != nil {
		t.Fatalf("count query (%q): %v", where, err)
	}
	if col.Rows() == 0 {
		return 0
	}
	return col.Row(0)
}
