package coldcache

import "sync/atomic"

// SplitBloom is a split-block (register-blocked) Bloom filter — the
// Impala/Parquet design adapted to a 512-bit cache line. Each key sets exactly
// ONE bit in EACH of the block's 8 words: word j's bit = top 6 bits of
// (h * salt[j]). vs the linear double-hash filter (BloomFilter/AtomicBloom in
// filter.go), which sets 8 bits along an arithmetic progression mod 512:
//
//   - 30-200x LOWER false-positive rate at identical memory (measured,
//     filter_improve_test.go) — the AP bits are clustered/correlated, so an
//     absent key's pattern is far likelier to be fully covered; one-bit-per-word
//     is maximally spread. Each avoided false positive is a skipped Pebble (~8us)
//     or ClickHouse (~20ms) lookup, so this is the dominant win.
//   - ~1.5x faster add (one OR per distinct word, sequential; the double-hash's
//     data-dependent word index can hit the same word twice -> serialized atomics).
//   - branchless mayContain (no early-exit) — equal on the filter-hit path,
//     ~3ns slower on a miss, negligible against the lookups the lower FPR avoids.
//
// It reuses negHash (the fastest hash measured, 5.2ns/52B) and needs only its h
// output, not (h, g). NO false negatives (Bloom invariant): every bit set on add
// is checked on mayContain, preserving the cold-tier contract that a written key
// always reports present (a false negative would let the authoritative gate reset
// a real position to zero).
type SplitBloom struct {
	blocks    []negBlock
	blockMask uint64
	atomicOps bool
}

// splitSalts are 8 distinct odd 64-bit mixing constants (well-known
// fractional-of-irrationals / hash finalizers). Odd => full-period multiplier;
// distinct => the 8 words get independent in-word bit positions.
var splitSalts = [blockWords]uint64{
	0x9E3779B97F4A7C15, 0xC2B2AE3D27D4EB4F, 0x165667B19E3779F9, 0xD6E8FEB86659FD93,
	0xA0761D6478BD642F, 0xE7037ED1A0B428DB, 0x8EBC6AF09C88C6E3, 0x589965CB6F1F2C9B,
}

func newSplitBloom(bitBudget uint64, atomicOps bool) *SplitBloom {
	n := splitBlockCount(bitBudget)
	return &SplitBloom{blocks: make([]negBlock, n), blockMask: n - 1, atomicOps: atomicOps}
}

func splitBlockCount(bitBudget uint64) uint64 {
	nb := bitBudget / blockBits
	const minBlocks = 64
	n := uint64(minBlocks)
	for n < nb {
		n <<= 1
	}
	return n
}

func (f *SplitBloom) add(key []byte) {
	h, _ := negHash(key)
	blk := &f.blocks[h&f.blockMask]
	if f.atomicOps {
		for j := 0; j < blockWords; j++ {
			atomic.OrUint64(&blk[j], 1<<((h*splitSalts[j])>>58))
		}
		return
	}
	for j := 0; j < blockWords; j++ {
		blk[j] |= 1 << ((h * splitSalts[j]) >> 58)
	}
}

func (f *SplitBloom) mayContain(key []byte) bool {
	h, _ := negHash(key)
	blk := &f.blocks[h&f.blockMask]
	var miss uint64
	if f.atomicOps {
		for j := 0; j < blockWords; j++ {
			miss |= (uint64(1) << ((h * splitSalts[j]) >> 58)) &^ atomic.LoadUint64(&blk[j])
		}
		return miss == 0
	}
	for j := 0; j < blockWords; j++ {
		miss |= (uint64(1) << ((h * splitSalts[j]) >> 58)) &^ blk[j]
	}
	return miss == 0
}
