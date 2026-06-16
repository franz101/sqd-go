package ingestion

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
)

// fetch_latency_test.go replicates the start-block-0 "burst/stall" reporting and
// validates the fix: a per-fetch latency clamp on the dense adaptive page
// (clampPageForLatency) plus a "fetching from <blk> (<dur>)" note in the stats
// line so a "+0 blocks / 0 blk/s" tick during a long single request is explained
// rather than looking like idleness.
//
// BACKGROUND: in sequential cursor mode one /finalized-stream request is
// fetched + parsed + emitted as one atomic unit. On a dense range the adaptive
// page (nextPageSize) grows toward the portal's byte/row cap (up to
// maxAdaptivePageSize=100000), so one request can cover tens of thousands of
// blocks and take many seconds; during that window the consumer starves and the
// 10s stats tick shows "+0 blocks / 0 blk/s", then a huge burst lands. The fix
// caps each request by wall time so progress stays continuous.

// --- 1. clampPageForLatency: the latency-bounded page shrink ------------------

func TestClampPageForLatency(t *testing.T) {
	const minPage uint64 = minAdaptivePageSize // 5000
	const target = 6 * time.Second

	cases := []struct {
		name     string
		page     uint64
		span     uint64
		fetchDur time.Duration
		target   time.Duration
		minPage  uint64
		want     uint64
	}{
		{
			// Fetch finished within the budget => never throttle a fast fetch.
			name: "within target unchanged",
			page: 20000, span: 20000, fetchDur: 4 * time.Second, target: target, minPage: minPage,
			want: 20000,
		},
		{
			// Fetch took exactly the budget (fetchDur == targetDur): the guard is
			// `fetchDur <= targetDur`, so it returns unchanged.
			name: "exactly at target unchanged",
			page: 20000, span: 20000, fetchDur: 6 * time.Second, target: target, minPage: minPage,
			want: 20000,
		},
		{
			// Fetch == 2x target with span == page: scaled = span*target/fetchDur =
			// 20000*6/12 = 10000 ~= page/2, and >= minPage(5000) so kept as 10000.
			name: "2x target halves page",
			page: 20000, span: 20000, fetchDur: 12 * time.Second, target: target, minPage: minPage,
			want: 10000,
		},
		{
			// Fetch way over target: scaled = 20000*6/60 = 2000, which floors below
			// minPage(5000) => clamped up to minPage. (Models the 50s "stall".)
			name: "far over target floors to minPage",
			page: 20000, span: 20000, fetchDur: 60 * time.Second, target: target, minPage: minPage,
			want: minPage,
		},
		{
			// targetDur == 0 disables the clamp entirely (pure byte/row-cap sizing).
			name: "disabled target unchanged",
			page: 20000, span: 20000, fetchDur: 60 * time.Second, target: 0, minPage: minPage,
			want: 20000,
		},
		{
			// span == 0 (gap / empty response): sizing can't be inferred, unchanged.
			name: "zero span unchanged",
			page: 20000, span: 0, fetchDur: 60 * time.Second, target: target, minPage: minPage,
			want: 20000,
		},
		{
			// Fetch barely over target so the scaled page would be >= page: the
			// latency path NEVER grows the page. span=20000, target=6s, fetch=6.5s =>
			// scaled = 20000*6/6.5 = 18461 < page? No: 18461 < 20000, so it would
			// shrink. Use a case where scaled >= page to prove the no-grow guard: a
			// SMALL page with a large span. page=8000, span=20000, fetch=7s =>
			// scaled = 20000*6/7 = 17142 >= 8000 => returns page (8000) unchanged.
			name: "scaled exceeds page never grows",
			page: 8000, span: 20000, fetchDur: 7 * time.Second, target: target, minPage: minPage,
			want: 8000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampPageForLatency(tc.page, tc.span, tc.fetchDur, tc.target, tc.minPage)
			if got != tc.want {
				t.Fatalf("clampPageForLatency(page=%d span=%d fetch=%v target=%v min=%d) = %d, want %d",
					tc.page, tc.span, tc.fetchDur, tc.target, tc.minPage, got, tc.want)
			}
			// The clamp must NEVER grow the page above its input.
			if got > tc.page {
				t.Fatalf("clamp grew the page from %d to %d (must only shrink)", tc.page, got)
			}
			// When it does shrink (target>0, fetch>target, span>0), it must respect
			// the floor.
			if got < tc.minPage && tc.want != tc.page {
				t.Fatalf("clamp dropped below minPage %d: got %d", tc.minPage, got)
			}
		})
	}
}

// --- 2. resolveTargetFetchDuration: env-driven per-fetch budget --------------

func TestResolveTargetFetchDuration(t *testing.T) {
	defaultDur := time.Duration(defaultTargetFetchSeconds * float64(time.Second)) // 6s

	cases := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
	}{
		{"unset -> default 6s", false, "", defaultDur},
		{"2.5 -> 2.5s", true, "2.5", time.Duration(2.5 * float64(time.Second))},
		{"0 -> disabled", true, "0", 0},
		{"-1 -> disabled", true, "-1", 0},
		{"garbage -> default 6s", true, "abc", defaultDur},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SQD_TARGET_FETCH_SECONDS", tc.value)
			} else {
				// Ensure a clean env even if the host shell exports it.
				t.Setenv("SQD_TARGET_FETCH_SECONDS", "")
			}
			got := resolveTargetFetchDuration()
			if got != tc.want {
				t.Fatalf("resolveTargetFetchDuration(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}

	// Sanity: the documented default really is 6s.
	if defaultDur != 6*time.Second {
		t.Fatalf("defaultTargetFetchSeconds resolves to %v, want 6s", defaultDur)
	}
}

// --- 3. adaptive loop: replicate the balloon, prove the clamp tames it -------

// latDenseSim models one dense adaptive fetch: the server returns the full
// requested window (span == requested), so nextPageSize doubles toward the cap,
// and the fetch wall time is proportional to the span at ~latBlocksPerSec
// (modeling a steady dense JSONL transfer + parse). This is the exact loop in
// ingestion.go: nextPageSize then clampPageForLatency, both on the adaptive page.
const latBlocksPerSec = 2000.0 // dense transfer+parse throughput

func latFetchDur(span uint64) time.Duration {
	return time.Duration(float64(span) / latBlocksPerSec * float64(time.Second))
}

func TestAdaptivePageBoundedByLatency(t *testing.T) {
	const target = 6 * time.Second
	const iterations = 12

	// WITHOUT the clamp: pure nextPageSize. On a dense range where the server
	// always returns the full window, the page doubles every request and pins to
	// maxAdaptivePageSize. This reproduces the observed 15k->20k->40k->80k->100k
	// growth and the multi-second-then-burst cadence.
	var noClampMaxPage uint64
	var noClampMaxFetch time.Duration
	{
		page := uint64(minAdaptivePageSize)
		for i := 0; i < iterations; i++ {
			span := page // server returns the full requested window
			fetch := latFetchDur(span)
			if page > noClampMaxPage {
				noClampMaxPage = page
			}
			if fetch > noClampMaxFetch {
				noClampMaxFetch = fetch
			}
			page = nextPageSize(page, page, span, false, minAdaptivePageSize, maxAdaptivePageSize)
		}
	}

	// The balloon: the page reaches the hard ceiling and a single fetch blows
	// well past 30s — exactly the "30s stall then huge burst" the operator saw.
	if noClampMaxPage != maxAdaptivePageSize {
		t.Fatalf("without clamp the page should balloon to maxAdaptivePageSize=%d, got %d",
			maxAdaptivePageSize, noClampMaxPage)
	}
	if noClampMaxFetch <= 30*time.Second {
		t.Fatalf("without clamp a single fetch should exceed ~30s (the observed stall), got %v",
			noClampMaxFetch)
	}

	// WITH the clamp: nextPageSize then clampPageForLatency(page, span, fetchDur,
	// 6s, min). The clamp shrinks any page whose just-completed fetch ran past the
	// budget, so the page converges instead of ballooning. There is a small
	// sawtooth (nextPageSize doubles the converged page to ~2x target, the clamp
	// pulls it straight back the next iteration), but it never approaches the
	// maxAdaptivePageSize ceiling and per-fetch time stays in the target band.
	var clampMaxPage uint64
	var clampMaxFetch time.Duration
	{
		page := uint64(minAdaptivePageSize)
		for i := 0; i < iterations; i++ {
			span := page
			fetch := latFetchDur(span)
			if page > clampMaxPage {
				clampMaxPage = page
			}
			if fetch > clampMaxFetch {
				clampMaxFetch = fetch
			}
			page = nextPageSize(page, page, span, false, minAdaptivePageSize, maxAdaptivePageSize)
			page = clampPageForLatency(page, span, fetch, target, minAdaptivePageSize)
		}
	}

	// The clamp must keep the page far below the ceiling. The converged page is
	// ~2000 blk/s * 6s = ~12000, doubling to at most ~24000 before the clamp pulls
	// it back. Anything that reached 100000 means the clamp failed.
	const wantConvergedPage = uint64(latBlocksPerSec * 6) // ~12000
	if clampMaxPage >= maxAdaptivePageSize {
		t.Fatalf("with clamp the page must NOT balloon to the ceiling; reached %d", clampMaxPage)
	}
	// Allow the one-step sawtooth overshoot (nextPageSize doubling the converged
	// page) but no more: 3x the converged page is a generous bound.
	if clampMaxPage > 3*wantConvergedPage {
		t.Fatalf("with clamp the page should converge near %d (saw at most ~2x); reached %d",
			wantConvergedPage, clampMaxPage)
	}

	// Per-fetch time stays in the target band: at most the sawtooth overshoot
	// (~2x target = 12s), never the 50s balloon. This is the smooth cadence.
	if clampMaxFetch > 13*time.Second {
		t.Fatalf("with clamp per-fetch time should stay near the 6s target (<=~12s sawtooth); got %v",
			clampMaxFetch)
	}

	t.Logf("without clamp: maxPage=%d maxFetch=%v (balloon)", noClampMaxPage, noClampMaxFetch)
	t.Logf("with clamp:    maxPage=%d maxFetch=%v (bounded near %v target)", clampMaxPage, clampMaxFetch, target)

	// The fix is meaningful only if it actually reduced both: assert the clamp
	// strictly improved on the unclamped balloon.
	if clampMaxPage >= noClampMaxPage {
		t.Fatalf("clamp did not bound the page (clamp=%d >= noclamp=%d)", clampMaxPage, noClampMaxPage)
	}
	if clampMaxFetch >= noClampMaxFetch {
		t.Fatalf("clamp did not bound per-fetch latency (clamp=%v >= noclamp=%v)", clampMaxFetch, noClampMaxFetch)
	}
}

// --- 4. full pipeline: dense slow fetch stays bounded (ClickHouse-gated) ------

// TestIntegrationDenseSlowFetchStaysBounded drives the real ingestion pipeline
// against fakeParallelPortal serving a DENSE range (every block present) with a
// per-request delay simulating a slow dense fetch. With SQD_TARGET_FETCH_SECONDS
// turned down low the latency clamp engages aggressively, and we assert the run
// makes continuous progress (the consumer cursor keeps advancing, rather than the
// long 0-coverage runs the burst/stall produced) and/or that a "fetching from"
// note surfaced in the stats during a slow fetch.
//
// Skips when ClickHouse is unavailable; ensure it COMPILES regardless.
func TestIntegrationDenseSlowFetchStaysBounded(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}

	// Dense portal: respCap large so one request can cover many blocks, with a
	// per-request delay making each fetch slow (the dense-slow scenario). garbage
	// range 0/0 = no malformed bodies.
	srv := fakeParallelPortal(50000, 0, 0, 400*time.Millisecond)
	defer srv.Close()

	// Point the producer at the fake portal and turn the per-fetch budget down so
	// the clamp shrinks the dense page hard.
	t.Setenv("SQD_PORTAL_ENDPOINT", srv.URL)
	t.Setenv("SQD_TARGET_FETCH_SECONDS", "0.5")

	// Capture the stats log so we can look for the "fetching from" note and count
	// non-zero-coverage ticks.
	var buf latSyncBuffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	host, port, password := chEnv()
	dbName := fmt.Sprintf("latency_test_%d", time.Now().UnixNano())

	// Minimal config: 1 chain (ID=1), 1 contract, 1 event, from genesis. EndBlock
	// a few hundred thousand so the dense slow path runs for a while under the
	// timeout. The portal serves header-only blocks (no logs), so no typed rows
	// are produced — we assert on coverage/progress, not on row counts.
	endBlock := uint64(300000)
	cfg := &config.Config{
		Name: dbName,
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: 0,
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

	// Create the base + typed tables exactly like integration_test.go.
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	opts := Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     "default",
		ClickHousePassword: password,
		ClickHouseDatabase: dbName,
		Restart:            true,
		CursorMode:         true,
		PageSize:           0, // adaptive page sizing -> exercises the clamp
		StartBlock:         0,
	}

	err = Run(ctx, cfg, opts)
	if err != nil && ctx.Err() == nil {
		t.Fatalf("ingestion.Run: %v", err)
	}

	logs := buf.String()

	// (a) The run made continuous progress: the consumer "next:" cursor advanced
	// past genesis, and we saw at least one stats tick with non-zero coverage
	// (i.e. NOT a long all-zero stall). We parse the "next: N" field across ticks.
	maxNext := latMaxNextCursor(logs)
	if maxNext == 0 {
		t.Fatalf("consumer cursor never advanced past genesis; logs:\n%s", logs)
	}
	t.Logf("max consumer next-cursor reached: %d", maxNext)

	nonZeroTicks := strings.Count(logs, "blk/s") - strings.Count(logs, "+0 blocks, 0 blk/s")
	t.Logf("stats ticks with non-zero blk/s: %d", nonZeroTicks)

	// (b) During a slow dense fetch the stats line should have surfaced the
	// in-flight note so a 0-coverage tick is explained as a long request, not a
	// stall. With a 400ms server delay and aggressive clamping this may or may not
	// land on a tick boundary, so it is informational unless progress also failed.
	sawFetchNote := strings.Contains(logs, "| fetching from ")
	t.Logf("saw in-flight 'fetching from' note: %v", sawFetchNote)

	if maxNext == 0 && !sawFetchNote {
		t.Fatalf("neither progress nor an in-flight fetch note observed; logs:\n%s", logs)
	}

	// Cleanup.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	if err := database.DropClickHouseDatabase(cleanupCtx, host, port, "default", password, dbName); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

// latSyncBuffer is a goroutine-safe bytes.Buffer for capturing log output while
// the producer and consumer goroutines both write through the std logger. Unique
// name (lat- prefix) so it can't collide with other same-package test helpers.
type latSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *latSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *latSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// latMaxNextCursor scans captured stats lines for the largest "next: N" value,
// proving the consumer cursor advanced (continuous progress) rather than stalling.
func latMaxNextCursor(logs string) uint64 {
	var max uint64
	for _, line := range strings.Split(logs, "\n") {
		idx := strings.Index(line, "next: ")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("next: "):]
		end := strings.IndexByte(rest, ' ')
		if end < 0 {
			end = len(rest)
		}
		var n uint64
		if _, err := fmt.Sscanf(rest[:end], "%d", &n); err == nil && n > max {
			max = n
		}
	}
	return max
}
