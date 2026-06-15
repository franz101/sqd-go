package coldcache

import (
	"encoding/binary"
	"testing"
)

// TestBloomNoFalseNegatives is the safety-critical invariant: a key that was Added
// must NEVER report absent. A false negative would make the cold reader treat an
// existing position as new and corrupt indexed state.
func TestBloomNoFalseNegatives(t *testing.T) {
	const n = 200000
	bf := NewBloom(n, 0.01)
	key := make([]byte, 52)
	mk := func(i int) {
		binary.LittleEndian.PutUint64(key, uint64(i)*0x9e3779b97f4a7c15)
		binary.LittleEndian.PutUint64(key[32:], uint64(i)+1)
	}
	for i := 0; i < n; i++ {
		mk(i)
		bf.Add(key)
	}
	for i := 0; i < n; i++ {
		mk(i)
		if !bf.MightContain(key) {
			t.Fatalf("FALSE NEGATIVE at key %d — would corrupt state", i)
		}
	}
	fp, trials := 0, 100000
	for i := n; i < n+trials; i++ {
		mk(i)
		if bf.MightContain(key) {
			fp++
		}
	}
	rate := float64(fp) / float64(trials)
	if rate > 0.03 {
		t.Fatalf("false-positive rate %.3f too high (want ~0.01)", rate)
	}
	t.Logf("0 false negatives over %d keys; new-key false-positive = %.2f%%", n, rate*100)
}

func TestBloomNilSafe(t *testing.T) {
	var bf *Bloom
	bf.Add([]byte("x")) // must not panic
	if !bf.MightContain([]byte("x")) {
		t.Fatal("nil bloom must report MightContain=true (can't rule out)")
	}
	var s *Store
	if !s.MightContain([]byte("x")) {
		t.Fatal("nil store MightContain must be true")
	}
}

func TestBlockedBloomNoFalseNegatives(t *testing.T) {
	const n = 200000
	bf := NewBlockedBloom(n, 0.01)
	key := make([]byte, 52)
	mk := func(i int) {
		key[0] = byte(i)
		key[1] = byte(i >> 8)
		key[2] = byte(i >> 16)
		key[32] = byte(i)
		key[40] = byte(i >> 8)
	}
	for i := 0; i < n; i++ {
		mk(i)
		bf.Add(key)
	}
	for i := 0; i < n; i++ {
		mk(i)
		if !bf.MightContain(key) {
			t.Fatalf("blocked bloom FALSE NEGATIVE at %d", i)
		}
	}
}
