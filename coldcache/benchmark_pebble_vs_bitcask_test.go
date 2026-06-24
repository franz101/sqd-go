//go:build bitcask

// This manual A/B benchmark compares Pebble against bitcask. It requires the
// optional go.mills.io/bitcask/v2 module, so it is gated behind the `bitcask`
// build tag (run with `go test -tags bitcask ./coldcache`). Without the tag the
// rest of the coldcache tests build and run normally.
package coldcache

import (
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	bitcask "go.mills.io/bitcask/v2"
)

// keySize, valueSize, randKey and randValue are shared helpers defined in the
// untagged bench_helpers_test.go.

// targetSizeGB returns the target size in GB for the benchmark (default 12GB).
func targetSizeGB() int {
	if gb := os.Getenv("BENCH_SIZE_GB"); gb != "" {
		var n int
		if _, err := fmt.Sscanf(gb, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 12
}

// numEntriesForSize calculates how many entries we need for target GB.
func numEntriesForSize(gb int) int {
	entrySize := keySize + valueSize
	totalBytes := int64(gb) << 30
	return int(totalBytes / int64(entrySize))
}

// ========== Pebble Benchmark Wrappers ==========

type pebbleStore struct {
	db  *pebble.DB
	dir string
}

func openPebble(dir string, cacheMB int64) (*pebbleStore, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cache := pebble.NewCache(cacheMB << 20)
	opts := &pebble.Options{
		Cache:                       cache,
		MemTableSize:                64 << 20,
		MemTableStopWritesThreshold: 4,
		DisableWAL:                  true, // ephemeral
	}
	db, err := pebble.Open(dir, opts)
	if err != nil {
		cache.Unref()
		return nil, err
	}
	return &pebbleStore{db: db, dir: dir}, nil
}

func (p *pebbleStore) putBatch(batch *pebble.Batch) error {
	return p.db.Apply(batch, pebble.NoSync)
}

func (p *pebbleStore) put(key, value []byte) error {
	return p.db.Set(key, value, pebble.NoSync)
}

func (p *pebbleStore) get(key []byte) ([]byte, bool, error) {
	v, closer, err := p.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := make([]byte, len(v))
	copy(out, v)
	closer.Close()
	return out, true, nil
}

func (p *pebbleStore) close() error {
	err := p.db.Close()
	if p.dir != "" {
		_ = os.RemoveAll(p.dir)
	}
	return err
}

// ========== Bitcask Benchmark Wrappers ==========

type bitcaskStore struct {
	db  *bitcask.Bitcask
	dir string
}

func openBitcask(dir string, sync bool) (*bitcaskStore, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var opts []bitcask.Option
	if sync {
		opts = append(opts, bitcask.WithSyncWrites(true))
	}
	db, err := bitcask.Open(dir, opts...)
	if err != nil {
		return nil, err
	}
	return &bitcaskStore{db: db, dir: dir}, nil
}

func (b *bitcaskStore) put(key, value []byte) error {
	return (*b.db).Put(key, value)
}

func (b *bitcaskStore) get(key []byte) ([]byte, bool, error) {
	v, err := (*b.db).Get(key)
	if err == bitcask.ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (b *bitcaskStore) close() error {
	err := (*b.db).Close()
	if b.dir != "" {
		_ = os.RemoveAll(b.dir)
	}
	return err
}

// ========== Benchmarks ==========

// Benchmark: Sequential writes (cold tier fill pattern)
func BenchmarkPebbleSequentialWrite12GB(b *testing.B) {
	gb := targetSizeGB()
	n := numEntriesForSize(gb)
	b.Logf("Writing %d entries (%d GB) with Pebble", n, gb)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dir := b.TempDir() + "/pebble"
		store, err := openPebble(dir, 256) // 256MB cache
		if err != nil {
			b.Fatalf("open: %v", err)
		}

		r := rand.New(rand.NewSource(42))
		start := time.Now()

		for j := 0; j < n; j++ {
			key := randKey(r)
			val := randValue(r)
			if err := store.put(key, val); err != nil {
				b.Fatalf("put: %v", err)
			}
		}

		elapsed := time.Since(start)
		b.ReportMetric(float64(elapsed.Seconds()), "s")
		b.ReportMetric(float64(n)/elapsed.Seconds(), "ops/sec")

		if err := store.close(); err != nil {
			b.Fatalf("close: %v", err)
		}

		if b.N > 1 {
			return // only run once for size benchmarks
		}
	}
}

func BenchmarkBitcaskSequentialWrite12GB(b *testing.B) {
	gb := targetSizeGB()
	n := numEntriesForSize(gb)
	b.Logf("Writing %d entries (%d GB) with Bitcask", n, gb)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dir := b.TempDir() + "/bitcask"
		store, err := openBitcask(dir, false) // no sync for fair comparison
		if err != nil {
			b.Fatalf("open: %v", err)
		}

		r := rand.New(rand.NewSource(42))
		start := time.Now()

		for j := 0; j < n; j++ {
			key := randKey(r)
			val := randValue(r)
			if err := store.put(key, val); err != nil {
				b.Fatalf("put: %v", err)
			}
		}

		elapsed := time.Since(start)
		b.ReportMetric(float64(elapsed.Seconds()), "s")
		b.ReportMetric(float64(n)/elapsed.Seconds(), "ops/sec")

		if err := store.close(); err != nil {
			b.Fatalf("close: %v", err)
		}

		if b.N > 1 {
			return
		}
	}
}

// Benchmark: Random reads (after warm fill)
func BenchmarkPebbleRandomRead12GB(b *testing.B) {
	dir := b.TempDir() + "/pebble"
	store, err := openPebble(dir, 256)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	r := rand.New(rand.NewSource(42))
	keys := make([][]byte, 100000) // sample 100k keys for reads
	b.Logf("Warming up Pebble with %d keys for read benchmark...", len(keys))
	for i := 0; i < len(keys); i++ {
		keys[i] = randKey(r)
		if err := store.put(keys[i], randValue(r)); err != nil {
			b.Fatalf("warm put: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		idx := r.Intn(len(keys))
		_, found, err := store.get(keys[idx])
		if err != nil || !found {
			b.Fatalf("get failed: err=%v found=%v", err, found)
		}
	}
}

func BenchmarkBitcaskRandomRead12GB(b *testing.B) {
	dir := b.TempDir() + "/bitcask"
	store, err := openBitcask(dir, false)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	r := rand.New(rand.NewSource(42))
	keys := make([][]byte, 100000)
	b.Logf("Warming up Bitcask with %d keys for read benchmark...", len(keys))
	for i := 0; i < len(keys); i++ {
		keys[i] = randKey(r)
		if err := store.put(keys[i], randValue(r)); err != nil {
			b.Fatalf("warm put: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		idx := r.Intn(len(keys))
		_, found, err := store.get(keys[idx])
		if err != nil || !found {
			b.Fatalf("get failed: err=%v found=%v", err, found)
		}
	}
}

// Benchmark: Mixed read/write (90% reads, 10% writes)
func BenchmarkPebbleMixedWorkload(b *testing.B) {
	dir := b.TempDir() + "/pebble"
	store, err := openPebble(dir, 256)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	// Warm up
	r := rand.New(rand.NewSource(42))
	keys := make([][]byte, 100000)
	b.Logf("Warming up Pebble with %d keys for mixed workload...", len(keys))
	for i := 0; i < len(keys); i++ {
		keys[i] = randKey(r)
		if err := store.put(keys[i], randValue(r)); err != nil {
			b.Fatalf("warm put: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if r.Intn(10) == 0 {
			// 10% writes
			newKey := randKey(r)
			if err := store.put(newKey, randValue(r)); err != nil {
				b.Fatalf("put: %v", err)
			}
		} else {
			// 90% reads
			idx := r.Intn(len(keys))
			_, _, err := store.get(keys[idx])
			if err != nil {
				b.Fatalf("get: %v", err)
			}
		}
	}
}

func BenchmarkBitcaskMixedWorkload(b *testing.B) {
	dir := b.TempDir() + "/bitcask"
	store, err := openBitcask(dir, false)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	// Warm up
	r := rand.New(rand.NewSource(42))
	keys := make([][]byte, 100000)
	b.Logf("Warming up Bitcask with %d keys for mixed workload...", len(keys))
	for i := 0; i < len(keys); i++ {
		keys[i] = randKey(r)
		if err := store.put(keys[i], randValue(r)); err != nil {
			b.Fatalf("warm put: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if r.Intn(10) == 0 {
			// 10% writes
			newKey := randKey(r)
			if err := store.put(newKey, randValue(r)); err != nil {
				b.Fatalf("put: %v", err)
			}
		} else {
			// 90% reads
			idx := r.Intn(len(keys))
			_, _, err := store.get(keys[idx])
			if err != nil {
				b.Fatalf("get: %v", err)
			}
		}
	}
}

// Benchmark: Point lookup pattern (single GetInto-style operation)
func BenchmarkPebblePointLookup(b *testing.B) {
	dir := b.TempDir() + "/pebble"
	store, err := openPebble(dir, 256)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	r := rand.New(rand.NewSource(42))
	keys := make([][]byte, 100000)
	for i := 0; i < len(keys); i++ {
		keys[i] = randKey(r)
		if err := store.put(keys[i], randValue(r)); err != nil {
			b.Fatalf("warm put: %v", err)
		}
	}

	var dst [valueSize]byte
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		idx := r.Intn(len(keys))
		val, found, err := store.get(keys[idx])
		if err != nil || !found {
			b.Fatalf("get failed: err=%v found=%v", err, found)
		}
		copy(dst[:], val)
	}
}

func BenchmarkBitcaskPointLookup(b *testing.B) {
	dir := b.TempDir() + "/bitcask"
	store, err := openBitcask(dir, false)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	r := rand.New(rand.NewSource(42))
	keys := make([][]byte, 100000)
	for i := 0; i < len(keys); i++ {
		keys[i] = randKey(r)
		if err := store.put(keys[i], randValue(r)); err != nil {
			b.Fatalf("warm put: %v", err)
		}
	}

	var dst [valueSize]byte
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		idx := r.Intn(len(keys))
		val, found, err := store.get(keys[idx])
		if err != nil || !found {
			b.Fatalf("get failed: err=%v found=%v", err, found)
		}
		copy(dst[:], val)
	}
}

// Benchmark: Batch writes (WriteBatch pattern)
func BenchmarkPebbleBatchWrite(b *testing.B) {
	dir := b.TempDir() + "/pebble"
	store, err := openPebble(dir, 256)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	const batchSize = 16384 // matches writeBatchFlushCount
	b.ReportAllocs()
	b.ResetTimer()

	batch := store.db.NewBatch()
	for i := 0; i < b.N; i++ {
		r := rand.New(rand.NewSource(42))
		for j := 0; j < batchSize; j++ {
			key := randKey(r)
			val := randValue(r)
			if err := batch.Set(key, val, nil); err != nil {
				b.Fatalf("batch set: %v", err)
			}
		}
		if err := store.db.Apply(batch, pebble.NoSync); err != nil {
			b.Fatalf("batch apply: %v", err)
		}
		batch.Reset()
	}
	batch.Close()
}

func BenchmarkBitcaskBatchWrite(b *testing.B) {
	dir := b.TempDir() + "/bitcask"
	store, err := openBitcask(dir, false)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	const batchSize = 16384
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r := rand.New(rand.NewSource(42))
		for j := 0; j < batchSize; j++ {
			key := randKey(r)
			val := randValue(r)
			if err := store.put(key, val); err != nil {
				b.Fatalf("put: %v", err)
			}
		}
	}
}

// Benchmark: Memory allocation pressure (allocs/op)
func BenchmarkPebbleAllocs(b *testing.B) {
	dir := b.TempDir() + "/pebble"
	store, err := openPebble(dir, 256)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	r := rand.New(rand.NewSource(42))
	key := randKey(r)
	val := randValue(r)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := store.put(key, val); err != nil {
			b.Fatalf("put: %v", err)
		}
		_, _, _ = store.get(key)
	}
}

func BenchmarkBitcaskAllocs(b *testing.B) {
	dir := b.TempDir() + "/bitcask"
	store, err := openBitcask(dir, false)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer store.close()

	r := rand.New(rand.NewSource(42))
	key := randKey(r)
	val := randValue(r)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := store.put(key, val); err != nil {
			b.Fatalf("put: %v", err)
		}
		_, _, _ = store.get(key)
	}
}

// Sub-benchmark: Different sizes
func BenchmarkPebble1GBWrite(b *testing.B) { benchmarkPebbleWriteSize(b, 1) }
func BenchmarkPebble4GBWrite(b *testing.B) { benchmarkPebbleWriteSize(b, 4) }
func BenchmarkPebble12GBWrite(b *testing.B) { benchmarkPebbleWriteSize(b, 12) }

func BenchmarkBitcask1GBWrite(b *testing.B) { benchmarkBitcaskWriteSize(b, 1) }
func BenchmarkBitcask4GBWrite(b *testing.B) { benchmarkBitcaskWriteSize(b, 4) }
func BenchmarkBitcask12GBWrite(b *testing.B) { benchmarkBitcaskWriteSize(b, 12) }

func benchmarkPebbleWriteSize(b *testing.B, gb int) {
	n := numEntriesForSize(gb)
	b.Logf("Writing %d entries (%d GB) with Pebble", n, gb)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dir := b.TempDir() + "/pebble"
		store, err := openPebble(dir, 256)
		if err != nil {
			b.Fatalf("open: %v", err)
		}

		r := rand.New(rand.NewSource(42))
		start := time.Now()

		for j := 0; j < n; j++ {
			if err := store.put(randKey(r), randValue(r)); err != nil {
				b.Fatalf("put: %v", err)
			}
		}

		elapsed := time.Since(start)
		b.ReportMetric(float64(elapsed.Seconds()), "s")
		b.ReportMetric(float64(n)/elapsed.Seconds(), "ops/sec")

		store.close()
		if b.N > 1 {
			return
		}
	}
}

func benchmarkBitcaskWriteSize(b *testing.B, gb int) {
	n := numEntriesForSize(gb)
	b.Logf("Writing %d entries (%d GB) with Bitcask", n, gb)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dir := b.TempDir() + "/bitcask"
		store, err := openBitcask(dir, false)
		if err != nil {
			b.Fatalf("open: %v", err)
		}

		r := rand.New(rand.NewSource(42))
		start := time.Now()

		for j := 0; j < n; j++ {
			if err := store.put(randKey(r), randValue(r)); err != nil {
				b.Fatalf("put: %v", err)
			}
		}

		elapsed := time.Since(start)
		b.ReportMetric(float64(elapsed.Seconds()), "s")
		b.ReportMetric(float64(n)/elapsed.Seconds(), "ops/sec")

		store.close()
		if b.N > 1 {
			return
		}
	}
}
