package ingestion

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/parser"
)

func TestPolymarketPortalUnboundedCursorThenOneBlockContinuation(t *testing.T) {
	if testing.Short() {
		t.Skip("real SQD portal integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cfg := loadPolymarketConfig(t)
	_, filters, err := parser.BuildEventDecoder(cfg.Chains[0].Contracts)
	if err != nil {
		t.Fatalf("build polymarket filters: %v", err)
	}

	sqd := client.New(chainEndpoint(137, true))
	defer sqd.Close()

	response, err := sqd.FetchWithParent(ctx, 6249531, nil, "", true, filters)
	if err != nil {
		t.Fatalf("fetch unbounded cursor page: %v", err)
	}
	summary := summarizePortalBlocks(t, response.Raw)
	if summary.blocks <= 5000 {
		t.Fatalf("unbounded cursor returned %d blocks, want more than one fixed page", summary.blocks)
	}
	if summary.first != 6249531 {
		t.Fatalf("unbounded first block = %d, want 6249531", summary.first)
	}
	if summary.gaps != 0 {
		t.Fatalf("unbounded response has %d block gaps", summary.gaps)
	}
	if summary.last < 6349531 {
		t.Fatalf("unbounded last block = %d, want at least 100k blocks beyond start", summary.last)
	}
	if summary.logs == 0 {
		t.Fatal("unbounded response had no matching logs")
	}

	next := summary.last + 1
	response, err = sqd.FetchWithParent(ctx, next, &next, summary.lastHash, true, filters)
	if err != nil {
		t.Fatalf("fetch one-block continuation from %d: %v", next, err)
	}
	continuation := summarizePortalBlocks(t, response.Raw)
	if continuation.blocks != 1 {
		t.Fatalf("continuation blocks = %d, want 1", continuation.blocks)
	}
	if continuation.first != next || continuation.last != next {
		t.Fatalf("continuation range = %d-%d, want %d", continuation.first, continuation.last, next)
	}
}

func TestPolymarketPortalBoundedPagesHaveNonEmptyLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("real SQD portal integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cfg := loadPolymarketConfig(t)
	_, filters, err := parser.BuildEventDecoder(cfg.Chains[0].Contracts)
	if err != nil {
		t.Fatalf("build polymarket filters: %v", err)
	}

	sqd := client.New(chainEndpoint(137, true))
	defer sqd.Close()

	firstEnd := uint64(6254530)
	response, err := sqd.FetchWithParent(ctx, 6249531, &firstEnd, "", true, filters)
	if err != nil {
		t.Fatalf("fetch first bounded page: %v", err)
	}
	first := summarizePortalBlocks(t, response.Raw)
	assertPortalPageSummary(t, "first", first, 6249531, 6254530, 5000, 18, 32)

	secondEnd := uint64(6259530)
	response, err = sqd.FetchWithParent(ctx, 6254531, &secondEnd, first.lastHash, true, filters)
	if err != nil {
		t.Fatalf("fetch second bounded page: %v", err)
	}
	second := summarizePortalBlocks(t, response.Raw)
	assertPortalPageSummary(t, "second", second, 6254531, 6259530, 5000, 36, 58)
}

func TestPolymarketPortalCursorPageSizeZeroTwoBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("real SQD portal and ClickHouse integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := loadPolymarketConfig(t)
	if len(cfg.Chains) != 1 {
		t.Fatalf("polymarket chains = %d, want 1", len(cfg.Chains))
	}
	chain := cfg.Chains[0]

	store := newIngestionIntegrationStore(t, ctx, filepath.Join("..", "..", "examples", "polymarket", "generated"), cfg.Name)
	proc := &recordingProcessor{}

	if err := processChain(ctx, store, cfg, &chain, 0, 6249531, 10000, true, config.ForkModeDefault, true, proc); err != nil {
		t.Fatalf("process polymarket portal range: %v", err)
	}

	if len(proc.calls) != 2 {
		t.Fatalf("custom processor calls = %d, want 2: %#v", len(proc.calls), proc.calls)
	}
	if got := proc.calls[0].logs; got != 32 {
		t.Fatalf("first batch custom logs = %d, want 32", got)
	}
	if got := proc.calls[1].logs; got != 58 {
		t.Fatalf("second batch custom logs = %d, want 58", got)
	}
	if proc.calls[0].lastBlock > 6254530 {
		t.Fatalf("first batch crossed cursor page boundary: last log block %d", proc.calls[0].lastBlock)
	}
	if proc.calls[1].firstBlock < 6254531 {
		t.Fatalf("second batch started before cursor page boundary: first log block %d", proc.calls[1].firstBlock)
	}

	state, ok, err := store.LastSyncState(ctx, 137)
	if err != nil {
		t.Fatalf("read sync state: %v", err)
	}
	if !ok {
		t.Fatal("sync state missing")
	}
	if state.Current.Number != 6259530 {
		t.Fatalf("cursor block = %d, want 6259530", state.Current.Number)
	}
	if state.Current.Hash != "0xe4f40b324d6c06bc1741b30da5a7cf6a66241dd8519c37fefe9cd07807a1454b" {
		t.Fatalf("cursor hash = %s, want final block hash", state.Current.Hash)
	}

	tables := eventTableNames(t, &chain)
	rows := eventuallyCountEventRows(t, ctx, store, tables, 90)
	if rows != 90 {
		t.Fatalf("typed event rows = %d, want 90", rows)
	}
}

type portalBlockSummary struct {
	blocks      uint64
	first       uint64
	last        uint64
	firstHash   string
	lastHash    string
	eventBlocks uint64
	logs        uint64
	gaps        uint64
}

func summarizePortalBlocks(t *testing.T, raw []byte) portalBlockSummary {
	t.Helper()

	if len(raw) == 0 {
		t.Fatal("portal response was empty")
	}
	var summary portalBlockSummary
	var prev uint64
	p := parser.NewFastJSONLParser(1024)
	if err := p.Parse(raw, func(block *parser.Block) error {
		if summary.blocks == 0 {
			summary.first = block.Header.Number
			summary.firstHash = block.Header.Hash
		} else if block.Header.Number != prev+1 {
			summary.gaps++
		}
		summary.blocks++
		summary.last = block.Header.Number
		summary.lastHash = block.Header.Hash
		if len(block.Logs) > 0 {
			summary.eventBlocks++
			summary.logs += uint64(len(block.Logs))
		}
		prev = block.Header.Number
		return nil
	}); err != nil {
		t.Fatalf("parse portal JSONL: %v", err)
	}
	return summary
}

func assertPortalPageSummary(t *testing.T, label string, summary portalBlockSummary, first, last, blocks, eventBlocks, logs uint64) {
	t.Helper()

	if summary.first != first || summary.last != last {
		t.Fatalf("%s page range = %d-%d, want %d-%d", label, summary.first, summary.last, first, last)
	}
	if summary.blocks != blocks {
		t.Fatalf("%s page blocks = %d, want %d", label, summary.blocks, blocks)
	}
	if summary.gaps != 0 {
		t.Fatalf("%s page has %d block gaps", label, summary.gaps)
	}
	if summary.eventBlocks != eventBlocks {
		t.Fatalf("%s page event blocks = %d, want %d", label, summary.eventBlocks, eventBlocks)
	}
	if summary.logs != logs {
		t.Fatalf("%s page logs = %d, want %d", label, summary.logs, logs)
	}
	if summary.logs == 0 {
		t.Fatalf("%s page logs are empty", label)
	}
}

func loadPolymarketConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg, err := config.LoadFile(filepath.Join("..", "..", "examples", "polymarket", "config.yaml"))
	if err != nil {
		t.Fatalf("load polymarket config: %v", err)
	}
	return cfg
}

type recordedProcessCall struct {
	logs       int
	firstBlock uint64
	lastBlock  uint64
}

type recordingProcessor struct {
	calls []recordedProcessCall
}

func (p *recordingProcessor) Process(ctx context.Context, store *database.Store, logs []CustomLog) error {
	if len(logs) == 0 {
		return nil
	}
	call := recordedProcessCall{
		logs:       len(logs),
		firstBlock: logs[0].BlockNumber,
		lastBlock:  logs[0].BlockNumber,
	}
	for _, lg := range logs[1:] {
		if lg.BlockNumber < call.firstBlock {
			call.firstBlock = lg.BlockNumber
		}
		if lg.BlockNumber > call.lastBlock {
			call.lastBlock = lg.BlockNumber
		}
	}
	p.calls = append(p.calls, call)
	return nil
}

func (p *recordingProcessor) RestoreToBlock(blockNumber uint64) (uint64, error) {
	return blockNumber, nil
}

func (p *recordingProcessor) LoadFromDatabase(blockNumber uint64) error {
	return nil
}

func newIngestionIntegrationStore(t *testing.T, ctx context.Context, generatedDir, sourceDB string) *database.Store {
	t.Helper()

	host := envOr("CLICKHOUSE_HOST", "127.0.0.1")
	port := envIntOr("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOr("CLICKHOUSE_USER", "default")
	password := envOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("polymarket_cursor_it_%d", time.Now().UnixNano())

	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse integration test requires ClickHouse at %s:%d: %v", host, port, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = database.DropClickHouseDatabase(context.Background(), host, port, user, password, db)
	})

	for _, name := range []string{"schema.sql", "custom_schema.sql"} {
		path := filepath.Join(generatedDir, name)
		if err := store.ApplySQLFileWithDatabase(ctx, path, sourceDB); err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
	}
	return store
}

func eventTableNames(t *testing.T, chain *config.Chain) []string {
	t.Helper()

	idx, err := buildTypedTableIndex(chain)
	if err != nil {
		t.Fatalf("build typed table index: %v", err)
	}
	seen := make(map[string]struct{})
	for _, table := range idx.byAddressEvent {
		seen[table.Name] = struct{}{}
	}
	for _, table := range idx.byEvent {
		seen[table.Name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func eventuallyCountEventRows(t *testing.T, ctx context.Context, store *database.Store, tables []string, want uint64) uint64 {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last uint64
	for {
		last = countEventRows(t, ctx, store, tables)
		if last >= want || time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func countEventRows(t *testing.T, ctx context.Context, store *database.Store, tables []string) uint64 {
	t.Helper()

	var total uint64
	for _, table := range tables {
		total += queryUInt64(t, ctx, store, fmt.Sprintf(
			"SELECT count() AS c FROM %s.%s",
			quoteTestIdent(store.DB()),
			quoteTestIdent(table),
		))
	}
	return total
}

func queryUInt64(t *testing.T, ctx context.Context, store *database.Store, query string) uint64 {
	t.Helper()

	var col proto.ColUInt64
	if err := store.Conn().Do(ctx, ch.Query{
		Body:   query,
		Result: proto.Results{{Name: "c", Data: &col}},
	}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if col.Rows() == 0 {
		return 0
	}
	return col.Row(0)
}

func quoteTestIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envIntOr(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}
