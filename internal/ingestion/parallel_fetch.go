package ingestion

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/envconfig"
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

// ParallelFetchSettings returns the default parallel fetch configuration.
// Workers: concurrent HTTP fetchers. Page size: blocks per work unit.
// RPS: target request rate to the portal (shared across all workers).
func ParallelFetchSettings() (workers, pageSize int, rps float64) {
	workers = envconfig.ParallelFetchers()
	pageSize = envconfig.ParallelPageSize()
	rps = envconfig.ParallelRPS()
	return
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

// fetchChunk represents one portal response in the parallel fetch stream.
// Unlike the previous page-based design, chunks are delivered as soon as
// a successful portal response completes, eliminating the stall where
// the consumer waits for a full 10,000-block page to accumulate.
type fetchChunk struct {
	seq         uint64      // sequence number for ordering
	from        uint64      // first requested block
	coveredTo   uint64      // last block covered by this response
	requestedTo uint64      // pinned upper bound of the request
	raw         []byte      // response JSONL
	head        client.Head // chain head
	err         error       // fetch error if any
}

type blockRange struct {
	from uint64
	to   uint64
}

type parallelPrefetcher struct {
	endpoint         string
	filters          []client.LogFilter
	includeAllBlocks bool
	startBlock       uint64
	endBlock         uint64 // inclusive
	pageSize         uint64 // grid-page width for work partitioning
	workers          int
	maxAhead         uint64 // bounded look-ahead window, in block numbers
	limiter          *rateLimiter

	nextBlock atomic.Uint64 // next fresh block to claim

	mu            sync.Mutex
	cond          *sync.Cond
	ready         map[uint64]*fetchChunk
	nextEmit      uint64 // next expected sequence number (starts at startBlock)
	stopClaim     bool   // an errored chunk was delivered; stop claiming new work
	doneWG        bool   // all workers have exited
	activeWorkers int    // number of workers currently fetching
	gaps          []blockRange // high-priority gaps
}

func newParallelPrefetcher(endpoint string, filters []client.LogFilter, includeAllBlocks bool, start, end, pageSize uint64, workers int, limiter *rateLimiter) *parallelPrefetcher {
	if workers < 1 {
		workers = 1
	}
	if pageSize == 0 {
		pageSize = defaultParallelPageSize
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
		maxAhead:         pageSize * uint64(workers),
		limiter:          limiter,
		ready:            make(map[uint64]*fetchChunk),
	}
	p.nextBlock.Store(start)
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *parallelPrefetcher) launch(ctx context.Context) {
	p.nextEmit = p.startBlock
	var wg sync.WaitGroup
	for range p.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx)
		}()
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

// Next returns the next chunk in ascending order, blocking until it is ready.
// ok=false means the range is fully emitted, the context was cancelled, or all
// workers exited before producing the needed chunk (the caller then resumes
// sequential fetch). A returned chunk carrying err surfaces a fatal fetch error.
func (p *parallelPrefetcher) Next(ctx context.Context) (*fetchChunk, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		// Try to get the exact expected sequence
		if chunk, ok := p.ready[p.nextEmit]; ok {
			delete(p.ready, p.nextEmit)
			p.nextEmit = chunk.coveredTo + 1 // advance to next expected block
			p.cond.Broadcast()               // slide the look-ahead window
			return chunk, true
		}

		// Expected sequence not ready - wait for it or context cancellation
		if ctx.Err() != nil {
			return nil, false
		}

		// Check if workers are done (no more chunks will arrive)
		if p.doneWG {
			// Find if any chunks are left in the ready map
			var minSeq uint64 = 0
			var found bool
			for seq := range p.ready {
				if !found || seq < minSeq {
					minSeq = seq
					found = true
				}
			}
			if !found {
				// No chunks available, truly done
				return nil, false
			}
			// There are chunks available - return the next one (may be error chunk)
			chunk := p.ready[minSeq]
			delete(p.ready, minSeq)
			p.nextEmit = chunk.coveredTo + 1
			p.cond.Broadcast()
			return chunk, true
		}

		p.cond.Wait()
	}
}
func (p *parallelPrefetcher) getWork(ctx context.Context) (blockRange, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for {
		if ctx.Err() != nil || p.stopClaim {
			return blockRange{}, false
		}

		if len(p.gaps) > 0 {
			gap := p.gaps[0]
			if gap.from <= p.nextEmit+p.maxAhead {
				p.gaps = p.gaps[1:]
				p.activeWorkers++
				return gap, true
			}
		}

		from := p.nextBlock.Load()
		if from <= p.endBlock {
			if from <= p.nextEmit+p.maxAhead {
				to := min(from+p.pageSize-1, p.endBlock)
				p.nextBlock.Store(to + 1)
				p.activeWorkers++
				return blockRange{from: from, to: to}, true
			}
		}

		if from > p.endBlock && len(p.gaps) == 0 && p.activeWorkers == 0 {
			p.stopClaim = true
			p.cond.Broadcast()
			return blockRange{}, false
		}

		p.cond.Wait()
	}
}

func (p *parallelPrefetcher) worker(ctx context.Context) {
	cl := client.New(p.endpoint)
	defer cl.Close()

	workerID := rand.Intn(10000) // for debug identification
	for {
		r, ok := p.getWork(ctx)
		if !ok {
			return
		}

		if !p.fetchAndDeliverChunks(ctx, cl, r, workerID) {
			p.mu.Lock()
			p.activeWorkers--
			if p.nextBlock.Load() > p.endBlock && len(p.gaps) == 0 && p.activeWorkers == 0 {
				p.stopClaim = true
				p.cond.Broadcast()
			}
			p.mu.Unlock()
			return
		}

		p.mu.Lock()
		p.activeWorkers--
		if p.nextBlock.Load() > p.endBlock && len(p.gaps) == 0 && p.activeWorkers == 0 {
			p.stopClaim = true
			p.cond.Broadcast()
		}
		p.mu.Unlock()
	}
}

func (p *parallelPrefetcher) fetchAndDeliverChunks(ctx context.Context, cl *client.Client, r blockRange, workerID int) bool {
	cur := r.from
	attempt := 0

	for cur <= r.to {
		if ctx.Err() != nil {
			return false
		}

		if err := p.limiter.wait(ctx); err != nil {
			p.deliverChunk(&fetchChunk{seq: cur, from: cur, err: err})
			return false
		}

		toPin := r.to
		resp, err := cl.FetchWithParent(ctx, cur, &toPin, "", p.includeAllBlocks, p.filters)
		if err != nil {
			if ctx.Err() != nil {
				p.deliverChunk(&fetchChunk{seq: cur, from: cur, err: ctx.Err()})
				return false
			}
			attempt++
			if attempt > parallelMaxAttempts {
				p.deliverChunk(&fetchChunk{
					seq:  cur,
					from: cur,
					err:  fmt.Errorf("worker %d: parallel fetch gave up after %d attempts: %w", workerID, attempt, err),
				})
				return false
			}
			p.backoff(ctx, attempt)
			continue
		}

		attempt = 0
		seq := cur

		coveredTo := cur
		if len(resp.Raw) > 0 {
			last, lerr := lastBlockNumber(resp.Raw)
			if lerr != nil {
				p.deliverChunk(&fetchChunk{seq: seq, from: cur, err: fmt.Errorf("worker %d: parse last block: %w", workerID, lerr)})
				return false
			}
			if last >= cur {
				coveredTo = last
			}
		}

		rawCopy := make([]byte, len(resp.Raw))
		copy(rawCopy, resp.Raw)

		p.deliverChunk(&fetchChunk{
			seq:         seq,
			from:        cur,
			coveredTo:   coveredTo,
			requestedTo: toPin,
			raw:         rawCopy,
			head:        resp.Head,
		})

		p.mu.Lock()
		stop := p.stopClaim
		p.mu.Unlock()
		if stop {
			return false
		}

		if coveredTo >= r.to {
			return true
		}

		remainingSpan := r.to - coveredTo
		batchSize := p.pageSize / 10
		if batchSize < 100 {
			batchSize = 100
		}

		if remainingSpan > batchSize {
			p.mu.Lock()
			nextStart := coveredTo + 1
			for nextStart <= r.to {
				nextEnd := min(nextStart+batchSize-1, r.to)
				p.gaps = append(p.gaps, blockRange{from: nextStart, to: nextEnd})
				nextStart = nextEnd + 1
			}
			sort.Slice(p.gaps, func(i, j int) bool {
				return p.gaps[i].from < p.gaps[j].from
			})
			p.cond.Broadcast()
			p.mu.Unlock()
			return true
		}

		cur = coveredTo + 1
	}

	return true
}

// deliverChunk adds a chunk to the ready map and broadcasts to wake the consumer.
func (p *parallelPrefetcher) deliverChunk(chunk *fetchChunk) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.ready[chunk.seq] = chunk
	if chunk.err != nil {
		p.stopClaim = true
	}
	p.cond.Broadcast()
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

// lastBlockNumber returns the highest block number in a raw JSONL response. The
// portal emits blocks in ascending order, so the last non-empty line is the
// highest — which for includeAllBlocks=false is the portal's marker block (the
// scanned high-water mark). It reverse-scans to that line and regexes the header
// number out of it: O(tail) and no full parse of every block (and no fastjson).
func lastBlockNumber(raw []byte) (uint64, error) {
	// Fast path: find the last non-empty line by scanning backwards
	end := len(raw)
	for end > 0 {
		// Skip trailing newlines
		for end > 0 && raw[end-1] == '\n' {
			end--
		}
		if end == 0 {
			break
		}
		// Find the start of this line
		nl := bytes.LastIndexByte(raw[:end], '\n')
		line := raw[nl+1 : end]
		if len(line) > 0 {
			// Apply regex to extract block number
			m := headerNumberRe.FindSubmatch(line)
			if m == nil {
				return 0, fmt.Errorf("no header number in last JSONL line")
			}
			return strconv.ParseUint(string(m[1]), 10, 64)
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
		// If an explicit end block is configured, use that instead
		if end != nil && *end < bound {
			bound = *end
		}
	} else {
		if end == nil {
			return 0, false // non-cursor mode requires an explicit end block
		}
		bound = *end
	}
	if bound <= from {
		return 0, false // nothing to parallelize
	}
	return bound, true
}

// parallelMinSpan returns the minimum span (in blocks) that justifies engaging
// parallel fetch, accounting for page size and worker coordination overhead.
func parallelMinSpan(pageSize, workers int) uint64 {
	// Reduced from 2 pages per worker to 1 page per worker for earlier engagement
	// With 6 workers and 10K page size: 60K blocks (down from 120K)
	minPages := workers
	if minPages < 2 {
		minPages = 2
	}
	return uint64(minPages) * uint64(pageSize)
}
