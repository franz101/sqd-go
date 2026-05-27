package ingestion

import (
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
)

type ForkTracker interface {
	Init(current, finalized *client.BlockRef, rollbackChain []client.BlockRef)
	Head() *client.BlockRef
	HandleFork(previousBlocks []client.BlockRef) (client.BlockRef, bool)
	ApplyBatch(finalized *client.BlockRef, blocks []client.BlockRef)
	Current() *client.BlockRef
	FinalizedHighWatermark() *client.BlockRef
	RecentUnfinalizedBlocks() []client.BlockRef
}

func NewForkTracker(_ config.ForkMode) ForkTracker {
	return &ringBufferTracker{BlockRingBuffer: NewBlockRingBuffer()}
}

func newForkTracker(mode config.ForkMode) ForkTracker {
	return NewForkTracker(mode)
}

// --- RingBuffer Tracker Wrapper ---

type ringBufferTracker struct {
	*BlockRingBuffer
	current *client.BlockRef
}

func (r *ringBufferTracker) Init(current, finalized *client.BlockRef, rollbackChain []client.BlockRef) {
	r.current = cloneBlockRef(current)
	r.BlockRingBuffer.Init(finalized, rollbackChain)
	if r.current == nil {
		r.current = r.BlockRingBuffer.Head()
	}
}

func (r *ringBufferTracker) Head() *client.BlockRef {
	if r.current != nil {
		return cloneBlockRef(r.current)
	}
	return r.BlockRingBuffer.Head()
}

func (r *ringBufferTracker) HandleFork(previousBlocks []client.BlockRef) (client.BlockRef, bool) {
	safe, err := r.BlockRingBuffer.FindCommonAncestor(previousBlocks)
	if err != nil {
		return client.BlockRef{}, false
	}
	if safe == nil {
		return client.BlockRef{}, false
	}
	r.current = cloneBlockRef(safe)
	return *safe, true
}

func (r *ringBufferTracker) ApplyBatch(finalized *client.BlockRef, blocks []client.BlockRef) {
	if len(blocks) > 0 {
		r.current = cloneBlockRef(&blocks[len(blocks)-1])
	}
	head := client.Head{Finalized: finalized}
	r.BlockRingBuffer.ApplyBatch(head, blocks)
	if r.current == nil {
		r.current = r.BlockRingBuffer.Head()
	}
}

func (r *ringBufferTracker) Current() *client.BlockRef {
	return cloneBlockRef(r.current)
}

func (r *ringBufferTracker) FinalizedHighWatermark() *client.BlockRef {
	return r.BlockRingBuffer.Finalized()
}

func (r *ringBufferTracker) RecentUnfinalizedBlocks() []client.BlockRef {
	return r.BlockRingBuffer.GetChain()
}
