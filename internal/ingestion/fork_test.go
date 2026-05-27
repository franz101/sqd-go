package ingestion

import (
	"testing"

	"github.com/franz101/sqd-go/internal/client"
)

func ref(number uint64, hash string) client.BlockRef {
	return client.BlockRef{Number: number, Hash: hash}
}

func refPtr(number uint64, hash string) *client.BlockRef {
	r := ref(number, hash)
	return &r
}

func TestFindRollbackIndexReturnsLastCommonBlockBeforeMismatch(t *testing.T) {
	currentChain := []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
	}
	forkChain := []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6a"),
		ref(7, "0x7a"),
	}

	idx := findRollbackIndex(currentChain, forkChain)

	if idx != 0 {
		t.Fatalf("rollback index = %d, want 0", idx)
	}
}

func TestFindRollbackIndexSkipsNonOverlappingHeights(t *testing.T) {
	currentChain := []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
	}
	forkChain := []client.BlockRef{
		ref(4, "0x4"),
		ref(5, "0x5"),
		ref(8, "0x8"),
	}

	idx := findRollbackIndex(currentChain, forkChain)

	if idx != 0 {
		t.Fatalf("rollback index = %d, want 0", idx)
	}
}

func TestFindRollbackIndexReturnsMinusOneWhenNoCommonBlock(t *testing.T) {
	currentChain := []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	}
	forkChain := []client.BlockRef{
		ref(5, "0x5a"),
		ref(6, "0x6a"),
	}

	idx := findRollbackIndex(currentChain, forkChain)

	if idx != -1 {
		t.Fatalf("rollback index = %d, want -1", idx)
	}
}

func TestProcessorStateHeadPrefersLatestUnfinalizedHead(t *testing.T) {
	var state processorState
	state.init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	head := state.head()

	if head == nil || *head != ref(6, "0x6") {
		t.Fatalf("head = %#v, want block 6", head)
	}
}

func TestProcessorStateHandleForkKeepsUnfinalizedBlocksThroughCommonBase(t *testing.T) {
	var state processorState
	state.init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
	})

	err := state.handleFork([]client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6a"),
		ref(7, "0x7a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	want := []client.BlockRef{ref(5, "0x5")}
	if !sameBlockRefs(state.unfinalizedHeads, want) {
		t.Fatalf("unfinalized heads = %#v, want %#v", state.unfinalizedHeads, want)
	}
	if state.finalizedHead == nil || *state.finalizedHead != ref(4, "0x4") {
		t.Fatalf("finalized head = %#v, want block 4", state.finalizedHead)
	}
}

func TestProcessorStateHandleForkToFinalizedHeadClearsUnfinalizedHeads(t *testing.T) {
	var state processorState
	state.init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	err := state.handleFork([]client.BlockRef{
		ref(4, "0x4"),
		ref(5, "0x5a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(state.unfinalizedHeads) != 0 {
		t.Fatalf("unfinalized heads = %#v, want empty", state.unfinalizedHeads)
	}
}

func TestProcessorStateRollbackForkReturnsSafeCursor(t *testing.T) {
	var state processorState
	state.init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
	})

	safe, err := state.rollbackFork([]client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if safe != ref(5, "0x5") {
		t.Fatalf("safe = %#v, want block 5", safe)
	}
	want := []client.BlockRef{ref(5, "0x5")}
	if !sameBlockRefs(state.unfinalizedHeads, want) {
		t.Fatalf("unfinalized heads = %#v, want %#v", state.unfinalizedHeads, want)
	}
}

func TestProcessorStateRollbackForkFallsBackToFinalizedHighWatermark(t *testing.T) {
	var state processorState
	state.init(refPtr(10, "0x10"), []client.BlockRef{
		ref(11, "0x11"),
		ref(12, "0x12"),
	})

	safe, err := state.rollbackFork([]client.BlockRef{
		ref(8, "0x8"),
		ref(9, "0x9"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if safe != ref(10, "0x10") {
		t.Fatalf("safe = %#v, want finalized block 10", safe)
	}
	if len(state.unfinalizedHeads) != 0 {
		t.Fatalf("unfinalized heads = %#v, want empty", state.unfinalizedHeads)
	}
}

func TestProcessorStateApplyBatchTracksOnlyUnfinalizedRollbackChain(t *testing.T) {
	var state processorState
	state.applyBatch(client.Head{Finalized: refPtr(6, "0x6")}, []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
		ref(8, "0x8"),
	})

	if state.finalizedHead == nil || *state.finalizedHead != ref(6, "0x6") {
		t.Fatalf("finalized head = %#v, want block 6", state.finalizedHead)
	}
	want := []client.BlockRef{ref(7, "0x7"), ref(8, "0x8")}
	if !sameBlockRefs(state.rollbackChain(), want) {
		t.Fatalf("rollback chain = %#v, want %#v", state.rollbackChain(), want)
	}
}

func TestProcessorStateApplyBatchDoesNotJumpFinalizedBeyondReturnedBlocks(t *testing.T) {
	var state processorState
	state.applyBatch(client.Head{Finalized: refPtr(100, "0x100")}, []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	if state.finalizedHead == nil || *state.finalizedHead != ref(6, "0x6") {
		t.Fatalf("finalized head = %#v, want last returned block 6", state.finalizedHead)
	}
	if state.head() == nil || *state.head() != ref(6, "0x6") {
		t.Fatalf("head = %#v, want block 6", state.head())
	}
}

func TestProcessorStateHandleForkWithoutFinalizedHeadCanDropAllUnfinalizedHeads(t *testing.T) {
	var state processorState
	state.init(nil, []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	err := state.handleFork([]client.BlockRef{
		ref(5, "0x5a"),
		ref(6, "0x6a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(state.unfinalizedHeads) != 0 {
		t.Fatalf("unfinalized heads = %#v, want empty", state.unfinalizedHeads)
	}
}

func TestProcessorStateHandleForkErrorsWhenForkGoesBelowFinalizedHead(t *testing.T) {
	var state processorState
	state.init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
	})

	err := state.handleFork([]client.BlockRef{
		ref(4, "0x4a"),
		ref(5, "0x5a"),
	})

	if err == nil {
		t.Fatal("expected fork below finalized head to fail")
	}
}

func TestMaxBlockRefDoesNotRegressFinalizedHead(t *testing.T) {
	current := refPtr(10, "0x10")
	incoming := refPtr(9, "0x9")

	got := maxBlockRef(incoming, current)

	if got == nil || *got != *current {
		t.Fatalf("max block ref = %#v, want current finalized head", got)
	}
}

func TestMaxBlockRefAdvancesFinalizedHead(t *testing.T) {
	current := refPtr(10, "0x10")
	incoming := refPtr(12, "0x12")

	got := maxBlockRef(incoming, current)

	if got == nil || *got != *incoming {
		t.Fatalf("max block ref = %#v, want incoming finalized head", got)
	}
}

func sameBlockRefs(a, b []client.BlockRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
