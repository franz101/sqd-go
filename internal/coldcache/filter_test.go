package coldcache

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

// keyAt builds a deterministic 52-byte key (20-byte addr + 32-byte hash shape,
// like a UserPositions cold key) for index i.
func keyAt(i uint64) []byte {
	k := make([]byte, 52)
	binary.BigEndian.PutUint64(k[12:20], i+1) // address tail
	binary.BigEndian.PutUint64(k[44:52], i*2654435761+7)
	return k
}

// TestNegFilterNoFalseNegatives is the property the cold-tier correctness depends
// on: every added key must report mayContain==true. A false negative would make a
// Get wrongly skip Pebble and lose an evicted entry (wrong PnL).
func TestNegFilterNoFalseNegatives(t *testing.T) {
	f := newNegFilter(1 << 20)
	const n = 200_000
	for i := uint64(0); i < n; i++ {
		f.add(keyAt(i))
	}
	for i := uint64(0); i < n; i++ {
		if !f.mayContain(keyAt(i)) {
			t.Fatalf("false negative for key %d — added but mayContain==false", i)
		}
	}
}

// TestNegFilterFalsePositiveRate sanity-checks that never-added keys are mostly
// rejected (so the filter actually saves Pebble probes). It does not need to be
// exact; a wildly high rate would mean the filter is broken/useless.
func TestNegFilterFalsePositiveRate(t *testing.T) {
	f := newNegFilter(1 << 23) // ~1M bits-ish after rounding; generous for 50k keys
	const n = 50_000
	for i := uint64(0); i < n; i++ {
		f.add(keyAt(i))
	}
	fp := 0
	const probes = 100_000
	for i := uint64(n); i < n+probes; i++ {
		if f.mayContain(keyAt(i)) {
			fp++
		}
	}
	rate := float64(fp) / float64(probes)
	if rate > 0.05 {
		t.Fatalf("false-positive rate %.3f too high (filter not discriminating)", rate)
	}
	t.Logf("blocked-bloom false-positive rate over %d probes: %.4f", probes, rate)
}

// TestStoreNegativeFilterCorrectness exercises the full Store path with the filter
// enabled: Put values are always readable (no false negative), never-written keys
// report absent, and Delete + filter cannot resurrect or lose a value.
func TestStoreNegativeFilterCorrectness(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store"), 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	s.EnableNegativeFilter(1 << 20)

	const n = 20_000
	for i := uint64(0); i < n; i++ {
		val := make([]byte, 8)
		binary.BigEndian.PutUint64(val, i+1)
		if err := s.Put(keyAt(i), val); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	// Every written key must be found with the right value (filter never hides it).
	for i := uint64(0); i < n; i++ {
		got, found, err := s.Get(keyAt(i))
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if !found {
			t.Fatalf("written key %d not found (false negative)", i)
		}
		if binary.BigEndian.Uint64(got) != i+1 {
			t.Fatalf("key %d value = %d, want %d", i, binary.BigEndian.Uint64(got), i+1)
		}
	}
	// Never-written keys are absent.
	missing := 0
	for i := uint64(n); i < n+5_000; i++ {
		if _, found, _ := s.Get(keyAt(i)); !found {
			missing++
		}
	}
	if missing == 0 {
		t.Fatal("expected most never-written keys to be absent")
	}
	if s.FilterSkips() == 0 {
		t.Fatal("expected the negative filter to have skipped some Pebble Gets")
	}
	// Delete then Get must report absent (filter is allowed to say "maybe" → Pebble
	// confirms the deletion).
	if err := s.Delete(keyAt(0)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := s.Get(keyAt(0)); found {
		t.Fatal("deleted key still found")
	}
}
