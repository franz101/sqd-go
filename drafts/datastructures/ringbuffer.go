package clock

import (
	"errors"
	"runtime"
	"sync/atomic"
	"time"
)

var (
	MaxConsumerError  = errors.New("max amount of consumers reached cannot create any more")
	InvalidBufferSize = errors.New("buffer must be of size 2^n")
	ErrBufferFull     = errors.New("historic block buffer is full")
)

// --- 1. LOW-LATENCY CACHE-LINE PADDING ---

type paddedUint32 struct {
	value uint32
	_     [124]byte // Pad to 128 bytes to completely eliminate false sharing on 64B and 128B cache lines.
}

// --- 2. GENERIC LOCKLESS RING BUFFER ---

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

func (buffer *RingBuffer[T]) CreateConsumer() (Consumer[T], error) {
	for readerIndex := range buffer.readerActiveFlags {
		if atomic.CompareAndSwapUint32(&buffer.readerActiveFlags[readerIndex].value, 0, 2) {
			buffer.readerIndexes[readerIndex].value = atomic.LoadUint32(&buffer.headIndex)
			atomic.StoreUint32(&buffer.readerActiveFlags[readerIndex].value, 1)
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
	var backoff Backoff

attemptWrite:
	nextReaderIndex := atomic.LoadUint32(&buffer.nextReaderIndex)

	for i = 0; i < nextReaderIndex; i++ {
		if atomic.LoadUint32(&buffer.readerActiveFlags[i].value) == 1 {
			offset = atomic.LoadUint32(&buffer.readerIndexes[i].value) + buffer.length

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

func (buffer *RingBuffer[T]) readIndex(readerIndex uint32) T {
	newIndex := buffer.readerIndexes[readerIndex].value + 1
	var backoff Backoff

	for newIndex > atomic.LoadUint32(&buffer.headIndex) {
		backoff.Step()
	}

	value := buffer.buffer[newIndex&buffer.bitWiseLength]
	atomic.AddUint32(&buffer.readerIndexes[readerIndex].value, 1)
	return value
}

// --- 3. EXPONENTIAL BACKOFF FOR CPU COOPERATIVE YIELDING ---

type Backoff struct {
	attempts int
}

func (b *Backoff) Step() {
	b.attempts++
	if b.attempts < 4 {
		// Active spinning relax loop
		for i := 0; i < b.attempts*20; i++ {
		}
	} else if b.attempts < 12 {
		// Cooperative scheduler yield
		runtime.Gosched()
	} else {
		// Fully park the thread briefly to prevent scheduler lock starvation and core throttling
		time.Sleep(time.Microsecond)
	}
}

func (b *Backoff) Reset() {
	b.attempts = 0
}

// --- 4. HIGH-THROUGHPUT POLYMARKET DATA MODELS ---

type BlockHeader struct {
	Number    uint64
	Hash      string
	Parent    string
	Timestamp uint64
}

type Transaction struct {
	Hash    string
	From    string
	To      string
	Value   uint64
	GasUsed uint64
}

type UserPosition struct {
	UserAddress string // FixedString(20)
	TokenID     string // FixedString(32)
	Amount      uint64
}

type OrderFilled struct {
	OrderHash        string
	Maker            string
	Taker            string
	Amount           uint64
	Price            uint64
	LogIndex         uint32
	TransactionIndex uint32
}

type Market struct {
	ID            string
	ConditionID   string
	Outcomes      [2]string
	OutcomePrices [2]string
}

// Preallocated Flat circular event buffer capacities (optimizing L1/L2 spatial locality)
const (
	FlatOrderFilledSize  = 2097152 // 2^21 ~ 2M events
	FlatUserPositionSize = 2097152 // 2^21 ~ 2M events
	FlatMarketSize       = 16384   // 2^14 ~ 16k events
)

// --- 5. HIGH-THROUGHPUT FLAT COLUMNAR CIRCULAR HISTORIC BUFFER ---

type HistoricBlockBuffer struct {
	length            uint32
	bitWiseLength     uint32
	maxReaders        int

	// Columnar block level metadata
	blockNumbers      []uint64
	headers           []BlockHeader
	transactions      [][]Transaction

	// Flat Circular Buffer index offsets per block
	orderFilledStart  []uint32
	orderFilledCount  []uint32

	userPositionStart []uint32
	userPositionCount []uint32

	marketStart       []uint32
	marketCount       []uint32

	// Preallocated contiguous memory segments (0 heap allocations in hot paths)
	flatOrderFilled   []OrderFilled
	flatUserPosition  []UserPosition
	flatMarkets       []Market

	freeOrderFilled   uint64
	freeUserPosition  uint64
	freeMarket        uint64

	// Cache line alignment to prevent false sharing
	_                 [128]byte

	headIndex         uint32

	_                 [128]byte

	nextReaderIndex   uint32

	_                 [128]byte

	readerIndexes     []paddedUint32
	readerActiveFlags []paddedUint32
}

type HistoricConsumer struct {
	buffer *HistoricBlockBuffer
	id     uint32
}

func CreateHistoricBuffer(size uint32, maxReaders uint32) (*HistoricBlockBuffer, error) {
	if size&(size-1) != 0 {
		return nil, InvalidBufferSize
	}

	return &HistoricBlockBuffer{
		length:            size,
		bitWiseLength:     size - 1,
		headIndex:         0,
		nextReaderIndex:   0,
		maxReaders:        int(maxReaders),
		blockNumbers:      make([]uint64, size),
		headers:           make([]BlockHeader, size),
		transactions:      make([][]Transaction, size),
		orderFilledStart:  make([]uint32, size),
		orderFilledCount:  make([]uint32, size),
		userPositionStart: make([]uint32, size),
		userPositionCount: make([]uint32, size),
		marketStart:       make([]uint32, size),
		marketCount:       make([]uint32, size),
		flatOrderFilled:   make([]OrderFilled, FlatOrderFilledSize),
		flatUserPosition:  make([]UserPosition, FlatUserPositionSize),
		flatMarkets:       make([]Market, FlatMarketSize),
		readerIndexes:     make([]paddedUint32, maxReaders),
		readerActiveFlags: make([]paddedUint32, maxReaders),
	}, nil
}

func (b *HistoricBlockBuffer) CreateConsumer() (HistoricConsumer, error) {
	for readerIndex := range b.readerActiveFlags {
		if atomic.CompareAndSwapUint32(&b.readerActiveFlags[readerIndex].value, 0, 2) {
			b.readerIndexes[readerIndex].value = atomic.LoadUint32(&b.headIndex)
			atomic.StoreUint32(&b.readerActiveFlags[readerIndex].value, 1)
			atomic.CompareAndSwapUint32(&b.nextReaderIndex, uint32(readerIndex), uint32(readerIndex)+1)

			return HistoricConsumer{
				id:     uint32(readerIndex),
				buffer: b,
			}, nil
		}
	}
	return HistoricConsumer{}, MaxConsumerError
}

func (b *HistoricBlockBuffer) removeConsumer(readerId uint32) {
	atomic.StoreUint32(&b.readerActiveFlags[readerId].value, 0)
	atomic.CompareAndSwapUint32(&b.nextReaderIndex, readerId-1, b.nextReaderIndex-1)
}

func (c *HistoricConsumer) Remove() {
	c.buffer.removeConsumer(c.id)
}

// Write adds a block and copies its high-frequency events directly into flat, preallocated circular buffers.
func (b *HistoricBlockBuffer) Write(
	blockNum uint64,
	header BlockHeader,
	txs []Transaction,
	positions []UserPosition,
	fills []OrderFilled,
	mkts []Market,
) {
	var offset uint32
	var i uint32
	var backoff Backoff

attemptWrite:
	nextReaderIndex := atomic.LoadUint32(&b.nextReaderIndex)

	for i = 0; i < nextReaderIndex; i++ {
		if atomic.LoadUint32(&b.readerActiveFlags[i].value) == 1 {
			offset = atomic.LoadUint32(&b.readerIndexes[i].value) + b.length
			if offset == b.headIndex {
				backoff.Step()
				goto attemptWrite
			}
		}
	}

	nextIndex := b.headIndex + 1
	targetIdx := nextIndex & b.bitWiseLength

	// Local references for SSA Bounds-Checking Elimination (BCE)
	blockNumbers := b.blockNumbers
	headers := b.headers
	transactions := b.transactions
	ofStart := b.orderFilledStart
	ofCount := b.orderFilledCount
	upStart := b.userPositionStart
	upCount := b.userPositionCount
	mStart := b.marketStart
	mCount := b.marketCount

	if targetIdx < uint32(len(blockNumbers)) &&
		targetIdx < uint32(len(headers)) &&
		targetIdx < uint32(len(transactions)) &&
		targetIdx < uint32(len(ofStart)) &&
		targetIdx < uint32(len(ofCount)) &&
		targetIdx < uint32(len(upStart)) &&
		targetIdx < uint32(len(upCount)) &&
		targetIdx < uint32(len(mStart)) &&
		targetIdx < uint32(len(mCount)) {

		blockNumbers[targetIdx] = blockNum
		headers[targetIdx] = header
		transactions[targetIdx] = txs

		// Copy OrderFilled events
		ofLen := uint32(len(fills))
		if ofLen > 0 {
			start := b.freeOrderFilled
			for j := uint32(0); j < ofLen; j++ {
				b.flatOrderFilled[(start+uint64(j))&(FlatOrderFilledSize-1)] = fills[j]
			}
			ofStart[targetIdx] = uint32(start & (FlatOrderFilledSize - 1))
			ofCount[targetIdx] = ofLen
			b.freeOrderFilled = start + uint64(ofLen)
		} else {
			ofStart[targetIdx] = 0
			ofCount[targetIdx] = 0
		}

		// Copy UserPosition events
		upLen := uint32(len(positions))
		if upLen > 0 {
			start := b.freeUserPosition
			for j := uint32(0); j < upLen; j++ {
				b.flatUserPosition[(start+uint64(j))&(FlatUserPositionSize-1)] = positions[j]
			}
			upStart[targetIdx] = uint32(start & (FlatUserPositionSize - 1))
			upCount[targetIdx] = upLen
			b.freeUserPosition = start + uint64(upLen)
		} else {
			upStart[targetIdx] = 0
			upCount[targetIdx] = 0
		}

		// Copy Market events
		mLen := uint32(len(mkts))
		if mLen > 0 {
			start := b.freeMarket
			for j := uint32(0); j < mLen; j++ {
				b.flatMarkets[(start+uint64(j))&(FlatMarketSize-1)] = mkts[j]
			}
			mStart[targetIdx] = uint32(start & (FlatMarketSize - 1))
			mCount[targetIdx] = mLen
			b.freeMarket = start + uint64(mLen)
		} else {
			mStart[targetIdx] = 0
			mCount[targetIdx] = 0
		}
	}

	atomic.StoreUint32(&b.headIndex, nextIndex)
}

// Read retrieves a block and populates the caller's preallocated slices to maintain zero allocations.
func (c *HistoricConsumer) Read(
	outPositions []UserPosition,
	outFills []OrderFilled,
	outMarkets []Market,
) (
	blockNum uint64,
	header BlockHeader,
	txs []Transaction,
	numPositions int,
	numFills int,
	numMarkets int,
) {
	b := c.buffer
	newIndex := b.readerIndexes[c.id].value + 1
	var backoff Backoff

	for newIndex > atomic.LoadUint32(&b.headIndex) {
		backoff.Step()
	}

	targetIdx := newIndex & b.bitWiseLength

	blockNumbers := b.blockNumbers
	headers := b.headers
	transactions := b.transactions
	ofStart := b.orderFilledStart
	ofCount := b.orderFilledCount
	upStart := b.userPositionStart
	upCount := b.userPositionCount
	mStart := b.marketStart
	mCount := b.marketCount

	if targetIdx < uint32(len(blockNumbers)) &&
		targetIdx < uint32(len(headers)) &&
		targetIdx < uint32(len(transactions)) &&
		targetIdx < uint32(len(ofStart)) &&
		targetIdx < uint32(len(ofCount)) &&
		targetIdx < uint32(len(upStart)) &&
		targetIdx < uint32(len(upCount)) &&
		targetIdx < uint32(len(mStart)) &&
		targetIdx < uint32(len(mCount)) {

		blockNum = blockNumbers[targetIdx]
		header = headers[targetIdx]
		txs = transactions[targetIdx]

		// Read OrderFilled events
		countFills := ofCount[targetIdx]
		startFills := uint64(ofStart[targetIdx])
		numFills = int(countFills)
		for j := uint32(0); j < countFills; j++ {
			if int(j) < len(outFills) {
				outFills[j] = b.flatOrderFilled[(startFills+uint64(j))&(FlatOrderFilledSize-1)]
			}
		}

		// Read UserPosition events
		countPositions := upCount[targetIdx]
		startPositions := uint64(upStart[targetIdx])
		numPositions = int(countPositions)
		for j := uint32(0); j < countPositions; j++ {
			if int(j) < len(outPositions) {
				outPositions[j] = b.flatUserPosition[(startPositions+uint64(j))&(FlatUserPositionSize-1)]
			}
		}

		// Read Market events
		countMarkets := mCount[targetIdx]
		startMarkets := uint64(mStart[targetIdx])
		numMarkets = int(countMarkets)
		for j := uint32(0); j < countMarkets; j++ {
			if int(j) < len(outMarkets) {
				outMarkets[j] = b.flatMarkets[(startMarkets+uint64(j))&(FlatMarketSize-1)]
			}
		}
	}

	atomic.AddUint32(&b.readerIndexes[c.id].value, 1)
	return
}
