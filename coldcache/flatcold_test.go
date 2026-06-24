package coldcache

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

// flatcold is an in-RAM, bounded, drop-in alternative to the Pebble cold tier: a
// CLOCK (second-chance) cache over a flat key/value buffer with a chained hash
// index (the same structure the generated hot ring uses), exploiting the cold
// tier's known access pattern — fixed-size pointer-free key->value, point lookups,
// no durability (ClickHouse is the source of truth; an evicted entry just
// re-resolves). These tests are written first and define its contract.

func k8(i uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, i*0x9E3779B97F4A7C15+0x1234)
	return b
}
func v4(i uint64) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(i))
	return b
}

func TestFlatcoldRoundTrip(t *testing.T) {
	f := newFlatcold(64)
	key, val := k8(1), v4(99)
	if _, ok := f.get(key); ok {
		t.Fatal("expected miss before put")
	}
	f.put(key, val)
	got, ok := f.get(key)
	if !ok || !bytes.Equal(got, val) {
		t.Fatalf("get: ok=%v got=%x want=%x", ok, got, val)
	}
	dst := make([]byte, 4)
	if !f.getInto(dst, key) || !bytes.Equal(dst, val) {
		t.Fatalf("getInto mismatch: %x want %x", dst, val)
	}
	if f.len() != 1 {
		t.Fatalf("len=%d want 1", f.len())
	}
}

func TestFlatcoldUpdate(t *testing.T) {
	f := newFlatcold(64)
	f.put(k8(7), v4(1))
	f.put(k8(7), v4(2))
	got, _ := f.get(k8(7))
	if !bytes.Equal(got, v4(2)) {
		t.Fatalf("update: got %x want %x", got, v4(2))
	}
	if f.len() != 1 {
		t.Fatalf("update changed len: %d want 1", f.len())
	}
}

func TestFlatcoldDelete(t *testing.T) {
	f := newFlatcold(64)
	f.put(k8(3), v4(5))
	if !f.del(k8(3)) {
		t.Fatal("delete of present key returned false")
	}
	if _, ok := f.get(k8(3)); ok {
		t.Fatal("get after delete should miss")
	}
	if f.del(k8(3)) {
		t.Fatal("delete of absent key returned true")
	}
	if f.len() != 0 {
		t.Fatalf("len after delete=%d want 0", f.len())
	}
	// Slot is reusable after delete.
	f.put(k8(3), v4(6))
	if got, ok := f.get(k8(3)); !ok || !bytes.Equal(got, v4(6)) {
		t.Fatalf("reinsert after delete failed: ok=%v got=%x", ok, got)
	}
}

func TestFlatcoldMiss(t *testing.T) {
	f := newFlatcold(64)
	f.put(k8(1), v4(1))
	if _, ok := f.get(k8(2)); ok {
		t.Fatal("absent key should miss")
	}
	if f.getInto(make([]byte, 4), k8(2)) {
		t.Fatal("getInto of absent key should be false")
	}
}

// TestFlatcoldBounded: inserting far more distinct keys than capacity must never
// exceed capacity, and a just-inserted key is always immediately retrievable.
func TestFlatcoldBounded(t *testing.T) {
	const cap = 128
	f := newFlatcold(cap)
	for i := uint64(0); i < 10*cap; i++ {
		f.put(k8(i), v4(uint64(i)))
		if f.len() > cap {
			t.Fatalf("len %d exceeded capacity %d at i=%d", f.len(), cap, i)
		}
		if got, ok := f.get(k8(i)); !ok || !bytes.Equal(got, v4(uint64(i))) {
			t.Fatalf("just-inserted key %d not retrievable", i)
		}
	}
	if f.len() != cap {
		t.Fatalf("final len=%d want full %d", f.len(), cap)
	}
}

// TestFlatcoldClockSecondChance: a key referenced (get) before each eviction pass
// survives while unreferenced keys are evicted — the CLOCK second-chance property.
func TestFlatcoldClockSecondChance(t *testing.T) {
	const cap = 64
	f := newFlatcold(cap)
	hot := k8(999999)
	f.put(hot, v4(7))
	// Churn many distinct keys; touch hot before each insert so its referenced bit
	// is set when the CLOCK hand reaches it.
	for i := uint64(0); i < 50*cap; i++ {
		if _, ok := f.get(hot); !ok {
			t.Fatalf("hot key evicted despite being referenced (i=%d)", i)
		}
		f.put(k8(i), v4(uint64(i)))
	}
	if got, ok := f.get(hot); !ok || !bytes.Equal(got, v4(7)) {
		t.Fatalf("hot key lost: ok=%v got=%x", ok, got)
	}
}

// TestFlatcoldCollisions forces many keys through the same hash bucket and checks
// every one round-trips (probe-chain correctness, no eviction).
func TestFlatcoldCollisions(t *testing.T) {
	f := newFlatcold(1024)
	// Keys whose hash collides: vary only high bytes so the low-bits bucket is equal.
	var keys [][]byte
	for i := 0; i < 200; i++ {
		key := make([]byte, 8)
		key[0] = 0x42 // same low byte -> same bucket if bucketing uses low bits
		key[7] = byte(i)
		binary.LittleEndian.PutUint16(key[5:], uint16(i))
		keys = append(keys, key)
		f.put(key, v4(uint64(i)))
	}
	for i, key := range keys {
		if got, ok := f.get(key); !ok || !bytes.Equal(got, v4(uint64(i))) {
			t.Fatalf("collision key %d lost: ok=%v", i, ok)
		}
	}
}

// TestFlatcoldOracle: against a map oracle for a NON-evicting capacity, a random
// mix of put/get/delete must agree exactly.
func TestFlatcoldOracle(t *testing.T) {
	f := newFlatcold(4096)
	oracle := map[string][]byte{}
	r := rand.New(rand.NewSource(42))
	keyspace := uint64(500) // << capacity, so no eviction
	for op := 0; op < 20000; op++ {
		key := k8(uint64(r.Intn(int(keyspace))))
		switch r.Intn(3) {
		case 0, 1:
			val := v4(uint64(r.Uint32()))
			f.put(key, val)
			oracle[string(key)] = append([]byte(nil), val...)
		case 2:
			f.del(key)
			delete(oracle, string(key))
		}
		if op%997 == 0 {
			if f.len() != len(oracle) {
				t.Fatalf("len mismatch at op %d: flat=%d oracle=%d", op, f.len(), len(oracle))
			}
		}
	}
	for ks, want := range oracle {
		got, ok := f.get([]byte(ks))
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("oracle mismatch key=%x: ok=%v got=%x want=%x", ks, ok, got, want)
		}
	}
}

// TestFlatcoldViaStore checks the flag path: Store with the flat backend behaves
// like the Pebble one for the byte KV contract.
func TestFlatcoldViaStore(t *testing.T) {
	t.Setenv("SQD_COLDCACHE_BACKEND", "flat")
	s, err := Open(t.TempDir()+"/cc", 8<<20, 0)
	if err != nil {
		t.Fatalf("open flat store: %v", err)
	}
	defer s.Close()
	key := struct{ A [8]byte }{}
	copy(key.A[:], k8(5))
	val := posLike{BlockNumber: 123, Amount: [4]uint64{1, 2, 3, 4}}

	if _, found, _ := s.Get(bytesOf(&key)); found {
		t.Fatal("miss before put")
	}
	if err := s.Put(bytesOf(&key), bytesOf(&val)); err != nil {
		t.Fatalf("put: %v", err)
	}
	var dst posLike
	if found, _ := s.GetInto(bytesOf(&dst), bytesOf(&key)); !found || dst != val {
		t.Fatalf("getInto via store mismatch: found=%v", found)
	}
	if err := s.Delete(bytesOf(&key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := s.Get(bytesOf(&key)); found {
		t.Fatal("present after delete")
	}
}
