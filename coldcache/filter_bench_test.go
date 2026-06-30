package coldcache

import (
	"crypto/rand"
	"sync/atomic"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/greatroar/blobloom"
	boom "github.com/tylertreat/boomfilters"
)

// Isolated lookup helper functions for negFilter
func mayContainPrecomputed(f *BloomFilter, h, g uint64) bool {
	blk := &f.blocks[h&f.blockMask]
	for i := uint(0); i < f.k; i++ {
		bit := (h>>9 + uint64(i)*g) & (blockBits - 1)
		if blk[bit>>6]&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}

func mayContainPrecomputedAtomic(f *AtomicBloom, h, g uint64) bool {
	blk := &f.blocks[h&f.blockMask]
	for i := uint(0); i < f.k; i++ {
		bit := (h>>9 + uint64(i)*g) & (blockBits - 1)
		if atomic.LoadUint64(&blk[bit>>6])&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}

type keyHashes struct {
	key  []byte
	hFnv uint64
	gFnv uint64
	hXx  uint64
}

func prepareBenchmarkData(numKeys int) ([]keyHashes, []keyHashes) {
	keys := make([][]byte, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = make([]byte, 52)
		_, _ = rand.Read(keys[i])
	}

	half := numKeys / 2
	hits := make([]keyHashes, half)
	for i := 0; i < half; i++ {
		h, g := negHash(keys[i])
		hits[i] = keyHashes{
			key:  keys[i],
			hFnv: h,
			gFnv: g,
			hXx:  xxhash.Sum64(keys[i]),
		}
	}

	misses := make([]keyHashes, half)
	for i := 0; i < half; i++ {
		h, g := negHash(keys[half+i])
		misses[i] = keyHashes{
			key:  keys[half+i],
			hFnv: h,
			gFnv: g,
			hXx:  xxhash.Sum64(keys[half+i]),
		}
	}

	return hits, misses
}

func BenchmarkFilterQueries(b *testing.B) {
	const numKeys = 20000
	const budget = 1 << 20 // 1M bits (128 KB)

	hits, misses := prepareBenchmarkData(numKeys)

	// Initialize our BloomFilter & AtomicBloom
	// 1M bits / 512 bits per block = 2048 blocks (already power of 2)
	filterBloom := &BloomFilter{
		blocks:    make([]negBlock, 2048),
		blockMask: 2047,
		k:         8,
	}
	filterAtomic := &AtomicBloom{
		blocks:    make([]negBlock, 2048),
		blockMask: 2047,
		k:         8,
	}

	// Initialize blobloom
	bbFilter := blobloom.New(budget, 8)
	bbSync := blobloom.NewSync(budget, 8)

	// Initialize boomfilters (optimized for numKeys/2 items with ~0.02 fpRate)
	// Let's use the standard constructors
	boomBloom := boom.NewBloomFilter(numKeys/2, 0.02)
	boomCuckoo := boom.NewCuckooFilter(numKeys/2, 0.02)
	boomScalable := boom.NewScalableBloomFilter(numKeys/2, 0.02, 0.5)
	boomStable := boom.NewStableBloomFilter(numKeys/2, 1, 0.02)

	// Populate filters
	for _, h := range hits {
		filterBloom.add(h.key)
		filterAtomic.add(h.key)
		bbFilter.Add(h.hXx)
		bbSync.Add(h.hXx)
		boomBloom.Add(h.key)
		boomCuckoo.Add(h.key)
		boomScalable.Add(h.key)
		boomStable.Add(h.key)
	}

	// Helper function for sub-benchmarks
	runBench := func(b *testing.B, name string, data []keyHashes, queryFn func(item keyHashes) bool) {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Access in a loop
				item := data[i%len(data)]
				_ = queryFn(item)
			}
		})
	}

	// Standard Queries (includes Hashing)
	b.Log("=== STANDARD QUERIES (Includes Hashing) ===")

	runBench(b, "Standard_BloomFilter_Hit", hits, func(item keyHashes) bool {
		return filterBloom.mayContain(item.key)
	})
	runBench(b, "Standard_BloomFilter_Miss", misses, func(item keyHashes) bool {
		return filterBloom.mayContain(item.key)
	})

	runBench(b, "Standard_AtomicBloom_Hit", hits, func(item keyHashes) bool {
		return filterAtomic.mayContain(item.key)
	})
	runBench(b, "Standard_AtomicBloom_Miss", misses, func(item keyHashes) bool {
		return filterAtomic.mayContain(item.key)
	})

	runBench(b, "Standard_BlobloomFilter_Hit", hits, func(item keyHashes) bool {
		return bbFilter.Has(xxhash.Sum64(item.key))
	})
	runBench(b, "Standard_BlobloomFilter_Miss", misses, func(item keyHashes) bool {
		return bbFilter.Has(xxhash.Sum64(item.key))
	})

	runBench(b, "Standard_BlobloomSyncFilter_Hit", hits, func(item keyHashes) bool {
		return bbSync.Has(xxhash.Sum64(item.key))
	})
	runBench(b, "Standard_BlobloomSyncFilter_Miss", misses, func(item keyHashes) bool {
		return bbSync.Has(xxhash.Sum64(item.key))
	})

	runBench(b, "Standard_BoomBloom_Hit", hits, func(item keyHashes) bool {
		return boomBloom.Test(item.key)
	})
	runBench(b, "Standard_BoomBloom_Miss", misses, func(item keyHashes) bool {
		return boomBloom.Test(item.key)
	})

	runBench(b, "Standard_BoomCuckoo_Hit", hits, func(item keyHashes) bool {
		return boomCuckoo.Test(item.key)
	})
	runBench(b, "Standard_BoomCuckoo_Miss", misses, func(item keyHashes) bool {
		return boomCuckoo.Test(item.key)
	})

	runBench(b, "Standard_BoomScalable_Hit", hits, func(item keyHashes) bool {
		return boomScalable.Test(item.key)
	})
	runBench(b, "Standard_BoomScalable_Miss", misses, func(item keyHashes) bool {
		return boomScalable.Test(item.key)
	})

	runBench(b, "Standard_BoomStable_Hit", hits, func(item keyHashes) bool {
		return boomStable.Test(item.key)
	})
	runBench(b, "Standard_BoomStable_Miss", misses, func(item keyHashes) bool {
		return boomStable.Test(item.key)
	})

	// Isolated Queries (Pre-computed Hashes, Hashing excluded)
	b.Log("=== ISOLATED QUERIES (Pre-computed Hashes) ===")

	runBench(b, "Isolated_BloomFilter_Hit", hits, func(item keyHashes) bool {
		return mayContainPrecomputed(filterBloom, item.hFnv, item.gFnv)
	})
	runBench(b, "Isolated_BloomFilter_Miss", misses, func(item keyHashes) bool {
		return mayContainPrecomputed(filterBloom, item.hFnv, item.gFnv)
	})

	runBench(b, "Isolated_AtomicBloom_Hit", hits, func(item keyHashes) bool {
		return mayContainPrecomputedAtomic(filterAtomic, item.hFnv, item.gFnv)
	})
	runBench(b, "Isolated_AtomicBloom_Miss", misses, func(item keyHashes) bool {
		return mayContainPrecomputedAtomic(filterAtomic, item.hFnv, item.gFnv)
	})

	runBench(b, "Isolated_BlobloomFilter_Hit", hits, func(item keyHashes) bool {
		return bbFilter.Has(item.hXx)
	})
	runBench(b, "Isolated_BlobloomFilter_Miss", misses, func(item keyHashes) bool {
		return bbFilter.Has(item.hXx)
	})

	runBench(b, "Isolated_BlobloomSyncFilter_Hit", hits, func(item keyHashes) bool {
		return bbSync.Has(item.hXx)
	})
	runBench(b, "Isolated_BlobloomSyncFilter_Miss", misses, func(item keyHashes) bool {
		return bbSync.Has(item.hXx)
	})
}
