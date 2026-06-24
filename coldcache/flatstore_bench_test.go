package coldcache

import (
	"testing"
	"unsafe"
)

// flatStore is an experiment: a fixed-slot open-addressing hash table over a flat
// byte buffer, exploiting the cold tier's known access pattern (fixed-size
// pointer-free key -> fixed-size value, point lookups only, no durability — the
// source of truth is ClickHouse). In production it would mmap the buffer so it is
// disk-backed + OS-page-cached; in-memory here it bounds the achievable speedup
// vs Pebble's general-purpose LSM.
type flatStore struct {
	buf      []byte
	capacity uint64 // power of two
	slotSize int
	keyLen   int
	valLen   int
}

const (
	slotEmpty = 0
	slotFull  = 1
)

func newFlatStore(capacityPow2 uint64, keyLen, valLen int) *flatStore {
	slotSize := 1 + keyLen + valLen
	return &flatStore{
		buf:      make([]byte, capacityPow2*uint64(slotSize)),
		capacity: capacityPow2,
		slotSize: slotSize,
		keyLen:   keyLen,
		valLen:   valLen,
	}
}

// keys here are random / keccak-derived, so the low 64 bits are already uniform.
func fhash(key []byte) uint64 {
	return *(*uint64)(unsafe.Pointer(&key[0]))
}

func (s *flatStore) put(key, val []byte) {
	i := fhash(key) & (s.capacity - 1)
	for {
		off := i * uint64(s.slotSize)
		sl := s.buf[off : off+uint64(s.slotSize)]
		if sl[0] == slotEmpty {
			sl[0] = slotFull
			copy(sl[1:1+s.keyLen], key)
			copy(sl[1+s.keyLen:], val)
			return
		}
		if string(sl[1:1+s.keyLen]) == string(key) {
			copy(sl[1+s.keyLen:], val)
			return
		}
		i = (i + 1) & (s.capacity - 1)
	}
}

func (s *flatStore) getInto(dst, key []byte) bool {
	i := fhash(key) & (s.capacity - 1)
	for {
		off := i * uint64(s.slotSize)
		sl := s.buf[off : off+uint64(s.slotSize)]
		if sl[0] == slotEmpty {
			return false
		}
		if string(sl[1:1+s.keyLen]) == string(key) {
			copy(dst, sl[1+s.keyLen:])
			return true
		}
		i = (i + 1) & (s.capacity - 1)
	}
}

// peek returns a zero-copy view into the slot (valid until overwritten).
func (s *flatStore) peek(key []byte) ([]byte, bool) {
	i := fhash(key) & (s.capacity - 1)
	for {
		off := i * uint64(s.slotSize)
		sl := s.buf[off : off+uint64(s.slotSize)]
		if sl[0] == slotEmpty {
			return nil, false
		}
		if string(sl[1:1+s.keyLen]) == string(key) {
			return sl[1+s.keyLen:], true
		}
		i = (i + 1) & (s.capacity - 1)
	}
}

const benchN = 400_000

func makeKV(n int) (keys [][]byte, vals [][]byte) {
	keys = make([][]byte, n)
	vals = make([][]byte, n)
	for i := 0; i < n; i++ {
		var k [keySize]byte
		var v posLike
		// deterministic, well-distributed
		x := uint64(i)*0x9E3779B97F4A7C15 + 0xABCDEF
		for j := 0; j < 8; j++ {
			k[j] = byte(x >> (8 * j))
		}
		k[40] = byte(i)
		v.BlockNumber = uint64(i)
		v.Amount[0] = x
		kb := make([]byte, keySize)
		copy(kb, k[:])
		keys[i] = kb
		vals[i] = append([]byte(nil), bytesOf(&v)...)
	}
	return
}

func BenchmarkColdStore_Pebble_GetInto(b *testing.B) {
	s, err := Open(b.TempDir()+"/cc", 512<<20, 64<<20)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	keys, vals := makeKV(benchN)
	for i := range keys {
		_ = s.Put(keys[i], vals[i])
	}
	var dst posLike
	db := bytesOf(&dst)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if found, _ := s.GetInto(db, keys[i%benchN]); !found {
			b.Fatal("miss")
		}
	}
}

func BenchmarkColdStore_Flat_GetInto(b *testing.B) {
	s := newFlatStore(1<<20, keySize, valueSize) // 1,048,576 slots, ~0.38 load
	keys, vals := makeKV(benchN)
	for i := range keys {
		s.put(keys[i], vals[i])
	}
	var dst posLike
	db := bytesOf(&dst)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !s.getInto(db, keys[i%benchN]) {
			b.Fatal("miss")
		}
	}
}

func BenchmarkColdStore_Flat_Peek(b *testing.B) {
	s := newFlatStore(1<<20, keySize, valueSize)
	keys, vals := makeKV(benchN)
	for i := range keys {
		s.put(keys[i], vals[i])
	}
	var sink byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, ok := s.peek(keys[i%benchN])
		if !ok {
			b.Fatal("miss")
		}
		sink ^= v[0]
	}
	_ = sink
}

func BenchmarkColdStore_Pebble_Put(b *testing.B) {
	s, err := Open(b.TempDir()+"/cc", 512<<20, 64<<20)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	keys, vals := makeKV(benchN)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Put(keys[i%benchN], vals[i%benchN])
	}
}

func BenchmarkColdStore_Flat_Put(b *testing.B) {
	s := newFlatStore(1<<21, keySize, valueSize)
	keys, vals := makeKV(benchN)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.put(keys[i%benchN], vals[i%benchN])
	}
}
