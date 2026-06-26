package coldcache

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

const (
	optKeySize   = 52  // User + TokenID
	optValueSize = 152 // Position struct
)

// Benchmark: Test different optimization profiles for point lookups
func BenchmarkOptimizedPointLookup(b *testing.B) {
	configs := []OptimConfig{
		ConfigBaseline,
		ConfigNoCompression,
		ConfigFastReads,
		ConfigLargeMem,
	}

	for _, config := range configs {
		b.Run(string(config), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_opt"
			store, err := OpenOptimized(dir, 256<<20, 0, config)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			// Warm up with 100K keys
			r := rand.New(rand.NewSource(42))
			keys := make([][]byte, 100000)
			for i := 0; i < len(keys); i++ {
				keys[i] = randKey(r)
				if err := store.Put(keys[i], randValue(r)); err != nil {
					b.Fatalf("warm put: %v", err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				idx := r.Intn(len(keys))
				_, found, err := store.Get(keys[idx])
				if err != nil || !found {
					b.Fatalf("get failed")
				}
			}
		})
	}
}

// Benchmark: Test optimization profiles for random reads
func BenchmarkOptimizedRandomRead(b *testing.B) {
	configs := []OptimConfig{
		ConfigBaseline,
		ConfigNoCompression,
		ConfigFastReads,
	}

	for _, config := range configs {
		b.Run(string(config), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_opt"
			store, err := OpenOptimized(dir, 256<<20, 0, config)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			r := rand.New(rand.NewSource(42))
			keys := make([][]byte, 100000)
			for i := 0; i < len(keys); i++ {
				keys[i] = randKey(r)
				if err := store.Put(keys[i], randValue(r)); err != nil {
					b.Fatalf("warm put: %v", err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				idx := r.Intn(len(keys))
				_, _, _ = store.Get(keys[idx])
			}
		})
	}
}

// Benchmark: Test optimization profiles for writes
func BenchmarkOptimizedWrite(b *testing.B) {
	configs := []OptimConfig{
		ConfigBaseline,
		ConfigNoCompression,
		ConfigAggressive,
	}

	for _, config := range configs {
		b.Run(string(config), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_opt"
			store, err := OpenOptimized(dir, 256<<20, 0, config)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			r := rand.New(rand.NewSource(42))

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				key := randKey(r)
				val := randValue(r)
				if err := store.Put(key, val); err != nil {
					b.Fatalf("put: %v", err)
				}
			}
		})
	}
}

// Benchmark: Test optimization profiles for batch writes
func BenchmarkOptimizedBatchWrite(b *testing.B) {
	configs := []OptimConfig{
		ConfigBaseline,
		ConfigNoCompression,
		ConfigAggressive,
		ConfigLargeMem,
	}

	const batchSize = 16384

	for _, config := range configs {
		b.Run(string(config), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_opt"
			store, err := OpenOptimized(dir, 256<<20, 0, config)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				batch := store.NewWriteBatch()
				r := rand.New(rand.NewSource(42))
				for j := 0; j < batchSize; j++ {
					key := randKey(r)
					val := randValue(r)
					if err := batch.Put(key, val); err != nil {
						b.Fatalf("batch put: %v", err)
					}
				}
				if err := batch.Close(); err != nil {
					b.Fatalf("batch close: %v", err)
				}
			}
		})
	}
}

// Benchmark: Test optimization profiles for mixed workload
func BenchmarkOptimizedMixedWorkload(b *testing.B) {
	configs := []OptimConfig{
		ConfigBaseline,
		ConfigNoCompression,
		ConfigFastReads,
		ConfigLargeMem,
	}

	for _, config := range configs {
		b.Run(string(config), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_opt"
			store, err := OpenOptimized(dir, 256<<20, 0, config)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			r := rand.New(rand.NewSource(42))
			keys := make([][]byte, 100000)
			for i := 0; i < len(keys); i++ {
				keys[i] = randKey(r)
				if err := store.Put(keys[i], randValue(r)); err != nil {
					b.Fatalf("warm put: %v", err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if r.Intn(10) == 0 {
					// 10% writes
					newKey := randKey(r)
					if err := store.Put(newKey, randValue(r)); err != nil {
						b.Fatalf("put: %v", err)
					}
				} else {
					// 90% reads
					idx := r.Intn(len(keys))
					_, _, _ = store.Get(keys[idx])
				}
			}
		})
	}
}

// Benchmark: Test different batch sizes
func BenchmarkOptimizedBatchSize(b *testing.B) {
	batchSizes := []int{4096, 8192, 16384, 32768}

	for _, size := range batchSizes {
		b.Run(fmt.Sprintf("batch_%d", size), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_batch"
			store, err := OpenOptimized(dir, 256<<20, 0, ConfigBaseline)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				r := rand.New(rand.NewSource(42))
				for j := 0; j < size; j++ {
					key := randKey(r)
					val := randValue(r)
					if err := store.Put(key, val); err != nil {
						b.Fatalf("put: %v", err)
					}
				}
			}
		})
	}
}

// Benchmark: Test with different cache sizes
func BenchmarkOptimizedCacheSize(b *testing.B) {
	cacheSizes := []int64{
		64 << 20,  // 64MB
		128 << 20, // 128MB
		256 << 20, // 256MB (baseline)
		512 << 20, // 512MB
	}

	for _, cacheSize := range cacheSizes {
		b.Run(fmt.Sprintf("%dMiB", cacheSize>>20), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_cache"
			store, err := OpenOptimized(dir, cacheSize, 0, ConfigFastReads)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			r := rand.New(rand.NewSource(42))
			keys := make([][]byte, 100000)
			for i := 0; i < len(keys); i++ {
				keys[i] = randKey(r)
				if err := store.Put(keys[i], randValue(r)); err != nil {
					b.Fatalf("warm put: %v", err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				idx := r.Intn(len(keys))
				_, _, _ = store.Get(keys[idx])
			}
		})
	}
}

// Benchmark: Throughput test with sequential writes
func BenchmarkOptimizedThroughput(b *testing.B) {
	configs := []OptimConfig{
		ConfigBaseline,
		ConfigNoCompression,
		ConfigFastReads,
	}

	const entries = 1000000 // 1M entries

	for _, config := range configs {
		b.Run(string(config), func(b *testing.B) {
			dir := b.TempDir() + "/pebble_throughput"
			store, err := OpenOptimized(dir, 256<<20, 0, config)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer store.Close()

			r := rand.New(rand.NewSource(42))

			b.ReportAllocs()
			start := time.Now()

			for i := 0; i < entries; i++ {
				key := randKey(r)
				val := randValue(r)
				if err := store.Put(key, val); err != nil {
					b.Fatalf("put: %v", err)
				}
			}

			elapsed := time.Since(start)
			b.ReportMetric(float64(elapsed.Seconds()), "s")
			b.ReportMetric(float64(entries)/elapsed.Seconds(), "ops/sec")
		})
	}
}
