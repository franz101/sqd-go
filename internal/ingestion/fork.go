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
	chain := append([]client.BlockRef(nil), s.unfinalizedHeads...)
	if s.finalizedHead != nil {
		chain = append([]client.BlockRef{*s.finalizedHead}, chain...)
	}

	rollbackIndex := findRollbackIndex(chain, previousBlocks)
	if rollbackIndex == -1 {
		if s.finalizedHead != nil {
			return fmt.Errorf("unable to process fork")
		}
		s.unfinalizedHeads = nil
		return nil
	}

	start := 0
	if s.finalizedHead != nil {
		start = 1
	}
	s.unfinalizedHeads = append([]client.BlockRef(nil), chain[start:rollbackIndex+1]...)
	return nil
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
