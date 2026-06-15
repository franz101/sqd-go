package ingestion

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

// headerNumberRe pulls the "number" out of a block-header JSONL line. With the
// portal's EVM field set, "number" appears only in the block header, and it is
// the first field, so the first match on a single line is the header number.
var headerNumberRe = regexp.MustCompile(`"number":\s*(\d+)`)

const (
	// defaultParallelFetchers is the worker count when --parallel-fetch is on and
	// SQD_PARALLEL_FETCHERS is unset. The shared rate limiter is the real throughput
	// lever (the portal caps ~5 req/s); workers only need to keep that budget
	// saturated despite per-request latency, so a handful suffices.
	defaultParallelFetchers = 6
	maxParallelFetchers     = 32
	// defaultParallelPageSize is the grid-page width (blocks) each worker claims
	// and cursors through. A page spans several portal responses; it only governs
	// how work is partitioned and how much raw JSONL is buffered ahead.
	defaultParallelPageSize = 10000
	minParallelPageSize     = 1000
	// defaultParallelRPS / defaultParallelBurst encode the portal's measured budget
	// of ~50 requests per 10s (≈5 req/s sustained, burst ~50). This is the backfill
	// ceiling — the per-request range cap (~1600 blocks) is fixed, so throughput is
	// rate × range, not worker count.
	defaultParallelRPS   = 5.0
	defaultParallelBurst = 50

	parallelMaxAttempts = 24
)

// rateLimiter is a shared token bucket: every concurrent worker draws from one
// instance so the aggregate request rate to the portal stays under its limit,
// regardless of worker count. Tokens refill continuously at ratePerSec up to
// burst. Plain wall-clock time is fine here (this is not a workflow script).
type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	burst    float64
	ratePerS float64
	last     time.Time
}

func newRateLimiter(ratePerSec float64, burst int) *rateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = defaultParallelRPS
	}
	if burst < 1 {
		burst = 1
	}
	return &rateLimiter{
		tokens:   float64(burst),
		burst:    float64(burst),
		ratePerS: ratePerSec,
		last:     time.Now(),
	}
}

// wait blocks until a token is available (or ctx is cancelled), then consumes it.
func (r *rateLimiter) wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		now := time.Now()
		r.tokens = min(r.burst, r.tokens+now.Sub(r.last).Seconds()*r.ratePerS)
		r.last = now
		if r.tokens >= 1 {
			r.tokens--
			r.mu.Unlock()
			return nil
		}
		deficit := 1 - r.tokens
		wait := time.Duration(deficit / r.ratePerS * float64(time.Second))
		r.mu.Unlock()
		if wait <= 0 {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// prefetchPage is one fixed-size grid page [from, to] of the finalized backfill
// range, fetched ahead of the consumer. raw is the newline-joined concatenation
// of every portal response that covered the page, copied out of the worker
// parser's view. head carries the last response's head (finalized/latest) for
// the downstream pipeline. With includeAllBlocks=false the raw is sparse
// (matching blocks + the portal's marker block at the scanned high-water mark).
type prefetchPage struct {
	idx  uint64
	from uint64
	to   uint64 // inclusive grid boundary (clamped to endBlock on the last page)
	raw  []byte
	head client.Head
	err  error
}

// parallelPrefetcher fetches a bounded, fully-finalized block range using N
// concurrent workers (paced by one shared rate limiter) and hands fixed-size
// pages to a single consumer in strict ascending block order. It exists because
// the sequential producer is bound by per-page HTTP round-trip latency to the
// portal, not by data, so a deep backfill is round-trip bound; running N cursor
// loops concurrently keeps the portal's request budget saturated.
//
// Correctness rests on the range being at or below the finalized head: those
// blocks are immutable, so workers skip the parent-hash fork-detection handshake
// (parentBlockHash="") and fetch disjoint grid pages out of order. The reorder
// buffer (the ready map + nextEmit) re-serializes them so the in-order consumer
// is unaffected. With includeAllBlocks=false the pages are sparse and the
// consumer skips the empty block-number gaps (see ReplayBuffer.CeilBlock).
type parallelPrefetcher struct {
	endpoint         string
	filters          []client.LogFilter
	includeAllBlocks bool
	startBlock       uint64
	endBlock         uint64 // inclusive
	pageSize         uint64
	workers          int
	maxAhead         uint64 // bounded look-ahead window, in pages
	limiter          *rateLimiter

	totalPages uint64
	nextClaim  atomic.Uint64

	mu        sync.Mutex
	cond      *sync.Cond
	ready     map[uint64]*prefetchPage
	nextEmit  uint64
	stopClaim bool // an errored page was delivered; stop claiming new work
	doneWG    bool // all workers have exited
}

func newParallelPrefetcher(endpoint string, filters []client.LogFilter, includeAllBlocks bool, start, end, pageSize uint64, workers int, limiter *rateLimiter) *parallelPrefetcher {
	if pageSize == 0 {
		pageSize = defaultParallelPageSize
	}
	if workers < 1 {
		workers = 1
	}
	if limiter == nil {
		limiter = newRateLimiter(defaultParallelRPS, defaultParallelBurst)
	}
	p := &parallelPrefetcher{
		endpoint:         endpoint,
		filters:          filters,
		includeAllBlocks: includeAllBlocks,
		startBlock:       start,
		endBlock:         end,
		pageSize:         pageSize,
		workers:          workers,
		maxAhead:         uint64(workers) + 2,
		limiter:          limiter,
		totalPages:       (end - start + pageSize) / pageSize, // ceil((end-start+1)/pageSize)
		ready:            make(map[uint64]*prefetchPage),
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// launch starts the worker pool plus two helper goroutines: one flips doneWG once
// every worker exits, and one broadcasts on cancellation so workers blocked on the
// look-ahead window and a consumer blocked in Next wake to observe ctx.Err().
func (p *parallelPrefetcher) launch(ctx context.Context) {
	var wg sync.WaitGroup
	for range p.workers {
		wg.Go(func() { p.worker(ctx) })
	}
	go func() {
		wg.Wait()
		p.mu.Lock()
		p.doneWG = true
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
	go func() {
		<-ctx.Done()
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
}

// Next returns the next page in ascending order, blocking until it is ready.
// ok=false means the range is fully emitted, the context was cancelled, or all
// workers exited before producing the needed page (the caller then resumes
// sequential fetch). A returned page carrying err surfaces a fatal fetch error.
func (p *parallelPrefetcher) Next(ctx context.Context) (*prefetchPage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if pg, ok := p.ready[p.nextEmit]; ok {
			delete(p.ready, p.nextEmit)
			p.nextEmit++
			p.cond.Broadcast() // slide the look-ahead window
			return pg, true
		}
		if p.nextEmit >= p.totalPages {
			return nil, false // fully emitted
		}
		if ctx.Err() != nil {
			return nil, false
		}
		if p.doneWG {
			// All workers exited and the page we need never arrived (cancellation
			// or a fatal error in a lower page that already returned). Resume
			// sequentially from the current cursor.
			return nil, false
		}
		p.cond.Wait()
	}
}

func (p *parallelPrefetcher) worker(ctx context.Context) {
	cl := client.New(p.endpoint)
	defer cl.Close()

	for {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		stop := p.stopClaim
		p.mu.Unlock()
		if stop {
			return
		}
		idx := p.nextClaim.Add(1) - 1
		if idx >= p.totalPages {
			return
		}
		if !p.waitForWindow(ctx, idx) {
			return
		}
		page := p.fetchPage(ctx, cl, idx)
		p.deliver(page)
		if page.err != nil {
			return
		}
	}
}

// waitForWindow blocks until page idx is within maxAhead of the consumer's
// emit cursor, bounding how far ahead (and how much raw JSONL) workers buffer.
func (p *parallelPrefetcher) waitForWindow(ctx context.Context, idx uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if ctx.Err() != nil || p.stopClaim {
			return false
		}
		if idx < p.nextEmit+p.maxAhead {
			return true
		}
		p.cond.Wait()
	}
}

func (p *parallelPrefetcher) deliver(page *prefetchPage) {
	p.mu.Lock()
	p.ready[page.idx] = page
	if page.err != nil {
		p.stopClaim = true
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

// fetchPage cursors through the grid page [from, to], pinning toBlock to the
// page boundary so a worker never crosses into another worker's range. The
// portal caps each response by range scanned (~1600 blocks), so a page is
// typically several round-trips, each gated by the shared rate limiter. Raw
// bytes are copied out of the client's reused decode buffer.
func (p *parallelPrefetcher) fetchPage(ctx context.Context, cl *client.Client, idx uint64) *prefetchPage {
	from := p.startBlock + idx*p.pageSize
	to := min(from+p.pageSize-1, p.endBlock)
	page := &prefetchPage{idx: idx, from: from, to: to}

	cur := from
	attempt := 0
	for cur <= to {
		if err := p.limiter.wait(ctx); err != nil {
			page.err = err
			return page
		}
		toPin := to
		resp, err := cl.FetchWithParent(ctx, cur, &toPin, "", p.includeAllBlocks, p.filters)
		if err != nil {
			if ctx.Err() != nil {
				page.err = ctx.Err()
				return page
			}
			// Finalized region: no ForkError is possible, so every error (incl. a
			// 429 that slips past the limiter, surfaced as a generic status error)
			// is transient. Back off with jitter so workers that throttle together
			// desynchronize, and give up only after a generous cap.
			attempt++
			if attempt > parallelMaxAttempts {
				page.err = fmt.Errorf("parallel fetch [%d-%d] gave up after %d attempts: %w", cur, to, attempt, err)
				return page
			}
			p.backoff(ctx, attempt)
			continue
		}
		attempt = 0
		page.head = resp.Head
		if len(resp.Raw) == 0 {
			break // NoContent: no data left at/after cur within this page
		}
		last, lerr := lastBlockNumber(resp.Raw)
		if lerr != nil {
			page.err = fmt.Errorf("parallel fetch [%d-%d] last block: %w", cur, to, lerr)
			return page
		}
		// resp.Raw aliases the client's reused decode buffer; copy before the next Fetch.
		page.raw = appendRawJSONL(page.raw, resp.Raw)
		if last < cur {
			break // portal returned nothing past cur — guard against a stuck cursor
		}
		cur = last + 1
	}
	return page
}

func (p *parallelPrefetcher) backoff(ctx context.Context, attempt int) {
	shift := min(attempt, 4)
	d := time.Duration(150*(1<<shift))*time.Millisecond + time.Duration(rand.Intn(250))*time.Millisecond
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// appendRawJSONL appends src to dst, guaranteeing a newline boundary between the
// previous response's last line and src's first so the downstream line-based
// parser sees well-formed JSONL across the join.
func appendRawJSONL(dst, src []byte) []byte {
	if len(dst) > 0 && dst[len(dst)-1] != '\n' {
		dst = append(dst, '\n')
	}
	dst = append(dst, src...)
	return dst
}

// lastBlockNumber returns the highest block number in a raw JSONL response. The
// portal emits blocks in ascending order, so the last non-empty line is the
// highest — which for includeAllBlocks=false is the portal's marker block (the
// scanned high-water mark). It reverse-scans to that line and regexes the header
// number out of it: O(tail) and no full parse of every block (and no fastjson).
func lastBlockNumber(raw []byte) (uint64, error) {
	end := len(raw)
	for end > 0 {
		nl := bytes.LastIndexByte(raw[:end], '\n')
		line := bytes.TrimSpace(raw[nl+1 : end]) // nl == -1 -> raw[0:end]
		if len(line) > 0 {
			m := headerNumberRe.FindSubmatch(line)
			if m == nil {
				return 0, fmt.Errorf("no header number in last JSONL line")
			}
			return strconv.ParseUint(string(m[1]), 10, 64)
		}
		if nl < 0 {
			break
		}
		end = nl
	}
	return 0, fmt.Errorf("no blocks in response")
}

// parallelFinalizedBound returns the highest block the parallel prefetcher may
// cover (inclusive) and whether parallel fetch can engage from `from`. In cursor
// mode it is the finalized head minus the catch-up margin (so the parallel range
// is entirely immutable), clamped to any configured end block. In non-cursor
// mode it is the configured end block, which must be set.
func parallelFinalizedBound(cursorMode bool, from, lastFinalized uint64, end *uint64) (uint64, bool) {
	var bound uint64
	if cursorMode {
		if lastFinalized <= finalizedCatchupMargin {
			return 0, false // finalized head not known yet (or too shallow)
		}
		bound = lastFinalized - finalizedCatchupMargin
	} else {
		if end == nil {
			return 0, false // unbounded backfill: no parallel target
		}
		bound = *end
	}
	if end != nil && *end < bound {
		bound = *end
	}
	if bound < from {
		return 0, false
	}
	return bound, true
}

// parallelMinSpan is the minimum finalized region worth parallelizing: below one
// page per worker the coordination overhead outweighs the gain, so the producer
// stays sequential.
func parallelMinSpan(pageSize uint64, workers int) uint64 {
	if pageSize == 0 {
		pageSize = defaultParallelPageSize
	}
	if workers < 1 {
		workers = 1
	}
	return pageSize * uint64(workers)
}

// ParallelFetchSettings resolves the worker count, grid-page width, and request
// rate from the environment (SQD_PARALLEL_FETCHERS, SQD_PARALLEL_PAGE,
// SQD_PARALLEL_RPS), falling back to the tuned defaults.
func ParallelFetchSettings() (workers int, pageSize uint64, rps float64) {
	workers = defaultParallelFetchers
	if v := strings.TrimSpace(os.Getenv("SQD_PARALLEL_FETCHERS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			workers = n
		}
	}
	if workers > maxParallelFetchers {
		workers = maxParallelFetchers
	}
	pageSize = defaultParallelPageSize
	if v := strings.TrimSpace(os.Getenv("SQD_PARALLEL_PAGE")); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n >= minParallelPageSize {
			pageSize = n
		}
	}
	rps = defaultParallelRPS
	if v := strings.TrimSpace(os.Getenv("SQD_PARALLEL_RPS")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			rps = f
		}
	}
	return workers, pageSize, rps
}
