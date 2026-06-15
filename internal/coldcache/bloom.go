package coldcache

import (
	"math"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

// Bloom is a concurrent, lock-free Bloom filter over the cold tier's key set.
//
// Why: in an authoritative cold tier, a hot+cold miss is provably a NEW key, and
// new keys are the common case (≈83% of misses are first-time positions). Each
// such miss otherwise costs a full Pebble Get that only bloom-rejects deep in the
// LSM (iterator setup + per-level filter probes + closer). A single in-memory
// Bloom answers "definitely new" in one hash + k bit loads, so the cold reader
// can skip Pebble for new keys and only fetch the survivors (the "maybe present"
// list). No false negatives, so skipping a "not present" key is always correct.
//
// Reads (MightContain) use plain atomic loads; Add uses atomic OR — both safe to
// call from the parallel cold-read workers and the recovery spill concurrently.
type Bloom struct {
	bits []atomic.Uint64
	m    uint64 // number of bits (always > 0)
	k    uint64 // number of hash probes
}

// NewBloom sizes a filter for ~n keys at the target false-positive rate.
func NewBloom(n uint64, fpRate float64) *Bloom {
	if n < 1 {
		n = 1
	}
	if fpRate <= 0 || fpRate >= 1 {
		fpRate = 0.01
	}
	m := uint64(math.Ceil(-float64(n) * math.Log(fpRate) / (math.Ln2 * math.Ln2)))
	if m < 64 {
		m = 64
	}
	k := uint64(math.Round(float64(m) / float64(n) * math.Ln2))
	if k < 1 {
		k = 1
	}
	if k > 30 {
		k = 30
	}
	return &Bloom{bits: make([]atomic.Uint64, (m+63)/64), m: m, k: k}
}

// probes derives the k bit positions via Kirsch–Mitzenmacher double hashing
// (one xxhash, two derived hashes) — cheap and well-distributed.
func (b *Bloom) hashes(key []byte) (uint64, uint64) {
	h1 := xxhash.Sum64(key)
	h2 := (h1 >> 32) | 1 // odd, non-zero
	return h1, h2
}

// Add records a key. Safe for concurrent use.
func (b *Bloom) Add(key []byte) {
	if b == nil {
		return
	}
	h1, h2 := b.hashes(key)
	for i := uint64(0); i < b.k; i++ {
		bit := (h1 + i*h2) % b.m
		b.bits[bit>>6].Or(1 << (bit & 63))
	}
}

// MightContain reports false only if the key was definitely never Added (no false
// negatives); true means "possibly present" — the caller must still consult Pebble.
func (b *Bloom) MightContain(key []byte) bool {
	if b == nil {
		return true // no filter => can't rule anything out
	}
	h1, h2 := b.hashes(key)
	for i := uint64(0); i < b.k; i++ {
		bit := (h1 + i*h2) % b.m
		if b.bits[bit>>6].Load()&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}

// Bits returns the filter size in bits (for sizing/telemetry).
func (b *Bloom) Bits() uint64 { return b.m }

// --- BlockedBloom: cache-line-local variant ---------------------------------
//
// A standard Bloom scatters its k bits across the whole array, so a lookup can
// touch k different cache lines. BlockedBloom hashes each key to ONE 64-byte
// block (8 uint64s = 512 bits = one cache line) and sets/tests all k bits inside
// it — one cache miss per op. Slightly higher false-positive rate for the same
// memory, but materially fewer cache misses on a large filter.
type BlockedBloom struct {
	blocks []bloomBlock
	n      uint64 // number of blocks
	k      uint64
}

type bloomBlock = [8]atomic.Uint64 // 512 bits, one cache line

func NewBlockedBloom(n uint64, fpRate float64) *BlockedBloom {
	if n < 1 {
		n = 1
	}
	if fpRate <= 0 || fpRate >= 1 {
		fpRate = 0.01
	}
	mBits := uint64(math.Ceil(-float64(n) * math.Log(fpRate) / (math.Ln2 * math.Ln2)))
	nBlocks := (mBits + 511) / 512
	if nBlocks < 1 {
		nBlocks = 1
	}
	k := uint64(math.Round(float64(512*nBlocks) / float64(n) * math.Ln2))
	if k < 1 {
		k = 1
	}
	if k > 16 {
		k = 16
	}
	return &BlockedBloom{blocks: make([]bloomBlock, nBlocks), n: nBlocks, k: k}
}

func (b *BlockedBloom) Add(key []byte) {
	if b == nil {
		return
	}
	h1 := xxhash.Sum64(key)
	blk := &b.blocks[h1%b.n]
	h2 := (h1 >> 32) | 1
	h3 := (h2*0x9e3779b97f4a7c15)>>32 | 1 // independent stride per key
	for i := uint64(0); i < b.k; i++ {
		bit := (h2 + i*h3) & 511
		blk[bit>>6].Or(1 << (bit & 63))
	}
}

func (b *BlockedBloom) MightContain(key []byte) bool {
	if b == nil {
		return true
	}
	h1 := xxhash.Sum64(key)
	blk := &b.blocks[h1%b.n]
	h2 := (h1 >> 32) | 1
	h3 := (h2*0x9e3779b97f4a7c15)>>32 | 1 // independent stride per key
	for i := uint64(0); i < b.k; i++ {
		bit := (h2 + i*h3) & 511
		if blk[bit>>6].Load()&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}

func (b *BlockedBloom) Bits() uint64 { return b.n * 512 }
