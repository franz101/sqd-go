//go:build ignore
// +build ignore

package experiment

import (
	"fmt"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

type OrderedHistoricRingBuffer struct {
	slots         []generated.BlockEventsSlot
	blockNumbers  []uint64
	allocs        []Allocator
	length        uint32
	bitWiseLength uint32
	headIndex     uint32
	tailIndex     uint32
	count         uint32
}

func NewOrderedHistoricRingBuffer(size uint32, useArena bool) (*OrderedHistoricRingBuffer, error) {
	if size == 0 || (size&(size-1) != 0) {
		return nil, fmt.Errorf("buffer size must be a power of two")
	}
	slots := make([]generated.BlockEventsSlot, size)
	allocs := make([]Allocator, size)
	for i := range allocs {
		if useArena {
			allocs[i] = NewArenaAllocator()
		} else {
			allocs[i] = NewStdAllocator()
		}
	}
	return &OrderedHistoricRingBuffer{
		slots:         slots,
		blockNumbers:  make([]uint64, size),
		allocs:        allocs,
		length:        size,
		bitWiseLength: size - 1,
	}, nil
}

func (r *OrderedHistoricRingBuffer) NextSlot(blockNum uint64, blockHash string) (*generated.BlockEventsSlot, Allocator) {
	targetIdx := r.headIndex & r.bitWiseLength
	slot := &r.slots[targetIdx]

	slot.BlockNumber = blockNum
	slot.BlockHash = blockHash
	slot.Sequence = slot.Sequence[:0]

	r.resetSlotSlices(slot)

	alloc := r.allocs[targetIdx]
	alloc.Reset()

	r.blockNumbers[targetIdx] = blockNum
	r.headIndex = (r.headIndex + 1) & r.bitWiseLength
	if r.count < r.length {
		r.count++
	} else {
		r.tailIndex = (r.tailIndex + 1) & r.bitWiseLength
	}
	return slot, alloc
}

func (r *OrderedHistoricRingBuffer) resetSlotSlices(slot *generated.BlockEventsSlot) {
	clear(slot.ConditionalTokensConditionPreparations[:cap(slot.ConditionalTokensConditionPreparations)])
	slot.ConditionalTokensConditionPreparations = slot.ConditionalTokensConditionPreparations[:0]

	clear(slot.ConditionalTokensConditionResolutions[:cap(slot.ConditionalTokensConditionResolutions)])
	slot.ConditionalTokensConditionResolutions = slot.ConditionalTokensConditionResolutions[:0]

	clear(slot.ConditionalTokensPositionSplits[:cap(slot.ConditionalTokensPositionSplits)])
	slot.ConditionalTokensPositionSplits = slot.ConditionalTokensPositionSplits[:0]

	clear(slot.ConditionalTokensPositionsMerges[:cap(slot.ConditionalTokensPositionsMerges)])
	slot.ConditionalTokensPositionsMerges = slot.ConditionalTokensPositionsMerges[:0]

	clear(slot.ConditionalTokensPayoutRedemptions[:cap(slot.ConditionalTokensPayoutRedemptions)])
	slot.ConditionalTokensPayoutRedemptions = slot.ConditionalTokensPayoutRedemptions[:0]

	clear(slot.ExchangeOrderFilleds[:cap(slot.ExchangeOrderFilleds)])
	slot.ExchangeOrderFilleds = slot.ExchangeOrderFilleds[:0]

	clear(slot.NegRiskAdapterMarketPrepareds[:cap(slot.NegRiskAdapterMarketPrepareds)])
	slot.NegRiskAdapterMarketPrepareds = slot.NegRiskAdapterMarketPrepareds[:0]

	clear(slot.NegRiskAdapterQuestionPrepareds[:cap(slot.NegRiskAdapterQuestionPrepareds)])
	slot.NegRiskAdapterQuestionPrepareds = slot.NegRiskAdapterQuestionPrepareds[:0]

	clear(slot.NegRiskAdapterPositionSplits[:cap(slot.NegRiskAdapterPositionSplits)])
	slot.NegRiskAdapterPositionSplits = slot.NegRiskAdapterPositionSplits[:0]

	clear(slot.NegRiskAdapterPositionsMerges[:cap(slot.NegRiskAdapterPositionsMerges)])
	slot.NegRiskAdapterPositionsMerges = slot.NegRiskAdapterPositionsMerges[:0]

	clear(slot.NegRiskAdapterPositionsConverteds[:cap(slot.NegRiskAdapterPositionsConverteds)])
	slot.NegRiskAdapterPositionsConverteds = slot.NegRiskAdapterPositionsConverteds[:0]

	clear(slot.NegRiskAdapterPayoutRedemptions[:cap(slot.NegRiskAdapterPayoutRedemptions)])
	slot.NegRiskAdapterPayoutRedemptions = slot.NegRiskAdapterPayoutRedemptions[:0]
}

func (r *OrderedHistoricRingBuffer) GetBlockEvents(blockNum uint64) (*generated.BlockEventsSlot, bool) {
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
