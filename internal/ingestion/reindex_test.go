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

	var initialCount proto.ColUInt64
	if err := verifyStore1.Conn().Do(queryCtx1, ch.Query{
		Body: fmt.Sprintf("SELECT count() FROM %s.usdc_transfer_events", quoteIdentForTest(dbName)),
		Result: proto.Results{
			{Name: "count()", Data: &initialCount},
		},
	}); err != nil {
		t.Fatalf("initial count query: %v", err)
	}
	initialRows := initialCount.Row(0)
	t.Logf("Phase 1 complete: %d rows in usdc_transfer_events", initialRows)
	if initialRows == 0 {
		t.Fatal("expected USDC Transfer events in initial ingestion")
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
	t.Logf("Initial max block_number: %d", initialMaxBlock)

	// Phase 2: Reindex from midpoint (block 22000100)
	// This should delete all blocks > 22000100
	reindexFromBlock := uint64(22000100)
	t.Logf("Phase 2: Reindexing from block %d (should delete blocks > %d)", reindexFromBlock, reindexFromBlock)

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

	// Check that max block_number is now at or below reindexFromBlock
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

	// Verify no blocks exist above reindexFromBlock
	var countAbove proto.ColUInt64
	if err := verifyStore3.Conn().Do(queryCtx3, ch.Query{
		Body: fmt.Sprintf("SELECT count() FROM %s.usdc_transfer_events WHERE block_number > %d", quoteIdentForTest(dbName), reindexFromBlock),
		Result: proto.Results{
			{Name: "count()", Data: &countAbove},
		},
	}); err != nil {
		t.Fatalf("count above reindex block query: %v", err)
	}
	countAboveRows := countAbove.Row(0)

	if countAboveRows > 0 {
		t.Errorf("Expected 0 rows with block_number > %d, found %d rows", reindexFromBlock, countAboveRows)
	} else {
		t.Logf("Verified: 0 rows with block_number > %d (correctly deleted)", reindexFromBlock)
	}

	// Verify blocks at or below reindexFromBlock still exist
	var countBelow proto.ColUInt64
	if err := verifyStore3.Conn().Do(queryCtx3, ch.Query{
		Body: fmt.Sprintf("SELECT count() FROM %s.usdc_transfer_events WHERE block_number <= %d", quoteIdentForTest(dbName), reindexFromBlock),
		Result: proto.Results{
			{Name: "count()", Data: &countBelow},
		},
	}); err != nil {
		t.Fatalf("count below reindex block query: %v", err)
	}
	countBelowRows := countBelow.Row(0)

	if countBelowRows == 0 {
		t.Errorf("Expected rows with block_number <= %d to be preserved, but found 0 rows", reindexFromBlock)
	} else {
		t.Logf("Verified: %d rows with block_number <= %d (correctly preserved)", countBelowRows, reindexFromBlock)
	}

	// Cleanup
	if err := database.DropClickHouseDatabase(queryCtx3, host, port, "default", password, dbName); err != nil {
		t.Logf("cleanup: %v", err)
	}
}
