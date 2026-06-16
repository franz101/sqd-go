package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeParallelPortal serves JSONL block ranges like the SQD portal: it honors the pinned
// toBlock and caps each response at respCap blocks so the prefetcher must cursor
// within a page across several round-trips. Blocks whose `from` lands in
// garbageFrom..garbageTo get a malformed (non-JSON) 200 body to exercise the
// error path without slow HTTP retries.
func fakeParallelPortal(respCap uint64, garbageFrom, garbageTo uint64, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")

		if garbageFrom != 0 || garbageTo != 0 {
			if q.FromBlock >= garbageFrom && q.FromBlock <= garbageTo {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not-json-at-all\n"))
				return
			}
		}

		last := q.FromBlock + respCap - 1
		if q.ToBlock != nil && last > *q.ToBlock {
			last = *q.ToBlock
		}
		var b strings.Builder
		for n := q.FromBlock; n <= last; n++ {
			fmt.Fprintf(&b, `{"header":{"number":%d,"hash":"0x%064x","timestamp":%d}}`+"\n", n, n, 1700000000+n)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
}

func blockNumbersOf(t *testing.T, raw []byte) []uint64 {
	t.Helper()
	var out []uint64
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var b struct {
			Header struct {
				Number uint64 `json:"number"`
			} `json:"header"`
		}
		if err := json.Unmarshal(line, &b); err != nil {
			t.Fatalf("unmarshal block line %q: %v", line, err)
		}
		out = append(out, b.Header.Number)
	}
	return out
}

func TestParallelPrefetcherInOrderComplete(t *testing.T) {
	const start, end uint64 = 1000, 5000
	srv := fakeParallelPortal(137, 0, 0, 0) // small cap forces multi-round-trip pages
	defer srv.Close()

	p := newParallelPrefetcher(srv.URL, nil, true, start, end, 500, 6, noRateLimit())
	p.launch(context.Background())

	var pages []*prefetchPage
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("unexpected page err at [%d-%d]: %v", pg.from, pg.to, pg.err)
		}
		pages = append(pages, pg)
	}

	// Pages must be strictly ascending and grid-contiguous (no gaps/overlaps).
	var gotBlocks []uint64
	prevTo := start - 1
	for i, pg := range pages {
		if pg.from != prevTo+1 {
			t.Fatalf("page %d: from=%d, want %d (gap/overlap)", i, pg.from, prevTo+1)
		}
		prevTo = pg.to
		gotBlocks = append(gotBlocks, blockNumbersOf(t, pg.raw)...)
	}
	if prevTo != end {
		t.Fatalf("last page to=%d, want %d", prevTo, end)
	}

	// Every block number in [start,end] must appear exactly once, in order —
	// proving the concurrent out-of-order fetch was correctly re-serialized.
	if uint64(len(gotBlocks)) != end-start+1 {
		t.Fatalf("got %d blocks, want %d", len(gotBlocks), end-start+1)
	}
	for i, n := range gotBlocks {
		if want := start + uint64(i); n != want {
			t.Fatalf("block[%d]=%d, want %d", i, n, want)
		}
	}
}

func TestParallelPrefetcherErrorSurfacedInOrder(t *testing.T) {
	// pageSize 500 over [1000,5000]: page 0 = [1000,1499] (valid), page 1 =
	// [1500,1999] (garbage). The error must surface at page 1, after page 0.
	srv := fakeParallelPortal(137, 1500, 1999, 0)
	defer srv.Close()

	p := newParallelPrefetcher(srv.URL, nil, true, 1000, 5000, 500, 4, noRateLimit())
	p.launch(context.Background())

	pg0, ok := p.Next(context.Background())
	if !ok || pg0.err != nil || pg0.from != 1000 {
		t.Fatalf("page 0: ok=%v from=%d err=%v", ok, pg0.from, errOf(pg0))
	}
	pg1, ok := p.Next(context.Background())
	if !ok || pg1.err == nil || pg1.from != 1500 {
		t.Fatalf("page 1: ok=%v from=%d err=%v (want non-nil err at from=1500)", ok, pg1.from, errOf(pg1))
	}
}

func TestParallelPrefetcherCancel(t *testing.T) {
	srv := fakeParallelPortal(2000, 0, 0, 40*time.Millisecond)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 1_000_000, 1000, 4, noRateLimit())
	p.launch(ctx)

	if _, ok := p.Next(ctx); !ok {
		t.Fatal("expected at least one page before cancel")
	}
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, ok := p.Next(ctx); !ok {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Next did not return ok=false after cancellation")
	}
}

func TestParallelFinalizedBound(t *testing.T) {
	u := func(v uint64) *uint64 { return &v }
	cases := []struct {
		name        string
		cursor      bool
		from        uint64
		lastFinal   uint64
		end         *uint64
		wantBound   uint64
		wantEngaged bool
	}{
		{"cursor finalized minus margin", true, 1000, 1_000_000, nil, 1_000_000 - finalizedCatchupMargin, true},
		{"cursor clamped to end block", true, 1000, 1_000_000, u(50_000), 50_000, true},
		{"cursor finalized unknown", true, 1000, 0, nil, 0, false},
		{"cursor caught up to head", true, 1_000_000, 1_000_000, nil, 0, false},
		{"non-cursor uses end block", false, 0, 0, u(20_000), 20_000, true},
		{"non-cursor unbounded declines", false, 0, 0, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bound, ok := parallelFinalizedBound(tc.cursor, tc.from, tc.lastFinal, tc.end)
			if ok != tc.wantEngaged {
				t.Fatalf("engaged=%v, want %v", ok, tc.wantEngaged)
			}
			if ok && bound != tc.wantBound {
				t.Fatalf("bound=%d, want %d", bound, tc.wantBound)
			}
		})
	}
}

// TestParallelPrefetcherLivePortal exercises the prefetcher against the real SQD
// portal (no ClickHouse needed) to confirm the parallel path both stays complete
// over real data and beats the serial round-trip-bound baseline. Gated behind an
// env var so the default suite stays hermetic.
func TestParallelPrefetcherLivePortal(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live portal throughput check")
	}
	const endpoint = "https://portal.sqd.dev/datasets/ethereum-mainnet/finalized-stream"
	const start, end uint64 = 20_540_854, 20_540_854 + 20_000 - 1 // 20k finalized blocks (LBTC era)

	run := func(workers int) (uint64, time.Duration) {
		p := newParallelPrefetcher(endpoint, nil, true /*includeAllBlocks*/, start, end, 5000, workers, newRateLimiter(defaultParallelRPS, defaultParallelBurst))
		t0 := time.Now()
		p.launch(context.Background())
		var blocks uint64
		for {
			pg, ok := p.Next(context.Background())
			if !ok {
				break
			}
			if pg.err != nil {
				t.Fatalf("workers=%d: %v", workers, pg.err)
			}
			blocks += uint64(len(blockNumbersOf(t, pg.raw)))
		}
		return blocks, time.Since(t0)
	}

	b1, d1 := run(1)
	b6, d6 := run(6)
	if b1 != end-start+1 || b6 != end-start+1 {
		t.Fatalf("incomplete: serial=%d parallel=%d, want %d", b1, b6, end-start+1)
	}
	t.Logf("serial   (1 worker):  %d blocks in %v (%.0f blk/s)", b1, d1.Round(time.Millisecond), float64(b1)/d1.Seconds())
	t.Logf("parallel (6 workers): %d blocks in %v (%.0f blk/s)", b6, d6.Round(time.Millisecond), float64(b6)/d6.Seconds())
	t.Logf("speedup: %.2fx", d1.Seconds()/d6.Seconds())
}

func errOf(pg *prefetchPage) error {
	if pg == nil {
		return nil
	}
	return pg.err
}

// noRateLimit returns a limiter that never throttles, so hermetic tests don't
// pay the ~5 req/s production pacing.
func noRateLimit() *rateLimiter { return newRateLimiter(1e9, 1_000_000) }

func TestLastBlockNumber(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    uint64
		wantErr bool
	}{
		{"single header", `{"header":{"number":15001611,"hash":"0xabc","timestamp":1}}`, 15001611, false},
		{"multi ascending picks last", "{\"header\":{\"number\":100}}\n{\"header\":{\"number\":250}}\n", 250, false},
		{"trailing blank lines", "{\"header\":{\"number\":7}}\n\n\n", 7, false},
		{"header with logs", `{"header":{"number":42,"hash":"0x1"},"logs":[{"address":"0xdead","topics":["0x1"],"data":"0x"}]}`, 42, false},
		{"no trailing newline", "{\"header\":{\"number\":1}}\n{\"header\":{\"number\":2}}", 2, false},
		{"empty", "", 0, true},
		{"garbage", "not json at all\n", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lastBlockNumber([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("lastBlockNumber = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReplayBufferCeilBlockAndLatest(t *testing.T) {
	rb := NewReplayBuffer(8)
	// Sparse stream: present blocks 10, 25, 26, 100 with empty gaps between.
	for _, n := range []uint64{10, 25, 26, 100} {
		rb.Write(1, n, "h", time.Time{}, nil, nil, nil, nil, false, "", n, nil)
	}
	if got := rb.LatestBlock(); got != 100 {
		t.Fatalf("LatestBlock = %d, want 100", got)
	}
	check := func(n, want uint64, wantOK bool) {
		t.Helper()
		got, ok := rb.CeilBlock(n)
		if ok != wantOK || (ok && got != want) {
			t.Fatalf("CeilBlock(%d) = %d,%v want %d,%v", n, got, ok, want, wantOK)
		}
	}
	check(0, 10, true)
	check(10, 10, true)
	check(11, 25, true) // gap 11..24 -> next present is 25
	check(26, 26, true)
	check(27, 100, true) // gap 27..99 -> next present is 100
	check(100, 100, true)
	check(101, 0, false) // nothing present beyond the high-water mark
}

func TestRateLimiterPacesAndCancels(t *testing.T) {
	// burst 2 then 50/s: the 3rd token must wait ~20ms (1/50s) after the burst.
	rl := newRateLimiter(50, 2)
	ctx := context.Background()
	for i := range 2 {
		if err := rl.wait(ctx); err != nil {
			t.Fatalf("burst token %d: %v", i, err)
		}
	}
	t0 := time.Now()
	if err := rl.wait(ctx); err != nil {
		t.Fatalf("paced token: %v", err)
	}
	if d := time.Since(t0); d < 10*time.Millisecond {
		t.Fatalf("3rd token arrived in %v; expected pacing (~20ms)", d)
	}

	// A cancelled context unblocks a waiting acquire.
	cctx, cancel := context.WithCancel(context.Background())
	slow := newRateLimiter(0.001, 1) // ~never refills
	if err := slow.wait(cctx); err != nil {
		t.Fatalf("first token: %v", err)
	}
	cancel()
	if err := slow.wait(cctx); err == nil {
		t.Fatal("expected ctx error after cancel, got nil")
	}
}
