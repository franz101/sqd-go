package coldcache

import (
	"encoding/binary"
	"os"
	"strconv"
	"sync/atomic"
)

// negFilter is a fixed-size, in-memory *blocked* Bloom filter used as a NEGATIVE
// cache in front of Pebble (the "V3" cold-tier optimization).
type negFilter interface {
	add(key []byte)
	mayContain(key []byte) bool
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

// BloomFilter is a standard, non-atomic blocked Bloom filter for single-writer access.
type BloomFilter struct {
	blocks    []negBlock
	blockMask uint64
	k         uint
}

// AtomicBloom is a thread-safe, atomic-based blocked Bloom filter.
type AtomicBloom struct {
	blocks    []negBlock
	blockMask uint64
	k         uint
}

// newNegFilter builds a blocked Bloom filter with at least bitBudget bits, rounded
// up so the block count is a power of two (minimum 64 blocks). k=8 keeps the
// false-positive rate low while the live key count stays under ~10 per block.
func newNegFilter(bitBudget uint64) negFilter {
	nb := bitBudget / blockBits
	const minBlocks = 64
	n := uint64(minBlocks)
	for n < nb {
		n <<= 1
	}

	if filterAtomicDefault() {
		return &AtomicBloom{
			blocks:    make([]negBlock, n),
			blockMask: n - 1,
			k:         8,
		}
	}
	return &BloomFilter{
		blocks:    make([]negBlock, n),
		blockMask: n - 1,
		k:         8,
	}
}

// filterAtomicDefault reports whether the filter should use atomic bit ops. It
// defaults to true (concurrency-safe); SQD_COLDCACHE_FILTER_ATOMIC=0 (or any
// other false-y value) opts into the non-atomic single-writer fast path.
func filterAtomicDefault() bool {
	v, ok := os.LookupEnv("SQD_COLDCACHE_FILTER_ATOMIC")
	if !ok {
		return true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return b
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

func (f *BloomFilter) add(key []byte) {
	h, g := negHash(key)
	blk := &f.blocks[h&f.blockMask]
	for i := uint(0); i < f.k; i++ {
		bit := (h>>9 + uint64(i)*g) & (blockBits - 1)
		blk[bit>>6] |= 1 << (bit & 63)
	}
}

func (f *BloomFilter) mayContain(key []byte) bool {
	h, g := negHash(key)
	blk := &f.blocks[h&f.blockMask]
	for i := uint(0); i < f.k; i++ {
		bit := (h>>9 + uint64(i)*g) & (blockBits - 1)
		if blk[bit>>6]&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}

func (f *AtomicBloom) add(key []byte) {
	h, g := negHash(key)
	blk := &f.blocks[h&f.blockMask]
	for i := uint(0); i < f.k; i++ {
		bit := (h>>9 + uint64(i)*g) & (blockBits - 1)
		atomic.OrUint64(&blk[bit>>6], 1<<(bit&63))
	}
}

func (f *AtomicBloom) mayContain(key []byte) bool {
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
