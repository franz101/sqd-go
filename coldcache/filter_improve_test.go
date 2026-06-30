package coldcache

import (
	"crypto/rand"
	"testing"
)

// makeKeys returns n random 52-byte keys (the cold-tier composite user+tokenID
// key width).
func makeKeys(n int) [][]byte {
	ks := make([][]byte, n)
	for i := range ks {
		ks[i] = make([]byte, 52)
		_, _ = rand.Read(ks[i])
	}
	return ks
}

func measureFPR(add func([]byte), test func([]byte) bool, inserted, probes [][]byte) float64 {
	for _, k := range inserted {
		add(k)
	}
	fp := 0
	for _, k := range probes {
		if test(k) {
			fp++
		}
	}
	return float64(fp) / float64(len(probes))
}

// TestFilterFPRComparison is the headline cache-improvement proof: the production
// SplitBloom vs the legacy double-hash filter at the same memory and load. Each
// avoided false positive is a skipped Pebble (~8us) / ClickHouse (~20ms) lookup.
func TestFilterFPRComparison(t *testing.T) {
	const probes = 1 << 20
	const nBlocks = 256
	bitBudget := uint64(nBlocks * blockBits)
	probeKeys := makeKeys(probes)

	for _, perBlock := range []int{6, 10, 16} {
		inserted := makeKeys(perBlock * nBlocks)

		legacy := &BloomFilter{blocks: make([]negBlock, nBlocks), blockMask: nBlocks - 1, k: 8}
		fprLegacy := measureFPR(legacy.add, legacy.mayContain, inserted, probeKeys)

		split := newSplitBloom(bitBudget, false)
		fprSplit := measureFPR(split.add, split.mayContain, inserted, probeKeys)

		ratio := 0.0
		if fprSplit > 0 {
			ratio = fprLegacy / fprSplit
		}
		t.Logf("~%2d keys/block: legacy double-hash FPR=%.4f%%  SplitBloom=%.4f%%  (%.0fx better)",
			perBlock, fprLegacy*100, fprSplit*100, ratio)
		if fprSplit > fprLegacy {
			t.Errorf("SplitBloom FPR (%.4f%%) worse than legacy (%.4f%%) at %d keys/block", fprSplit*100, fprLegacy*100, perBlock)
		}
	}
}

// --- end-to-end add / mayContain (INCLUDES hashing) ---

func benchFilterEndToEnd(b *testing.B, name string, add func([]byte), test func([]byte) bool, hits, miss [][]byte) {
	b.Run(name+"/add", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			add(hits[i%len(hits)])
		}
	})
	for _, k := range hits {
		add(k)
	}
	b.Run(name+"/mayContain_hit", func(b *testing.B) {
		var x bool
		for i := 0; i < b.N; i++ {
			x = test(hits[i%len(hits)])
		}
		_ = x
	})
	b.Run(name+"/mayContain_miss", func(b *testing.B) {
		var x bool
		for i := 0; i < b.N; i++ {
			x = test(miss[i%len(miss)])
		}
		_ = x
	})
}

func BenchmarkFilterEndToEnd(b *testing.B) {
	const budget = 1 << 20
	const nBlocks = budget / blockBits
	keys := makeKeys(20000)
	hits := keys[:len(keys)/2]
	miss := keys[len(keys)/2:]

	legacy := &AtomicBloom{blocks: make([]negBlock, nBlocks), blockMask: nBlocks - 1, k: 8}
	benchFilterEndToEnd(b, "legacy_double_hash", legacy.add, legacy.mayContain, hits, miss)

	split := newSplitBloom(budget, true)
	benchFilterEndToEnd(b, "split_block", split.add, split.mayContain, hits, miss)
}

// TestSplitNoFalseNegatives pins the cold-tier correctness contract for the
// production filter: a key that was added must always report present (a false
// negative would let the authoritative gate drop a real position).
func TestSplitNoFalseNegatives(t *testing.T) {
	keys := makeKeys(50000)
	for _, atomicOps := range []bool{false, true} {
		f := newSplitBloom(1<<20, atomicOps)
		for _, k := range keys {
			f.add(k)
		}
		for _, k := range keys {
			if !f.mayContain(k) {
				t.Fatalf("atomic=%v: FALSE NEGATIVE on an added key — cold-tier correctness violated", atomicOps)
			}
		}
	}
}
