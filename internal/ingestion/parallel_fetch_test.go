package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	var pages []*fetchChunk
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("unexpected page err at [%d-%d]: %v", pg.from, pg.coveredTo, pg.err)
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
		prevTo = pg.coveredTo
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
	// Requests whose fromBlock lands in [1500,1999] return garbage. The error must
	// surface in order: after every valid chunk below the garbage region, at the
	// first unit whose from falls inside it. The exact from depends on the unit
	// stride, so we only require it to be within the garbage region.
	srv := fakeParallelPortal(137, 1500, 1999, 0)
	defer srv.Close()

	p := newParallelPrefetcher(srv.URL, nil, true, 1000, 5000, 500, 4, noRateLimit())
	p.launch(context.Background())

	// Consume all valid chunks up to but not including the error chunk
	validChunks := 0
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			t.Fatalf("parallel fetch stopped before reaching the garbage region")
		}
		if pg.err != nil {
			// Error chunk found - must surface inside the garbage region [1500,1999].
			if pg.from < 1500 || pg.from > 1999 {
				t.Fatalf("error chunk at from=%d, want within [1500,1999] (err=%v)", pg.from, pg.err)
			}
			return // success
		}
		// Valid chunks must all start below the garbage region (in order).
		if pg.from < 1000 || pg.from >= 1500 {
			t.Fatalf("valid chunk at from=%d outside expected range [1000,1499]", pg.from)
		}
		validChunks++
		if validChunks > 100 {
			t.Fatalf("too many valid chunks (%d) without hitting the garbage region", validChunks)
		}
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

func errOf(pg *fetchChunk) error {
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

// ─────────────────────────────────────────────────────────────────────────────
// Empty-response / deadlock regression tests (c40b5fd follow-up)
// ─────────────────────────────────────────────────────────────────────────────

func TestParallelPrefetcherRegression_EmptyResponseNoDeadlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{})
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 1000, 2000, 500, 6, noRateLimit())
	p.launch(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, ok := p.Next(context.Background()); !ok {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: parallel fetch did not terminate on all-empty responses")
	}
}

func TestParallelPrefetcherRegression_MultipleWorkersAllEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{})
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 5000, 500, 6, noRateLimit())
	p.launch(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, ok := p.Next(context.Background()); !ok {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("6-worker parallel fetch deadlocked on all-empty responses")
	}
}

func TestParallelPrefetcherRegression_EmptyThenData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		if q.FromBlock == 1500 {
			fmt.Fprintf(w, "{\"header\":{\"number\":1500,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", 1500, 1700001500)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{})
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 1000, 2000, 500, 1, noRateLimit())
	p.launch(context.Background())
	done := make(chan []uint64, 1)
	go func() {
		var blocks []uint64
		for {
			pg, ok := p.Next(context.Background())
			if !ok {
				break
			}
			if pg.err != nil {
				t.Errorf("unexpected fatal err: %v", pg.err)
				break
			}
			blocks = append(blocks, blockNumbersOf(t, pg.raw)...)
		}
		done <- blocks
	}()
	select {
	case blocks := <-done:
		if len(blocks) != 1 || blocks[0] != 1500 {
			t.Fatalf("got blocks=%v, want [1500]", blocks)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parallel fetch did not terminate after empty-then-data sequence")
	}
}

func TestParallelPrefetcherRegression_SingleWorkerEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{})
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 10000, 1000, 1, noRateLimit())
	p.launch(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, ok := p.Next(context.Background()); !ok {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("single-worker deadlocked on empty responses")
	}
}

func TestParallelPrefetcherRegression_SparseEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		// Forward-scanning skipEmpties portal: a request [from, pin] returns every
		// matching block (multiples of 1000) in that range — the portal does not
		// require the request to start exactly on a data block. An empty 200 is
		// returned only when the pinned range genuinely contains no data.
		pin := uint64(1) << 62
		if q.ToBlock != nil {
			pin = *q.ToBlock
		}
		var b strings.Builder
		for n := ((q.FromBlock + 999) / 1000) * 1000; n <= pin; n += 1000 {
			fmt.Fprintf(&b, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", n, n, 1700000000+n)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, false, 1000, 3000, 500, 3, noRateLimit())
	p.launch(context.Background())
	var got []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("unexpected err: %v", pg.err)
		}
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	if len(got) != 3 || got[0] != 1000 || got[1] != 2000 || got[2] != 3000 {
		t.Fatalf("got %v, want [1000,2000,3000]", got)
	}
}

func TestParallelPrefetcherRegression_HighConcurrencyStress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		last := q.FromBlock + 99
		if q.ToBlock != nil && last > *q.ToBlock {
			last = *q.ToBlock
		}
		var b strings.Builder
		for n := q.FromBlock; n <= last; n++ {
			fmt.Fprintf(&b, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", n, n, 1700000000+n)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 100000, 1000, 32, noRateLimit())
	p.launch(context.Background())
	var got []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("err: %v", pg.err)
		}
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	if uint64(len(got)) != 100001 {
		t.Fatalf("got %d blocks, want 100001", len(got))
	}
	for i, n := range got {
		if n != uint64(i) {
			t.Fatalf("block[%d]=%d, want %d", i, n, i)
		}
	}
}

func TestParallelPrefetcherRegression_MultipleTransientErrors(t *testing.T) {
	var mu sync.Mutex
	attempts := make(map[uint64]int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		mu.Lock()
		attempts[q.FromBlock]++
		n := attempts[q.FromBlock]
		mu.Unlock()
		if n <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("transient"))
			return
		}
		fmt.Fprintf(w, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", q.FromBlock, q.FromBlock, 1700000000+q.FromBlock)
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 0, 1, 1, noRateLimit())
	p.launch(context.Background())
	done := make(chan []uint64, 1)
	go func() {
		var blocks []uint64
		for {
			pg, ok := p.Next(context.Background())
			if !ok {
				break
			}
			if pg.err != nil {
				t.Errorf("unexpected fatal err: %v", pg.err)
				break
			}
			blocks = append(blocks, blockNumbersOf(t, pg.raw)...)
		}
		done <- blocks
	}()
	select {
	case blocks := <-done:
		if len(blocks) != 1 || blocks[0] != 0 {
			t.Fatalf("got blocks=%v, want [0]", blocks)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parallel fetch did not recover from multiple transient errors")
	}
}

func TestParallelPrefetcherRegression_MixedEmptyAndData(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		n := requestCount.Add(1)
		if n%2 == 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{})
			return
		}
		fmt.Fprintf(w, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", q.FromBlock, q.FromBlock, 1700000000+q.FromBlock)
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 100, 10, 2, noRateLimit())
	p.launch(context.Background())
	done := make(chan []uint64, 1)
	go func() {
		var blocks []uint64
		for {
			pg, ok := p.Next(context.Background())
			if !ok {
				break
			}
			if pg.err != nil {
				t.Errorf("err: %v", pg.err)
				break
			}
			blocks = append(blocks, blockNumbersOf(t, pg.raw)...)
		}
		done <- blocks
	}()
	select {
	case blocks := <-done:
		if len(blocks) == 0 {
			t.Fatal("got no blocks")
		}
		for i := 1; i < len(blocks); i++ {
			if blocks[i] <= blocks[i-1] {
				t.Fatalf("blocks not ascending: ...%d, %d...", blocks[i-1], blocks[i])
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled on mixed empty/data chunks")
	}
}

func TestParallelPrefetcherRegression_SingleBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		fmt.Fprintf(w, "{\"header\":{\"number\":42,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", 42, 1700000042)
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 42, 42, 100, 1, noRateLimit())
	p.launch(context.Background())
	var got []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("err: %v", pg.err)
		}
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Fatalf("got %v, want [42]", got)
	}
}

func TestParallelPrefetcherRegression_PageSizeOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		fmt.Fprintf(w, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", q.FromBlock, q.FromBlock, 1700000000+q.FromBlock)
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 9, 1, 3, noRateLimit())
	p.launch(context.Background())
	var got []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("err: %v", pg.err)
		}
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	if uint64(len(got)) != 10 {
		t.Fatalf("got %d blocks, want 10", len(got))
	}
	for i, n := range got {
		if n != uint64(i) {
			t.Fatalf("block[%d]=%d, want %d", i, n, i)
		}
	}
}

func TestParallelPrefetcherRegression_Boundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		last := q.FromBlock + 99
		if q.ToBlock != nil && last > *q.ToBlock {
			last = *q.ToBlock
		}
		var b strings.Builder
		for n := q.FromBlock; n <= last; n++ {
			fmt.Fprintf(&b, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", n, n, 1700000000+n)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 99, 100, 1, noRateLimit())
	p.launch(context.Background())
	var got []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("err: %v", pg.err)
		}
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	if uint64(len(got)) != 100 {
		t.Fatalf("got %d blocks, want 100", len(got))
	}
	for i, n := range got {
		if n != uint64(i) {
			t.Fatalf("block[%d]=%d, want %d", i, n, i)
		}
	}
}

func TestParallelPrefetcherRegression_OutOfOrderReassembly(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		n := requestCount.Add(1)
		if n%2 == 1 {
			time.Sleep(20 * time.Millisecond)
		}
		last := q.FromBlock + 49
		if q.ToBlock != nil && last > *q.ToBlock {
			last = *q.ToBlock
		}
		var b strings.Builder
		for nn := q.FromBlock; nn <= last; nn++ {
			fmt.Fprintf(&b, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", nn, nn, 1700000000+nn)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 199, 50, 4, noRateLimit())
	p.launch(context.Background())
	var got []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("err: %v", pg.err)
		}
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	if uint64(len(got)) != 200 {
		t.Fatalf("got %d blocks, want 200", len(got))
	}
	for i, n := range got {
		if n != uint64(i) {
			t.Fatalf("block[%d]=%d, want %d", i, n, i)
		}
	}
}

func TestParallelPrefetcherRegression_WorkerCountExceedsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		fmt.Fprintf(w, "{\"header\":{\"number\":0,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", 0, 1700000000)
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 0, 1, 16, noRateLimit())
	p.launch(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, ok := p.Next(context.Background()); !ok {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("excess workers did not exit")
	}
}

func TestParallelPrefetcherRegression_CancelDuringBackoff(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		if n <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("transient"))
			return
		}
		fmt.Fprintf(w, "{\"header\":{\"number\":0,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", 0, 1700000000)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := newParallelPrefetcher(srv.URL, nil, true, 0, 10000, 1000, 2, noRateLimit())
	p.launch(ctx)
	time.Sleep(50 * time.Millisecond)
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
		t.Fatal("workers did not exit after cancel")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Adaptive density-driven gap-fill tests
// ─────────────────────────────────────────────────────────────────────────────

// densityPortal emulates the portal's dynamic no-toBlock walk: it scans forward
// from fromBlock and returns blocks for the dense region [denseFrom, denseTo],
// skipping empty regions in a single wide jump. With includeAllBlocks=false an
// empty scan returns an empty 200 body whose effective coverage is the pinned
// toBlock (the high-water mark the portal scanned to). It records every requested
// [from,to] range and the peak number of in-flight requests so a test can assert
// both the gap sizing and the fan-out.
type densityPortal struct {
	srv       *httptest.Server
	denseFrom uint64
	denseTo   uint64
	denseCap  uint64 // max blocks returned per response inside the dense region

	mu       sync.Mutex
	ranges   [][2]uint64 // recorded [from, pinnedTo]
	inflight int
	peak     int
}

func newDensityPortal(denseFrom, denseTo, denseCap uint64, delay time.Duration) *densityPortal {
	dp := &densityPortal{denseFrom: denseFrom, denseTo: denseTo, denseCap: denseCap}
	dp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")

		// toBlock is always pinned by the prefetcher; default huge if absent.
		pinnedTo := uint64(1) << 62
		if q.ToBlock != nil {
			pinnedTo = *q.ToBlock
		}

		dp.mu.Lock()
		dp.ranges = append(dp.ranges, [2]uint64{q.FromBlock, pinnedTo})
		dp.inflight++
		if dp.inflight > dp.peak {
			dp.peak = dp.inflight
		}
		dp.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		defer func() {
			dp.mu.Lock()
			dp.inflight--
			dp.mu.Unlock()
		}()

		// Dense region: return up to denseCap contiguous blocks (a short "page").
		if q.FromBlock >= dp.denseFrom && q.FromBlock <= dp.denseTo && pinnedTo >= dp.denseFrom {
			last := q.FromBlock + dp.denseCap - 1
			if last > dp.denseTo {
				last = dp.denseTo
			}
			if last > pinnedTo {
				last = pinnedTo
			}
			var b strings.Builder
			for n := q.FromBlock; n <= last; n++ {
				fmt.Fprintf(&b, `{"header":{"number":%d,"hash":"0x%064x","timestamp":%d}}`+"\n", n, n, 1700000000+n)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(b.String()))
			return
		}

		// Empty region: portal scanned [from, pinnedTo] and found no matching data.
		// Mirrors includeAllBlocks=false behavior — an empty 200 body.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{})
	}))
	return dp
}

func (dp *densityPortal) close() { dp.srv.Close() }

func (dp *densityPortal) snapshot() (ranges [][2]uint64, peak int) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	cp := make([][2]uint64, len(dp.ranges))
	copy(cp, dp.ranges)
	return cp, dp.peak
}

// drain consumes the prefetcher to completion, returning the matched block numbers.
func drain(t *testing.T, p *parallelPrefetcher) []uint64 {
	t.Helper()
	var got []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("unexpected err at from=%d: %v", pg.from, pg.err)
		}
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	return got
}

// TestParallelPrefetcherAdaptiveCompletenessMixed proves the adaptive gap-fill
// stays complete and in-order across a mixed empty+dense range with
// includeAllBlocks=false: a large empty prefix, a dense band, then an empty
// suffix. Every dense block must appear exactly once, ascending.
func TestParallelPrefetcherAdaptiveCompletenessMixed(t *testing.T) {
	const start, end uint64 = 0, 60000
	const denseFrom, denseTo, denseCap uint64 = 30000, 32000, 100
	dp := newDensityPortal(denseFrom, denseTo, denseCap, 0)
	defer dp.close()

	p := newParallelPrefetcher(dp.srv.URL, nil, false /*includeAllBlocks*/, start, end, defaultParallelPageSize, 6, noRateLimit())
	p.launch(context.Background())
	got := drain(t, p)

	wantN := denseTo - denseFrom + 1
	if uint64(len(got)) != wantN {
		t.Fatalf("got %d dense blocks, want %d", len(got), wantN)
	}
	for i, n := range got {
		if want := denseFrom + uint64(i); n != want {
			t.Fatalf("block[%d]=%d, want %d (gap/overlap/dup)", i, n, want)
		}
	}
}

// TestParallelPrefetcherAdaptiveDenseGapSizing proves that in a dense region the
// prefetcher contracts its toBlock pin toward the observed delivery size and fans
// the resulting small gap units across multiple workers concurrently.
func TestParallelPrefetcherAdaptiveDenseGapSizing(t *testing.T) {
	// Entirely dense range so every response is a short denseCap page. The pin must
	// converge near denseCap and many requests must overlap in flight.
	const start, end uint64 = 0, 20000
	const denseCap uint64 = 100
	dp := newDensityPortal(start, end, denseCap, 5*time.Millisecond)
	defer dp.close()

	p := newParallelPrefetcher(dp.srv.URL, nil, true /*includeAllBlocks*/, start, end, defaultParallelPageSize, 6, noRateLimit())
	p.launch(context.Background())
	got := drain(t, p)

	if uint64(len(got)) != end-start+1 {
		t.Fatalf("got %d blocks, want %d", len(got), end-start+1)
	}

	ranges, peak := dp.snapshot()
	if peak < 2 {
		t.Fatalf("dense region did not fan out: peak in-flight requests = %d, want >= 2", peak)
	}

	// After warm-up the pinned spans must contract toward the observed denseCap,
	// not stay at the wide initial estimate. Inspect the back half of the requests
	// (the estimate has converged by then) and require the median pinned span to be
	// within a small multiple of denseCap.
	var spans []uint64
	for _, rg := range ranges[len(ranges)/2:] {
		spans = append(spans, rg[1]-rg[0]+1)
	}
	if len(spans) == 0 {
		t.Fatal("no requests recorded")
	}
	// Use the MEDIAN (as documented above), not the mean: the final unit is an
	// inevitable sub-floor remainder — the lone last block when denseCap does not
	// evenly divide the range — and a single such outlier would drag the mean just
	// below the floor even though every steady-state span sits right at denseCap.
	sort.Slice(spans, func(i, j int) bool { return spans[i] < spans[j] })
	median := spans[len(spans)/2]
	if median > denseCap*4 {
		t.Fatalf("pinned span did not contract to density: median=%d, want <= %d (denseCap=%d)", median, denseCap*4, denseCap)
	}
	if median < adaptiveGapMin {
		t.Fatalf("pinned span contracted below floor: median=%d, floor=%d", median, adaptiveGapMin)
	}
}

// TestParallelPrefetcherAdaptiveEmptyMinimalRequests proves a fully-empty range
// completes with very few requests — each empty response lets the pin jump a wide
// span, so the prefetcher does NOT pointlessly fan out one request per small page.
func TestParallelPrefetcherAdaptiveEmptyMinimalRequests(t *testing.T) {
	const start, end uint64 = 0, 200000
	// denseFrom > denseTo => no dense region at all: every response is empty.
	dp := newDensityPortal(1, 0, 100, 0)
	defer dp.close()

	p := newParallelPrefetcher(dp.srv.URL, nil, false /*includeAllBlocks*/, start, end, defaultParallelPageSize, 6, noRateLimit())
	p.launch(context.Background())
	got := drain(t, p)
	if len(got) != 0 {
		t.Fatalf("got %d blocks over an empty range, want 0", len(got))
	}

	ranges, _ := dp.snapshot()
	// With a wide adaptiveGapMax pin, the span is covered in jumps of ~adaptiveGapMax.
	// A naive fixed-small-page walk would need orders of magnitude more requests.
	// Allow generous slack for grid partitioning but require it stays bounded.
	span := end - start + 1
	maxExpected := int(span/adaptiveGapMax) + p.workers*4 + 8
	if len(ranges) > maxExpected {
		t.Fatalf("empty range fanned out pointlessly: %d requests, want <= %d", len(ranges), maxExpected)
	}
}

func TestParallelPrefetcherRegression_SparseConsumerGapSkip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "99999999")
		if q.FromBlock%2000 != 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{})
			return
		}
		last := q.FromBlock + 49
		if q.ToBlock != nil && last > *q.ToBlock {
			last = *q.ToBlock
		}
		var b strings.Builder
		for n := q.FromBlock; n <= last; n += 2000 {
			fmt.Fprintf(&b, "{\"header\":{\"number\":%d,\"hash\":\"0x%064x\",\"timestamp\":%d}}\n", n, n, 1700000000+n)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	p := newParallelPrefetcher(srv.URL, nil, false, 0, 10000, 1000, 3, noRateLimit())
	p.launch(context.Background())
	var consumed []uint64
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("err: %v", pg.err)
		}
		consumed = append(consumed, blockNumbersOf(t, pg.raw)...)
	}
	if len(consumed) == 0 {
		t.Fatal("consumed no blocks")
	}
	for i := 1; i < len(consumed); i++ {
		if consumed[i] <= consumed[i-1] {
			t.Fatalf("blocks not ascending: ...%d, %d...", consumed[i-1], consumed[i])
		}
	}
}
