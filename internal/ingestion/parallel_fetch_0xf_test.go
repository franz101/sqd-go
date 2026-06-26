package ingestion

import (
	"context"
	"os"
	"sort"
	"testing"
)

// TestParallelVsSequential_0xfWindow is the high-concurrency (0xf=15 workers)
// sibling of the 0x1 completeness check: the parallel prefetcher must return the
// exact same per-block matching logs as a sequential walk even at max fan-out,
// where window stitching / ordering bugs are most likely to drop or duplicate a
// log-bearing block. Reuses the dense 0x1 wallet filter as ground truth.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -run TestParallelVsSequential_0xfWindow -v
func TestParallelVsSequential_0xfWindow(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live 0xf-window (15-worker) completeness check")
	}

	const endpoint = "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream"
	const start, end uint64 = 86_121_000, 86_133_000
	const pageSize uint64 = 1000
	const concurrency = 15 // 0xf

	seq := map[uint64]int{}
	cl := clientNew(endpoint)
	defer cl.Close()
	for cur := start; cur <= end; {
		to := end
		resp, err := cl.FetchWithParent(context.Background(), cur, &to, "", false, polymarket0x1Filter)
		if err != nil {
			t.Fatalf("sequential fetch from %d: %v", cur, err)
		}
		counts := logCountsOf(t, resp.Raw)
		if len(counts) == 0 {
			break
		}
		var last uint64
		for bn, c := range counts {
			seq[bn] += c
			if bn > last {
				last = bn
			}
		}
		if last < cur {
			break
		}
		cur = last + 1
	}

	par := map[uint64]int{}
	p := newParallelPrefetcher(endpoint, polymarket0x1Filter, false, start, end, pageSize, concurrency,
		newRateLimiter(defaultParallelRPS, defaultParallelBurst))
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
			par[bn] += c
		}
	}

	var dropped, extra, mismatched []uint64
	var seqTotal, parTotal int
	for bn, c := range seq {
		seqTotal += c
		if par[bn] != c {
			if par[bn] == 0 {
				dropped = append(dropped, bn)
			} else {
				mismatched = append(mismatched, bn)
			}
		}
	}
	for bn, c := range par {
		parTotal += c
		if seq[bn] == 0 && c > 0 {
			extra = append(extra, bn)
		}
	}
	sort.Slice(dropped, func(i, j int) bool { return dropped[i] < dropped[j] })
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	sort.Slice(mismatched, func(i, j int) bool { return mismatched[i] < mismatched[j] })

	t.Logf("seq: %d blocks/%d logs; par(15w): %d blocks/%d logs", len(seq), seqTotal, len(par), parTotal)
	if len(dropped) > 0 {
		t.Errorf("PARALLEL(15w) DROPPED logs in %d blocks (first 20): %v", len(dropped), firstNBlocks(dropped, 20))
	}
	if len(extra) > 0 {
		t.Errorf("PARALLEL(15w) extra logs in %d blocks (first 20): %v", len(extra), firstNBlocks(extra, 20))
	}
	if len(mismatched) > 0 {
		t.Errorf("PARALLEL(15w) count mismatch in %d blocks (first 20): %v", len(mismatched), firstNBlocks(mismatched, 20))
	}
	if len(dropped)+len(extra)+len(mismatched) == 0 {
		t.Logf("OK: 15-worker parallel matches sequential log-for-log over the 0xf window")
	}
}
