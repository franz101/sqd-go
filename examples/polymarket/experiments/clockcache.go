package polymarket

import "sync"

// clockCache is a fixed-capacity, sharded CLOCK (second-chance) cache.
//
// It replaces the previous "flush the whole map once it hits the cap" eviction.
// That strategy periodically dropped the entire hot working set, forcing the
// expensive collection-ID computation (keccak + a 256-bit modular-exponentiation
// loop) to re-run for every active market right after each flush — a recurring
// latency cliff during backfill.
//
// CLOCK keeps recently-used entries across evictions: every Load sets a per-slot
// reference bit, and eviction only reclaims a slot whose bit is clear, sweeping
// (and clearing) set bits as it goes. Hot entries therefore survive — they get a
// "second chance" each sweep — while cold entries are reclaimed one at a time in
// O(1) amortized work. Memory is bounded and the backing arrays are allocated
// once, so unlike the old map-pointer swap there is no data race on concurrent
// access.
//
// Sharding bounds lock scope. In practice the custom processor drives these
// lookups from a single goroutine, so the per-shard mutex is essentially
// always uncontended (~tens of ns) — negligible next to the microsecond-scale
// modexp it elides.
type clockCache[K comparable, V any] struct {
	shards []clockShard[K, V]
	mask   uint64
	hash   func(K) uint64
}

type clockShard[K comparable, V any] struct {
	mu   sync.Mutex
	idx  map[K]int32
	keys []K
	vals []V
	ref  []uint8
	hand int32
	used int32
	cap  int32
}

// newClockCache builds a cache holding up to roughly capacity entries, split
// across the next-power-of-two >= shardCount shards. hash maps a key to a shard
// (only the low bits are used); it does not need to be cryptographic, just
// well-distributed. Keys here are keccak outputs, so reading 8 of their bytes is
// already uniform.
func newClockCache[K comparable, V any](capacity, shardCount int, hash func(K) uint64) *clockCache[K, V] {
	if shardCount < 1 {
		shardCount = 1
	}
	s := 1
	for s < shardCount {
		s <<= 1
	}
	perShard := capacity / s
	if perShard < 1 {
		perShard = 1
	}
	c := &clockCache[K, V]{
		shards: make([]clockShard[K, V], s),
		mask:   uint64(s - 1),
		hash:   hash,
	}
	for i := range c.shards {
		sh := &c.shards[i]
		sh.idx = make(map[K]int32, perShard)
		sh.keys = make([]K, perShard)
		sh.vals = make([]V, perShard)
		sh.ref = make([]uint8, perShard)
		sh.cap = int32(perShard)
	}
	return c
}

func (c *clockCache[K, V]) shardFor(key K) *clockShard[K, V] {
	return &c.shards[c.hash(key)&c.mask]
}

// Load returns the cached value and sets the entry's reference bit so it
// survives the next eviction sweep.
func (c *clockCache[K, V]) Load(key K) (V, bool) {
	sh := c.shardFor(key)
	sh.mu.Lock()
	if i, ok := sh.idx[key]; ok {
		v := sh.vals[i]
		sh.ref[i] = 1
		sh.mu.Unlock()
		return v, true
	}
	sh.mu.Unlock()
	var zero V
	return zero, false
}

// Store inserts or updates key. When the shard is full it reclaims a slot via
// the CLOCK hand: advance past set reference bits (clearing them) until a clear
// bit is found, then evict that slot.
func (c *clockCache[K, V]) Store(key K, val V) {
	sh := c.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if i, ok := sh.idx[key]; ok {
		sh.vals[i] = val
		sh.ref[i] = 1
		return
	}

	if sh.used < sh.cap {
		i := sh.used
		sh.used++
		sh.keys[i] = key
		sh.vals[i] = val
		sh.ref[i] = 1
		sh.idx[key] = i
		return
	}

	for {
		i := sh.hand
		sh.hand++
		if sh.hand >= sh.cap {
			sh.hand = 0
		}
		if sh.ref[i] == 0 {
			delete(sh.idx, sh.keys[i])
			sh.keys[i] = key
			sh.vals[i] = val
			sh.ref[i] = 1
			sh.idx[key] = i
			return
		}
		sh.ref[i] = 0
	}
}
