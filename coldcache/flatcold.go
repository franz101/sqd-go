package coldcache

import (
	"bytes"
	"encoding/binary"
	"sync"
)

// flatcold is an in-RAM, bounded, drop-in cold-tier backend (selected with
// SQD_COLDCACHE_BACKEND=flat). It is a CLOCK (second-chance) cache over flat
// key/value byte buffers with a chained hash index — the same structure the
// generated hot ring uses, generalized to opaque fixed-size records.
//
// Why this shape: the cold tier's access pattern is known and narrow — fixed-size
// pointer-free key->value, point lookups only, no ordered scans, and no durability
// (ClickHouse is the source of truth; an evicted entry simply re-resolves). Pebble
// is a general-purpose ordered LSM and pays ~2us/op for machinery we never use; a
// purpose-built flat store is ~26x faster (see flatstore_bench_test.go).
//
// Bounding / TTL: the store is hard-capped at `capacity` slots. CLOCK eviction
// keeps the working set and drops cold entries when full — and because the
// processor writes in block order, insertion-order eviction approximates a
// block-height TTL for free. Overflow is correct, not lossy: a miss falls through
// to the existing batched ClickHouse resolver.
//
// Concurrency: a single mutex makes it safe for the parallel cold-recovery writers
// (the steady-state hot path is single-owner). A lock-free / sharded index
// (cf. Firedancer fd_map_slot_para) is the next step if the recovery lock matters.
type flatcold struct {
	mu sync.Mutex

	budgetBytes int64 // used to derive capacity on first init when capacity==0
	capacity    uint64
	keyLen      int
	valLen      int

	slotKey []byte // capacity*keyLen
	slotVal []byte // capacity*valLen
	inUse   []bool // capacity
	ref     []bool // capacity: CLOCK referenced bit

	buckets    []int32 // bucketCount: chain head per bucket (-1 empty)
	next       []int32 // capacity: chain links
	bucketMask uint64

	hand uint64
	size int
}

func newFlatcold(capacity uint64) *flatcold {
	if capacity == 0 {
		capacity = 1
	}
	return &flatcold{capacity: capacity}
}

func newFlatcoldBudget(budgetBytes int64) *flatcold {
	return &flatcold{budgetBytes: budgetBytes}
}

const flatcoldMinCapacity = 1 << 16

func (f *flatcold) init(keyLen, valLen int) {
	if f.capacity == 0 {
		slot := int64(keyLen + valLen + 2) // +2 ~ inUse/ref/chain bookkeeping
		if f.budgetBytes > 0 && slot > 0 {
			f.capacity = uint64(f.budgetBytes / slot)
		}
		if f.capacity < flatcoldMinCapacity {
			f.capacity = flatcoldMinCapacity
		}
	}
	f.keyLen = keyLen
	f.valLen = valLen
	f.slotKey = make([]byte, f.capacity*uint64(keyLen))
	f.slotVal = make([]byte, f.capacity*uint64(valLen))
	f.inUse = make([]bool, f.capacity)
	f.ref = make([]bool, f.capacity)
	f.next = make([]int32, f.capacity)
	bc := uint64(1)
	for bc < f.capacity {
		bc <<= 1
	}
	f.buckets = make([]int32, bc)
	for i := range f.buckets {
		f.buckets[i] = -1
	}
	f.bucketMask = bc - 1
}

// fhash is a word-at-a-time FNV-style hash: it mixes every byte (robust for
// structured keys) but consumes 8 bytes per iteration instead of one, so a 52-byte
// cold key costs ~6 multiplies instead of 52. A final avalanche spreads the high
// bits into the low bits the bucket mask uses.
func fnv1a(key []byte) uint64 {
	const prime = 1099511628211
	var h uint64 = 1469598103934665603
	i := 0
	for ; i+8 <= len(key); i += 8 {
		h = (h ^ binary.LittleEndian.Uint64(key[i:])) * prime
	}
	for ; i < len(key); i++ {
		h = (h ^ uint64(key[i])) * prime
	}
	h ^= h >> 32
	h *= prime
	h ^= h >> 29
	return h
}

func (f *flatcold) keyAt(i uint64) []byte {
	off := i * uint64(f.keyLen)
	return f.slotKey[off : off+uint64(f.keyLen)]
}
func (f *flatcold) valAt(i uint64) []byte {
	off := i * uint64(f.valLen)
	return f.slotVal[off : off+uint64(f.valLen)]
}

func (f *flatcold) lookup(key []byte) int64 {
	if f.buckets == nil {
		return -1
	}
	b := fnv1a(key) & f.bucketMask
	for i := f.buckets[b]; i >= 0; i = f.next[i] {
		if f.inUse[i] && bytes.Equal(f.keyAt(uint64(i)), key) {
			return int64(i)
		}
	}
	return -1
}

func (f *flatcold) idxInsert(key []byte, slot uint64) {
	b := fnv1a(key) & f.bucketMask
	f.next[slot] = f.buckets[b]
	f.buckets[b] = int32(slot)
}

func (f *flatcold) idxUnlink(key []byte, slot uint64) {
	b := fnv1a(key) & f.bucketMask
	prev := int32(-1)
	for i := f.buckets[b]; i >= 0; i = f.next[i] {
		if uint64(i) == slot {
			if prev < 0 {
				f.buckets[b] = f.next[i]
			} else {
				f.next[prev] = f.next[i]
			}
			f.next[i] = -1
			return
		}
		prev = i
	}
}

// allocSlot returns a usable slot index. When not full it returns the next free
// slot; when full it CLOCK-evicts an unreferenced entry and returns its slot.
func (f *flatcold) allocSlot() uint64 {
	full := uint64(f.size) >= f.capacity
	for {
		idx := f.hand % f.capacity
		f.hand++
		if !f.inUse[idx] {
			return idx
		}
		if !full {
			continue // don't disturb CLOCK bits while there is still room
		}
		if f.ref[idx] {
			f.ref[idx] = false // second chance
			continue
		}
		f.idxUnlink(f.keyAt(idx), idx)
		f.inUse[idx] = false
		f.size--
		return idx
	}
}

func (f *flatcold) put(key, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keyLen == 0 {
		f.init(len(key), len(value))
	}
	if i := f.lookup(key); i >= 0 {
		copy(f.valAt(uint64(i)), value)
		f.ref[i] = true
		return
	}
	slot := f.allocSlot()
	copy(f.keyAt(slot), key)
	copy(f.valAt(slot), value)
	f.inUse[slot] = true
	f.ref[slot] = false
	f.idxInsert(key, slot)
	f.size++
}

func (f *flatcold) getInto(dst, key []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.lookup(key)
	if i < 0 {
		return false
	}
	f.ref[i] = true
	copy(dst, f.valAt(uint64(i)))
	return true
}

func (f *flatcold) get(key []byte) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.lookup(key)
	if i < 0 {
		return nil, false
	}
	f.ref[i] = true
	out := make([]byte, f.valLen)
	copy(out, f.valAt(uint64(i)))
	return out, true
}

func (f *flatcold) del(key []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.lookup(key)
	if i < 0 {
		return false
	}
	f.idxUnlink(key, uint64(i))
	f.inUse[i] = false
	f.ref[i] = false
	f.size--
	return true
}

func (f *flatcold) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.size
}
