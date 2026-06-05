package locklessgenericringbuffer

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- 1. EXPONENTIAL BACKOFF RING BUFFER ---

type Backoff struct {
	attempts int
}

func (b *Backoff) Step() {
	b.attempts++
	if b.attempts < 4 {
		// Busy-loop active spin for very short-term stalls
		for i := 0; i < b.attempts*20; i++ {
			// CPU relax equivalent or compiler-barrier-like activity
		}
	} else if b.attempts < 12 {
		// Coarse-grained yield via scheduler
		runtime.Gosched()
	} else {
		// Fully yield OS thread to prevent CPU starvation and thermal throttling
		time.Sleep(time.Microsecond)
	}
}

func (b *Backoff) Reset() {
	b.attempts = 0
}

type BackoffRingBuffer[T any] struct {
	length            uint32
	bitWiseLength     uint32
	headIndex         uint32
	nextReaderIndex   uint32
	maxReaders        int
	buffer            []T
	readerIndexes     []uint32
	readerActiveFlags []uint32
}

type BackoffConsumer[T any] struct {
	ring *BackoffRingBuffer[T]
	id   uint32
}

func CreateBackoffBuffer[T any](size uint32, maxReaders uint32) (*BackoffRingBuffer[T], error) {
	if size&(size-1) != 0 {
		return nil, InvalidBufferSize
	}

	return &BackoffRingBuffer[T]{
		buffer:            make([]T, size),
		length:            size,
		bitWiseLength:     size - 1,
		headIndex:         0,
		nextReaderIndex:   0,
		maxReaders:        int(maxReaders),
		readerIndexes:     make([]uint32, maxReaders),
		readerActiveFlags: make([]uint32, maxReaders),
	}, nil
}

func (buffer *BackoffRingBuffer[T]) CreateConsumer() (BackoffConsumer[T], error) {
	for readerIndex := range buffer.readerActiveFlags {
		if atomic.CompareAndSwapUint32(&buffer.readerActiveFlags[readerIndex], 0, 2) {
			buffer.readerIndexes[readerIndex] = atomic.LoadUint32(&buffer.headIndex)
			atomic.StoreUint32(&buffer.readerActiveFlags[readerIndex], 1)
			atomic.CompareAndSwapUint32(&buffer.nextReaderIndex, uint32(readerIndex), uint32(readerIndex)+1)

			return BackoffConsumer[T]{
				id:   uint32(readerIndex),
				ring: buffer,
			}, nil
		}
	}
	return BackoffConsumer[T]{}, MaxConsumerError
}

func (buffer *BackoffRingBuffer[T]) Write(value T) {
	var offset uint32
	var i uint32
	var backoff Backoff

attemptWrite:
	nextReaderIndex := atomic.LoadUint32(&buffer.nextReaderIndex)

	for i = 0; i < nextReaderIndex; i++ {
		if atomic.LoadUint32(&buffer.readerActiveFlags[i]) == 1 {
			offset = atomic.LoadUint32(&buffer.readerIndexes[i]) + buffer.length
			if offset == buffer.headIndex {
				backoff.Step()
				goto attemptWrite
			}
		}
	}

	nextIndex := buffer.headIndex + 1
	buffer.buffer[nextIndex&buffer.bitWiseLength] = value
	atomic.StoreUint32(&buffer.headIndex, nextIndex)
}

func (consumer *BackoffConsumer[T]) Get() T {
	ring := consumer.ring
	readerIndex := consumer.id
	newIndex := ring.readerIndexes[readerIndex] + 1
	var backoff Backoff

	for newIndex > atomic.LoadUint32(&ring.headIndex) {
		backoff.Step()
	}

	value := ring.buffer[newIndex&ring.bitWiseLength]
	atomic.AddUint32(&ring.readerIndexes[readerIndex], 1)
	return value
}

// --- 2. CONDITION VARIABLE (SYNC.COND) RING BUFFER ---

type CondRingBuffer[T any] struct {
	mu                sync.Mutex
	cond              *sync.Cond
	length            uint32
	bitWiseLength     uint32
	headIndex         uint32
	nextReaderIndex   uint32
	maxReaders        int
	buffer            []T
	readerIndexes     []uint32
	readerActiveFlags []uint32
}

type CondConsumer[T any] struct {
	ring *CondRingBuffer[T]
	id   uint32
}

func CreateCondBuffer[T any](size uint32, maxReaders uint32) (*CondRingBuffer[T], error) {
	if size&(size-1) != 0 {
		return nil, InvalidBufferSize
	}

	rb := &CondRingBuffer[T]{
		buffer:            make([]T, size),
		length:            size,
		bitWiseLength:     size - 1,
		headIndex:         0,
		nextReaderIndex:   0,
		maxReaders:        int(maxReaders),
		readerIndexes:     make([]uint32, maxReaders),
		readerActiveFlags: make([]uint32, maxReaders),
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb, nil
}

func (buffer *CondRingBuffer[T]) CreateConsumer() (CondConsumer[T], error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	for readerIndex := range buffer.readerActiveFlags {
		if buffer.readerActiveFlags[readerIndex] == 0 {
			buffer.readerIndexes[readerIndex] = buffer.headIndex
			buffer.readerActiveFlags[readerIndex] = 1
			if uint32(readerIndex) >= buffer.nextReaderIndex {
				buffer.nextReaderIndex = uint32(readerIndex) + 1
			}
			return CondConsumer[T]{
				id:   uint32(readerIndex),
				ring: buffer,
			}, nil
		}
	}
	return CondConsumer[T]{}, MaxConsumerError
}

func (buffer *CondRingBuffer[T]) Write(value T) {
	buffer.mu.Lock()

	// Wait until buffer has space
	for {
		hasSpace := true
		for i := uint32(0); i < buffer.nextReaderIndex; i++ {
			if buffer.readerActiveFlags[i] == 1 {
				offset := buffer.readerIndexes[i] + buffer.length
				if offset == buffer.headIndex {
					hasSpace = false
					break
				}
			}
		}
		if hasSpace {
			break
		}
		buffer.cond.Wait()
	}

	nextIndex := buffer.headIndex + 1
	buffer.buffer[nextIndex&buffer.bitWiseLength] = value
	buffer.headIndex = nextIndex

	buffer.cond.Broadcast()
	buffer.mu.Unlock()
}

func (consumer *CondConsumer[T]) Get() T {
	ring := consumer.ring
	readerIndex := consumer.id

	ring.mu.Lock()
	newIndex := ring.readerIndexes[readerIndex] + 1

	for newIndex > ring.headIndex {
		ring.cond.Wait()
	}

	value := ring.buffer[newIndex&ring.bitWiseLength]
	ring.readerIndexes[readerIndex] = newIndex

	ring.cond.Broadcast()
	ring.mu.Unlock()
	return value
}

// --- 3. COMPARATIVE BENCHMARKS ---

// High contention: buffer size of 64, making writes and reads block frequently.

func BenchmarkHighContention_Gosched(b *testing.B) {
	rb, _ := CreateBuffer[int](64, 2)
	c, _ := rb.CreateConsumer()

	var wg sync.WaitGroup
	wg.Add(2)

	b.ResetTimer()

	// Consumer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			_ = c.Get()
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			rb.Write(i)
		}
	}()

	wg.Wait()
}

func BenchmarkHighContention_Backoff(b *testing.B) {
	rb, _ := CreateBackoffBuffer[int](64, 2)
	c, _ := rb.CreateConsumer()

	var wg sync.WaitGroup
	wg.Add(2)

	b.ResetTimer()

	// Consumer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			_ = c.Get()
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			rb.Write(i)
		}
	}()

	wg.Wait()
}

func BenchmarkHighContention_CondVar(b *testing.B) {
	rb, _ := CreateCondBuffer[int](64, 2)
	c, _ := rb.CreateConsumer()

	var wg sync.WaitGroup
	wg.Add(2)

	b.ResetTimer()

	// Consumer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			_ = c.Get()
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			rb.Write(i)
		}
	}()

	wg.Wait()
}

func BenchmarkHighContention_Channel(b *testing.B) {
	ch := make(chan int, 64)

	var wg sync.WaitGroup
	wg.Add(2)

	b.ResetTimer()

	// Consumer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			_ = <-ch
		}
	}()

	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			ch <- i
		}
	}()

	wg.Wait()
}
