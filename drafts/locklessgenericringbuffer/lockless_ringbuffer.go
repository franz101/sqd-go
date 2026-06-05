package locklessgenericringbuffer

import (
	"errors"
	"runtime"
	"sync/atomic"
)

var (
	MaxConsumerError  = errors.New("max amount of consumers reached cannot create any more")
	InvalidBufferSize = errors.New("buffer must be of size 2^n")
)

type paddedUint32 struct {
	value uint32
	_     [124]byte // Pad to 128 bytes to completely eliminate false sharing on 64B and 128B cache lines.
}

type RingBuffer[T any] struct {
	// Read-only/Read-mostly configuration fields grouped together
	length            uint32
	bitWiseLength     uint32
	maxReaders        int
	buffer            []T

	// 128-byte alignment padding to isolate headIndex
	_                 [128]byte

	// headIndex: heavily modified by producer, read by consumers
	headIndex         uint32

	// 128-byte alignment padding to isolate headIndex from nextReaderIndex
	_                 [128]byte

	// nextReaderIndex: read by producer, modified by consumer registration/deregistration
	nextReaderIndex   uint32

	// 128-byte alignment padding to isolate nextReaderIndex from slice elements
	_                 [128]byte

	// readerIndexes and readerActiveFlags slices themselves are read-mostly struct fields,
	// but their backing array elements are updated concurrently by separate goroutines.
	readerIndexes     []paddedUint32
	readerActiveFlags []paddedUint32
}

type Consumer[T any] struct {
	ring *RingBuffer[T]
	id   uint32
}

func CreateBuffer[T any](size uint32, maxReaders uint32) (*RingBuffer[T], error) {
	if size&(size-1) != 0 {
		return nil, InvalidBufferSize
	}

	return &RingBuffer[T]{
		buffer:            make([]T, size),
		length:            size,
		bitWiseLength:     size - 1,
		headIndex:         0,
		nextReaderIndex:   0,
		maxReaders:        int(maxReaders),
		readerIndexes:     make([]paddedUint32, maxReaders),
		readerActiveFlags: make([]paddedUint32, maxReaders),
	}, nil
}

/*
CreateConsumer

Create a consumer by assigning it the id of the first empty position in the consumerPosition array. Consumer status is tracked
via a flag array with 0 meaning empty, 1 in use and 2 as an intermittent state of being created
*/
func (buffer *RingBuffer[T]) CreateConsumer() (Consumer[T], error) {
	for readerIndex := range buffer.readerActiveFlags {
		if atomic.CompareAndSwapUint32(&buffer.readerActiveFlags[readerIndex].value, 0, 2) {
			// as read state is set to 2, we can afford to non atomically set readIndex, no writer will access it
			buffer.readerIndexes[readerIndex].value = atomic.LoadUint32(&buffer.headIndex)
			atomic.StoreUint32(&buffer.readerActiveFlags[readerIndex].value, 1)

			// case where reader has the current maximum id, and it is needed to be incremented
			atomic.CompareAndSwapUint32(&buffer.nextReaderIndex, uint32(readerIndex), uint32(readerIndex)+1)

			return Consumer[T]{
				id:   uint32(readerIndex),
				ring: buffer,
			}, nil
		}
	}

	return Consumer[T]{}, MaxConsumerError
}

func (buffer *RingBuffer[T]) removeConsumer(readerId uint32) {
	atomic.StoreUint32(&buffer.readerActiveFlags[readerId].value, 0)
	atomic.CompareAndSwapUint32(&buffer.nextReaderIndex, readerId-1, buffer.nextReaderIndex-1)
}

func (consumer *Consumer[T]) Remove() {
	consumer.ring.removeConsumer(consumer.id)
}

func (consumer *Consumer[T]) Get() T {
	return consumer.ring.readIndex(consumer.id)
}

func (buffer *RingBuffer[T]) Write(value T) {
	var offset uint32
	var i uint32
	/*
		We are blocking until at least one space is available in the buffer to attemptWrite.

		As overflow properties of uint32 are utilized to ensure slice index boundaries are adhered to we add the length
		of buffer to current reader's position allowing us to determine the least read reader.

		For example: buffer of size 2

		uint8 head = 1
		uint8 tail = 255
		tail + 2 => 1 with overflow, same as buffer
	*/

attemptWrite:
	nextReaderIndex := atomic.LoadUint32(&buffer.nextReaderIndex)

	for i = 0; i < nextReaderIndex; i++ {
		if atomic.LoadUint32(&buffer.readerActiveFlags[i].value) == 1 {
			offset = atomic.LoadUint32(&buffer.readerIndexes[i].value) + buffer.length

			// only true if the offset between at least one reader and the writer is equal to the size of the buffer
			if offset == buffer.headIndex {
				runtime.Gosched()
				goto attemptWrite
			}
		}
	}

	nextIndex := buffer.headIndex + 1
	buffer.buffer[nextIndex&buffer.bitWiseLength] = value
	atomic.StoreUint32(&buffer.headIndex, nextIndex)
}

func (buffer *RingBuffer[T]) readIndex(readerIndex uint32) T {
	newIndex := buffer.readerIndexes[readerIndex].value + 1
	// yield until work is available
	for newIndex > atomic.LoadUint32(&buffer.headIndex) {
		runtime.Gosched()
	}

	value := buffer.buffer[newIndex&buffer.bitWiseLength]
	atomic.AddUint32(&buffer.readerIndexes[readerIndex].value, 1)
	return value
}
