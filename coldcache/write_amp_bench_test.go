// Benchmark Pebble write amplification for different batch sizes.
//
// Write amplification measures how many bytes are actually written to disk
// versus the logical bytes we intend to write. In LSM trees like Pebble,
// write amplification comes from:
// 1. MemTable flushes (L0 -> L1)
// 2. Compaction cascades (L0 -> L1 -> L2 -> ...)
// 3. WAL writes (if enabled)
//
// This benchmark measures:
// - Throughput impact of different batch sizes
// - Memory allocations for different batch sizes
// - Flush count reduction from batching
package coldcache

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
)

// writeAmpStats tracks write operation metrics
type writeAmpStats struct {
	logicalBytes int64 // bytes intended to write
	writeCount    int64 // number of write operations
	flushCount    int64 // number of batch flushes
}

// writeAmpBenchmark runs the write benchmark for a given batch size
func writeAmpBenchmark(b *testing.B, flushCount int, totalWrites int) *writeAmpStats {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("write_amp_bench_%d_%d", flushCount, time.Now().UnixNano()))
	defer os.RemoveAll(dir)

	// Open Pebble store with minimal settings (no WAL for this benchmark)
	opts := &pebble.Options{
		DisableWAL:                 true,
		MaxManifestFileSize:        1 << 20, // 1MB
		MemTableSize:               4 << 20, // 4MB
		MemTableStopWritesThreshold: 2,
		LBaseMaxBytes:              8 << 20, // 8MB per level
		Levels: []pebble.LevelOptions{
			{BlockSize: 4 << 10}, // 4KB blocks
			{BlockSize: 4 << 10},
			{BlockSize: 4 << 10},
			{BlockSize: 4 << 10},
			{BlockSize: 4 << 10},
			{BlockSize: 4 << 10},
			{BlockSize: 4 << 10},
		},
	}

	db, err := pebble.Open(dir, opts)
	if err != nil {
		b.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	stats := &writeAmpStats{}
	r := rand.New(rand.NewSource(42))

	// Write loop with batching
	batch := db.NewBatch()
	currentBatch := 0

	for i := 0; i < totalWrites; i++ {
		key := randKey(r)
		value := randValue(r)

		stats.logicalBytes += int64(len(key) + len(value))
		stats.writeCount++

		if err := batch.Set(key, value, nil); err != nil {
			b.Fatalf("set: %v", err)
		}

		currentBatch++
		if currentBatch >= flushCount {
			if err := batch.Commit(pebble.NoSync); err != nil {
				b.Fatalf("commit: %v", err)
			}
			stats.flushCount++
			batch.Close()
			batch = db.NewBatch()
			currentBatch = 0
		}
	}

	// Final flush
	if currentBatch > 0 {
		if err := batch.Commit(pebble.NoSync); err != nil {
			b.Fatalf("final commit: %v", err)
		}
		stats.flushCount++
	}
	batch.Close()

	return stats
}

// BenchmarkWriteBatchThroughput measures throughput of different batch sizes
func BenchmarkWriteBatchThroughput(b *testing.B) {
	sizes := []int{1, 10, 100, 1000, 10000, 16384}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("batch_%d", size), func(b *testing.B) {
			dir := filepath.Join(os.TempDir(), fmt.Sprintf("throughput_%d_%d", size, time.Now().UnixNano()))
			defer os.RemoveAll(dir)

			opts := &pebble.Options{
				DisableWAL:                  true,
				MemTableSize:                4 << 20,
				MemTableStopWritesThreshold: 2,
			}

			db, err := pebble.Open(dir, opts)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer db.Close()

			r := rand.New(rand.NewSource(42))
			keys := make([][]byte, size)
			values := make([][]byte, size)

			// Pre-generate keys and values
			for i := 0; i < size; i++ {
				keys[i] = randKey(r)
				values[i] = randValue(r)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				batch := db.NewBatch()
				for j := 0; j < size; j++ {
					if err := batch.Set(keys[j], values[j], nil); err != nil {
						b.Fatalf("set: %v", err)
					}
				}
				if err := batch.Commit(pebble.NoSync); err != nil {
					b.Fatalf("commit: %v", err)
				}
				batch.Close()
			}
		})
	}
}

// BenchmarkWriteBatchScale measures performance at scale with different batch sizes
func BenchmarkWriteBatchScale(b *testing.B) {
	scales := []struct {
		name       string
		batchSize  int
		numWrites  int
	}{
		{"direct_1w", 1, 10000},
		{"batch10_10w", 10, 100000},
		{"batch100_100w", 100, 100000},
		{"batch1k_100w", 1000, 100000},
		{"batch16k_100w", 16384, 100000},
	}

	for _, scale := range scales {
		b.Run(scale.name, func(b *testing.B) {
			b.StopTimer()
			stats := writeAmpBenchmark(b, scale.batchSize, scale.numWrites)
			b.StartTimer()

			// Report metrics
			flushReduction := float64(scale.numWrites) / float64(stats.flushCount)
			b.ReportMetric(flushReduction, "flush_reduction")
			b.ReportMetric(float64(stats.flushCount), "flushes")
			b.ReportMetric(float64(stats.writeCount), "writes")
		})
	}
}

// BenchmarkWriteBatchMemory measures memory allocations for different batch sizes
func BenchmarkWriteBatchMemory(b *testing.B) {
	sizes := []int{1, 10, 100, 1000, 10000, 16384}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("batch_%d", size), func(b *testing.B) {
			r := rand.New(rand.NewSource(42))

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				batch := make([][]byte, size)
				values := make([][]byte, size)

				for j := 0; j < size; j++ {
					batch[j] = randKey(r)
					values[j] = randValue(r)
				}

				// Simulate batch operations (allocations only)
				_ = batch
				_ = values
			}
		})
	}
}

