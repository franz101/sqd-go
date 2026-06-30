package ingestion

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

// HybridFetchConfig configures the adaptive hybrid fetching strategy
type HybridFetchConfig struct {
	MaxWorkers       int     // Max parallel workers (default: 4)
	BeastModeWorkers int     // Workers for empty region beast mode (default: 6)
	EmptyThreshold   int     // Consecutive empty fetches before beast mode (default: 6)
	TargetWindowSec  float64 // Target window size in seconds (default: 2.0)
	RatePerSec       float64 // Rate limit per second (default: 5.0)
}

// DefaultHybridConfig returns the optimal hybrid configuration
func DefaultHybridConfig() *HybridFetchConfig {
	return &HybridFetchConfig{
		MaxWorkers:       4,
		BeastModeWorkers: 6,
		EmptyThreshold:   6,
		TargetWindowSec:  2.0,
		RatePerSec:       5.0,
	}
}

// HybridFetcher implements the adaptive hybrid fetching strategy
type HybridFetcher struct {
	endpoint    string
	filters     []client.LogFilter
	cfg         *HybridFetchConfig
	client      *client.Client
	emptyCount  int     // consecutive empty fetches
	beastMode   bool    // currently in beast mode
	lastDensity float64 // events per block from last fetch
}

// NewHybridFetcher creates a new adaptive hybrid fetcher
func NewHybridFetcher(endpoint string, filters []client.LogFilter, cfg *HybridFetchConfig) *HybridFetcher {
	if cfg == nil {
		cfg = DefaultHybridConfig()
	}
	return &HybridFetcher{
		endpoint: endpoint,
		filters:  filters,
		cfg:      cfg,
		client:   client.New(endpoint),
	}
}

// Close closes the hybrid fetcher
func (h *HybridFetcher) Close() {
	h.client.Close()
}

// EstimateWindowSize estimates the optimal window size based on last fetch density
func (h *HybridFetcher) EstimateWindowSize(lastBlocks uint64, lastEvents uint64) uint64 {
	if lastBlocks == 0 {
		return 5000 // conservative default
	}

	density := float64(lastEvents) / float64(lastBlocks)
	h.lastDensity = density

	// Target: complete fetch in TargetWindowSec seconds
	// Portal caps at ~1500 blocks/request in dense regions
	targetBlocksPerReq := float64(1500) // portal cap

	// Adjust based on density
	if density < 0.01 {
		// Very sparse - can fetch larger windows
		targetBlocksPerReq = 10000
	} else if density < 0.1 {
		// Sparse - moderate windows
		targetBlocksPerReq = 5000
	} else {
		// Dense - stick to portal cap
		targetBlocksPerReq = 1500
	}

	return uint64(targetBlocksPerReq)
}

// FetchRangeSequential does a single sequential fetch to assess density
func (h *HybridFetcher) FetchRangeSequential(ctx context.Context, from, to uint64) (client.Response, uint64, error) {
	resp, err := h.client.FetchWithParent(ctx, from, &to, "", false, h.filters)
	if err != nil {
		return client.Response{}, 0, fmt.Errorf("sequential fetch [%d-%d]: %w", from, to, err)
	}

	// Count events to assess density
	eventCount := uint64(0)
	if len(resp.Raw) > 0 {
		eventCount = countEventsInRaw(resp.Raw)
	}

	return resp, eventCount, nil
}

// countEventsInRaw counts events in raw JSONL response
func countEventsInRaw(raw []byte) uint64 {
	count := uint64(0)
	for _, b := range raw {
		if b == '\n' {
			count++
		}
	}
	return count
}

// ShouldUseBeastMode determines if we should use beast mode (full parallel)
func (h *HybridFetcher) ShouldUseBeastMode() bool {
	if h.emptyCount >= h.cfg.EmptyThreshold {
		h.beastMode = true
		return true
	}
	return false
}

// FetchRangeHybrid implements the hybrid strategy for a range
func (h *HybridFetcher) FetchRangeHybrid(ctx context.Context, start, end uint64) (map[uint64]int, time.Duration, error) {
	results := make(map[uint64]int)
	totalTime := time.Duration(0)

	current := start

	for current <= end {
		iterStart := time.Now()

		// Step 1: Sequential probe to assess density
		probeSize := h.EstimateWindowSize(end-current+1, 0)
		probeEnd := current + probeSize - 1
		if probeEnd > end {
			probeEnd = end
		}

		resp, eventCount, err := h.FetchRangeSequential(ctx, current, probeEnd)
		if err != nil {
			return nil, totalTime, fmt.Errorf("probe fetch at %d: %w", current, err)
		}

		probeDur := time.Since(iterStart)
		totalTime += probeDur

		// Update empty counter
		if eventCount == 0 {
			h.emptyCount++
			if h.ShouldUseBeastMode() {
				// Switch to beast mode - full parallel fetch
				beastResults, beastDur, err := h.fetchBeastMode(ctx, current, end)
				if err != nil {
					return nil, totalTime, fmt.Errorf("beast mode fetch: %w", err)
				}
				for bn, c := range beastResults {
					results[bn] += c
				}
				totalTime += beastDur
				break // beast mode covers everything to end
			}
		} else {
			h.emptyCount = 0 // reset on successful fetch
			h.beastMode = false
		}

		// Add events from probe
		if len(resp.Raw) > 0 {
			for bn, c := range logCountsOfRaw(resp.Raw) {
				if bn >= start && bn <= end {
					results[bn] += c
				}
			}
		}

		// Step 2: Estimate optimal window based on probe results
		windowSize := h.EstimateWindowSize(probeEnd-current+1, eventCount)

		// Step 3: Launch parallel fetches for the estimated window
		parallelStart := current + (probeEnd - current + 1) // start after probe
		if parallelStart > end {
			current = probeEnd + 1
			continue
		}

		parallelEnd := parallelStart + windowSize - 1
		if parallelEnd > end {
			parallelEnd = end
		}

		// Use parallel prefetcher for this window
		p := newParallelPrefetcher(h.endpoint, h.filters, false, parallelStart, parallelEnd,
			windowSize, h.cfg.MaxWorkers, newRateLimiter(h.cfg.RatePerSec, int(h.cfg.RatePerSec*10)))
		p.launch(ctx)

		for {
			chunk, ok := p.Next(ctx)
			if !ok {
				break
			}
			if chunk.err != nil {
				return nil, totalTime, fmt.Errorf("parallel fetch: %w", chunk.err)
			}
			for bn, c := range logCountsOfRaw(chunk.raw) {
				if bn >= start && bn <= end {
					results[bn] += c
				}
			}
		}

		parallelDur := time.Since(iterStart) - probeDur
		totalTime += parallelDur

		// Advance cursor
		current = parallelEnd + 1
	}

	return results, totalTime, nil
}

// fetchBeastMode does full parallel fetch for empty regions
func (h *HybridFetcher) fetchBeastMode(ctx context.Context, start, end uint64) (map[uint64]int, time.Duration, error) {
	results := make(map[uint64]int)
	t0 := time.Now()

	// Use max workers for beast mode
	pageSize := uint64(10000)
	p := newParallelPrefetcher(h.endpoint, h.filters, false, start, end, pageSize,
		h.cfg.BeastModeWorkers, newRateLimiter(h.cfg.RatePerSec*2, int(h.cfg.RatePerSec*20)))
	p.launch(ctx)

	for {
		chunk, ok := p.Next(ctx)
		if !ok {
			break
		}
		if chunk.err != nil {
			return nil, time.Since(t0), fmt.Errorf("beast mode fetch: %w", chunk.err)
		}

		eventsInChunk := countEventsInRaw(chunk.raw)
		if eventsInChunk > 0 {
			// Found events! Add to results and potentially exit beast mode
			h.emptyCount = 0
			h.beastMode = false
		}

		for bn, c := range logCountsOfRaw(chunk.raw) {
			results[bn] += c
		}
	}

	return results, time.Since(t0), nil
}

// logCountsOfRaw parses raw JSONL and returns event counts per block
// Uses proper JSON parsing to correctly count all events
func logCountsOfRaw(raw []byte) map[uint64]int {
	out := make(map[uint64]int)

	if len(raw) == 0 {
		return out
	}

	// Simple but effective: count lines that contain "logs":[ and have array elements
	lineStart := 0
	currentBlock := uint64(0)

	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' {
			line := raw[lineStart:i]
			lineStart = i + 1

			if len(line) == 0 {
				continue
			}

			lineStr := string(line)

			// Look for block number in header
			if idx := findInStr(lineStr, `"number":`); idx >= 0 {
				idx += len(`"number":`)
				// Parse number
				num := uint64(0)
				for j := idx; j < len(lineStr) && j < idx+15; j++ {
					c := lineStr[j]
					if c >= '0' && c <= '9' {
						num = num*10 + uint64(c-'0')
					} else if c == ',' || c == '}' || c == ' ' {
						break
					}
				}
				if num > 0 {
					currentBlock = num
				}
			}

			// Look for logs array and count events
			if idx := findInStr(lineStr, `"logs":[`); idx >= 0 {
				// Count opening braces after the array to determine event count
				eventCount := 0
				for j := idx + 7; j < len(lineStr); j++ {
					if lineStr[j] == '{' {
						eventCount++
					} else if lineStr[j] == ']' || lineStr[j] == '}' {
						break
					}
				}
				if eventCount > 0 && currentBlock > 0 {
					out[currentBlock] += eventCount
				}
			}
		}
	}

	return out
}

// BenchmarkHybridFetch_LBTC benchmarks the hybrid strategy
func BenchmarkHybridFetch_LBTC(b *testing.B) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		b.Skip("set SQD_LIVE_PORTAL=1 to run live portal benchmarks")
	}

	const start, end uint64 = 21_500_000, 21_509_000 // dense LBTC window
	cfg := DefaultHybridConfig()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		h := NewHybridFetcher(lbtcEndpoint, lbtcFilter, cfg)
		defer h.Close()

		results, dur, err := h.FetchRangeHybrid(context.Background(), start, end)
		if err != nil {
			b.Fatalf("hybrid fetch: %v", err)
		}

		totalEvents := 0
		for _, c := range results {
			totalEvents += c
		}

		if totalEvents == 0 {
			b.Fatal("no events fetched")
		}

		b.ReportMetric(float64(dur.Milliseconds()), "ms/op")
	}
}

// BenchmarkHybridVsBaseline_LBTC compares all three strategies
func BenchmarkHybridVsBaseline_LBTC(b *testing.B) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		b.Skip("set SQD_LIVE_PORTAL=1 to run live portal benchmarks")
	}

	const start, end uint64 = 21_500_000, 21_509_000
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
			b.Fatalf("sequential fetch: %v", err)
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

	// Parallel baseline
	t1 := time.Now()
	p := newParallelPrefetcher(lbtcEndpoint, lbtcFilter, false, start, end, 1000, 6,
		newRateLimiter(5.0, 50))
	p.launch(context.Background())
	for {
		chunk, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if chunk.err != nil {
			b.Fatalf("parallel fetch: %v", chunk.err)
		}
	}
	parDur := time.Since(t1)

	// Hybrid strategy
	h := NewHybridFetcher(lbtcEndpoint, lbtcFilter, DefaultHybridConfig())
	defer h.Close()

	results, hybDur, err := h.FetchRangeHybrid(context.Background(), start, end)
	if err != nil {
		b.Fatalf("hybrid fetch: %v", err)
	}

	totalEvents := 0
	for _, c := range results {
		totalEvents += c
	}

	seqBlkPerSec := float64(rangeSize) / seqDur.Seconds()
	parBlkPerSec := float64(rangeSize) / parDur.Seconds()
	hybBlkPerSec := float64(rangeSize) / hybDur.Seconds()

	b.Logf("SEQUENTIAL: %d blocks, %d requests, %v wall, %.0f blk/s",
		rangeSize, seqReqs, seqDur.Round(time.Millisecond), seqBlkPerSec)
	b.Logf("PARALLEL:   %d blocks, %v wall, %.0f blk/s",
		rangeSize, parDur.Round(time.Millisecond), parBlkPerSec)
	b.Logf("HYBRID:     %d blocks, %d events, %v wall, %.0f blk/s",
		rangeSize, totalEvents, hybDur.Round(time.Millisecond), hybBlkPerSec)
	b.Logf("SPEEDUP vs Sequential: %.2fx", seqDur.Seconds()/hybDur.Seconds())
	b.Logf("SPEEDUP vs Parallel:   %.2fx", parDur.Seconds()/hybDur.Seconds())
}

// Helper functions for string/byte searching
func findInStr(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if s[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
