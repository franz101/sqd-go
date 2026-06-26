package coldcache

import "testing"

// Realistic A/B: the thread-safe flatcold (mutex-locked, the actual backend) vs
// Pebble, on the cold access pattern. flatstore_bench_test.go's unlocked flatStore
// is the theoretical ceiling; this is what the SQD_COLDCACHE_BACKEND=flat path ships.
func BenchmarkColdStore_Flatcold_GetInto(b *testing.B) {
	f := newFlatcold(1 << 20)
	keys, vals := makeKV(benchN)
	for i := range keys {
		f.put(keys[i], vals[i])
	}
	dst := make([]byte, valueSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !f.getInto(dst, keys[i%benchN]) {
			b.Fatal("miss")
		}
	}
}

func BenchmarkColdStore_Flatcold_Put(b *testing.B) {
	// Capacity < benchN so the steady state is insert-with-CLOCK-eviction — the
	// real cold spill path. Warm up once to move the one-time buffer init outside
	// the timer (otherwise it shows as a phantom ~600 B/op = the 500 MiB init / b.N).
	f := newFlatcold(1 << 18)
	keys, vals := makeKV(benchN)
	f.put(keys[0], vals[0])
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.put(keys[i%benchN], vals[i%benchN])
	}
}
