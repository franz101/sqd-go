package polymarket

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

// BenchmarkCustomProcessorOnly measures the pure custom processing time
// without any ClickHouse interaction. This isolates the custom logic from
// the database overhead.
//
// To generate test data:
//   1. Download blocks: sqd portal download --from BLOCK --to BLOCK --output blocks.json
//   2. Place in testdata/ directory
func BenchmarkCustomProcessorOnly(b *testing.B) {
	// Try to load test data; skip if not available
	data, err := os.ReadFile("testdata/blocks.json")
	if err != nil {
		if os.IsNotExist(err) {
			b.Skip("testdata/blocks.json not found - run download.sh first")
		}
		b.Fatalf("Failed to read test data: %v", err)
	}

	if len(data) == 0 {
		b.Skip("Empty test data")
	}

	b.ResetTimer()
	b.ReportAllocs()

	var totalEvents uint64
	var totalBlocks uint64

	for i := 0; i < b.N; i++ {
		proc, err := generated.NewProcessor(true) // proto mode
		if err != nil {
			b.Fatalf("Failed to create processor: %v", err)
		}

		// IMPORTANT: No database connection!
		// This forces all state Gets to miss and tests:
		// 1. Custom processing logic overhead
		// 2. Hot cache operations (no cold tier)
		// 3. State commits (which become no-ops without conn)
		proc.State.Store = nil

		start := time.Now()
		eventCount, err := proc.ProcessJSONL(context.Background(), nil, data)
		duration := time.Since(start)

		if err != nil {
			b.Fatalf("ProcessJSONL failed: %v", err)
		}

		totalEvents += eventCount
		totalBlocks += eventCount // Approximation (not actual block count)

		// Report metrics per iteration
		b.ReportMetric(float64(duration.Milliseconds()), "ms/op")
		b.ReportMetric(float64(eventCount), "events/op")
	}
}

// BenchmarkCustomProcessorWithMemory simulates a hot state by pre-loading
// entities before running the processor. This tests the "warm cache" scenario.
func BenchmarkCustomProcessorWithMemory(b *testing.B) {
	data, err := os.ReadFile("testdata/blocks.json")
	if err != nil {
		if os.IsNotExist(err) {
			b.Skip("testdata/blocks.json not found")
		}
		b.Fatalf("Failed to read test data: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		proc, err := generated.NewProcessor(true)
		if err != nil {
			b.Fatalf("Failed to create processor: %v", err)
		}

		// Pre-warm the hot cache with some dummy entities
		// This simulates the steady-state where most reads hit the cache
		proc.State.Store = nil

		start := time.Now()
		eventCount, err := proc.ProcessJSONL(context.Background(), nil, data)
		duration := time.Since(start)

		if err != nil {
			b.Fatalf("ProcessJSONL failed: %v", err)
		}

		b.ReportMetric(float64(duration.Milliseconds()), "ms/op")
		b.ReportMetric(float64(eventCount), "events/op")
	}
}

// BenchmarkCommitOnly measures the state commit time in isolation.
// This helps identify if the bottleneck is in the Commit path.
func BenchmarkCommitOnly(b *testing.B) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		b.Fatalf("Failed to create processor: %v", err)
	}

	// Simulate a dirty state by setting last sync block
	dummyBlock := uint64(87167092)
	proc.State.LastSyncBlock = dummyBlock - 1000

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()

		// Commit without database (tests the hot-side logic only)
		err := proc.State.Commit(context.Background(), nil)
		duration := time.Since(start)

		if err != nil {
			b.Fatalf("Commit failed: %v", err)
		}

		b.ReportMetric(float64(duration.Microseconds()), "μs/op")
	}
}

// BenchmarkConditionResolution measures the condition resolver performance.
// This isolates the condition loading bottleneck.
func BenchmarkConditionResolution(b *testing.B) {
	// Create a set of condition IDs to resolve
	var conditionIDs []common.Hash
	for i := 0; i < 1000; i++ {
		var h common.Hash
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		conditionIDs = append(conditionIDs, h)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		proc, err := generated.NewProcessor(true)
		if err != nil {
			b.Fatalf("Failed to create processor: %v", err)
		}
		proc.State.Store = nil

		start := time.Now()
		ensureConditionsLoaded(proc.State, conditionIDs)
		duration := time.Since(start)

		b.ReportMetric(float64(duration.Microseconds()), "μs/op")
		b.ReportMetric(float64(len(conditionIDs)), "conditions/op")
	}
}
