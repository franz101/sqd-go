package coldcache

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
)

// TestMightContainSafeWhenFilterDisabled pins BUGREPORTZ §7.8 / §2: with no
// negative filter (SQD_COLDFILTER_BITS=0), MightContain MUST return true
// (conservative). If it ever returns false on a nil filter, the authoritative
// read-path gate `coldAuthoritative && !ColdMightContain(...)` would fire for
// EXISTING positions on a hot+cold miss and reset them to zero. This is a
// correctness invariant, not a perf one — pin the safe direction.
func TestMightContainSafeWhenFilterDisabled(t *testing.T) {
	t.Setenv("SQD_COLDFILTER_BITS", "0")
	dir := t.TempDir()
	s, err := Open(dir, 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if s.neg != nil {
		t.Fatalf("expected nil negative filter with SQD_COLDFILTER_BITS=0, got non-nil")
	}
	if !s.MightContain([]byte("a-key-that-was-never-written-0123456789abcdef")) {
		t.Fatal("MightContain returned false with no negative filter — " +
			"authoritative mode would silently reset existing positions to zero")
	}
}

// TestNegFilterNoFalseNegatives pins the core invariant (BUGREPORTZ §7.3): the
// add-only negative Bloom filter must NEVER forget a key it was given. A false
// negative would let the authoritative gate skip ClickHouse for a key that WAS
// written (e.g. an evicted position), resetting a real position to zero. False
// positives (extra, harmless CH probes) are allowed; false negatives are not.
func TestNegFilterNoFalseNegatives(t *testing.T) {
	f := newNegFilter(1 << 20) // ~1 Mbit
	const n = 50_000
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("position-key-%08d-padding-padding", i))
		f.add(keys[i])
	}
	for i, k := range keys {
		if !f.mayContain(k) {
			t.Fatalf("FALSE NEGATIVE at key %d: the filter forgot a key it added — "+
				"authoritative mode would reset this position", i)
		}
	}
}

// TestSplitBloomConcurrentAddNoFalseNegatives pins the parallel-recovery safety of
// the PRODUCTION filter (SplitBloom). The cold filter is populated from ~8 goroutines
// during recovery (recoverColdParallel / recoverFilterKeysParallel). If add() does a
// non-atomic read-modify-write on the shared 64-bit block words, two keys touching the
// same word race (torn RMW) and one bit-set is lost -> a FALSE NEGATIVE -> the
// authoritative gate resets a real position to zero (silent ClickHouse corruption).
// Exercise atomicOps=false (the non-atomic READ mode) under concurrent adds: the WRITE
// must still be atomic, so zero false negatives AND clean under `go test -race`.
func TestSplitBloomConcurrentAddNoFalseNegatives(t *testing.T) {
	f := newSplitBloom(1<<24, false) // atomicOps=false: relaxed reads, but writes stay atomic
	const (
		workers   = 8
		perWorker = 200_000
		total     = workers * perWorker
	)
	key := func(i int) []byte {
		b := make([]byte, 32)
		binary.BigEndian.PutUint64(b[0:], uint64(i)*0x9E3779B97F4A7C15)
		binary.BigEndian.PutUint64(b[8:], uint64(i)*0xC2B2AE3D27D4EB4F)
		binary.BigEndian.PutUint64(b[16:], uint64(i)*0x165667B19E3779F9)
		binary.BigEndian.PutUint64(b[24:], uint64(i)*0xD6E8FEB86659FD93)
		return b
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < total; i += workers { // interleave so words collide across workers
				f.add(key(i))
			}
		}(w)
	}
	wg.Wait()
	falseNeg := 0
	for i := 0; i < total; i++ {
		if !f.mayContain(key(i)) {
			falseNeg++
		}
	}
	if falseNeg != 0 {
		t.Fatalf("FALSE NEGATIVES after concurrent add: %d of %d — non-atomic add() lost bits "+
			"under parallel recovery; authoritative gate would reset real positions to zero", falseNeg, total)
	}
}

// TestMightContainTrueAfterPutEvenWhenEvicted pins the property the read-path fix
// relies on (BUGREPORTZ §0/§1): once a key is written to the cold tier, MightContain
// keeps reporting "maybe present" for it forever — even after the value itself is
// CLOCK-evicted from a bounded flat tier — so the authoritative gate falls through
// to ClickHouse instead of treating the key as brand-new.
func TestMightContainTrueAfterPutEvenWhenEvicted(t *testing.T) {
	t.Setenv("SQD_COLDCACHE_BACKEND", "flat")
	dir := t.TempDir()
	s, err := Open(dir, 1 /*tiny budget => floors at 65536 slots*/, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	victim := []byte("victim-key-00000000-padding-pad!")
	val := make([]byte, 16)
	if err := s.Put(victim, val); err != nil {
		t.Fatalf("put victim: %v", err)
	}
	// Overflow the flat tier so the victim is CLOCK-evicted from cold storage.
	filler := make([]byte, len(victim))
	copy(filler, victim)
	for i := 0; i < 80_000; i++ {
		// vary the key deterministically without Math.rand
		filler[len(filler)-1] = byte(i)
		filler[len(filler)-2] = byte(i >> 8)
		filler[len(filler)-3] = byte(i >> 16)
		if err := s.Put(filler, val); err != nil {
			t.Fatalf("put filler %d: %v", i, err)
		}
	}
	// The value may be gone from the flat slots, but the filter must still say "maybe".
	if !s.MightContain(victim) {
		t.Fatal("MightContain returned false for an evicted-but-once-written key — " +
			"the eviction-reset bug would re-open (authoritative gate would treat it as new)")
	}
}
