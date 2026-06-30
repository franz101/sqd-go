package ingestion

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

// BenchmarkSequentialFetch_LBTC benchmarks sequential fetching over a dense LBTC window
// to establish the round-trip-bound baseline. This shows the maximum throughput possible
// when each request must complete before the next one starts.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -bench BenchmarkSequentialFetch_LBTC -benchtime=10s
func BenchmarkSequentialFetch_LBTC(b *testing.B) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		b.Skip("set SQD_LIVE_PORTAL=1 to run live portal benchmarks")
	}

	const start, end uint64 = 21_500_000, 21_509_000 // dense LBTC window (~9k blocks)
	cl := client.New(lbtcEndpoint)
	defer cl.Close()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cur := start
		totalEvents := 0
		for cur <= end {
			resp, err := cl.FetchWithParent(context.Background(), cur, nil, "", false, lbtcFilter)
			if err != nil {
				b.Fatalf("fetch from %d: %v", cur, err)
			}
			if len(resp.Raw) == 0 {
				break
			}
			// Count events (lightweight)
			for _, line := range resp.Raw {
				if line == '\n' {
					totalEvents++
				}
			}
			last, err := lastBlockNumber(resp.Raw)
			if err != nil {
				b.Fatalf("parse last block: %v", err)
			}
			if last <= cur {
				break
			}
			cur = last + 1
		}
		// Prevent compiler optimization
		if totalEvents == 0 {
			b.Fatal("no events fetched")
		}
	}
}

// BenchmarkParallelFetch_LBTC benchmarks parallel fetching over the same dense LBTC window
// to show the throughput improvement from concurrent workers.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -bench BenchmarkParallelFetch_LBTC -benchtime=10s
func BenchmarkParallelFetch_LBTC(b *testing.B) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		b.Skip("set SQD_LIVE_PORTAL=1 to run live portal benchmarks")
	}

	const start, end uint64 = 21_500_000, 21_509_000 // dense LBTC window (~9k blocks)
	const pageSize uint64 = 1000

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		p := newParallelPrefetcher(lbtcEndpoint, lbtcFilter, false, start, end, pageSize, defaultParallelFetchers,
			newRateLimiter(defaultParallelRPS, defaultParallelBurst))
		p.launch(context.Background())
		totalEvents := 0
		for {
			chunk, ok := p.Next(context.Background())
			if !ok {
				break
			}
			if chunk.err != nil {
				b.Fatalf("parallel fetch error: %v", chunk.err)
			}
			// Count events (lightweight)
			for _, line := range chunk.raw {
				if line == '\n' {
					totalEvents++
				}
			}
		}
		// Prevent compiler optimization
		if totalEvents == 0 {
			b.Fatal("no events fetched")
		}
	}
}

// BenchmarkFetchComparison_LBTC runs a single comparison between sequential and parallel
// fetching to show the actual throughput and speedup. This is more useful than the
// microbenchmark for understanding real-world performance.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -run BenchmarkFetchComparison_LBTC -v
func BenchmarkFetchComparison_LBTC(b *testing.B) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		b.Skip("set SQD_LIVE_PORTAL=1 to run live portal benchmarks")
	}

	const start, end uint64 = 21_500_000, 21_509_000 // dense LBTC window (~9k blocks)
	const pageSize uint64 = 1000
	const rangeSize = end - start + 1

	// Sequential baseline
	cl := client.New(lbtcEndpoint)
	defer cl.Close()

	seqReqs := 0
	t0 := time.Now()
	cur := start
	for cur <= end {
		resp, err := cl.FetchWithParent(context.Background(), cur, nil, "", false, lbtcFilter)
		if err != nil {
			b.Fatalf("sequential fetch from %d: %v", cur, err)
		}
		seqReqs++
		if len(resp.Raw) == 0 {
			break
		}
		last, err := lastBlockNumber(resp.Raw)
		if err != nil {
			b.Fatalf("parse last block: %v", err)
		}
		if last <= cur {
			break
		}
		cur = last + 1
	}
	seqDur := time.Since(t0)
	seqBlkPerSec := float64(rangeSize) / seqDur.Seconds()

	// Parallel fetch
	t1 := time.Now()
	p := newParallelPrefetcher(lbtcEndpoint, lbtcFilter, false, start, end, pageSize, defaultParallelFetchers,
		newRateLimiter(defaultParallelRPS, defaultParallelBurst))
	p.launch(context.Background())
	chunks := 0
	for {
		chunk, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if chunk.err != nil {
			b.Fatalf("parallel fetch error: %v", chunk.err)
		}
		chunks++
	}
	parDur := time.Since(t1)
	parBlkPerSec := float64(rangeSize) / parDur.Seconds()

	b.Logf("SEQUENTIAL: %d blocks, %d requests, %v wall, %.0f blk/s, %.2f ms/req",
		rangeSize, seqReqs, seqDur.Round(time.Millisecond), seqBlkPerSec,
		float64(seqDur.Milliseconds())/float64(seqReqs))

	b.Logf("PARALLEL:   %d blocks, %d chunks, %v wall, %.0f blk/s",
		rangeSize, chunks, parDur.Round(time.Millisecond), parBlkPerSec)

	b.Logf("SPEEDUP:    %.2fx (parallel vs sequential)", seqDur.Seconds()/parDur.Seconds())
	b.Logf("EFFICIENCY: %.2fx throughput improvement", parBlkPerSec/seqBlkPerSec)
}
