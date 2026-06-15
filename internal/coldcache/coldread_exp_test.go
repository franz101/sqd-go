// Cold-read bottleneck experiments. READ-ONLY w.r.t. the live DB — builds its own
// throwaway Pebble stores under t.TempDir() (tmpfs here, exactly like production),
// never touches ClickHouse or the running indexer's store.
//
// Profiling showed cold-tier Get = ~55% of consumer CPU (pebble getIter.Next
// 42.7% + iterator/seek/blockcache + Snappy decode). Because /tmp is tmpfs the
// SSTables are already in RAM, so that cost is PURE CPU: LSM merge traversal +
// block-cache lookup + decompression per Get. This measures the three levers:
//
//	x) Pebble params  — Snappy vs None compression, alloc per Get
//	a) own structure  — flat Go map (O(1)) as the upper bound
//	   parallelism    — N-goroutine Get throughput (Pebble reads are concurrent)
//
// Run:
//
//	COLDREAD_EXP=1 go test ./internal/coldcache/ -run TestColdReadExperiment -v -count=1 -timeout 1200s
package coldcache

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/cockroachdb/pebble/sstable"
)

const (
	expKeySize = 52  // UserPositionsClockKey: User(20) + TokenID(32)
	expValSize = 224 // MemoryUserPosition footprint
)

// fillKey/fillVal derive a deterministic entry from a seed. The value mimics real
// cold data: the first 52 bytes (address + token id) are incompressible; the rest
// (decimals/block numbers) are mostly zero, so Snappy gets a ~2x ratio like the
// real store (15 GB compressed / ~30 GB raw).
func fillKey(b []byte, s uint64) {
	binary.LittleEndian.PutUint64(b[0:], s)
	binary.LittleEndian.PutUint64(b[8:], s*2654435761)
	binary.LittleEndian.PutUint64(b[16:], s*40503)
	binary.LittleEndian.PutUint64(b[24:], s^0x9e3779b97f4a7c15)
	binary.LittleEndian.PutUint64(b[32:], s*0xff51afd7ed558ccd)
	binary.LittleEndian.PutUint64(b[40:], s*0xc4ceb9fe1a85ec53)
	binary.LittleEndian.PutUint32(b[48:], uint32(s))
}

func fillVal(b []byte, s uint64) {
	for i := range b {
		b[i] = 0
	}
	// 52 incompressible bytes (address + token id), like the embedded key columns.
	for i := 0; i < 52; i += 8 {
		binary.LittleEndian.PutUint64(b[i:], s*(uint64(i)+1)*0x9e3779b97f4a7c15)
	}
	// a few small structured numbers; the rest stays zero (compressible).
	binary.LittleEndian.PutUint64(b[200:], s%1_000_000) // amount-ish
	binary.LittleEndian.PutUint64(b[208:], 84_000_000)  // block number
	binary.LittleEndian.PutUint64(b[216:], s%5000)      // log index
}

func buildExpStore(t testing.TB, dir string, n int, comp sstable.Compression, cacheBytes int64) {
	cache := pebble.NewCache(cacheBytes)
	defer cache.Unref()
	opts := &pebble.Options{Cache: cache, MemTableSize: 64 << 20, DisableWAL: true, MemTableStopWritesThreshold: 4}
	opts.EnsureDefaults()
	for i := range opts.Levels {
		opts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
		opts.Levels[i].Compression = comp
	}
	db, err := pebble.Open(dir, opts)
	if err != nil {
		t.Fatalf("open build store: %v", err)
	}
	key := make([]byte, expKeySize)
	val := make([]byte, expValSize)
	batch := db.NewBatch()
	for i := 0; i < n; i++ {
		s := uint64(i) + 1
		fillKey(key, s)
		fillVal(val, s)
		if err := batch.Set(key, val, nil); err != nil {
			t.Fatalf("batch set: %v", err)
		}
		if batch.Len() >= 4<<20 {
			if err := db.Apply(batch, pebble.NoSync); err != nil {
				t.Fatalf("apply: %v", err)
			}
			batch.Reset()
		}
	}
	if err := db.Apply(batch, pebble.NoSync); err != nil {
		t.Fatalf("final apply: %v", err)
	}
	batch.Close()
	if err := db.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// Verify persistence: iterate and count, fail loudly if empty.
	it, _ := db.NewIter(nil)
	cnt := 0
	for it.First(); it.Valid(); it.Next() {
		cnt++
	}
	it.Close()
	_ = db.Close()
	if cnt != n {
		t.Fatalf("store %s persisted %d keys, want %d", dir, cnt, n)
	}
}

func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func openExp(t testing.TB, dir string, cacheBytes int64) (*pebble.DB, func()) {
	cache := pebble.NewCache(cacheBytes)
	opts := &pebble.Options{Cache: cache, MemTableSize: 16 << 20, DisableWAL: true, ReadOnly: true}
	opts.EnsureDefaults()
	for i := range opts.Levels {
		opts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
	}
	db, err := pebble.Open(dir, opts)
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	return db, func() { db.Close(); cache.Unref() }
}

// pebbleGet replicates coldcache.Store.GetInto: db.Get + copy into dst + close.
func pebbleGet(db *pebble.DB, key, dst []byte) bool {
	v, closer, err := db.Get(key)
	if err != nil {
		return false
	}
	copy(dst, v)
	closer.Close()
	return true
}

func TestColdReadExperiment(t *testing.T) {
	if os.Getenv("COLDREAD_EXP") != "1" {
		t.Skip("set COLDREAD_EXP=1 to run the cold-read bottleneck experiments")
	}
	n := 5_000_000
	if v := os.Getenv("COLDREAD_N"); v != "" {
		fmt.Sscan(v, &n)
	}
	// Cache deliberately << store so reads exercise the real LSM+decode path
	// (working set > cache), like the 15 GB store vs 8 GB cache in production.
	cacheBytes := int64(512 << 20)
	if v := os.Getenv("COLDREAD_CACHE_MB"); v != "" {
		var mb int64
		fmt.Sscan(v, &mb)
		cacheBytes = mb << 20
	}
	lookups := 1_000_000
	if n < lookups {
		lookups = n
	}

	snappyDir := t.TempDir() + "/snappy"
	noneDir := t.TempDir() + "/none"

	t.Logf("building 2x %d-entry stores (key=%dB val=%dB, cache=%dMB)...", n, expKeySize, expValSize, cacheBytes>>20)
	bt := time.Now()
	buildExpStore(t, snappyDir, n, sstable.SnappyCompression, cacheBytes)
	buildExpStore(t, noneDir, n, sstable.NoCompression, cacheBytes)
	t.Logf("built in %v | snappy=%.2f GB  none=%.2f GB",
		time.Since(bt).Round(time.Second),
		float64(dirBytes(snappyDir))/1e9, float64(dirBytes(noneDir))/1e9)

	// Deterministic random lookup order (zipfian-ish: bias toward recent keys to
	// mimic a working set, but with a long cold tail). Fixed seed.
	rng := rand.New(rand.NewSource(7))
	seeds := make([]uint64, lookups)
	for i := range seeds {
		seeds[i] = uint64(rng.Intn(n)) + 1
	}

	measurePebble := func(label, dir string, conc int) {
		db, closeDB := openExp(t, dir, cacheBytes)
		defer closeDB()
		// warm: nothing — we WANT cold/cache-miss behaviour
		var found int64
		var memBefore, memAfter runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&memBefore)
		t0 := time.Now()
		if conc <= 1 {
			dst := make([]byte, expValSize)
			key := make([]byte, expKeySize)
			for _, s := range seeds {
				fillKey(key, s)
				if pebbleGet(db, key, dst) {
					found++
				}
			}
		} else {
			var wg sync.WaitGroup
			var f int64
			chunk := (len(seeds) + conc - 1) / conc
			for w := 0; w < conc; w++ {
				lo := w * chunk
				hi := lo + chunk
				if hi > len(seeds) {
					hi = len(seeds)
				}
				if lo >= hi {
					break
				}
				wg.Add(1)
				go func(lo, hi int) {
					defer wg.Done()
					dst := make([]byte, expValSize)
					key := make([]byte, expKeySize)
					var local int64
					for _, s := range seeds[lo:hi] {
						fillKey(key, s)
						if pebbleGet(db, key, dst) {
							local++
						}
					}
					atomic.AddInt64(&f, local)
				}(lo, hi)
			}
			wg.Wait()
			found = f
		}
		dur := time.Since(t0)
		runtime.ReadMemStats(&memAfter)
		allocs := (memAfter.Mallocs - memBefore.Mallocs) / uint64(len(seeds))
		t.Logf("%-26s conc=%2d => %8.0f Get/s  %6.0f ns/Get  %3d allocs/Get  (found %d/%d)",
			label, conc, float64(len(seeds))/dur.Seconds(), float64(dur.Nanoseconds())/float64(len(seeds)), allocs, found, len(seeds))
	}

	// x) compression: Snappy vs None (decompression CPU)
	measurePebble("pebble-snappy", snappyDir, 1)
	measurePebble("pebble-none", noneDir, 1)
	// parallelism: does Pebble Get scale across goroutines?
	measurePebble("pebble-snappy-par4", snappyDir, 4)
	measurePebble("pebble-snappy-par8", snappyDir, 8)
	measurePebble("pebble-none-par8", noneDir, 8)

	// a) own structure: flat Go map = O(1) RAM upper bound
	{
		m := make(map[[expKeySize]byte][expValSize]byte, n)
		var key [expKeySize]byte
		var val [expValSize]byte
		for i := 0; i < n; i++ {
			s := uint64(i) + 1
			fillKey(key[:], s)
			fillVal(val[:], s)
			m[key] = val
		}
		var memStats runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&memStats)
		var found int64
		var dst [expValSize]byte
		t0 := time.Now()
		for _, s := range seeds {
			fillKey(key[:], s)
			if v, ok := m[key]; ok {
				dst = v
				found++
			}
		}
		dur := time.Since(t0)
		_ = dst
		t.Logf("%-26s conc=%2d => %8.0f Get/s  %6.0f ns/Get  (heap≈%.1f GB for %d keys)  found %d/%d",
			"go-map(O(1) RAM)", 1, float64(len(seeds))/dur.Seconds(),
			float64(dur.Nanoseconds())/float64(len(seeds)), float64(memStats.HeapAlloc)/1e9, n, found, len(seeds))
	}
}
