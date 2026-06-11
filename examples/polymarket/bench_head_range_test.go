package polymarket

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
)

// TestBenchHeadRange replays a fixed head-density block range (fixture files,
// no portal variance) against a clone of the live state tables — i.e. the
// resumed-run scenario where hot state starts empty, the cold tier is
// non-authoritative, and every first-touch key may fall through to ClickHouse.
//
// Fetch the fixture once:
//
//	go run debugger/fetchUntil.go -start 60444592 -end 60544592 -out debugger/data/bench_head
//
// Run:
//
//	go test ./examples/polymarket/ -run TestBenchHeadRange -v
//
// The clone source defaults to the local `polymarket` database (override with
// SQD_BENCH_SRC_DB) and must contain state covering the fixture's start block.
func TestBenchHeadRange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bench in short mode")
	}

	projectRoot := findProjectRoot()
	dataDir := filepath.Join(projectRoot, "debugger/data/bench_head")
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Skipf("fixture not found: %s (run: go run debugger/fetchUntil.go -start 60444592 -end 60544592 -out debugger/data/bench_head)", dataDir)
	}

	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("create processor: %v", err)
	}

	ctx := context.Background()
	store := setupWalletTestClickHouse(t, ctx, projectRoot)

	// Clone live state into the scratch DB so resolver lookups behave like a
	// real resume (point-SELECTs against a populated positions table).
	srcDB := os.Getenv("SQD_BENCH_SRC_DB")
	if srcDB == "" {
		srcDB = "polymarket"
	}
	for _, tbl := range []string{
		"memory_user_positions",
		"memory_conditions",
		"memory_fixed_product_market_makers",
		"memory_neg_risk_events",
	} {
		q := fmt.Sprintf("INSERT INTO %s.%s SELECT * FROM %s.%s", store.DB(), tbl, srcDB, tbl)
		if err := store.Conn().Do(ctx, ch.Query{Body: q}); err != nil {
			t.Skipf("clone %s.%s: %v (is the live database populated?)", srcDB, tbl, err)
		}
	}

	// Real resume flow: enable the cold tier first, then rebuild state from
	// ClickHouse. LoadFromDatabase re-opens the tier fresh, runs Recover with it
	// attached (rows past the hot ring capacity spill to Pebble), and marks it
	// authoritative — steady state then never point-SELECTs ClickHouse.
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := proc.EnableColdCache(coldDir, false); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	defer func() { _ = proc.CloseColdCache() }()
	proc.State.SetSnapshotsEnabled(false)
	loadStart := time.Now()
	if err := proc.LoadFromDatabase(60444591); err != nil {
		t.Fatalf("load state from clickhouse: %v", err)
	}
	t.Logf("state rebuild from ClickHouse (recovery path): %s, authoritative=%v",
		time.Since(loadStart).Round(time.Millisecond), proc.State.HotState.ColdAuthoritative())

	started := time.Now()
	stats, err := processDataFiles(ctx, t, proc, store, dataDir)
	if err != nil {
		t.Fatalf("process fixture: %v", err)
	}
	if _, err := proc.Flush(ctx, store, stats.lastBlock); err != nil {
		t.Fatalf("flush: %v", err)
	}
	wall := time.Since(started)

	// Count the ClickHouse round-trips this run issued against the scratch DB.
	var selects, selMS proto.ColUInt64
	_ = store.Conn().Do(ctx, ch.Query{Body: "SYSTEM FLUSH LOGS"})
	err = store.Conn().Do(ctx, ch.Query{
		Body: fmt.Sprintf(`SELECT count()::UInt64 AS n, toUInt64(sum(query_duration_ms)) AS ms
			FROM system.query_log
			WHERE type='QueryFinish' AND query_kind='Select'
			  AND query LIKE '%%%s%%' AND event_time >= now() - INTERVAL 30 MINUTE`, store.DB()),
		Result: proto.Results{{Name: "n", Data: &selects}, {Name: "ms", Data: &selMS}},
	})
	if err != nil {
		t.Logf("query_log lookup failed: %v", err)
	}

	var nSel, msSel uint64
	if selects.Rows() > 0 {
		nSel, msSel = selects.Row(0), selMS.Row(0)
	}
	t.Logf("BENCH: %d blocks, %d events in %s -> %.0f blk/s | state SELECTs: %d (%.1f/s, %dms total)",
		stats.blocks, stats.events, wall.Round(time.Millisecond),
		float64(stats.blocks)/wall.Seconds(), nSel, float64(nSel)/wall.Seconds(), msSel)
}
