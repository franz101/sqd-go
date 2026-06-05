package fork_sqd

const DefaultLimit = 1000

type BlockCursor struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

type Tracker struct {
	current                 *BlockCursor
	recentUnfinalizedBlocks []BlockCursor
	finalizedHighWatermark  *BlockCursor
	limit                   int
}

func New(limit int) *Tracker {
	var t Tracker
	t.SetLimit(limit)
	return &t
}

func (t *Tracker) SetLimit(limit int) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	t.limit = limit
	t.capRecent()
}

func (t *Tracker) Init(current, finalized *BlockCursor, recentUnfinalized []BlockCursor) {
	t.current = cloneCursor(current)
	t.finalizedHighWatermark = cloneCursor(finalized)
	t.recentUnfinalizedBlocks = append(t.recentUnfinalizedBlocks[:0], recentUnfinalized...)
	t.pruneFinalized()
	t.capRecent()
	if t.current == nil {
		t.current = t.headFromChain()
	}
}

func (t *Tracker) AddBatch(finalized *BlockCursor, rollbackChain []BlockCursor) {
	if len(rollbackChain) > 0 {
		t.current = cloneCursor(&rollbackChain[len(rollbackChain)-1])
	}
	t.addRollbackChain(finalized, rollbackChain)
	if t.current == nil {
		t.current = t.headFromChain()
	}
}

func (t *Tracker) ApplyBatch(finalized *BlockCursor, blocks []BlockCursor) {
	effectiveFinalized := EffectiveFinalizedHead(finalized, blocks)
	if len(blocks) > 0 {
		t.current = cloneCursor(&blocks[len(blocks)-1])
	} else if t.current == nil {
		t.current = cloneCursor(effectiveFinalized)
	}
	t.addRollbackChain(effectiveFinalized, RollbackChain(effectiveFinalized, blocks))
}

func (t *Tracker) HandleFork(previousBlocks []BlockCursor) (BlockCursor, bool) {
	chain, recentStart := t.currentChain()
	rollbackIndex := FindRollbackIndex(chain, previousBlocks)
	if rollbackIndex >= 0 {
		safe := chain[rollbackIndex]
		if rollbackIndex < recentStart {
			t.recentUnfinalizedBlocks = t.recentUnfinalizedBlocks[:0]
		} else {
			t.recentUnfinalizedBlocks = append(t.recentUnfinalizedBlocks[:0], chain[recentStart:rollbackIndex+1]...)
		}
		t.current = cloneCursor(&safe)
		return safe, true
	}

	if t.finalizedHighWatermark != nil && allBefore(previousBlocks, t.finalizedHighWatermark.Number) {
		safe := *t.finalizedHighWatermark
		t.filterRecent(func(block BlockCursor) bool {
			return block.Number <= safe.Number
		})
		t.current = cloneCursor(&safe)
		return safe, true
	}

	t.recentUnfinalizedBlocks = t.recentUnfinalizedBlocks[:0]
	t.current = nil
	return BlockCursor{}, false
}

func (t *Tracker) Head() *BlockCursor {
	return cloneCursor(t.headFromChain())
}

func (t *Tracker) Current() *BlockCursor {
	return cloneCursor(t.current)
}

func (t *Tracker) headFromChain() *BlockCursor {
	if t.current != nil {
		return t.current
	}
	if len(t.recentUnfinalizedBlocks) > 0 {
		return cloneCursor(&t.recentUnfinalizedBlocks[len(t.recentUnfinalizedBlocks)-1])
	}
	return t.finalizedHighWatermark
}

func (t *Tracker) FinalizedHighWatermark() *BlockCursor {
	return cloneCursor(t.finalizedHighWatermark)
}

func (t *Tracker) RecentUnfinalizedBlocks() []BlockCursor {
	return append([]BlockCursor(nil), t.recentUnfinalizedBlocks...)
}

func (t *Tracker) TruncateAfter(cursor BlockCursor) {
	t.filterRecent(func(block BlockCursor) bool {
		return block.Number <= cursor.Number
	})
	if t.current != nil && t.current.Number > cursor.Number {
		t.current = cloneCursor(&cursor)
	}
}

func FindRollbackIndex(chainA, chainB []BlockCursor) int {
	aIndex := 0
	bIndex := 0
	lastCommonIndex := -1

	for aIndex < len(chainA) && bIndex < len(chainB) {
		a := chainA[aIndex]
		b := chainB[bIndex]

		if a.Number < b.Number {
			aIndex++
			continue
		}
		if a.Number > b.Number {
			bIndex++
			continue
		}
		if a.Hash != b.Hash {
			return lastCommonIndex
		}

		lastCommonIndex = aIndex
		aIndex++
		bIndex++
	}

	return lastCommonIndex
}

func EffectiveFinalizedHead(headerFinalized *BlockCursor, blocks []BlockCursor) *BlockCursor {
	if headerFinalized == nil || len(blocks) == 0 {
		return cloneCursor(headerFinalized)
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
		return cloneCursor(headerFinalized)
	case firstUnfinalized > 0:
		return cloneCursor(&blocks[firstUnfinalized-1])
	default:
		return cloneCursor(&blocks[len(blocks)-1])
	}
}

func RollbackChain(finalized *BlockCursor, blocks []BlockCursor) []BlockCursor {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]BlockCursor, 0, len(blocks))
	for _, block := range blocks {
		if finalized != nil && block.Number <= finalized.Number {
			continue
		}
		out = append(out, block)
	}
	return out
}

func (t *Tracker) addRollbackChain(finalized *BlockCursor, rollbackChain []BlockCursor) {
	if finalized != nil && (t.finalizedHighWatermark == nil || finalized.Number > t.finalizedHighWatermark.Number) {
		t.finalizedHighWatermark = cloneCursor(finalized)
	}
	t.pruneFinalized()
	for _, block := range rollbackChain {
		t.appendRecent(block)
	}
	t.capRecent()
}

func (t *Tracker) appendRecent(block BlockCursor) {
	if t.finalizedHighWatermark != nil && block.Number <= t.finalizedHighWatermark.Number {
		return
	}
	if len(t.recentUnfinalizedBlocks) > 0 {
		last := t.recentUnfinalizedBlocks[len(t.recentUnfinalizedBlocks)-1]
		if block.Number < last.Number {
			return
		}
		if block.Number == last.Number {
			t.recentUnfinalizedBlocks[len(t.recentUnfinalizedBlocks)-1] = block
			return
		}
	}
	t.recentUnfinalizedBlocks = append(t.recentUnfinalizedBlocks, block)
}

func (t *Tracker) currentChain() ([]BlockCursor, int) {
	if t.finalizedHighWatermark == nil {
		return append([]BlockCursor(nil), t.recentUnfinalizedBlocks...), 0
	}
	chain := make([]BlockCursor, 0, len(t.recentUnfinalizedBlocks)+1)
	chain = append(chain, *t.finalizedHighWatermark)
	chain = append(chain, t.recentUnfinalizedBlocks...)
	return chain, 1
}

func (t *Tracker) pruneFinalized() {
	if t.finalizedHighWatermark == nil {
		return
	}
	finalized := t.finalizedHighWatermark.Number
	t.filterRecent(func(block BlockCursor) bool {
		return block.Number > finalized
	})
}

func (t *Tracker) capRecent() {
	if t.limit <= 0 || len(t.recentUnfinalizedBlocks) <= t.limit {
		return
	}
	start := len(t.recentUnfinalizedBlocks) - t.limit
	copy(t.recentUnfinalizedBlocks, t.recentUnfinalizedBlocks[start:])
	t.recentUnfinalizedBlocks = t.recentUnfinalizedBlocks[:t.limit]
}

func (t *Tracker) filterRecent(keep func(BlockCursor) bool) {
	out := t.recentUnfinalizedBlocks[:0]
	for _, block := range t.recentUnfinalizedBlocks {
		if keep(block) {
			out = append(out, block)
		}
	}
	t.recentUnfinalizedBlocks = out
}

func allBefore(blocks []BlockCursor, number uint64) bool {
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

func cloneCursor(cursor *BlockCursor) *BlockCursor {
	if cursor == nil {
		return nil
	}
	clone := *cursor
	return &clone
}
