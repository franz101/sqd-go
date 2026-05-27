package ingestion

import (
	"fmt"

	"github.com/franz101/sqd-go/internal/client"
)

// BlockRingBuffer implements a zero-allocation ring buffer for fork handling.
// It corresponds to the Firedancer approach proposed in PROPOSAL.md.
type BlockRingBuffer struct {
	buffer [1024]client.BlockRef
	head   int // index of the newest block (last pushed)
	tail   int // index of the oldest block (unpruned)
	count  int // number of elements in the buffer

	finalizedHead *client.BlockRef
}

// NewBlockRingBuffer creates a new initialized ring buffer.
func NewBlockRingBuffer() *BlockRingBuffer {
	return &BlockRingBuffer{
		head:  -1,
		tail:  0,
		count: 0,
	}
}

// Push adds a new block to the ring buffer. Zero allocations.
func (r *BlockRingBuffer) Push(ref client.BlockRef) {
	if r.finalizedHead != nil && ref.Number <= r.finalizedHead.Number {
		return
	}

	if r.count == 0 {
		r.head = 0
		r.tail = 0
		r.buffer[0] = ref
		r.count = 1
		return
	}

	// Ignore older blocks if pushed out of order
	last := r.buffer[r.head]
	if ref.Number < last.Number {
		return
	}

	// Overwrite head if same block number
	if last.Number == ref.Number {
		r.buffer[r.head] = ref
		return
	}

	r.head = (r.head + 1) & 1023 // equivalent to % 1024 since 1024 is power of 2
	r.buffer[r.head] = ref

	if r.count < 1024 {
		r.count++
	} else {
		// Buffer is full, move tail forward
		r.tail = (r.tail + 1) & 1023
	}
}

// Prune moves the tail pointer forward past the finalized block.
func (r *BlockRingBuffer) Prune(finalizedHead uint64) {
	for r.count > 0 {
		if r.buffer[r.tail].Number > finalizedHead {
			break
		}
		// Move tail forward and decrease count
		r.tail = (r.tail + 1) & 1023
		r.count--
	}
	if r.count == 0 {
		r.head = -1
		r.tail = 0
	}
}

// SetFinalizedHead updates the finalized head and prunes the buffer.
func (r *BlockRingBuffer) SetFinalizedHead(ref *client.BlockRef) {
	if ref == nil {
		return
	}
	if r.finalizedHead == nil || ref.Number > r.finalizedHead.Number {
		r.finalizedHead = cloneBlockRef(ref)
		r.Prune(ref.Number)
	}
}

// HandleFork attempts to find a common ancestor and rolls back.
// If it fails but there is no finalized head, it drops all unfinalized blocks.
func (r *BlockRingBuffer) HandleFork(previousBlocks []client.BlockRef) error {
	_, err := r.FindCommonAncestor(previousBlocks)
	if err != nil {
		if r.finalizedHead == nil {
			r.count = 0
			r.head = -1
			r.tail = 0
			return nil
		}
		return err
	}
	return nil
}

// FindCommonAncestor scans the ring buffer to find the common ancestor with forkChain.
// Rolls back the buffer state if found and returns the common block.
func (r *BlockRingBuffer) FindCommonAncestor(forkChain []client.BlockRef) (*client.BlockRef, error) {
	if len(forkChain) == 0 {
		return nil, fmt.Errorf("fork chain is empty")
	}

	lastCommonIdx := -1
	forkIndex := 0

	// First handle the finalized head as an imaginary index -2
	if r.finalizedHead != nil && forkIndex < len(forkChain) {
		for forkIndex < len(forkChain) && r.finalizedHead.Number > forkChain[forkIndex].Number {
			forkIndex++
		}
		if forkIndex < len(forkChain) {
			if r.finalizedHead.Number == forkChain[forkIndex].Number {
				if r.finalizedHead.Hash == forkChain[forkIndex].Hash {
					lastCommonIdx = -2 // -2 means finalizedHead
					forkIndex++
				} else {
					// Finalized head mismatch, impossible to process fork!
					return nil, fmt.Errorf("unable to process fork: finalized head mismatch")
				}
			}
		}
	}

	lastCommonBufferIdx := -1
	itemsProcessed := 0
	currIdx := r.tail

	for itemsProcessed < r.count && forkIndex < len(forkChain) {
		currentBlock := r.buffer[currIdx]
		forkBlock := forkChain[forkIndex]

		if currentBlock.Number > forkBlock.Number {
			forkIndex++
			continue
		}
		if currentBlock.Number < forkBlock.Number {
			currIdx = (currIdx + 1) & 1023
			itemsProcessed++
			continue
		}
		if currentBlock.Hash != forkBlock.Hash {
			break
		}

		lastCommonIdx = itemsProcessed
		lastCommonBufferIdx = currIdx

		currIdx = (currIdx + 1) & 1023
		itemsProcessed++
		forkIndex++
	}

	if lastCommonIdx == -1 {
		if r.finalizedHead != nil && allBlocksBefore(forkChain, r.finalizedHead.Number) {
			r.count = 0
			r.head = -1
			r.tail = 0
			return cloneBlockRef(r.finalizedHead), nil
		}
		if r.finalizedHead != nil {
			return nil, fmt.Errorf("unable to process fork")
		}
		return nil, fmt.Errorf("unable to process fork without finalized head")
	}

	if lastCommonIdx == -2 { // Common ancestor is the finalized head
		r.count = 0
		r.head = -1
		r.tail = 0
		return cloneBlockRef(r.finalizedHead), nil
	}

	// Rollback the buffer to lastCommonBufferIdx
	r.head = lastCommonBufferIdx
	r.count = lastCommonIdx + 1
	return cloneBlockRef(&r.buffer[r.head]), nil
}

// GetChain returns a copy of the current unfinalized chain
func (r *BlockRingBuffer) GetChain() []client.BlockRef {
	if r.count == 0 {
		return nil
	}
	chain := make([]client.BlockRef, 0, r.count)
	idx := r.tail
	for i := 0; i < r.count; i++ {
		chain = append(chain, r.buffer[idx])
		idx = (idx + 1) & 1023
	}
	return chain
}

// Head returns the head block
func (r *BlockRingBuffer) Head() *client.BlockRef {
	if r.count > 0 {
		return cloneBlockRef(&r.buffer[r.head])
	}
	return cloneBlockRef(r.finalizedHead)
}

// Finalized returns the finalized head
func (r *BlockRingBuffer) Finalized() *client.BlockRef {
	return cloneBlockRef(r.finalizedHead)
}

// Init can be used to set an initial state (e.g. from database sync state)
func (r *BlockRingBuffer) Init(finalizedHead *client.BlockRef, unfinalizedHeads []client.BlockRef) {
	r.count = 0
	r.head = -1
	r.tail = 0
	r.finalizedHead = cloneBlockRef(finalizedHead)

	for _, b := range unfinalizedHeads {
		r.Push(b)
	}
}

// ApplyBatch processes a new batch of blocks and a finalized head update.
func (r *BlockRingBuffer) ApplyBatch(head client.Head, blocks []client.BlockRef) {
	finalized := effectiveFinalizedHead(head.Finalized, blocks)
	r.SetFinalizedHead(finalized)
	for _, block := range blocks {
		r.Push(block)
	}
}
