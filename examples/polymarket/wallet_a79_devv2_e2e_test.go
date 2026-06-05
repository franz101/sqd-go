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

// runWalletA79V2Pipeline runs the full V2 proto ingestion pipeline against the
// wallet fixture via a mock SQD portal, then returns the realized PnL and open
// value read back from ClickHouse. parseDecodeV2 toggles the raw-JSONL ->
// ProcessJSONL path (SQD_PARSE_DECODE_V2).
func runWalletA79V2Pipeline(t *testing.T, parseDecodeV2 bool) (pnl, openVal decimal.Decimal, positions int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	srv := fixtureSQDServer(t, "../../tests/wallet_0xa79af3b_compact.jsonl", 60)
	defer srv.Close()
	t.Setenv("SQD_PORTAL_ENDPOINT", srv.URL)
	t.Setenv("SQD_COMMIT_INTERVAL", "1") // commit every block so 59-block fixture persists
	if parseDecodeV2 {
		t.Setenv("SQD_PARSE_DECODE_V2", "1")
	} else {
		t.Setenv("SQD_PARSE_DECODE_V2", "")
	}

	project, err := config.LoadProject(".")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	cfg := project.Config
	proto := true
	cfg.ProtoMode = &proto
	end := uint64(60)
	for i := range cfg.Chains {
		cfg.Chains[i].ID = 137
		cfg.Chains[i].StartBlock = 1
		cfg.Chains[i].EndBlock = &end
	}

	host := envOr("CLICKHOUSE_HOST", "127.0.0.1")
	port := envIntOr("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOr("CLICKHOUSE_USER", "default")
	password := envOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("polymarket_e2e_v2_%d", time.Now().UnixNano())

	probe, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse required at %s:%d: %v", host, port, err)
	}
	_ = probe.Close()
	t.Cleanup(func() {
		_ = database.DropClickHouseDatabase(context.Background(), host, port, user, password, db)
	})

	proc, err := generated.NewProcessor(true) // V2 proto mode
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	opts := ingestion.Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     user,
		ClickHousePassword: password,
		ClickHouseDatabase: db,
		GeneratedSQLDir:    "generated",
		Restart:            true,
		CursorMode:         false,
		Processor:          proc,
	}

	if err := ingestion.Run(ctx, cfg, opts); err != nil && ctx.Err() == nil {
		t.Fatalf("ingestion.Run: %v", err)
	}

	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Fatalf("reopen clickhouse: %v", err)
	}
	defer store.Close()

	fresh := generated.NewState()
	if err := fresh.HotState.UserPositions.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover positions: %v", err)
	}

	wallet := common.HexToAddress("0xa79af3bab636f41f1f7bd1c568857dbdf4650beb")
	million := decimal.NewFromInt(1_000_000)
	realized := decimal.Zero
	openValueHalf := decimal.Zero
	var n, nonzero int
	fresh.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.User != wallet {
			return true
		}
		n++
		realized = realized.Add(toDecimal(pos.RealizedPnL))
		if !pos.Amount.IsZero() {
			nonzero++
			openValueHalf = openValueHalf.Add(toDecimal(pos.Amount).Mul(decimal.NewFromFloat(0.5)))
		}
		return true
	})

	return realized.Div(million), openValueHalf.Div(million), n
}

// TestWalletA79DevV2EndToEnd runs the full V2 proto pipeline (proto Process path,
// no SQD_PARSE_DECODE_V2) batched through the real ingestion loop and verifies
// parity with V1: realized -$13.93, open ~$3.00.
func TestWalletA79DevV2EndToEnd(t *testing.T) {
	pnl, openVal, n := runWalletA79V2Pipeline(t, false)
	t.Logf("[dev-v2 e2e proto-Process] positions=%d realized=$%s open=$%s",
		n, pnl.StringFixed(4), openVal.StringFixed(4))
	if n == 0 {
		t.Fatal("no wallet positions persisted by dev-v2 pipeline")
	}
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("dev-v2 realized PnL = %s, want -13.93", pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("dev-v2 open positions value = %s, want 3.00", openVal.StringFixed(4))
	}
}

// TestWalletA79DevV2ParseDecodeEndToEnd runs the full V2 pipeline via the
// raw-JSONL ProcessJSONL path (SQD_PARSE_DECODE_V2=1) and verifies parity.
func TestWalletA79DevV2ParseDecodeEndToEnd(t *testing.T) {
	pnl, openVal, n := runWalletA79V2Pipeline(t, true)
	t.Logf("[dev-v2 e2e ProcessJSONL] positions=%d realized=$%s open=$%s",
		n, pnl.StringFixed(4), openVal.StringFixed(4))
	if n == 0 {
		t.Fatal("no wallet positions persisted by dev-v2 parse-decode pipeline")
	}
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("dev-v2 parse-decode realized PnL = %s, want -13.93", pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("dev-v2 parse-decode open positions value = %s, want 3.00", openVal.StringFixed(4))
	}
}
