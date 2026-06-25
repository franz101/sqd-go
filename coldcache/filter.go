package coldcache

import (
	"encoding/binary"
	"sync/atomic"
)

// negFilter is a fixed-size, in-memory *blocked* Bloom filter used as a NEGATIVE
// cache in front of Pebble (the "V3" cold-tier optimization).
//
// Contract: keys are only ever added (on Put), never removed. A Bloom filter
// therefore has no false negatives, so `mayContain(k) == false` is an
// authoritative "k was never written to the cold store" — and a Get can skip the
// Pebble lookup (and, in authoritative from-genesis mode, the ClickHouse SELECT)
// entirely. False positives are allowed and simply fall through to a correct
// Pebble Get, so the filter can never produce a wrong answer; if it saturates it
// degrades to "always probe Pebble" (i.e. V2 behaviour), never to data loss.
//
// Why blocked: a classic Bloom filter sets/tests k bits scattered across the whole
// bitset — k cache misses per probe. Once the bitset is larger than L2 that is
// *slower* than a hot Pebble negative Get, which defeats the purpose. A blocked
// filter confines all k bits of a key to a single 64-byte block (one cache line),
// so each add/mayContain touches exactly one cache line. This is what makes the
// negative cache actually faster than Pebble.
//
// Why it matters: during a from-genesis backfill almost every hot-miss is a
// brand-new key, so V2 fires one negative Pebble Get per event (iterator setup +
// getInternal + per-level sstable bloom checks). V3 turns those into a single
// cache-line test in RAM.
//
// Bounded memory: the block array is a power-of-two allocated once at
// construction, so RSS is fixed regardless of how many keys flow through.
//
// Single-writer: like the cold Store, all access is from the one processor
// goroutine, so no synchronisation is required.
type negFilter struct {
	blocks    []negBlock
	blockMask uint64 // len(blocks)-1; len(blocks) is a power of two
	k         uint   // bits set per key, all within one block
}

// negBlock is one cache line (512 bits) of the filter.
const (
	blockBits  = 512
	blockWords = blockBits / 64 // 8
)

type negBlock [blockWords]uint64

const (
	fnvOffset64 = 1469598103934665603
	fnvPrime64  = 1099511628211
)

// newNegFilter builds a blocked Bloom filter with at least bitBudget bits, rounded
// up so the block count is a power of two (minimum 64 blocks). k=8 keeps the
// false-positive rate low while the live key count stays under ~10 per block;
// beyond that it rises gracefully (more wasted Pebble probes, never a wrong
// result).
func newNegFilter(bitBudget uint64) *negFilter {
	nb := bitBudget / blockBits
	const minBlocks = 64
	n := uint64(minBlocks)
	for n < nb {
		n <<= 1
	}
	return &negFilter{
		blocks:    make([]negBlock, n),
		blockMask: n - 1,
		k:         8,
	}
}

// negHash hashes the key with a word-at-a-time FNV-1a pass (≈len/8 multiplies
// instead of len), then derives two independent 64-bit values via avalanche
// mixing: h picks the block and seeds the in-block bit walk, g is the stride.
func negHash(key []byte) (uint64, uint64) {
	h := uint64(fnvOffset64)
	i := 0
	for ; i+8 <= len(key); i += 8 {
		h ^= binary.LittleEndian.Uint64(key[i:])
		h *= fnvPrime64
	}
	for ; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= fnvPrime64
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	g := h * 0x9e3779b97f4a7c15
	g ^= g >> 29
	g |= 1 // odd stride so the k probes spread across the block
	return h, g
}

func (f *negFilter) add(key []byte) {
	h, g := negHash(key)
	blk := &f.blocks[h&f.blockMask]
	for i := uint(0); i < f.k; i++ {
		bit := (h>>9 + uint64(i)*g) & (blockBits - 1)
		atomic.OrUint64(&blk[bit>>6], 1<<(bit&63))
	}
}

func (f *negFilter) mayContain(key []byte) bool {
	h, g := negHash(key)
	blk := &f.blocks[h&f.blockMask]
	for i := uint(0); i < f.k; i++ {
		bit := (h>>9 + uint64(i)*g) & (blockBits - 1)
		if atomic.LoadUint64(&blk[bit>>6])&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}
