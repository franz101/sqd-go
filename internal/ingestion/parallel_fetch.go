package ingestion

import (
	"bytes"
	"context"
	"fmt"
	"log"
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

	// Adaptive gap-fill sizing bounds. The portal returns blocks dynamically when
	// no toBlock is pinned: it scans far through empty regions (a single response
	// can cover many thousands of blocks) but returns only a short run once it hits
	// matching/dense data. A lone sequential walker is therefore fast through empty
	// regions but slow through dense ones. To beat that, we pin each request's
	// toBlock to the *recently observed delivered span* so that:
	//   - in empty regions the pin is wide (≈ adaptiveGapMax), letting one request
	//     jump far — no pointless fan-out, matching the no-toBlock walk speed; and
	//   - in dense regions the pin shrinks toward what the portal actually returns,
	//     turning the remaining span into many small in-order gap units that the
	//     worker pool fills concurrently (the shared rate limiter is the ceiling).
	// The estimate re-measures continuously, so the pin keeps tracking density.
	adaptiveGapMin = 100    // floor: never pin tighter than a dense portal page (~100)
	adaptiveGapMax = 131072 // ceiling: at/above the portal's ~106k empty-region scan cap

	// beastModeThreshold is the number of consecutive "empty" chunks (portal
	// extent-marker only, no matching events) before workers stop pinning toBlock and
	// let the portal decide its own stride. In empty regions the portal can jump
	// hundreds of thousands of blocks per nil-toBlock request; pinning would cap it.
	beastModeThreshold = 5
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

// blockRange is one work unit: a half-open-free inclusive [from, to] span a
// worker fetches in a single portal request. Units are claimed at ~one-request
// granularity (stride ≈ the portal's observed jump) so CONSECUTIVE requests go to
// DIFFERENT workers — the in-order head is filled concurrently instead of by one
// worker walking a whole page sequentially.
type blockRange struct {
	from uint64
	to   uint64
}

// parallelPrefetcher fetches a bounded, fully-finalized block range using N
// concurrent workers (paced by one shared rate limiter) and hands chunks
// (individual portal responses) to a single consumer in strict ascending
// block order. It exists because the sequential producer is bound by per-page
// HTTP round-trip latency to the portal, not by data, so a deep backfill is
// round-trip bound; running N cursor loops concurrently keeps the portal's
// request budget saturated.
//
// The chunk-based design ensures visible progress immediately: the first
// successful response becomes available to the consumer without waiting
// for the entire 10,000-block page to accumulate. Chunks are reordered
// so the in-order consumer is unaffected by out-of-order completion.
//
// Correctness rests on the range being at or below the finalized head: those
// blocks are immutable, so workers skip the parent-hash fork-detection handshake
// (parentBlockHash="") and fetch disjoint ranges out of order. The reorder
// buffer (the ready map + nextEmit) re-serializes them so the in-order consumer
// is unaffected. With includeAllBlocks=false the chunks are sparse and the
// consumer skips the empty block-number gaps (see ReplayBuffer.CeilBlock).
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

	nextBlock atomic.Uint64 // next unclaimed block (monotonic cursor over [start,end])

	// gapSpan is the shared, continuously re-measured estimate of how many blocks
	// a single portal response delivers (covered span), used to size each work
	// unit. All workers read and update it so density learned by one worker steers
	// the whole pool. It is an exponential moving average kept in fixed point
	// (block count); see updateGapSpan / currentGapSpan.
	// Not updated in beast mode: unguided portal jumps don't reflect pin-optimal density.
	gapSpan atomic.Uint64

	// Beast mode: after beastModeThreshold consecutive empty chunks workers stop
	// pinning toBlock (nil toBlock = portal decides stride). In truly empty regions
	// the portal can jump millions of blocks in one request; pinned mode would cap it
	// to adaptiveGapMax. Beast mode exits immediately on the first chunk with events,
	// resetting gapSpan to the observed dense span so pinned mode resumes at the
	// right window size.
	consecutiveEmpty atomic.Int64
	beastMode        atomic.Bool

	mu            sync.Mutex
	cond          *sync.Cond
	ready         map[uint64]*fetchChunk
	nextEmit      uint64       // next expected sequence number (starts at startBlock)
	gaps          []blockRange // high-priority remainders of short (dense) responses, ascending
	activeWorkers int          // units currently being fetched (in flight)
	stopClaim     bool         // an errored chunk was delivered; stop claiming new work
	doneWG        bool         // all workers have exited
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
	// Start in beast mode (nil toBlock). The first request(s) serve as probes:
	//   - Empty region:  portal jumps far; beast mode stays on and skips empties fast.
	//   - Dense region:  portal returns ~N blocks with events; beast mode exits and
	//                    gapSpan is snapped directly to N (not EWMA from 131072).
	//                    Workers then claim N-block units in parallel, matching what
	//                    sequential would do but overlapping the requests.
	//
	// Without this probe the old code set gapSpan=131072, claimed huge ranges, got
	// 1500 blocks back, and cascaded into ~21 gap-fill requests for 9000 blocks vs
	// sequential's 6 — making parallel 23% slower in dense regions.
	p.gapSpan.Store(adaptiveGapMax)
	p.beastMode.Store(true) // probe-first: beast mode until first density reading
	p.cond = sync.NewCond(&p.mu)
	return p
}

// launch starts the worker pool plus two helper goroutines: one flips doneWG once
// every worker exits, and one broadcasts on cancellation so workers blocked on the
// look-ahead window and a consumer blocked in Next wake to observe ctx.Err().
func (p *parallelPrefetcher) launch(ctx context.Context) {
	// Initialize nextEmit to the start block (sequences are block-based)
	p.nextEmit = p.startBlock

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

func (p *parallelPrefetcher) worker(ctx context.Context) {
	cl := client.New(p.endpoint)
	defer cl.Close()

	workerID := rand.Intn(10000) // for debug identification
	for {
		r, ok := p.getWork(ctx)
		if !ok {
			return
		}
		// One unit == one request. The worker returns to getWork immediately
		// after, so consecutive requests are claimed by whichever worker is free —
		// the in-order head is filled concurrently rather than by one worker
		// cursoring a whole page.
		if !p.fetchOne(ctx, cl, r, workerID) {
			return // fatal error or context cancelled
		}
	}
}

// windowLimit is the look-ahead budget (in blocks) ahead of nextEmit that workers
// may claim. It tracks the adaptive stride so the window always admits about
// 2×workers units regardless of density: wide in empty regions (a few large-span
// but tiny-payload chunks) and narrow in dense regions (many small-span chunks,
// bounding buffered raw JSONL). Floored at the configured page window.
func (p *parallelPrefetcher) windowLimit() uint64 {
	w := p.currentGapSpan() * uint64(p.workers) * 8
	if w < p.maxAhead {
		w = p.maxAhead
	}
	return w
}

// getWork claims the next unit: a pending gap (short-response remainder, highest
// priority since it is the lowest unemitted block) or a fresh stride off the
// cursor. It blocks until a unit is claimable within the look-ahead window, or
// returns ok=false when the range is exhausted, an error latched stopClaim, or
// ctx is cancelled. activeWorkers is incremented for every unit returned and must
// be balanced by finishUnit.
func (p *parallelPrefetcher) getWork(ctx context.Context) (blockRange, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if ctx.Err() != nil || p.stopClaim {
			return blockRange{}, false
		}
		window := p.nextEmit + p.windowLimit()

		// Gaps first: they are below the cursor and gate nextEmit progress.
		if len(p.gaps) > 0 && p.gaps[0].from <= window {
			g := p.gaps[0]
			p.gaps = p.gaps[1:]
			p.activeWorkers++
			return g, true
		}

		// Fresh stride off the cursor. Probe slightly above the current estimate
		// so the stride can grow back toward the portal's natural jump after a
		// dense stretch; an overshoot just yields a (re-queued) gap, never a miss.
		from := p.nextBlock.Load()
		if from <= p.endBlock && from <= window {
			// In beast mode the portal may jump the cursor far beyond our claimed
			// range, advancing nextEmit to a block number that no other in-flight
			// worker's chunk starts at. If concurrent workers have already claimed
			// intermediate ranges those chunks sit orphaned in p.ready and the
			// consumer deadlocks waiting for a seq it will never see.
			// Serialize fresh-cursor claims to one at a time in beast mode: the
			// single in-flight worker's coveredTo becomes the next claim's r.from,
			// so nextEmit always has a corresponding chunk. Gap claims are still
			// parallel (they come from the block above and bypass this check).
			if p.beastMode.Load() && p.activeWorkers > 0 {
				p.cond.Wait()
				continue
			}
			stride := p.currentGapSpan()
			stride += stride / 4
			if stride > adaptiveGapMax {
				stride = adaptiveGapMax
			}
			to := min(from+stride-1, p.endBlock)
			p.nextBlock.Store(to + 1)
			p.activeWorkers++
			return blockRange{from: from, to: to}, true
		}

		// Termination: cursor exhausted, no gaps, nothing in flight that could
		// enqueue a new gap. Latch stopClaim so peers and Next() unblock.
		if from > p.endBlock && len(p.gaps) == 0 && p.activeWorkers == 0 {
			p.stopClaim = true
			p.cond.Broadcast()
			return blockRange{}, false
		}

		p.cond.Wait()
	}
}

// finishUnit balances the activeWorkers increment from getWork and wakes peers so
// a worker blocked in getWork re-evaluates termination or a freshly enqueued gap.
func (p *parallelPrefetcher) finishUnit() {
	p.mu.Lock()
	p.activeWorkers--
	p.cond.Broadcast()
	p.mu.Unlock()
}

// fetchOne issues exactly one portal request for the unit [r.from, r.to] (pinning
// toBlock=r.to so the response never escapes the unit), delivers the chunk, folds
// the observed span into the density estimate, and — if the response fell short of
// r.to (a dense region, where the portal returns fewer blocks than pinned) —
// re-queues the uncovered remainder [coveredTo+1, r.to] as gap units. Completeness:
// every block of the unit is covered by this chunk OR by a re-queued gap, and units
// themselves tile [start,end] via the cursor, so no block is ever skipped.
// Returns false on a fatal/cancelled fetch (an error chunk is delivered first).
func (p *parallelPrefetcher) fetchOne(ctx context.Context, cl *client.Client, r blockRange, workerID int) bool {
	defer p.finishUnit()

	attempt := 0
	for {
		if ctx.Err() != nil {
			return false
		}
		if err := p.limiter.wait(ctx); err != nil {
			p.deliverChunk(&fetchChunk{seq: r.from, from: r.from, err: err})
			return false
		}

		// Snapshot beast mode for this attempt. In beast mode workers use nil toBlock
		// so the portal decides its own stride — it can jump over millions of empty
		// blocks in one request. In pinned mode toBlock = r.to, matching old behaviour.
		isBeast := p.beastMode.Load()
		var toBlockPtr *uint64
		if !isBeast {
			tp := r.to
			toBlockPtr = &tp
		}

		resp, err := cl.FetchWithParent(ctx, r.from, toBlockPtr, "", p.includeAllBlocks, p.filters)
		if err != nil {
			if ctx.Err() != nil {
				p.deliverChunk(&fetchChunk{seq: r.from, from: r.from, err: ctx.Err()})
				return false
			}
			attempt++
			if attempt > parallelMaxAttempts {
				p.deliverChunk(&fetchChunk{seq: r.from, from: r.from,
					err: fmt.Errorf("worker %d: parallel fetch gave up after %d attempts: %w", workerID, attempt, err)})
				return false
			}
			p.backoff(ctx, attempt)
			continue
		}

		// coveredTo is the scanned high-water mark.
		//
		// Pinned mode: the portal honours the toBlock pin, so coveredTo ≤ r.to. A
		// short/empty body gets a conservative bound of one pageSize so a dense band
		// is never skipped by more than pageSize (the uncovered remainder is re-queued).
		//
		// Beast mode (nil toBlock): the portal decides how far to scan; coveredTo can
		// legitimately exceed r.to. We do NOT clamp to r.to here — the caller advances
		// the shared cursor to coveredTo+1 to prevent other workers re-fetching the
		// already-covered tail.
		coveredTo := min(r.from+p.pageSize-1, r.to)
		if len(resp.Raw) > 0 {
			last, lerr := lastBlockNumber(resp.Raw)
			if lerr != nil {
				p.deliverChunk(&fetchChunk{seq: r.from, from: r.from,
					err: fmt.Errorf("worker %d: parse last block: %w", workerID, lerr)})
				return false
			}
			if last < r.from {
				last = r.from
			}
			// Clamp to endBlock always; clamp to r.to only in pinned mode.
			if last > p.endBlock {
				last = p.endBlock
			}
			if !isBeast && last > r.to {
				last = r.to
			}
			coveredTo = last
		}

		// Density estimate: update only in pinned mode. Beast-mode spans are unguided
		// portal jumps; feeding them into gapSpan would corrupt the pin for dense regions.
		if !isBeast {
			if coveredTo < r.to {
				p.updateGapSpan(coveredTo - r.from + 1)
			} else {
				p.updateGapSpan(r.to - r.from + 1)
			}
		}

		rawCopy := make([]byte, len(resp.Raw))
		copy(rawCopy, resp.Raw)

		// Beast mode tracking (skip-empties mode only; includeAllBlocks never has
		// empty-marker-only responses since every block is always returned).
		if !p.includeAllBlocks {
			// A response with zero internal newlines (after trimming any trailing \n)
			// is the extent-marker block only — no matching events in the scanned range.
			// Trim trailing newlines first: some portal responses end with an extra \n
			// that would make the count 1 even for a single-line extent-marker response,
			// falsely treating every empty range as non-empty and preventing beast mode
			// from re-engaging.
			lineCount := bytes.Count(bytes.TrimRight(rawCopy, "\n"), []byte{'\n'})
			isEmpty := lineCount == 0

			if isEmpty {
				if n := p.consecutiveEmpty.Add(1); n >= beastModeThreshold && !p.beastMode.Load() {
					p.beastMode.Store(true)
					log.Printf("[parallel] beast mode ON at block %d (%d consecutive empty ranges; nil toBlock)", r.from, n)
				}
			} else {
				p.consecutiveEmpty.Store(0)
				if isBeast {
					// Exit beast mode immediately: events found. Set gapSpan directly to
					// the observed dense span so pinned mode starts at the right window
					// size without waiting for EWMA to descend from adaptiveGapMax.
					p.beastMode.Store(false)
					denseSpan := coveredTo - r.from + 1
					if denseSpan < adaptiveGapMin {
						denseSpan = adaptiveGapMin
					}
					if denseSpan > adaptiveGapMax {
						denseSpan = adaptiveGapMax
					}
					p.gapSpan.Store(denseSpan)
					log.Printf("[parallel] beast mode OFF at block %d (events found, span=%d; resuming pinned mode)", r.from, denseSpan)
				}
			}
		}

		// In beast mode the portal may jump beyond our claimed range [r.from, r.to].
		// Advance the shared nextBlock cursor to coveredTo+1 so other workers skip
		// the already-covered tail (best-effort; a race means a worker may still claim
		// a now-redundant range, but it will get a small/empty response and move on).
		if isBeast && coveredTo > r.to {
			for {
				old := p.nextBlock.Load()
				if old > coveredTo+1 {
					break // another worker already advanced past this point
				}
				if p.nextBlock.CompareAndSwap(old, coveredTo+1) {
					break
				}
			}
		}

		var requestedToVal uint64
		if toBlockPtr != nil {
			requestedToVal = *toBlockPtr
		} else {
			requestedToVal = coveredTo // beast mode: portal decided
		}
		p.deliverChunk(&fetchChunk{
			seq:         r.from,
			from:        r.from,
			coveredTo:   coveredTo,
			requestedTo: requestedToVal,
			raw:         rawCopy,
			head:        resp.Head,
		})

		// Re-queue the uncovered remainder. In pinned mode this is the classic dense
		// gap-fill. In beast mode (undershoot only — overshoot was handled above by
		// advancing the cursor), it ensures no blocks are silently skipped.
		if coveredTo < r.to {
			p.enqueueGaps(coveredTo+1, r.to)
		}
		return true
	}
}

// enqueueGaps splits [from,to] into estimate-sized units and inserts them into the
// priority gap queue (kept ascending so the lowest unemitted block is fetched
// first). Sized to the current — now contracted — estimate so a dense remainder
// becomes several concurrent units rather than one serial cursor.
func (p *parallelPrefetcher) enqueueGaps(from, to uint64) {
	span := p.currentGapSpan()
	if span < 1 {
		span = 1
	}
	p.mu.Lock()
	for cur := from; cur <= to; {
		end := min(cur+span-1, to)
		p.gaps = append(p.gaps, blockRange{from: cur, to: end})
		cur = end + 1
	}
	sort.Slice(p.gaps, func(i, j int) bool { return p.gaps[i].from < p.gaps[j].from })
	p.cond.Broadcast()
	p.mu.Unlock()
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

// currentGapSpan returns the adaptive per-request batch size: how many blocks the
// next request should pin its toBlock to span, clamped to [adaptiveGapMin,
// adaptiveGapMax]. This is the gap-fill unit fanned out across workers.
func (p *parallelPrefetcher) currentGapSpan() uint64 {
	s := p.gapSpan.Load()
	if s < adaptiveGapMin {
		return adaptiveGapMin
	}
	if s > adaptiveGapMax {
		return adaptiveGapMax
	}
	return s
}

// updateGapSpan folds a freshly observed delivered span into the shared estimate
// with an exponential moving average (1/4 weight on the new sample). The smoothing
// makes the pin react quickly when responses shrink (density rising) or grow (back
// to empty) without thrashing on a single outlier. A delivered span only counts as
// "the portal's natural page" when the request was NOT artificially capped by the
// page/range boundary; a boundary-capped response says nothing about density, so
// callers pass observed=0 to skip the update in that case.
func (p *parallelPrefetcher) updateGapSpan(observed uint64) {
	if observed == 0 {
		return
	}
	for {
		old := p.gapSpan.Load()
		// EWMA: new = old*3/4 + observed*1/4.
		next := old - old/4 + observed/4
		if next < adaptiveGapMin {
			next = adaptiveGapMin
		}
		if next > adaptiveGapMax {
			next = adaptiveGapMax
		}
		if p.gapSpan.CompareAndSwap(old, next) {
			return
		}
	}
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
	// At least two full pages per worker so coordination overhead is amortized
	minPages := 2
	if workers*2 > minPages {
		minPages = workers * 2
	}
	return uint64(minPages) * uint64(pageSize)
}
