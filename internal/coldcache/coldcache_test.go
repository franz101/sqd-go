package coldcache

import (
	"testing"
	"unsafe"
)

// posLike mirrors a pointer-free hot value (generated.MemoryUserPosition): fixed
// arrays + integers + a bool, no slices/strings/pointers. This is the exact shape
// the codegen unsafe codec relies on.
type posLike struct {
	User           [20]byte
	TokenID        [32]byte
	Amount         [4]uint64
	AvgPrice       [4]uint64
	RealizedPnL    [4]uint64
	TotalBought    [4]uint64
	UpdatedAtBlock uint64
	UpdatedAt      int64
	BlockNumber    uint64
	TxIndex        uint64
	LogIndex       uint64
	Tombstone      bool
}

func bytesOf[T any](v *T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v))
}

// decode copies raw bytes into a properly-aligned struct (the codegen pattern;
// never cast a []byte pointer directly — alignment is not guaranteed).
func decode[T any](b []byte) T {
	var out T
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)), b)
	return out
}

func TestStoreRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir()+"/cc", 8<<20, 4<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	key := struct {
		User    [20]byte
		TokenID [32]byte
	}{}
	for i := range key.User {
		key.User[i] = byte(i + 1)
	}
	for i := range key.TokenID {
		key.TokenID[i] = byte(0xa0 + i)
	}

	want := posLike{
		Amount:         [4]uint64{1, 2, 3, 4},
		RealizedPnL:    [4]uint64{0xdeadbeef, 0, 0, 0},
		UpdatedAtBlock: 12345,
		BlockNumber:    12345,
		LogIndex:       7,
	}
	copy(want.User[:], key.User[:])
	copy(want.TokenID[:], key.TokenID[:])

	// miss before put
	if _, found, err := s.Get(bytesOf(&key)); err != nil || found {
		t.Fatalf("expected miss, got found=%v err=%v", found, err)
	}

	if err := s.Put(bytesOf(&key), bytesOf(&want)); err != nil {
		t.Fatalf("put: %v", err)
	}

	raw, found, err := s.Get(bytesOf(&key))
	if err != nil || !found {
		t.Fatalf("expected hit, got found=%v err=%v", found, err)
	}
	got := decode[posLike](raw)
	if got != want {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}

	// tombstone bool survives
	tomb := posLike{Tombstone: true}
	copy(tomb.User[:], key.User[:])
	copy(tomb.TokenID[:], key.TokenID[:])
	if err := s.Put(bytesOf(&key), bytesOf(&tomb)); err != nil {
		t.Fatalf("put tombstone: %v", err)
	}
	raw, found, _ = s.Get(bytesOf(&key))
	if !found || !decode[posLike](raw).Tombstone {
		t.Fatalf("tombstone bool did not round-trip")
	}

	// delete
	if err := s.Delete(bytesOf(&key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := s.Get(bytesOf(&key)); found {
		t.Fatalf("expected miss after delete")
	}
}

func TestNilStoreSafe(t *testing.T) {
	var s *Store
	if err := s.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("nil Put: %v", err)
	}
	if _, found, err := s.Get([]byte("k")); err != nil || found {
		t.Fatalf("nil Get: found=%v err=%v", found, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestDefaultCacheBytesClampedToBounds(t *testing.T) {
	c := defaultCacheBytes()
	if c < MinDefaultCacheBytes || c > MaxDefaultCacheBytes {
		t.Fatalf("defaultCacheBytes()=%d outside [%d, %d]", c, MinDefaultCacheBytes, MaxDefaultCacheBytes)
	}
	// When total RAM is known, the default must be exactly RAM/8 clamped to the
	// bounds — no other value is acceptable.
	if total := totalRAMBytes(); total > 0 {
		want := min(max(total/8, MinDefaultCacheBytes), MaxDefaultCacheBytes)
		if c != want {
			t.Fatalf("defaultCacheBytes()=%d, want clamp(RAM/8)=%d (RAM=%d)", c, want, total)
		}
	}
}

func TestTotalRAMBytesNonNegative(t *testing.T) {
	// Must never report a negative figure; on Linux (this CI) it should be > 0.
	if got := totalRAMBytes(); got < 0 {
		t.Fatalf("totalRAMBytes()=%d, want >= 0", got)
	}
}

func TestColdCacheMBOverride(t *testing.T) {
	// SQD_COLDCACHE_MB must win over the RAM-aware default. We can't read the
	// resolved cap back out of Store, but Open must succeed with the override set
	// and the override path must not panic or misparse.
	t.Setenv("SQD_COLDCACHE_MB", "37")
	s, err := Open(t.TempDir()+"/cc", 0, 0)
	if err != nil {
		t.Fatalf("Open with SQD_COLDCACHE_MB: %v", err)
	}
	defer s.Close()
}
