package ingestion

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

// TestProbePinnedEmptyScan_LBTC answers the design question that decides the
// parallel-fetch policy: in an EMPTY region, does the SQD portal honor a wide
// pinned toBlock (scanning the whole pinned range in one response), or does it
// cap its own scan regardless of the pin?
//
//   - If a wide pin is honored → pinned pages can match the open-ended walk's
//     per-request reach, so the prefetcher never needs an open-ended (no-toBlock)
//     fallback and adaptiveGapMax should be raised to the honored ceiling.
//   - If the portal caps its scan below the pin → open-ended fetch reaches
//     further per request than any pin, so a genuine no-toBlock fallback is
//     warranted for deep-empty stretches.
//
// It issues one request per strategy from the same empty start block and reports
// the marker (last block in the response) each reached. Gated behind
// SQD_LIVE_PORTAL=1.
func TestProbePinnedEmptyScan_LBTC(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live pinned-vs-open empty-scan probe")
	}

	const start uint64 = 1_000_000 // deep in the empty pre-LBTC region

	cl := client.New(lbtcEndpoint)
	defer cl.Close()

	reach := func(label string, toBlock *uint64) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		t0 := time.Now()
		resp, err := cl.FetchWithParent(ctx, start, toBlock, "", false /*includeAllBlocks*/, lbtcFilter)
		dur := time.Since(t0)
		if err != nil {
			t.Fatalf("%s: fetch err: %v", label, err)
		}
		if len(resp.Raw) == 0 {
			t.Logf("%-14s -> empty body (no marker), %v", label, dur.Round(time.Millisecond))
			return
		}
		last, err := lastBlockNumber(resp.Raw)
		if err != nil {
			t.Fatalf("%s: marker parse: %v", label, err)
		}
		t.Logf("%-14s -> scanned to block %d (advance %d blocks) in %v",
			label, last, last-start, dur.Round(time.Millisecond))
	}

	pin := func(span uint64) *uint64 { v := start + span - 1; return &v }

	reach("open(no-pin)", nil)
	reach("pin=50k", pin(50_000))
	reach("pin=100k", pin(100_000))
	reach("pin=200k", pin(200_000))
	reach("pin=500k", pin(500_000))
	reach("pin=1M", pin(1_000_000))
}

// TestEmptyConcurrencyProbe_LBTC is the decisive experiment for whether the empty
// record can be beaten at all: does the portal let CONCURRENT requests exceed the
// single-stream request rate, or does it throttle aggregate throughput?
//
// Phase A: one open-ended walker for a fixed window -> requests/sec_1.
// Phase B: N open-ended walkers on DISJOINT sub-ranges (own client each) for the
//
//	same window -> aggregate requests/sec_N.
//
// If req/s_N >> req/s_1, the portal allows concurrency, so a parallel open-ended
// (no-toBlock) fetch can beat the sequential empty walk. If req/s_N ~= req/s_1,
// the portal is the ceiling and the empty record == the sequential walk.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -run TestEmptyConcurrencyProbe_LBTC -v
func TestEmptyConcurrencyProbe_LBTC(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live empty-region concurrency probe")
	}
	const window = 20 * time.Second
	const subSpan uint64 = 3_000_000 // each walker's private range, big enough not to overlap in 20s

	// walk does an open-ended (no-toBlock) sparse walk from `from`, bounded by ctx,
	// returning (requests, blocksCovered).
	walk := func(ctx context.Context, from uint64) (int, uint64) {
		cl := client.New(lbtcEndpoint)
		defer cl.Close()
		reqs := 0
		cur := from
		for {
			if ctx.Err() != nil {
				return reqs, cur - from
			}
			resp, err := cl.FetchWithParent(ctx, cur, nil, "", false, lbtcFilter)
			if err != nil {
				return reqs, cur - from
			}
			reqs++
			if len(resp.Raw) == 0 {
				return reqs, cur - from
			}
			last, err := lastBlockNumber(resp.Raw)
			if err != nil || last < cur {
				return reqs, cur - from
			}
			cur = last + 1
		}
	}

	// Phase A: single walker.
	ctxA, cancelA := context.WithTimeout(context.Background(), window)
	defer cancelA()
	tA := time.Now()
	reqA, blkA := walk(ctxA, 0)
	durA := time.Since(tA)
	rpsA := float64(reqA) / durA.Seconds()
	t.Logf("1 walker:  %d requests, %d blocks in %v -> %.2f req/s, %.0f blk/s", reqA, blkA, durA.Round(time.Millisecond), rpsA, float64(blkA)/durA.Seconds())

	// Phase B: N concurrent walkers on disjoint ranges.
	const n = 6
	ctxB, cancelB := context.WithTimeout(context.Background(), window)
	defer cancelB()
	var wg sync.WaitGroup
	var totReq, totBlk atomic.Uint64
	tB := time.Now()
	for i := 0; i < n; i++ {
		from := uint64(i) * subSpan
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, b := walk(ctxB, from)
			totReq.Add(uint64(r))
			totBlk.Add(b)
		}()
	}
	wg.Wait()
	durB := time.Since(tB)
	rpsB := float64(totReq.Load()) / durB.Seconds()
	t.Logf("%d walkers: %d requests, %d blocks in %v -> %.2f req/s, %.0f blk/s (aggregate)", n, totReq.Load(), totBlk.Load(), durB.Round(time.Millisecond), rpsB, float64(totBlk.Load())/durB.Seconds())
	t.Logf("CONCURRENCY GAIN: %.2fx req/s  (%.2f -> %.2f).  >1 means parallel open-ended can beat the empty record.", rpsB/rpsA, rpsA, rpsB)
}
