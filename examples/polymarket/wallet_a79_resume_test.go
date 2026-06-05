package polymarket

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/shopspring/decimal"
)

// chConn holds resolved ClickHouse connection params for the resume tests.
type chConn struct {
	host     string
	port     int
	user     string
	password string
}

func resolveCH() chConn {
	return chConn{
		host:     envOr("CLICKHOUSE_HOST", "127.0.0.1"),
		port:     envIntOr("CLICKHOUSE_NATIVE_PORT", 9003),
		user:     envOr("CLICKHOUSE_USER", "default"),
		password: envOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse"),
	}
}

// runV2Phase runs one V2 proto ingestion phase against the compact fixture on a
// fixed DB name (so phases share state and exercise resume).
func runV2Phase(t *testing.T, db string, startBlock, endBlock uint64, restart bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	srv := fixtureSQDServer(t, "../../tests/wallet_0xa79af3b_compact.jsonl", 60)
	defer srv.Close()
	t.Setenv("SQD_PORTAL_ENDPOINT", srv.URL)

	project, err := config.LoadProject(".")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	cfg := project.Config
	proto := true
	cfg.ProtoMode = &proto
	start := startBlock
	end := endBlock
	for i := range cfg.Chains {
		cfg.Chains[i].ID = 137
		cfg.Chains[i].StartBlock = start
		cfg.Chains[i].EndBlock = &end
	}

	c := resolveCH()
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	opts := ingestion.Options{
		ClickHouseHost:     c.host,
		ClickHousePort:     c.port,
		ClickHouseUser:     c.user,
		ClickHousePassword: c.password,
		ClickHouseDatabase: db,
		GeneratedSQLDir:    "generated",
		Restart:            restart,
		CursorMode:         false,
		Processor:          proc,
	}
	if err := ingestion.Run(ctx, cfg, opts); err != nil && ctx.Err() == nil {
		t.Fatalf("ingestion.Run (start=%d end=%d restart=%v): %v", start, end, restart, err)
	}
}

// assertA79 reads the wallet positions from ClickHouse and asserts the canonical
// PnL (-13.93 / 3.00). Returns the values for logging.
func assertA79(t *testing.T, db string) (pnl, openVal decimal.Decimal) {
	t.Helper()
	ctx := context.Background()
	c := resolveCH()
	store, err := database.NewClickHouse(ctx, c.host, c.port, c.user, c.password, db)
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	defer store.Close()

	fresh := generated.NewState()
	if err := fresh.HotState.UserPositions.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover positions: %v", err)
	}
	wallet := common.HexToAddress("0xa79af3bab636f41f1f7bd1c568857dbdf4650beb")
	million := decimal.NewFromInt(1_000_000)
	realized := decimal.Zero
	openHalf := decimal.Zero
	var n int
	fresh.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.User != wallet {
			return true
		}
		n++
		realized = realized.Add(toDecimal(pos.RealizedPnL))
		if !pos.Amount.IsZero() {
			openHalf = openHalf.Add(toDecimal(pos.Amount).Mul(decimal.NewFromFloat(0.5)))
		}
		return true
	})
	if n == 0 {
		t.Fatal("no A79 positions in ClickHouse")
	}
	pnl = realized.Div(million)
	openVal = openHalf.Div(million)
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("realized PnL = %s, want -13.93 (data loss?)", pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("open value = %s, want 3.00 (data loss?)", openVal.StringFixed(4))
	}
	return pnl, openVal
}

// TestWalletA79ResumeFromCheckpoint runs the V2 pipeline to a mid-point, then
// restarts with a FRESH processor and resumes to completion. Proves the
// checkpoint -> rollback -> lazy-resolve -> re-process path reconstructs the
// correct final state across a restart boundary (no data loss).
func TestWalletA79ResumeFromCheckpoint(t *testing.T) {
	c := resolveCH()
	ctx := context.Background()
	db := fmt.Sprintf("polymarket_resume_%d", time.Now().UnixNano())
	probe, err := database.NewClickHouse(ctx, c.host, c.port, c.user, c.password, db)
	if err != nil {
		t.Skipf("ClickHouse required: %v", err)
	}
	_ = probe.Close()
	t.Cleanup(func() { _ = database.DropClickHouseDatabase(context.Background(), c.host, c.port, c.user, c.password, db) })

	// Phase 1: process blocks 1..30 (Restart drops + creates), clean exit flushes
	// + checkpoints at 30.
	runV2Phase(t, db, 1, 30, true)
	// Phase 2: fresh processor, resume (Restart=false) from the durable checkpoint
	// through block 59.
	runV2Phase(t, db, 1, 59, false)

	pnl, openVal := assertA79(t, db)
	t.Logf("[resume] realized=$%s open=$%s", pnl.StringFixed(4), openVal.StringFixed(4))
}

// TestWalletA79CrashOrphanedStateRecovery simulates a crash that left committed
// hot state in ClickHouse but NO durable checkpoint (hot state commits at a
// cadence; the checkpoint is written after). On resume the pipeline must wipe the
// orphaned state (rollback to start) and rebuild from scratch — never
// double-applying onto the orphaned state.
func TestWalletA79CrashOrphanedStateRecovery(t *testing.T) {
	ctx := context.Background()

	// Phase 0: directly commit hot state for the first 30 blocks' worth of A79
	// events WITHOUT writing any sync_state checkpoint (the "crash" leaves this
	// orphaned committed-ahead state behind).
	store := newPolymarketIntegrationStore(t, ctx)
	db := store.DB()
	allLogs := loadWalletCustomLogs(t, "../../tests/wallet_0xa79af3b_all.jsonl")
	var partial []ingestion.CustomLog
	// Take logs from the first ~half of the (block-ordered) fixture.
	cut := len(allLogs) / 2
	partial = append(partial, allLogs[:cut]...)
	proc0, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if err := proc0.Process(ctx, store, partial); err != nil {
		t.Fatalf("phase0 process: %v", err)
	}
	if _, err := proc0.Flush(ctx, store, 0); err != nil { // commit hot state, no checkpoint
		t.Fatalf("phase0 flush: %v", err)
	}
	_ = store.Close()

	// Phase 1: full pipeline resume (Restart=false) on the SAME db. With no
	// checkpoint present, resume must roll back the orphaned state to the start
	// block and rebuild, yielding the canonical PnL.
	runV2Phase(t, db, 1, 59, false)

	pnl, openVal := assertA79(t, db)
	t.Logf("[crash-orphan] realized=$%s open=$%s", pnl.StringFixed(4), openVal.StringFixed(4))
}
