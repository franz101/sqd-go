package ingestion

import (
	"fmt"

	"github.com/franz101/sqd-go/internal/client"
)

type processorState struct {
	finalizedHead    *client.BlockRef
	unfinalizedHeads []client.BlockRef
}

func (s *processorState) head() *client.BlockRef {
	if len(s.unfinalizedHeads) > 0 {
		return &s.unfinalizedHeads[len(s.unfinalizedHeads)-1]
	}
	return s.finalizedHead
}

func (s *processorState) init(finalizedHead *client.BlockRef, unfinalizedHeads []client.BlockRef) {
	s.finalizedHead = cloneBlockRef(finalizedHead)
	s.unfinalizedHeads = append([]client.BlockRef(nil), unfinalizedHeads...)
}

func (s *processorState) handleFork(previousBlocks []client.BlockRef) error {
	_, err := s.rollbackFork(previousBlocks)
	if err != nil {
		if s.finalizedHead == nil {
			s.unfinalizedHeads = nil
			return nil
		}
		return err
	}
	return nil
}

func (s *processorState) rollbackFork(previousBlocks []client.BlockRef) (client.BlockRef, error) {
	chain := append([]client.BlockRef(nil), s.unfinalizedHeads...)
	if s.finalizedHead != nil {
		chain = append([]client.BlockRef{*s.finalizedHead}, chain...)
	}

	rollbackIndex := findRollbackIndex(chain, previousBlocks)
	if rollbackIndex == -1 {
		if s.finalizedHead != nil && allBlocksBefore(previousBlocks, s.finalizedHead.Number) {
			s.unfinalizedHeads = nil
			return *s.finalizedHead, nil
		}
		if s.finalizedHead != nil {
			return client.BlockRef{}, fmt.Errorf("unable to process fork")
		}
		return client.BlockRef{}, fmt.Errorf("unable to process fork without finalized head")
	}

	start := 0
	if s.finalizedHead != nil {
		start = 1
	}
	rollbackHead := chain[rollbackIndex]
	s.unfinalizedHeads = append([]client.BlockRef(nil), chain[start:rollbackIndex+1]...)
	return rollbackHead, nil
}

func (s *processorState) applyBatch(head client.Head, blocks []client.BlockRef) {
	finalized := effectiveFinalizedHead(head.Finalized, blocks)
	s.finalizedHead = maxBlockRef(finalized, s.finalizedHead)
	s.pruneFinalized()
	for _, block := range blocks {
		s.appendUnfinalized(block)
	}
}

func effectiveFinalizedHead(headerFinalized *client.BlockRef, blocks []client.BlockRef) *client.BlockRef {
	if headerFinalized == nil || len(blocks) == 0 {
		return cloneBlockRef(headerFinalized)
	}
	firstUnfinalized := -1
	for i, block := range blocks {
		if block.Number > headerFinalized.Number {
			firstUnfinalized = i
			break
		}
	}
	switch {
	case firstUnfinalized == 0:
		return cloneBlockRef(headerFinalized)
	case firstUnfinalized > 0:
		return cloneBlockRef(&blocks[firstUnfinalized-1])
	default:
		return cloneBlockRef(&blocks[len(blocks)-1])
	}
}

func (s *processorState) appendUnfinalized(block client.BlockRef) {
	if s.finalizedHead != nil && block.Number <= s.finalizedHead.Number {
		return
	}
	if len(s.unfinalizedHeads) > 0 {
		last := s.unfinalizedHeads[len(s.unfinalizedHeads)-1]
		if block.Number < last.Number {
			return
		}
		if block.Number == last.Number {
			s.unfinalizedHeads[len(s.unfinalizedHeads)-1] = block
			return
		}
	}
	s.unfinalizedHeads = append(s.unfinalizedHeads, block)
	const maxRollbackHeads = 4096
	if len(s.unfinalizedHeads) > maxRollbackHeads {
		s.unfinalizedHeads = append([]client.BlockRef(nil), s.unfinalizedHeads[len(s.unfinalizedHeads)-maxRollbackHeads:]...)
	}
}

func (s *processorState) pruneFinalized() {
	if s.finalizedHead == nil || len(s.unfinalizedHeads) == 0 {
		return
	}
	idx := 0
	for idx < len(s.unfinalizedHeads) && s.unfinalizedHeads[idx].Number <= s.finalizedHead.Number {
		idx++
	}
	if idx == 0 {
		return
	}
	s.unfinalizedHeads = append([]client.BlockRef(nil), s.unfinalizedHeads[idx:]...)
}

func (s *processorState) rollbackChain() []client.BlockRef {
	return append([]client.BlockRef(nil), s.unfinalizedHeads...)
}

func (s *processorState) finalized() *client.BlockRef {
	return cloneBlockRef(s.finalizedHead)
}

func allBlocksBefore(blocks []client.BlockRef, number uint64) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.Number >= number {
			return false
		}
	}
	return true
}

func findRollbackIndex(currentChain, forkChain []client.BlockRef) int {
	currentIndex := 0
	forkIndex := 0
	lastCommonIndex := -1

	for currentIndex < len(currentChain) && forkIndex < len(forkChain) {
		currentBlock := currentChain[currentIndex]
		forkBlock := forkChain[forkIndex]

		if currentBlock.Number > forkBlock.Number {
			forkIndex++
			continue
		}
		if currentBlock.Number < forkBlock.Number {
			currentIndex++
			continue
		}
		if currentBlock.Hash != forkBlock.Hash {
			return lastCommonIndex
		}

		lastCommonIndex = currentIndex
		currentIndex++
		forkIndex++
	}

	return lastCommonIndex
}

func maxBlockRef(a, b *client.BlockRef) *client.BlockRef {
	if a == nil {
		return cloneBlockRef(b)
	}
	if b == nil {
		return cloneBlockRef(a)
	}
	if a.Number >= b.Number {
		return cloneBlockRef(a)
	}
	return cloneBlockRef(b)
}

func cloneBlockRef(ref *client.BlockRef) *client.BlockRef {
	if ref == nil {
		return nil
	}
	clone := *ref
	return &clone
}
