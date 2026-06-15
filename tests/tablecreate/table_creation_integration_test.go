// Package tablecreate holds an end-to-end integration test that runs the real
// ingestion pipeline (SQD portal + live ClickHouse) and asserts that both the
// generated event tables and a custom table are created AND populated with real
// data once a bounded run completes.
//
// It lives in its own package (not internal/database or tests/) on purpose:
//   - internal/database cannot import internal/codegen (codegen imports database).
//   - the tests/ package currently does not compile (it imports the broken
//     examples/polymarket/generated and references a removed ingestion.Options
//     field), so a new test there would never run.
//
// This package imports only codegen/config/database/ingestion, all of which
// build cleanly, so `go test ./tests/tablecreate/` is self-contained.
//
// The test is gated behind a reachable ClickHouse: if none is found it skips
// (like the other integration tests). It is also skipped under `-short`.
package tablecreate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/internal/codegen"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
)

// UniswapV2Factory mainnet deployment block. The first PairCreated event
// (the canonical USDC/WETH pair) lands at block 10008355, so a small window
// from the deploy block reliably contains real events.
const (
	uniswapV2FactoryAddr   = "0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"
	uniswapDeployBlock     = 10000835
	runBlockCount          = 20000 // covers the first several PairCreated events
	usdcWethPairHex        = "B4E16D0168E52D35CACD2C6185B44281EC28C9DC"
	pairCreatedEventTable  = "uniswap_v2_factory_pair_created_events"
	derivedCustomTableName = "derived_pairs"
)

// TestRunCreatesAndPopulatesEventAndCustomTables runs a bounded UniswapV2Factory
// backfill and verifies that, after the run:
//   - the core blocks table has rows,
//   - the generated event table exists and holds real decoded PairCreated rows
//     (asserted down to the known USDC/WETH pair address), and
//   - a custom table written by the run's custom processor exists and holds rows
//     derived from the same real events.
func TestRunCreatesAndPopulatesEventAndCustomTables(t *testing.T) {
	if testing.Short() {
		t.Skip("real SQD portal + ClickHouse integration test")
	}

	host := envOr("CLICKHOUSE_HOST", "127.0.0.1")
	port := envIntOr("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOr("CLICKHOUSE_USER", "default")
	password := envOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	dbName := fmt.Sprintf("tablecreate_it_%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Pre-flight: skip cleanly if ClickHouse is unreachable.
	if probe, err := database.NewClickHouse(ctx, host, port, user, password, dbName); err != nil {
		t.Skipf("ClickHouse integration test requires ClickHouse at %s:%d: %v", host, port, err)
	} else {
		_ = probe.Close()
	}
	t.Cleanup(func() {
		_ = database.DropClickHouseDatabase(context.Background(), host, port, user, password, dbName)
	})

	// Scaffold a minimal UniswapV2Factory project and codegen its schema.
	root := t.TempDir()
	writeUniswapProject(t, root, dbName)
	project, err := config.LoadProject(root)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	outPath, err := codegen.GenerateProject(project)
	if err != nil {
		t.Fatalf("codegen: %v", err)
	}
	genDir := filepath.Dir(outPath)

	// Run a bounded backfill over the deploy window. The custom processor
	// writes a row per delivered PairCreated log into its own custom table.
	proc := &derivedPairProcessor{db: dbName}
	runErr := ingestion.Run(ctx, project.Config, ingestion.Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     user,
		ClickHousePassword: password,
		ClickHouseDatabase: dbName,
		StartBlock:         uniswapDeployBlock,
		BlockCount:         runBlockCount,
		CursorMode:         true,
		Restart:            true,
		GeneratedSQLDir:    genDir,
		Processor:          proc,
	})
	if runErr != nil {
		t.Fatalf("ingestion run: %v", runErr)
	}
	if proc.conn != nil {
		_ = proc.conn.Close()
	}
	t.Logf("custom processor received %d PairCreated logs across %d batches", proc.total, proc.batches)

	// Re-open a store to inspect the resulting tables.
	store, err := database.NewClickHouse(ctx, host, port, user, password, dbName)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()

	// --- Core + event tables created and populated ---
	for _, name := range []string{"blocks", "sync_state", pairCreatedEventTable, derivedCustomTableName} {
		if !tableExists(ctx, store, dbName, name) {
			t.Fatalf("table %q was not created", name)
		}
	}

	if got := queryUint64(t, ctx, store, fmt.Sprintf("SELECT count() AS c FROM %s.blocks", quoteIdent(dbName))); got == 0 {
		t.Error("blocks table has 0 rows after run")
	}

	eventRows := queryUint64(t, ctx, store, fmt.Sprintf("SELECT count() AS c FROM %s.%s", quoteIdent(dbName), quoteIdent(pairCreatedEventTable)))
	if eventRows == 0 {
		t.Fatalf("event table %s has 0 rows after run", pairCreatedEventTable)
	}
	t.Logf("event table %s: %d rows", pairCreatedEventTable, eventRows)

	// Real-data assertion: the canonical USDC/WETH pair must be present and
	// correctly decoded (FixedString(20) address columns, not zeroed/garbled).
	usdcWeth := queryUint64(t, ctx, store, fmt.Sprintf(
		"SELECT count() AS c FROM %s.%s WHERE hex(pair) = '%s'",
		quoteIdent(dbName), quoteIdent(pairCreatedEventTable), usdcWethPairHex))
	if usdcWeth == 0 {
		t.Errorf("expected the known USDC/WETH pair %s in event table, found none", usdcWethPairHex)
	}

	// --- Custom table created and populated with real run data ---
	customRows := queryUint64(t, ctx, store, fmt.Sprintf("SELECT count() AS c FROM %s.%s", quoteIdent(dbName), quoteIdent(derivedCustomTableName)))
	if customRows == 0 {
		t.Fatalf("custom table %s has 0 rows after run", derivedCustomTableName)
	}
	t.Logf("custom table %s: %d rows", derivedCustomTableName, customRows)

	// Custom rows must carry real block numbers from the run window, not zeros.
	belowDeploy := queryUint64(t, ctx, store, fmt.Sprintf(
		"SELECT count() AS c FROM %s.%s WHERE block_number < %d",
		quoteIdent(dbName), quoteIdent(derivedCustomTableName), uniswapDeployBlock))
	if belowDeploy != 0 {
		t.Errorf("custom table has %d rows with block_number below the deploy block — data looks wrong", belowDeploy)
	}

	// The custom table is derived from the same events, so its row count should
	// match what the processor saw (sanity that the run actually drove it).
	if customRows != uint64(proc.total) {
		t.Errorf("custom table rows = %d, want %d (logs delivered to processor)", customRows, proc.total)
	}
}

// derivedPairProcessor is a custom processor that, during the run, lazily
// creates a custom table and inserts one row per PairCreated log it receives.
// It uses a dedicated maintenance connection so it never shares a ch.Client
// with the ingestion commit/insert path.
type derivedPairProcessor struct {
	db      string
	conn    *ch.Client
	created bool
	total   int
	batches int
}

func (p *derivedPairProcessor) Process(ctx context.Context, store *database.Store, logs []ingestion.CustomLog) error {
	if p.conn == nil {
		conn, err := store.DialMaintenanceConn(ctx)
		if err != nil {
			return fmt.Errorf("dial custom conn: %w", err)
		}
		p.conn = conn
	}
	if !p.created {
		ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
			block_number UInt64,
			topic0 String,
			data String
		) ENGINE = MergeTree ORDER BY block_number`, quoteIdent(p.db), quoteIdent(derivedCustomTableName))
		if err := p.conn.Do(ctx, ch.Query{Body: ddl}); err != nil {
			return fmt.Errorf("create custom table: %w", err)
		}
		p.created = true
	}
	if len(logs) == 0 {
		return nil
	}
	p.batches++

	var blockCol proto.ColUInt64
	var topicCol proto.ColStr
	var dataCol proto.ColStr
	for _, lg := range logs {
		var topic0 string
		if len(lg.Topics) > 0 {
			topic0 = lg.Topics[0]
		}
		blockCol.Append(lg.BlockNumber)
		topicCol.Append(topic0)
		dataCol.Append(lg.Data)
		p.total++
	}
	// Synchronous insert (no async_insert) so rows are queryable immediately
	// after the run returns.
	return p.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf("INSERT INTO %s.%s (block_number, topic0, data) VALUES", quoteIdent(p.db), quoteIdent(derivedCustomTableName)),
		Input: []proto.InputColumn{
			{Name: "block_number", Data: &blockCol},
			{Name: "topic0", Data: &topicCol},
			{Name: "data", Data: &dataCol},
		},
	})
}

func (p *derivedPairProcessor) RestoreToBlock(blockNumber uint64) (uint64, error) {
	return blockNumber, nil
}
func (p *derivedPairProcessor) LoadFromDatabase(ctx context.Context, blockNumber uint64) error {
	return nil
}

// writeUniswapProject scaffolds a minimal UniswapV2Factory config.yaml under root.
func writeUniswapProject(t *testing.T, root, name string) {
	t.Helper()
	cfg := fmt.Sprintf(`name: %s
ecosystem: evm
store_blocks: true
chains:
  - id: 1
    start_block: %d
    contracts:
      - name: UniswapV2Factory
        address: %s
        events:
          - event: PairCreated(address indexed token0, address indexed token1, address pair, uint256 arg3)
`, name, uniswapDeployBlock, uniswapV2FactoryAddr)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func tableExists(ctx context.Context, store *database.Store, db, name string) bool {
	var col proto.ColUInt64
	q := fmt.Sprintf("SELECT count() AS c FROM system.tables WHERE database = '%s' AND name = '%s'", db, name)
	if err := store.Conn().Do(ctx, ch.Query{Body: q, Result: proto.Results{{Name: "c", Data: &col}}}); err != nil {
		return false
	}
	return col.Rows() > 0 && col.Row(0) > 0
}

func queryUint64(t *testing.T, ctx context.Context, store *database.Store, query string) uint64 {
	t.Helper()
	var col proto.ColUInt64
	if err := store.Conn().Do(ctx, ch.Query{Body: query, Result: proto.Results{{Name: "c", Data: &col}}}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if col.Rows() == 0 {
		return 0
	}
	return col.Row(0)
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envIntOr(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
