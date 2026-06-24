package coldcache

import (
	"testing"
)

// TestGetIntoMatchesGet proves GetInto returns the same bytes as Get for a
// pointer-free value, decoding straight into the destination struct. This is the
// path the generated clock-cache cold fallback uses (finding C6).
func TestGetIntoMatchesGet(t *testing.T) {
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
		key.User[i] = byte(i + 3)
	}
	for i := range key.TokenID {
		key.TokenID[i] = byte(0x10 + i)
	}
	want := posLike{
		Amount:         [4]uint64{9, 8, 7, 6},
		AvgPrice:       [4]uint64{5, 0, 0, 0},
		RealizedPnL:    [4]uint64{0xfeed, 0, 0, 0},
		UpdatedAtBlock: 42,
		BlockNumber:    42,
		TxIndex:        3,
		LogIndex:       9,
	}
	copy(want.User[:], key.User[:])
	copy(want.TokenID[:], key.TokenID[:])

	// miss before put
	var miss posLike
	if found, err := s.GetInto(bytesOf(&miss), bytesOf(&key)); err != nil || found {
		t.Fatalf("expected GetInto miss, found=%v err=%v", found, err)
	}

	if err := s.Put(bytesOf(&key), bytesOf(&want)); err != nil {
		t.Fatalf("put: %v", err)
	}

	var got posLike
	found, err := s.GetInto(bytesOf(&got), bytesOf(&key))
	if err != nil || !found {
		t.Fatalf("expected GetInto hit, found=%v err=%v", found, err)
	}
	if got != want {
		t.Fatalf("GetInto mismatch:\n got=%+v\nwant=%+v", got, want)
	}

	// GetInto and Get must agree.
	raw, found2, _ := s.Get(bytesOf(&key))
	if !found2 || decode[posLike](raw) != got {
		t.Fatalf("GetInto and Get disagree")
	}
}

// BenchmarkColdHit_Get and BenchmarkColdHit_GetInto are the A/B for finding C6:
// the value-returning Get allocates a fresh []byte per hit (make+copy), whereas
// GetInto copies straight into the caller's struct and allocates nothing on the
// read. The generated cold fallback was switched from Get to GetInto.
func benchColdStore(b *testing.B) (*Store, []byte) {
	b.Helper()
	s, err := Open(b.TempDir()+"/cc", 8<<20, 4<<20)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	key := struct {
		User    [20]byte
		TokenID [32]byte
	}{}
	for i := range key.User {
		key.User[i] = byte(i + 1)
	}
	val := posLike{Amount: [4]uint64{1, 2, 3, 4}, BlockNumber: 99}
	copy(val.User[:], key.User[:])
	if err := s.Put(bytesOf(&key), bytesOf(&val)); err != nil {
		b.Fatalf("put: %v", err)
	}
	// Return a stable copy of the key bytes for the hot loop.
	kb := make([]byte, len(bytesOf(&key)))
	copy(kb, bytesOf(&key))
	return s, kb
}

func BenchmarkColdHit_Get(b *testing.B) {
	s, kb := benchColdStore(b)
	defer s.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, found, _ := s.Get(kb)
		if !found {
			b.Fatal("miss")
		}
		_ = decode[posLike](raw)
	}
}

func BenchmarkColdHit_GetInto(b *testing.B) {
	s, kb := benchColdStore(b)
	defer s.Close()
	var dst posLike
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		found, _ := s.GetInto(bytesOf(&dst), kb)
		if !found {
			b.Fatal("miss")
		}
	}
}
