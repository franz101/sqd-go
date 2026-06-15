package coldcache

import (
	"os"
	"testing"

	"github.com/cockroachdb/pebble/sstable"
)

// BenchmarkColdNewKey proves the "create new" fast path. ~83% of cold-tier misses
// are brand-new keys; today each costs a full Pebble Get that only bloom-rejects
// deep in the LSM (iterator setup + per-level filter probes + closer). An in-memory
// Bloom answers "definitely new" in one hash + k bit loads, so the cold reader can
// skip Pebble entirely for new keys.
//
//	COLDREAD_EXP=1 go test ./internal/coldcache/ -run NONE -bench BenchmarkColdNewKey -benchmem -count=1 -timeout 600s
func BenchmarkColdNewKey(b *testing.B) {
	if os.Getenv("COLDREAD_EXP") != "1" {
		b.Skip("set COLDREAD_EXP=1 to run the cold new-key benchmark")
	}
	n := 2_000_000
	dir := b.TempDir() + "/store"
	buildExpStore(b, dir, n, sstable.SnappyCompression, 512<<20)

	// Bloom over the n existing keys.
	bf := NewBloom(uint64(n), 0.01)
	key := make([]byte, expKeySize)
	for i := 0; i < n; i++ {
		fillKey(key, uint64(i)+1)
		bf.Add(key)
	}

	db, closeDB := openExp(b, dir, 512<<20)
	defer closeDB()
	dst := make([]byte, expValSize)

	// Correctness: no false negatives on existing keys; measure FP on new keys.
	for i := 0; i < n; i += n / 1000 {
		fillKey(key, uint64(i)+1)
		if !bf.MightContain(key) {
			b.Fatalf("FALSE NEGATIVE: existing key %d reported absent (bloom is broken)", i)
		}
	}
	fp, trials := 0, 100000
	for i := 0; i < trials; i++ {
		fillKey(key, uint64(n)+uint64(i)+1)
		if bf.MightContain(key) {
			fp++
		}
	}
	b.Logf("bloom: %.1f MB for %d keys; new-key false-positive = %.2f%% (=> %.2f%% of new keys still hit Pebble)",
		float64(bf.Bits())/8/1e6, n, 100*float64(fp)/float64(trials), 100*float64(fp)/float64(trials))

	newSeed := func(i int) uint64 { return uint64(n) + uint64(i) + 1 }

	b.Run("pebble-getinto-newkey", func(b *testing.B) {
		var hit int
		for i := 0; i < b.N; i++ {
			fillKey(key, newSeed(i))
			if pebbleGet(db, key, dst) {
				hit++
			}
		}
		if hit != 0 {
			b.Fatalf("new keys must miss; got %d hits", hit)
		}
	})
	b.Run("bloom-mightcontain-newkey", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			fillKey(key, newSeed(i))
			_ = bf.MightContain(key)
		}
	})
}
