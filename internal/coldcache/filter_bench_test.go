package coldcache

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v3"
)

// BenchmarkFilters compares membership filters for the cold-tier new-key check:
//
//	standard Bloom  — k bits scattered across the array (k cache misses/op)
//	blocked Bloom   — all k bits in one 64-byte block (1 cache miss/op)
//	xsync.MapOf set — EXACT membership (no false positives), but ~key-sized RAM/key
//
// All three are safe for the concurrent Add (recovery/eviction) + concurrent
// lookup (parallel cold reads) the cold tier needs.
//
//	COLDREAD_EXP=1 go test ./internal/coldcache/ -run NONE -bench BenchmarkFilters -benchmem -count=1 -timeout 600s
func BenchmarkFilters(b *testing.B) {
	if os.Getenv("COLDREAD_EXP") != "1" {
		b.Skip("set COLDREAD_EXP=1 to run the filter comparison")
	}
	n := 5_000_000
	key := make([]byte, expKeySize)
	var ka [expKeySize]byte
	mk := func(i int) { fillKey(key, uint64(i)+1) }

	std := NewBloom(uint64(n), 0.01)
	blk := NewBlockedBloom(uint64(n), 0.01)
	xs := xsync.NewMapOf[[expKeySize]byte, struct{}]()

	t0 := time.Now()
	for i := 0; i < n; i++ {
		mk(i)
		std.Add(key)
	}
	stdBuild := time.Since(t0)

	t0 = time.Now()
	for i := 0; i < n; i++ {
		mk(i)
		blk.Add(key)
	}
	blkBuild := time.Since(t0)

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	t0 = time.Now()
	for i := 0; i < n; i++ {
		mk(i)
		copy(ka[:], key)
		xs.Store(ka, struct{}{})
	}
	xsBuild := time.Since(t0)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	xsMB := float64(m1.HeapAlloc-m0.HeapAlloc) / 1e6

	// false positives on new keys (exact set = 0)
	fpStd, fpBlk, trials := 0, 0, 200000
	for i := n; i < n+trials; i++ {
		mk(i)
		if std.MightContain(key) {
			fpStd++
		}
		if blk.MightContain(key) {
			fpBlk++
		}
	}
	// no false negatives (safety)
	for i := 0; i < n; i += n / 1000 {
		mk(i)
		if !std.MightContain(key) || !blk.MightContain(key) {
			b.Fatalf("false negative at %d", i)
		}
	}

	b.Logf("BUILD %dM keys: std=%v blocked=%v xsync=%v", n/1_000_000, stdBuild.Round(time.Millisecond), blkBuild.Round(time.Millisecond), xsBuild.Round(time.Millisecond))
	b.Logf("MEMORY: std=%.0f MB  blocked=%.0f MB  xsync(exact)=%.0f MB", float64(std.Bits())/8/1e6, float64(blk.Bits())/8/1e6, xsMB)
	b.Logf("FALSE-POS(new): std=%.2f%%  blocked=%.2f%%  xsync=0%% (exact)", 100*float64(fpStd)/float64(trials), 100*float64(fpBlk)/float64(trials))

	newKey := func(i int) { fillKey(key, uint64(n)+uint64(i)+1) }

	b.Run("std-newkey", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			newKey(i)
			_ = std.MightContain(key)
		}
	})
	b.Run("blocked-newkey", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			newKey(i)
			_ = blk.MightContain(key)
		}
	})
	b.Run("xsync-newkey", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			newKey(i)
			copy(ka[:], key)
			_, _ = xs.Load(ka)
		}
	})
	// concurrent lookup (the parallel cold-read scenario)
	b.Run("std-newkey-parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			k := make([]byte, expKeySize)
			i := uint64(0)
			for pb.Next() {
				fillKey(k, uint64(n)+i+1)
				i++
				_ = std.MightContain(k)
			}
		})
	})
	b.Run("blocked-newkey-parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			k := make([]byte, expKeySize)
			i := uint64(0)
			for pb.Next() {
				fillKey(k, uint64(n)+i+1)
				i++
				_ = blk.MightContain(k)
			}
		})
	})
	b.Run("xsync-newkey-parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			k := make([]byte, expKeySize)
			var a [expKeySize]byte
			i := uint64(0)
			for pb.Next() {
				fillKey(k, uint64(n)+i+1)
				i++
				copy(a[:], k)
				_, _ = xs.Load(a)
			}
		})
	})
}
