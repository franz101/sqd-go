package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/shopspring/decimal"
)

// fixtureSQDServer serves wallet fixture blocks via the SQD finalized-stream
// protocol: POST {fromBlock,...} -> JSONL of blocks with number >= fromBlock,
// with finalized-head headers; 204 once exhausted.
func fixtureSQDServer(t *testing.T, fixturePath string, finalizedHead uint64) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		number uint64
		bytes  []byte
	}
	var lines []line
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l == "" {
			continue
		}
		var b struct {
			Header struct {
				Number uint64 `json:"number"`
			} `json:"header"`
		}
		if err := json.Unmarshal([]byte(l), &b); err != nil {
			t.Fatalf("parse fixture line: %v", err)
		}
		lines = append(lines, line{number: b.Header.Number, bytes: []byte(l)})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].number < lines[j].number })

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q struct {
			FromBlock uint64 `json:"fromBlock"`
		}
		_ = json.Unmarshal(body, &q)

		w.Header().Set("X-Sqd-Finalized-Head-Number", fmt.Sprintf("%d", finalizedHead))
		w.Header().Set("X-Sqd-Finalized-Head-Hash", "0xfinalizedhead")
		w.Header().Set("X-Sqd-Head-Number", fmt.Sprintf("%d", finalizedHead))
		w.Header().Set("Content-Type", "application/jsonl")

		var out [][]byte
		for _, ln := range lines {
			if ln.number >= q.FromBlock {
				out = append(out, ln.bytes)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Join(bytesSliceToStrings(out), "\n") + "\n"))
	}))
}

func bytesSliceToStrings(in [][]byte) []string {
	out := make([]string, len(in))
	for i, b := range in {
		out[i] = string(b)
	}
	return out
}

// TestWalletA79DevV1EndToEnd runs the full dev-v1 ingestion pipeline
// (fetch -> parse -> decode -> ringbuffer -> custom processor -> ClickHouse)
// against the wallet fixture (block numbers compacted to a contiguous range,
// preserving event order) via a mock SQD portal,
// then verifies the wallet PnL persisted in ClickHouse equals -$13.93.
func TestWalletA79DevV1EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	srv := fixtureSQDServer(t, "../../tests/wallet_0xa79af3b_compact.jsonl", 60)
	defer srv.Close()
	t.Setenv("SQD_PORTAL_ENDPOINT", srv.URL)
	t.Setenv("SQD_COMMIT_INTERVAL", "1") // commit every block so 59-block fixture persists

	project, err := config.LoadProject(".")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	cfg := project.Config
	proto := false
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
	db := fmt.Sprintf("polymarket_e2e_%d", time.Now().UnixNano())

	// Verify ClickHouse is reachable before running the pipeline.
	probe, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse required at %s:%d: %v", host, port, err)
	}
	_ = probe.Close()
	t.Cleanup(func() {
		_ = database.DropClickHouseDatabase(context.Background(), host, port, user, password, db)
	})

	proc, err := generated.NewProcessor(false) // V1 parsed mode
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
		CursorMode:         false, // backfill: stream to end, no fork/rollback
		Processor:          proc,
	}

	if err := ingestion.Run(ctx, cfg, opts); err != nil && ctx.Err() == nil {
		t.Fatalf("ingestion.Run: %v", err)
	}

	// Read positions back from ClickHouse into a fresh cache (post-run recovery).
	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Fatalf("reopen clickhouse: %v", err)
	}
	defer store.Close()

	fresh := generated.NewState()
	if err := fresh.HotState.UserPositions.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover positions: %v", err)
	}

	// Positions store human units (stake / USDC) since the unit migration —
	// realized PnL and amounts are read back directly, no 1e6 rescale.
	wallet := common.HexToAddress("0xa79af3bab636f41f1f7bd1c568857dbdf4650beb")
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

	pnl := realized
	openVal := openValueHalf
	t.Logf("[dev-v1 e2e] positions=%d nonzero=%d realized=$%s open=$%s",
		n, nonzero, pnl.StringFixed(4), openVal.StringFixed(4))

	if n == 0 {
		t.Fatal("no wallet positions persisted by dev-v1 pipeline")
	}
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("dev-v1 realized PnL = %s, want -13.93", pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("dev-v1 open positions value = %s, want 3.00", openVal.StringFixed(4))
	}
}
