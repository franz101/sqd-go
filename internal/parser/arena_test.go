package parser

import (
	"testing"
	"unsafe"
)

func TestArenaUnsafe_Allocate(t *testing.T) {
	a := NewArenaUnsafe()

	// Allocate within first chunk
	buf1 := a.Allocate(100)
	if len(buf1) != 100 {
		t.Fatalf("expected 100 bytes, got %d", len(buf1))
	}

	buf2 := a.Allocate(200)
	if len(buf2) != 200 {
		t.Fatalf("expected 200 bytes, got %d", len(buf2))
	}

	// Verify they're in the same chunk and consecutive
	ptr1 := uintptr(unsafe.Pointer(&buf1[0]))
	ptr2 := uintptr(unsafe.Pointer(&buf2[0]))
	if ptr2 != ptr1+100 {
		t.Fatal("buffers should be consecutive in the same chunk")
	}

	// Verify no overlap
	if ptr2 < ptr1+100 {
		t.Fatal("buffers overlap")
	}
}

func TestArenaUnsafe_AllocateString(t *testing.T) {
	a := NewArenaUnsafe()

	s1 := a.AllocateString("hello")
	if string(s1) != "hello" {
		t.Fatalf("expected 'hello', got '%s'", string(s1))
	}

	s2 := a.AllocateString("world")
	if string(s2) != "world" {
		t.Fatalf("expected 'world', got '%s'", string(s2))
	}

	// Verify they're sequential
	if len(s1) != 5 || len(s2) != 5 {
		t.Fatal("unexpected lengths")
	}
}

func TestArenaUnsafe_Reset(t *testing.T) {
	a := NewArenaUnsafe()

	// Allocate some data
	buf1 := a.Allocate(100)
	buf1[0] = 42

	// Reset
	a.Reset()

	// Allocate again - should reuse first chunk
	buf2 := a.Allocate(50)
	if len(buf2) != 50 {
		t.Fatalf("expected 50 bytes, got %d", len(buf2))
	}

	// Verify we're in the same chunk
	if &buf1[0] != &buf2[0] {
		t.Fatal("should reuse first chunk")
	}
}

func TestArenaUnsafe_ChunkOverflow(t *testing.T) {
	a := NewArenaUnsafe()

	// Allocate larger than default chunk size
	large := a.Allocate(defaultChunkSize + 100)
	if len(large) != defaultChunkSize+100 {
		t.Fatalf("expected %d bytes, got %d", defaultChunkSize+100, len(large))
	}

	// Verify we have a new chunk
	if len(a.chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(a.chunks))
	}

	// Allocate more - should create another chunk
	small := a.Allocate(100)
	if len(small) != 100 {
		t.Fatalf("expected 100 bytes, got %d", len(small))
	}

	// Should have 2 chunks now
	if len(a.chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(a.chunks))
	}
}

func TestArenaUnsafe_GrowTo(t *testing.T) {
	a := NewArenaUnsafe()

	// Grow to a specific size
	a.GrowTo(5000)

	// Verify we can allocate without creating new chunks
	buf := a.Allocate(5000)
	if len(buf) != 5000 {
		t.Fatalf("expected 5000 bytes, got %d", len(buf))
	}

	// Should still have 1 chunk
	if len(a.chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(a.chunks))
	}
}

func TestArenaUnsafe_ZeroAlloc(t *testing.T) {
	a := NewArenaUnsafe()

	allocs := testing.AllocsPerRun(10, func() {
		a.Reset()
		for i := 0; i < 100; i++ {
			a.Allocate(100)
		}
	})

	// Reset and chunk reuse should minimize allocations
	// First chunk allocation, then reuses
	if allocs > 2 {
		t.Fatalf("expected <= 2 allocs, got %.2f", allocs)
	}
}

// Benchmark arena allocation vs Go allocation

func BenchmarkArenaAllocate(b *testing.B) {
	a := NewArenaUnsafe()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		a.Reset()
		for j := 0; j < 1000; j++ {
			buf := a.Allocate(100)
			_ = buf
		}
	}
}

func BenchmarkGoAllocate(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			buf := make([]byte, 100)
			_ = buf
		}
	}
}
