package codegen

import (
	"bytes"
	"fmt"
	"go/format"

	"github.com/franz101/sqd-go/internal/template"
)

func generateRingBufferGo(events []eventSpec) ([]byte, error) {
	tmplData := struct {
		Events []eventSpec
	}{
		Events: events,
	}

	src := template.MustExecute("code/ringbufferGo", tmplData)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return []byte(src), fmt.Errorf("format source: %w", err)
	}

	// Append proto ring buffer
	protoRingCode, err := generateProtoRingBuffer(events)
	if err != nil {
		return formatted, fmt.Errorf("proto ring buffer: %w", err)
	}

	// Combine both files
	result := fmt.Sprintf("%s\n\n%s", string(formatted), string(protoRingCode))

	return []byte(result), nil
}

// generateProtoRingBuffer generates the V2 proto ring buffer
// This stores events in columnar proto format, reducing memory by ~50x
func generateProtoRingBuffer(events []eventSpec) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString(`
// =============================================================================
// V2: Proto ring buffer (columnar storage, ~50x memory reduction)
// =============================================================================

// ProtoRingBuffer stores parsed events in columnar proto format.
// This is the ClickHouse-native storage format with ~50x less memory than struct slices.
type ProtoRingBuffer struct {
	slots         []*ProtoEventBlock
	blockNumbers  []uint64
	length        uint32
	bitWiseLength uint32
	headIndex     uint32
	tailIndex     uint32
	count         uint32
}

// NewProtoRingBuffer creates a new proto ring buffer with power-of-two size.
func NewProtoRingBuffer(size uint32) (*ProtoRingBuffer, error) {
	if size == 0 || (size&(size-1) != 0) {
		return nil, fmt.Errorf("buffer size must be a power of two")
	}
	slots := make([]*ProtoEventBlock, size)
	for i := range slots {
		slots[i] = NewProtoEventBlock()
	}
	return &ProtoRingBuffer{
		slots:         slots,
		blockNumbers:  make([]uint64, size),
		length:        size,
		bitWiseLength: size - 1,
	}, nil
}

// NextProtoSlot returns a proto slot for the given block number.
// The slot is reset before return.
func (r *ProtoRingBuffer) NextProtoSlot(blockNum uint64, blockHash string) *ProtoEventBlock {
	targetIdx := r.headIndex & r.bitWiseLength
	slot := r.slots[targetIdx]

	slot.Reset()
	slot.HeaderBlockNumber = blockNum
	slot.HeaderBlockHash = blockHash

	r.blockNumbers[targetIdx] = blockNum
	r.headIndex = (r.headIndex + 1) & r.bitWiseLength
	if r.count < r.length {
		r.count++
	} else {
		r.tailIndex = (r.tailIndex + 1) & r.bitWiseLength
	}
	return slot
}

// Push stores decoded logs for a block in the next proto slot.
func (r *ProtoRingBuffer) Push(blockNum uint64, blockHash string, logs []DecodedLog) {
	slot := r.NextProtoSlot(blockNum, blockHash)
	for _, log := range logs {
		slot.AppendDecodedLog(log)
	}
}

// GetProtoBlockNumber returns the block number at the given index.
func (r *ProtoRingBuffer) GetProtoBlockNumber(idx uint32) uint64 {
	return r.blockNumbers[idx&r.bitWiseLength]
}

// GetProtoEventBlock returns the proto event block for the given block number.
func (r *ProtoRingBuffer) GetProtoEventBlock(blockNum uint64) (*ProtoEventBlock, bool) {
	curr := r.tailIndex
	for i := uint32(0); i < r.count; i++ {
		slotIdx := curr & r.bitWiseLength
		if r.blockNumbers[slotIdx] == blockNum {
			return r.slots[slotIdx], true
		}
		curr = (curr + 1) & r.bitWiseLength
	}
	return nil, false
}

// Len returns the number of blocks currently in the buffer.
func (r *ProtoRingBuffer) Len() int {
	return int(r.count)
}

// Reset resets the ring buffer to empty state.
func (r *ProtoRingBuffer) Reset() {
	r.headIndex = 0
	r.tailIndex = 0
	r.count = 0
	for i := range r.blockNumbers {
		r.blockNumbers[i] = 0
	}
}
`)

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return buf.Bytes(), fmt.Errorf("format proto ring buffer: %w", err)
	}
	return formatted, nil
}
