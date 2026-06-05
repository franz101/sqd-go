package clock

import (
	"sync"
	"sync/atomic"

	"github.com/franz101/sqd-go/internal/parser"
)

// EventRef represents a reference to an event inside a slot to reconstruct chronological order.
type EventRef struct {
	EventName string
	Index     int // Offset inside the slice for this event name
}

// BlockEventsSlot represents a single slot in the ring buffer storing a block's events.
type BlockEventsSlot struct {
	BlockNumber uint64
	BlockHash   string
	
	// Events grouped by name: {[event_name]: [events]}
	EventsByName map[string][]parser.DecodedEvent
	
	// chronological order list: [(event_name, arr_idx)]
	Order []EventRef
}

// OrderedHistoricRingBuffer maintains a thread-safe, high-performance ring buffer of blocks.
type OrderedHistoricRingBuffer struct {
	slots        []BlockEventsSlot
	blockNumbers []uint64 // Fast ringbuffer idx to blocknumber mapping array
	
	length        uint32
	bitWiseLength uint32
	headIndex     uint32
	tailIndex     uint32
	count         uint32
	
	mu sync.RWMutex
}

// NewOrderedHistoricRingBuffer creates a pre-allocated ring buffer for block events.
func NewOrderedHistoricRingBuffer(size uint32) (*OrderedHistoricRingBuffer, error) {
	if size&(size-1) != 0 {
		return nil, InvalidBufferSize
	}

	slots := make([]BlockEventsSlot, size)
	for i := uint32(0); i < size; i++ {
		slots[i] = BlockEventsSlot{
			EventsByName: make(map[string][]parser.DecodedEvent),
			Order:        make([]EventRef, 0, 128),
		}
	}

	return &OrderedHistoricRingBuffer{
		slots:         slots,
		blockNumbers:  make([]uint64, size),
		length:        size,
		bitWiseLength: size - 1,
		headIndex:     0,
		tailIndex:     0,
		count:         0,
	}, nil
}

// Push adds a new block and its decoded events into the ring buffer.
func (r *OrderedHistoricRingBuffer) Push(blockNum uint64, blockHash string, events []parser.DecodedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	targetIdx := r.headIndex & r.bitWiseLength
	slot := &r.slots[targetIdx]

	// Reset/recycle slot to avoid heap allocations
	slot.BlockNumber = blockNum
	slot.BlockHash = blockHash
	slot.Order = slot.Order[:0]
	
	// Clear existing slices in map but retain capacity
	for name := range slot.EventsByName {
		slot.EventsByName[name] = slot.EventsByName[name][:0]
	}

	// Populate events grouped by event name and record original chronological order
	for _, ev := range events {
		name := ev.EventName
		slice := slot.EventsByName[name]
		idx := len(slice)
		slot.EventsByName[name] = append(slice, ev)
		
		slot.Order = append(slot.Order, EventRef{
			EventName: name,
			Index:     idx,
		})
	}

	// Update fast index-to-blocknumber array
	r.blockNumbers[targetIdx] = blockNum

	// Advance head
	r.headIndex = (r.headIndex + 1) & r.bitWiseLength
	if r.count < r.length {
		r.count++
	} else {
		// Buffer full, advance tail to evict oldest
		r.tailIndex = (r.tailIndex + 1) & r.bitWiseLength
	}
}

// GetBlockNumber returns the block number at a specific ring buffer index.
func (r *OrderedHistoricRingBuffer) GetBlockNumber(idx uint32) uint64 {
	return atomic.LoadUint64(&r.blockNumbers[idx&r.bitWiseLength])
}

// GetBlockEvents retrieves the events for a block number if it exists in the ring buffer.
func (r *OrderedHistoricRingBuffer) GetBlockEvents(blockNum uint64) (*BlockEventsSlot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Scan through unexpired slots starting from tail
	curr := r.tailIndex
	for i := uint32(0); i < r.count; i++ {
		slotIdx := curr & r.bitWiseLength
		if r.blockNumbers[slotIdx] == blockNum {
			return &r.slots[slotIdx], true
		}
		curr = (curr + 1) & r.bitWiseLength
	}
	return nil, false
}

// Reconstruct chronologically traverses events of a block slot.
func (slot *BlockEventsSlot) Reconstruct(callback func(parser.DecodedEvent) error) error {
	for _, ref := range slot.Order {
		ev := slot.EventsByName[ref.EventName][ref.Index]
		if err := callback(ev); err != nil {
			return err
		}
	}
	return nil
}
