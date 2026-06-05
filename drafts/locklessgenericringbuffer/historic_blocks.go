package locklessgenericringbuffer

import (
	"errors"
	"runtime"
	"sync/atomic"
)

var (
	ErrBlockBufferFull = errors.New("historic block buffer is full")
)

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

// Flat sizes for preallocated circular event buffers
const (
	FlatOrderFilledSize  = 2097152 // 2^21 ~ 2M events
	FlatUserPositionSize = 2097152 // 2^21 ~ 2M events
	FlatMarketSize       = 16384   // 2^14 ~ 16k events
)

type HistoricBlockBuffer struct {
	length            uint32
	bitWiseLength     uint32
	maxReaders        int

	// Block-level Metadata
	blockNumbers      []uint64
	headers           []BlockHeader
	transactions      [][]Transaction

	// Flat Circular Buffer Index Offsets
	orderFilledStart  []uint32
	orderFilledCount  []uint32

	userPositionStart []uint32
	userPositionCount []uint32

	marketStart       []uint32
	marketCount       []uint32

	// Massive Preallocated Contiguous Flat Event Circular Arrays
	flatOrderFilled   []OrderFilled
	flatUserPosition  []UserPosition
	flatMarkets       []Market

	// Writer internal offsets (monotonically increasing, masked at write time)
	freeOrderFilled   uint64
	freeUserPosition  uint64
	freeMarket        uint64

	// Alignment padding to prevent false sharing
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

// CreateHistoricBuffer creates a new lockless historic block buffer.
// size must be a power of 2 (e.g. 8192 for 5000 blocks).
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

attemptWrite:
	nextReaderIndex := atomic.LoadUint32(&b.nextReaderIndex)

	for i = 0; i < nextReaderIndex; i++ {
		if atomic.LoadUint32(&b.readerActiveFlags[i].value) == 1 {
			offset = atomic.LoadUint32(&b.readerIndexes[i].value) + b.length
			if offset == b.headIndex {
				runtime.Gosched()
				goto attemptWrite
			}
		}
	}

	nextIndex := b.headIndex + 1
	targetIdx := nextIndex & b.bitWiseLength

	// Local references for BCE
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

		// Copy OrderFilled events to flat preallocated circular buffer
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
// It returns the actual sizes read.
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

	for newIndex > atomic.LoadUint32(&b.headIndex) {
		runtime.Gosched()
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
