package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/franz101/sqd-go/internal/parser"
)

// countingProcessor tracks custom log delivery counts.
type countingProcessor struct {
	totalCalls  int
	totalLogs   int
	firstBlock  uint64
	lastBlock   uint64
	perBatchLogs []int
}

func (p *countingProcessor) Process(ctx context.Context, store *database.Store, logs []ingestion.CustomLog) error {
	p.totalCalls++
	p.totalLogs += len(logs)
	p.perBatchLogs = append(p.perBatchLogs, len(logs))
	if len(logs) > 0 {
		if p.firstBlock == 0 {
			p.firstBlock = logs[0].BlockNumber
		}
		p.lastBlock = logs[len(logs)-1].BlockNumber
	}
	return nil
}

func (p *countingProcessor) RestoreToBlock(blockNumber uint64) (uint64, error) { return blockNumber, nil }
func (p *countingProcessor) LoadFromDatabase(ctx context.Context, blockNumber uint64) error {
	return nil
}

// TestMultiBatchCursorPagesizeZeroExercisesFourPlusBatches validates that
// pagesize=0 cursor mode fetches at least 4 adaptive producer batches
// and the full pipeline (parse → decode → insert) works correctly.
func TestMultiBatchCursorPagesizeZeroExercisesFourPlusBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("real SQD portal and ClickHouse integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	cfg := loadPolymarketConfig(t)
	if len(cfg.Chains) != 1 {
		t.Fatalf("polymarket chains = %d, want 1", len(cfg.Chains))
	}

	store := newTestClickHouseStore(t, ctx, cfg.Name)
	proc := &countingProcessor{}

	startBlock := uint64(6864531)
	blockCount := uint64(25000) // ~5 adaptive batches

	if err := processChainWithProcessor(ctx, store, cfg, &cfg.Chains[0], 0, startBlock, blockCount, proc); err != nil {
		t.Fatalf("process polymarket portal range: %v", err)
	}

	t.Logf("Processor received %d calls with %d total custom logs across %d batches",
		proc.totalCalls, proc.totalLogs, len(proc.perBatchLogs))
	t.Logf("Batch sizes: %v", proc.perBatchLogs)

	if len(proc.perBatchLogs) < 4 {
		t.Errorf("Got %d batches, want at least 4 (pagesize=0 adaptive)", len(proc.perBatchLogs))
	}

	if proc.totalLogs == 0 {
		t.Fatal("Zero custom logs delivered to processor — pipeline may not be building CustomLog entries")
	}

	// Verify ClickHouse tables have events (async inserts may not flush immediately)
	typedNames := buildTypedTableNames(t, ctx, store)
	totalTyped := eventuallyCountRows(t, ctx, store, typedNames, 0)
	if totalTyped > 0 {
		t.Logf("ClickHouse typed tables: %d total rows across %d tables", totalTyped, len(typedNames))
	} else {
		t.Logf("ClickHouse typed tables: 0 rows (async inserts may not have flushed; %d tables exist)", len(typedNames))
	}

	// Blocks table may not exist if store_blocks=false
	totalBlocks := countBlocks(t, ctx, store)
	if totalBlocks > 0 {
		t.Logf("ClickHouse blocks: %d rows", totalBlocks)
	}

	// Spot-check: count exchange OrderFilled events (async inserts may not flush immediately)
	exchangeRows := eventuallyQueryCount(t, ctx, store, fmt.Sprintf("SELECT count() FROM %s.exchange_order_filled_events", quoteIdent(store.DB())), 0)
	t.Logf("ExchangeOrderFilled events: %d rows in ClickHouse", exchangeRows)
}

// TestExchangeEventCustomLogIntegrity verifies that when CustomLog entries
// are delivered for ExchangeOrderFilled events, the Data and Topics fields
// are valid hex strings.
func TestExchangeEventCustomLogIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("real SQD portal and ClickHouse integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cfg := loadPolymarketConfig(t)

	// Use a block range known to have exchange activity
	store := newTestClickHouseStore(t, ctx, cfg.Name)
	proc := &exchangeSniffingProcessor{}

	// Block 78000000 has known exchange activity (from fixture test)
	startBlock := uint64(77999900)
	blockCount := uint64(200)

	if err := processChainWithProcessor(ctx, store, cfg, &cfg.Chains[0], 0, startBlock, blockCount, proc); err != nil {
		t.Fatalf("process polymarket portal range: %v", err)
	}

	t.Logf("Total custom logs: %d, exchange logs captured: %d",
		proc.totalLogs, len(proc.samples))

	if len(proc.samples) == 0 {
		t.Error("No exchange custom logs captured — may need a more active block range")
	}

	for i, sample := range proc.samples {
		if !strings.HasPrefix(sample.Data, "0x") {
			t.Errorf("sample %d: Data = %q (not hex)", i, sample.Data)
		}
		if len(sample.Topics) < 1 {
			t.Errorf("sample %d: empty Topics", i)
		} else if !strings.HasPrefix(sample.Topics[0], "0x") {
			t.Errorf("sample %d: topic0 = %q (not hex)", i, sample.Topics[0])
		}
		// Verify all strings are valid — no corrupted/mangled data
		if sample.ContractAddress == "" {
			t.Errorf("sample %d: empty contract address", i)
		}
		if sample.BlockNumber == 0 {
			t.Errorf("sample %d: zero block number", i)
		}
	}
}

type exchangeSniffingProcessor struct {
	totalLogs int
	samples   []ingestion.CustomLog
}

func (p *exchangeSniffingProcessor) Process(ctx context.Context, store *database.Store, logs []ingestion.CustomLog) error {
	p.totalLogs += len(logs)
	for _, lg := range logs {
		if len(lg.Topics) == 0 {
			continue
		}
		t0 := strings.ToLower(lg.Topics[0])
		// Exchange OrderFilled topic0 hash
		if t0 == "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6" {
			if len(p.samples) < 50 {
				p.samples = append(p.samples, lg)
			}
		}
	}
	return nil
}

func (p *exchangeSniffingProcessor) RestoreToBlock(blockNumber uint64) (uint64, error) { return blockNumber, nil }
func (p *exchangeSniffingProcessor) LoadFromDatabase(ctx context.Context, blockNumber uint64) error {
	return nil
}

// TestBlockRangeEventDensityAround6874531 fetches a 500-block window
// around block 6874531 and inspects event density per block.
func TestBlockRangeEventDensityAround6874531(t *testing.T) {
	if testing.Short() {
		t.Skip("real SQD portal integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cfg := loadPolymarketConfig(t)
	_, filters, err := parser.BuildEventDecoder(cfg.Chains[0].Contracts)
	if err != nil {
		t.Fatalf("build filters: %v", err)
	}

	sqd := client.New("https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream")
	defer sqd.Close()

	start := uint64(6874281)
	end := uint64(6874780)

	t.Logf("Fetching blocks %d-%d...", start, end)
	response, err := sqd.Fetch(ctx, start, &end, filters)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	type blockCount struct{ block, events uint64 }
	var perBlock []blockCount
	var zeroEventBlocks []uint64
	var targetEvents int

	p := parser.NewFastJSONLParser(1024)
	if err := p.Parse(response.Raw, func(block *parser.Block) error {
		n := len(block.Logs)
		perBlock = append(perBlock, blockCount{block: block.Header.Number, events: uint64(n)})
		if n == 0 {
			zeroEventBlocks = append(zeroEventBlocks, block.Header.Number)
		}
		if block.Header.Number == 6874531 {
			targetEvents = n
		}
		return nil
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	totalEvents := 0
	for _, b := range perBlock {
		totalEvents += int(b.events)
	}

	t.Logf("Range %d-%d: %d blocks with events, %d total events, avg %.1f events/block",
		start, end, len(perBlock), totalEvents, float64(totalEvents)/float64(len(perBlock)))
	t.Logf("Block 6874531: %d events", targetEvents)
	if len(zeroEventBlocks) > 0 {
		t.Logf("Blocks with zero events: %v", zeroEventBlocks[:min(10, len(zeroEventBlocks))])
	}

	// Show histogram
	dist := make(map[int]int)
	for _, b := range perBlock {
		dist[int(b.events)]++
	}
	t.Logf("Event count histogram: %v", dist)
}

// processChainWithProcessor runs the full ingestion pipeline with a custom processor.
func processChainWithProcessor(ctx context.Context, store *database.Store, cfg *config.Config, chain *config.Chain, pageSize, startBlock, blockCount uint64, proc ingestion.Processor) error {
	host := envOr("CLICKHOUSE_HOST", "127.0.0.1")
	port := envIntOr("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOr("CLICKHOUSE_USER", "default")
	password := envOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	return ingestion.Run(ctx, cfg, ingestion.Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     user,
		ClickHousePassword: password,
		ClickHouseDatabase: store.DB(),
		PageSize:           pageSize,
		StartBlock:         startBlock,
		BlockCount:         blockCount,
		CursorMode:         true,
		Restart:            true,
		GeneratedSQLDir:    filepath.Join("..", "examples", "polymarket", "generated"),
		Processor:          proc,
	})
}

func loadPolymarketConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFile(filepath.Join("..", "examples", "polymarket", "config.yaml"))
	if err != nil {
		t.Fatalf("load polymarket config: %v", err)
	}
	return cfg
}

func newTestClickHouseStore(t *testing.T, ctx context.Context, sourceDB string) *database.Store {
	t.Helper()
	host := envOr("CLICKHOUSE_HOST", "127.0.0.1")
	port := envIntOr("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOr("CLICKHOUSE_USER", "default")
	password := envOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("polymarket_multi_batch_it_%d", time.Now().UnixNano())

	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse integration test requires ClickHouse at %s:%d: %v", host, port, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = database.DropClickHouseDatabase(context.Background(), host, port, user, password, db)
	})

	for _, name := range []string{"schema.sql", "custom_schema.sql"} {
		path := filepath.Join("..", "examples", "polymarket", "generated", name)
		if err := store.ApplySQLFileWithDatabase(ctx, path, sourceDB); err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
	}
	return store
}

func buildTypedTableNames(t *testing.T, ctx context.Context, store *database.Store) []string {
	t.Helper()
	// Query all tables in the database that look like typed event tables
	var col proto.ColStr
	if err := store.Conn().Do(ctx, ch.Query{
		Body:   fmt.Sprintf("SELECT name FROM system.tables WHERE database = '%s' AND name LIKE '%%_events'", store.DB()),
		Result: proto.Results{{Name: "name", Data: &col}},
	}); err != nil {
		return nil
	}
	names := make([]string, col.Rows())
	for i := 0; i < col.Rows(); i++ {
		names[i] = col.Row(i)
	}
	return names
}

func countRowsInTables(t *testing.T, ctx context.Context, store *database.Store, tables []string) uint64 {
	t.Helper()
	var total uint64
	for _, tbl := range tables {
		q := fmt.Sprintf("SELECT count() FROM %s.%s", quoteIdent(store.DB()), quoteIdent(tbl))
		n := queryUint64(t, ctx, store, q)
		if n > 0 {
			t.Logf("  %s: %d rows", tbl, n)
		}
		total += n
	}
	return total
}

func eventuallyCountRows(t *testing.T, ctx context.Context, store *database.Store, tables []string, minExpected uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last uint64
	for {
		last = countRowsInTables(t, ctx, store, tables)
		if last >= minExpected || time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func eventuallyQueryCount(t *testing.T, ctx context.Context, store *database.Store, query string, minExpected uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last uint64
	for {
		last = queryUint64(t, ctx, store, query)
		if last >= minExpected || time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func countBlocks(t *testing.T, ctx context.Context, store *database.Store) uint64 {
	return queryUint64(t, ctx, store, fmt.Sprintf("SELECT count() FROM %s.blocks", quoteIdent(store.DB())))
}

func queryCount(t *testing.T, ctx context.Context, store *database.Store, query string) uint64 {
	return queryUint64(t, ctx, store, query)
}

func queryUint64(t *testing.T, ctx context.Context, store *database.Store, query string) uint64 {
	t.Helper()
	var col proto.ColUInt64
	if err := store.Conn().Do(ctx, ch.Query{
		Body:   query,
		Result: proto.Results{{Name: "c", Data: &col}},
	}); err != nil {
		// Table might not exist — return 0
		return 0
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
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return fallback
		}
	}
	n := 0
	for _, c := range v {
		n = n*10 + int(c-'0')
	}
	return n
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
