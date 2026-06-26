package ingestion

import (
	"context"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

// lbtcEndpoint and lbtcFilter mirror examples/uniswap/config.yaml: chain 1
// (ethereum-mainnet), contract LBTC at 0x8236a87084f8B84306f72007F36F2618A5634494,
// event Transfer(address indexed from, address indexed to, uint256 value). The
// Transfer signature hashes to topic0 0xddf252ad…523b3ef. This filter is extremely
// sparse for the first ~20M blocks (LBTC launched in 2024) then clusters near tip.
const lbtcEndpoint = "https://portal.sqd.dev/datasets/ethereum-mainnet/finalized-stream"

var lbtcFilter = []client.LogFilter{{
	Address: []string{"0x8236a87084f8B84306f72007F36F2618A5634494"},
	Topic0:  []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
}}

// TestSequentialVsParallelBaseline_LBTC establishes the SEQUENTIAL-DYNAMIC
// (no-toBlock) throughput baseline and compares it to the parallel prefetcher over
// the SAME dense LBTC window. The "dynamic no-toBlock" walk is what the user wants
// to baseline: repeatedly fetch from cur with toBlock=nil and includeAllBlocks=false,
// letting the portal pick the response high-water mark, then advance cur to (last
// block)+1. The portal caps each response at its scanned range (~1500 blocks here),
// so over a dense window this is round-trip-bound — exactly the bottleneck the
// parallel path removes.
//
// The dense window [21_500_000, 21_509_000] was chosen by probing the portal: nearly
// every block carries an LBTC Transfer (~430 event-bearing blocks per ~1500-block
// scan cap), so a data-driven walk is provably complete and the parallel path has
// real ground truth to reproduce. The portal caps each response at its ~1500-block
// scanned range, so this ~9k-block window is ~6 sequential round trips — enough to
// expose the round-trip-bound baseline the parallel path removes, while still running
// in well under a minute at 5 req/s.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -run TestSequentialVsParallelBaseline_LBTC -v
func TestSequentialVsParallelBaseline_LBTC(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live LBTC sequential-vs-parallel baseline")
	}

	const start, end uint64 = 21_500_000, 21_509_000 // dense LBTC window, ~9k blocks (~6 round trips)
	const pageSize uint64 = 1000

	// ── Sequential dynamic walk (toBlock=nil, includeAllBlocks=false) ──
	// This is the baseline: no upper bound is pinned, so the portal returns matching
	// blocks plus a marker block at its scanned high-water mark. We advance cur to
	// (last block in response)+1 and stop once coverage reaches end. Counting both
	// blk/s (range coverage rate) and req count (the round-trip cost) characterizes
	// the round-trip-bound baseline.
	seqEvents := map[uint64]int{}
	seqReqs := 0
	cl := client.New(lbtcEndpoint)
	defer cl.Close()
	t0 := time.Now()
	cur := start
	for cur <= end {
		resp, err := cl.FetchWithParent(context.Background(), cur, nil /*toBlock*/, "", false /*includeAllBlocks*/, lbtcFilter)
		if err != nil {
			t.Fatalf("sequential fetch from %d: %v", cur, err)
		}
		seqReqs++
		if len(resp.Raw) == 0 {
			t.Fatalf("sequential fetch from %d returned no blocks (no high-water marker)", cur)
		}
		last, err := lastBlockNumber(resp.Raw)
		if err != nil {
			t.Fatalf("sequential parse last block from %d: %v", cur, err)
		}
		for bn, c := range logCountsOf(t, resp.Raw) {
			if bn >= start && bn <= end && c > 0 {
				seqEvents[bn] += c
			}
		}
		if last <= cur {
			// Portal did not advance past cur; nothing more to read.
			break
		}
		cur = last + 1
	}
	seqDur := time.Since(t0)
	seqCovered := end - start + 1
	seqBlkPerSec := float64(seqCovered) / seqDur.Seconds()
	seqEventTotal := 0
	for _, c := range seqEvents {
		seqEventTotal += c
	}

	t.Logf("SEQUENTIAL dynamic walk: %d blocks covered, %d event-bearing blocks, %d events, %d requests, %v wall, %.0f blk/s",
		seqCovered, len(seqEvents), seqEventTotal, seqReqs, seqDur.Round(time.Millisecond), seqBlkPerSec)

	// ── Parallel prefetcher (default 6 workers, real rate limiter) ──
	parEvents := map[uint64]int{}
	p := newParallelPrefetcher(lbtcEndpoint, lbtcFilter, false /*includeAllBlocks*/, start, end, pageSize, defaultParallelFetchers,
		newRateLimiter(defaultParallelRPS, defaultParallelBurst))
	t1 := time.Now()
	p.launch(context.Background())
	for {
		chunk, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if chunk.err != nil {
			t.Fatalf("parallel fetch error: %v", chunk.err)
		}
		for bn, c := range logCountsOf(t, chunk.raw) {
			if bn >= start && bn <= end && c > 0 {
				parEvents[bn] += c
			}
		}
	}
	parDur := time.Since(t1)
	parBlkPerSec := float64(seqCovered) / parDur.Seconds()
	parEventTotal := 0
	for _, c := range parEvents {
		parEventTotal += c
	}

	t.Logf("PARALLEL (6 workers):    %d blocks covered, %d event-bearing blocks, %d events, %v wall, %.0f blk/s",
		seqCovered, len(parEvents), parEventTotal, parDur.Round(time.Millisecond), parBlkPerSec)
	t.Logf("speedup (parallel vs sequential): %.2fx", seqDur.Seconds()/parDur.Seconds())

	// ── Completeness: parallel must reproduce the sequential ground truth exactly,
	// per block, with no dropped, extra, or miscounted event-bearing blocks. ──
	if len(seqEvents) == 0 {
		t.Fatalf("sequential walk found no LBTC events in [%d,%d]; dense window assumption broken", start, end)
	}
	var dropped, extra, mismatched []uint64
	for bn, c := range seqEvents {
		if parEvents[bn] != c {
			if parEvents[bn] == 0 {
				dropped = append(dropped, bn)
			} else {
				mismatched = append(mismatched, bn)
			}
		}
	}
	for bn, c := range parEvents {
		if seqEvents[bn] == 0 && c > 0 {
			extra = append(extra, bn)
		}
	}
	sort.Slice(dropped, func(i, j int) bool { return dropped[i] < dropped[j] })
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	sort.Slice(mismatched, func(i, j int) bool { return mismatched[i] < mismatched[j] })

	if len(dropped) > 0 {
		t.Errorf("PARALLEL DROPPED event blocks in %d blocks (first 20): %v", len(dropped), firstNBlocks(dropped, 20))
	}
	if len(extra) > 0 {
		t.Errorf("PARALLEL has event blocks in %d blocks sequential did not (first 20): %v", len(extra), firstNBlocks(extra, 20))
	}
	if len(mismatched) > 0 {
		t.Errorf("PARALLEL per-block event count mismatch in %d blocks (first 20): %v", len(mismatched), firstNBlocks(mismatched, 20))
	}
	if len(dropped) == 0 && len(extra) == 0 && len(mismatched) == 0 {
		t.Logf("OK: parallel fetch matches sequential event-for-event over the LBTC dense window")
	}
}

// TestEmptyRegionSequentialVsParallel_LBTC is a fixed-time throughput race over
// the empty region starting at block 0 (zero LBTC logs): each method gets a
// 60-second budget and we measure how many blocks it covers, proving the
// parallel path covers its prefix with NO MISSING BLOCKS.
//
// Early shutdown: each phase cancels at 60s and reports partial progress rather
// than failing — the range to the finalized tip is far larger than 60s of fetch,
// so this is a "blocks covered in 60s" record, not a run-to-completion test.
//
// Completeness in an empty region cannot be checked against event ground truth
// (there are none), so instead we verify that the prefetcher's chunks tile the
// covered prefix CONTIGUOUSLY: each chunk's `from` equals the previous chunk's
// coveredTo+1 (no gap, no overlap). That is the strongest possible "no block was
// silently skipped" guarantee — the portal scanned every sub-range and the
// prefetcher accounted for all of it, right up to wherever the 60s cutoff fell.
//
// IMPORTANT TUNING: the prefetcher pins toBlock = min(cur+gapSpan-1, pageEnd),
// so the empty-region jump is capped by pageSize. To let the portal's ~106k
// natural empty scan through, the page must be large; we use a wide page here
// and report it. (This is itself a finding: the default 10k page would cap empty
// jumps at 10k and waste ~10x the requests through empty regions.)
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -run TestEmptyRegionSequentialVsParallel_LBTC -v
func TestEmptyRegionSequentialVsParallel_LBTC(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live LBTC empty-region throughput record")
	}

	const start uint64 = 0        // empty for the LBTC filter from genesis
	const end uint64 = 18_000_000 // safely below LBTC's ~20.5M launch, so the whole range has 0 logs
	// pageSize ~= one portal empty-jump so each page is ~1 request and N workers
	// fetch CONSECUTIVE pages concurrently. The contiguous empty stream has no gaps
	// to distribute, so a large page serializes on the in-order head (one worker
	// per page); a small page spreads the head across workers, letting them
	// saturate the 5 req/s budget — the lever that beats the latency-bound single
	// walker. Overridable via SQD_BENCH_PAGE_SIZE for sweeps.
	pageSize := uint64(50_000)
	if v, err := strconv.ParseUint(os.Getenv("SQD_BENCH_PAGE_SIZE"), 10, 64); err == nil && v > 0 {
		pageSize = v
	}
	const budget = 60 * time.Second // early shutdown per phase: whichever of [start,end]/60s comes first

	// ── Sequential dynamic walk (toBlock=nil): the baseline to beat ──
	cl := client.New(lbtcEndpoint)
	defer cl.Close()
	seqCtx, seqCancel := context.WithTimeout(context.Background(), budget)
	defer seqCancel()
	seqReqs := 0
	seqEvents := 0
	t0 := time.Now()
	cur := start
	for cur <= end {
		resp, err := cl.FetchWithParent(seqCtx, cur, nil /*toBlock*/, "", false /*includeAllBlocks*/, lbtcFilter)
		if err != nil {
			if seqCtx.Err() != nil {
				break // 60s early shutdown — report partial progress
			}
			t.Fatalf("sequential fetch from %d: %v", cur, err)
		}
		seqReqs++
		if len(resp.Raw) == 0 {
			t.Fatalf("sequential fetch from %d returned no blocks (no high-water marker)", cur)
		}
		last, err := lastBlockNumber(resp.Raw)
		if err != nil {
			t.Fatalf("sequential parse last block from %d: %v", cur, err)
		}
		for _, c := range logCountsOf(t, resp.Raw) {
			seqEvents += c
		}
		if last < cur {
			t.Fatalf("portal did not advance: cur=%d last=%d", cur, last)
		}
		cur = last + 1
	}
	seqDur := time.Since(t0)
	seqCovered := cur - start
	seqBlkPerSec := float64(seqCovered) / seqDur.Seconds()
	t.Logf("SEQUENTIAL empty walk: covered %d blocks (to %d) in %v, %d requests, %.0f blk/s (%d events — expected 0)",
		seqCovered, cur-1, seqDur.Round(time.Millisecond), seqReqs, seqBlkPerSec, seqEvents)

	// ── Parallel prefetcher (default workers, real 5 req/s limiter) ──
	// Drain in order, asserting contiguous coverage as we go: this is the
	// no-missing-blocks proof. The context cancels the whole prefetcher at 60s.
	parCtx, parCancel := context.WithTimeout(context.Background(), budget)
	defer parCancel()
	p := newParallelPrefetcher(lbtcEndpoint, lbtcFilter, false /*includeAllBlocks*/, start, end, pageSize, defaultParallelFetchers,
		newRateLimiter(defaultParallelRPS, defaultParallelBurst))
	parReqs := 0
	parEvents := 0
	expectedFrom := start // next block the in-order stream must start at
	t1 := time.Now()
	p.launch(parCtx)
	for {
		chunk, ok := p.Next(parCtx)
		if !ok {
			break // either fully drained or 60s early shutdown
		}
		if chunk.err != nil {
			if parCtx.Err() != nil {
				break
			}
			t.Fatalf("parallel fetch error at [%d-%d]: %v", chunk.from, chunk.coveredTo, chunk.err)
		}
		parReqs++
		if chunk.from != expectedFrom {
			t.Fatalf("COVERAGE GAP/OVERLAP: chunk from=%d, expected %d (a block range would be missed)", chunk.from, expectedFrom)
		}
		if chunk.coveredTo < chunk.from {
			t.Fatalf("chunk [%d-%d] covers nothing", chunk.from, chunk.coveredTo)
		}
		for _, c := range logCountsOf(t, chunk.raw) {
			parEvents += c
		}
		expectedFrom = chunk.coveredTo + 1
	}
	parDur := time.Since(t1)
	parCovered := expectedFrom - start
	parBlkPerSec := float64(parCovered) / parDur.Seconds()

	if parEvents != 0 {
		t.Fatalf("expected 0 LBTC events in empty region [%d,%d], got %d", start, expectedFrom-1, parEvents)
	}

	t.Logf("PARALLEL empty walk:   covered %d blocks (to %d) in %v, %d chunks, %.0f blk/s (pageSize=%d, %d workers) — contiguous from %d, no missing blocks",
		parCovered, expectedFrom-1, parDur.Round(time.Millisecond), parReqs, parBlkPerSec, pageSize, defaultParallelFetchers, start)
	t.Logf("RECORD (blocks covered in ~60s, empty region): parallel %d vs sequential %d  =  %.2fx  [%.0f -> %.0f blk/s]",
		parCovered, seqCovered, float64(parCovered)/float64(seqCovered), seqBlkPerSec, parBlkPerSec)
}

// TestSequentialDynamicEmptyRegionWalk_LBTC characterizes how fast the dynamic
// no-toBlock sequential method jumps through EMPTY regions. With the very sparse
// LBTC filter, the early chain (~block 1,000,000) has no matching logs at all, so
// each fetch (toBlock=nil, includeAllBlocks=false) returns only the portal's marker
// block at its scanned high-water mark. The per-request advance (marker minus cur)
// measures how many blocks the portal skips per round trip — the "no-toBlock returns
// ~20k blocks when empty" behavior. This documents why the early backfill of a
// sparse contract is dominated by these large empty jumps rather than by data.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -run TestSequentialDynamicEmptyRegionWalk_LBTC -v
func TestSequentialDynamicEmptyRegionWalk_LBTC(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live LBTC empty-region walk")
	}

	const start uint64 = 1_000_000
	const maxRequests = 20 // bound wall time; 20 jumps already characterize the stride

	cl := client.New(lbtcEndpoint)
	defer cl.Close()

	var advances []uint64
	var eventsSeen int
	t0 := time.Now()
	cur := start
	for i := 0; i < maxRequests; i++ {
		resp, err := cl.FetchWithParent(context.Background(), cur, nil /*toBlock*/, "", false /*includeAllBlocks*/, lbtcFilter)
		if err != nil {
			t.Fatalf("empty-region fetch from %d: %v", cur, err)
		}
		if len(resp.Raw) == 0 {
			t.Fatalf("empty-region fetch from %d returned no blocks (no high-water marker)", cur)
		}
		last, err := lastBlockNumber(resp.Raw)
		if err != nil {
			t.Fatalf("empty-region parse last block from %d: %v", cur, err)
		}
		for _, c := range logCountsOf(t, resp.Raw) {
			eventsSeen += c
		}
		if last < cur {
			t.Fatalf("portal did not advance: cur=%d last=%d", cur, last)
		}
		advances = append(advances, last-cur+1)
		cur = last + 1
	}
	dur := time.Since(t0)

	// Summary stats over per-request advances.
	var total, minAdv, maxAdv uint64
	minAdv = ^uint64(0)
	for _, a := range advances {
		total += a
		if a < minAdv {
			minAdv = a
		}
		if a > maxAdv {
			maxAdv = a
		}
	}
	mean := float64(total) / float64(len(advances))

	// Histogram of advance sizes (log-ish buckets in blocks).
	type bucket struct {
		label string
		lo    uint64
		hi    uint64
	}
	buckets := []bucket{
		{"<1k", 0, 999},
		{"1k-5k", 1_000, 4_999},
		{"5k-10k", 5_000, 9_999},
		{"10k-20k", 10_000, 19_999},
		{"20k-50k", 20_000, 49_999},
		{">=50k", 50_000, ^uint64(0)},
	}
	counts := make([]int, len(buckets))
	for _, a := range advances {
		for bi, b := range buckets {
			if a >= b.lo && a <= b.hi {
				counts[bi]++
				break
			}
		}
	}

	t.Logf("EMPTY-REGION walk from block %d: %d requests, advanced %d blocks total (to %d) in %v, %d matching events seen",
		start, len(advances), total, cur-1, dur.Round(time.Millisecond), eventsSeen)
	t.Logf("per-request advance: min=%d mean=%.0f max=%d blocks", minAdv, mean, maxAdv)
	for bi, b := range buckets {
		if counts[bi] > 0 {
			t.Logf("  advance %-7s : %d requests", b.label, counts[bi])
		}
	}
	t.Logf("raw advances (blocks/request): %v", advances)
}
