package parser

import (
	"sync"
)

// Arena is a bump allocator for variable-length data during parsing.
// It eliminates per-row allocations for dynamic arrays (e.g., topics[], data[]).
type Arena struct {
	mu     sync.Mutex
	chunks [][]byte
	offset int
	// Chunk size for new allocations
	chunkSize int
}

const (
	defaultChunkSize = 1 << 20 // 1MB chunks
)

// NewArena creates a new arena with the default chunk size.
func NewArena() *Arena {
	return &Arena{
		chunks:    make([][]byte, 0, 4),
		chunkSize: defaultChunkSize,
	}
}

// Allocate allocates n bytes from the arena.
// The returned slice is valid until Reset() is called.
func (a *Arena) Allocate(n int) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Find or create a chunk with enough space
	for {
		// Check if we have a chunk with enough space
		if len(a.chunks) > 0 {
			currentChunk := a.chunks[len(a.chunks)-1]
			if a.offset+n <= len(currentChunk) {
				slice := currentChunk[a.offset : a.offset+n]
				a.offset += n
				return slice
			}
		}

		// Need a new chunk
		chunkSize := a.chunkSize
		if n > chunkSize {
			chunkSize = n
		}
		newChunk := make([]byte, chunkSize)
		a.chunks = append(a.chunks, newChunk)
		a.offset = 0
		// Loop will use the new chunk
	}
}

// AllocateString allocates space for a string and copies it into the arena.
// Returns a byte slice pointing to the arena-allocated data.
func (a *Arena) AllocateString(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	buf := a.Allocate(len(s))
	copy(buf, s)
	return buf
}

// AllocateCopy allocates space and copies from src into the arena.
func (a *Arena) AllocateCopy(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	buf := a.Allocate(len(src))
	copy(buf, src)
	return buf
}

// Reset clears the arena, keeping the first chunk for reuse.
// All previously allocated slices become invalid.
func (a *Arena) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.chunks) == 0 {
		return
	}

	// Keep the first chunk, discard the rest
	if len(a.chunks) > 1 {
		a.chunks = a.chunks[:1]
	}
	a.offset = 0
}

// Size returns the total allocated bytes across all chunks.
func (a *Arena) Size() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	total := 0
	for _, chunk := range a.chunks {
		total += len(chunk)
	}
	return total
}

// Used returns the number of bytes currently allocated in the active chunk.
func (a *Arena) Used() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.offset
}

// GrowTo ensures the arena has at least n bytes of capacity in the current chunk.
// This is useful for pre-allocating when you know the expected size.
func (a *Arena) GrowTo(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.chunks) == 0 {
		a.chunks = append(a.chunks, make([]byte, max(n, a.chunkSize)))
		return
	}

	currentChunk := a.chunks[len(a.chunks)-1]
	if len(currentChunk) < n {
		newChunk := make([]byte, max(n, a.chunkSize))
		a.chunks = append(a.chunks, newChunk)
		a.offset = 0
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ─── Thread-unsafe Arena (for single-threaded parsing) ───

// ArenaUnsafe is a non-thread-safe version of Arena for use in single-threaded parsing.
// It avoids the overhead of mutex locks during parsing.
type ArenaUnsafe struct {
	chunks    [][]byte
	offset    int
	chunkSize int
}

// NewArenaUnsafe creates a new thread-unsafe arena.
func NewArenaUnsafe() *ArenaUnsafe {
	return &ArenaUnsafe{
		chunks:    make([][]byte, 0, 4),
		chunkSize: defaultChunkSize,
	}
}

// Allocate allocates n bytes from the arena without locking.
func (a *ArenaUnsafe) Allocate(n int) []byte {
	// Check if we have a chunk with enough space
	if len(a.chunks) > 0 {
		currentChunk := a.chunks[len(a.chunks)-1]
		if a.offset+n <= len(currentChunk) {
			slice := currentChunk[a.offset : a.offset+n]
			a.offset += n
			return slice
		}
	}

	// Need a new chunk
	chunkSize := a.chunkSize
	if n > chunkSize {
		chunkSize = n
	}
	newChunk := make([]byte, chunkSize)
	a.chunks = append(a.chunks, newChunk)
	a.offset = n
	return newChunk[:n]
}

// AllocateString allocates and copies a string into the arena.
func (a *ArenaUnsafe) AllocateString(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	buf := a.Allocate(len(s))
	copy(buf, s)
	return buf
}

// Reset clears the arena, keeping the first chunk.
func (a *ArenaUnsafe) Reset() {
	if len(a.chunks) == 0 {
		return
	}
	if len(a.chunks) > 1 {
		a.chunks = a.chunks[:1]
	}
	a.offset = 0
}

// GrowTo ensures the arena has at least n bytes of capacity in the current chunk.
func (a *ArenaUnsafe) GrowTo(n int) {
	if len(a.chunks) == 0 {
		a.chunks = append(a.chunks, make([]byte, max(n, a.chunkSize)))
		return
	}

	currentChunk := a.chunks[len(a.chunks)-1]
	if len(currentChunk) < n {
		newChunk := make([]byte, max(n, a.chunkSize))
		a.chunks = append(a.chunks, newChunk)
		a.offset = 0
	}
}
