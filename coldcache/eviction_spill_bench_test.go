package coldcache

import (
	"math/rand"
	"testing"
)

// Finding C7 claims hot-cache eviction spills via per-key Put (db.Set) allocate a
// fresh Pebble batch each, vs the reused WriteBatch. These A/B benchmarks measure
// the real per-spill cost. Pebble pools its internal batches for db.Set, so the
// per-key path may already be allocation-light.
func BenchmarkEvictionSpill_PerKeyPut(b *testing.B) {
	s, err := Open(b.TempDir()+"/cc", 8<<20, 4<<20)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer s.Close()
	r := rand.New(rand.NewSource(1))
	keys := make([][]byte, b.N)
	vals := make([][]byte, b.N)
	for i := range keys {
		keys[i] = randKey(r)
		vals[i] = randValue(r)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Put(keys[i], vals[i])
	}
}

func BenchmarkEvictionSpill_BatchedPut(b *testing.B) {
	s, err := Open(b.TempDir()+"/cc", 8<<20, 4<<20)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer s.Close()
	r := rand.New(rand.NewSource(1))
	keys := make([][]byte, b.N)
	vals := make([][]byte, b.N)
	for i := range keys {
		keys[i] = randKey(r)
		vals[i] = randValue(r)
	}
	wb := s.NewWriteBatch()
	defer wb.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wb.Put(keys[i], vals[i])
	}
}
