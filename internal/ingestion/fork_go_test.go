package ingestion

import (
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/parser"
)

func TestRingBufferHeadPrefersLatestUnfinalizedHead(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.Init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	head := buf.Head()

	if head == nil || *head != ref(6, "0x6") {
		t.Fatalf("head = %#v, want block 6", head)
	}
}

func TestRingBufferFindCommonAncestorKeepsUnfinalizedBlocksThroughCommonBase(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.Init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
	})

	safe, err := buf.FindCommonAncestor([]client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6a"),
		ref(7, "0x7a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if safe == nil || *safe != ref(5, "0x5") {
		t.Fatalf("safe = %#v, want block 5", safe)
	}
	want := []client.BlockRef{ref(5, "0x5")}
	if !sameBlockRefs(buf.GetChain(), want) {
		t.Fatalf("unfinalized heads = %#v, want %#v", buf.GetChain(), want)
	}
	if buf.Finalized() == nil || *buf.Finalized() != ref(4, "0x4") {
		t.Fatalf("finalized head = %#v, want block 4", buf.Finalized())
	}
}

func TestRingBufferFindCommonAncestorToFinalizedHeadClearsUnfinalizedHeads(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.Init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	safe, err := buf.FindCommonAncestor([]client.BlockRef{
		ref(4, "0x4"),
		ref(5, "0x5a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if safe == nil || *safe != ref(4, "0x4") {
		t.Fatalf("safe = %#v, want block 4", safe)
	}
	if len(buf.GetChain()) != 0 {
		t.Fatalf("unfinalized heads = %#v, want empty", buf.GetChain())
	}
}

func TestRingBufferFindCommonAncestorReturnsSafeCursor(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.Init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
	})

	safe, err := buf.FindCommonAncestor([]client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if safe == nil || *safe != ref(5, "0x5") {
		t.Fatalf("safe = %#v, want block 5", safe)
	}
	want := []client.BlockRef{ref(5, "0x5")}
	if !sameBlockRefs(buf.GetChain(), want) {
		t.Fatalf("unfinalized heads = %#v, want %#v", buf.GetChain(), want)
	}
}

func TestRingBufferFindCommonAncestorFallsBackToFinalizedHighWatermark(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.Init(refPtr(10, "0x10"), []client.BlockRef{
		ref(11, "0x11"),
		ref(12, "0x12"),
	})

	safe, err := buf.FindCommonAncestor([]client.BlockRef{
		ref(8, "0x8"),
		ref(9, "0x9"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if safe == nil || *safe != ref(10, "0x10") {
		t.Fatalf("safe = %#v, want finalized block 10", safe)
	}
	if len(buf.GetChain()) != 0 {
		t.Fatalf("unfinalized heads = %#v, want empty", buf.GetChain())
	}
}

func TestRingBufferApplyBatchTracksOnlyUnfinalizedRollbackChain(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.ApplyBatch(client.Head{Finalized: refPtr(6, "0x6")}, []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
		ref(8, "0x8"),
	})

	if buf.Finalized() == nil || *buf.Finalized() != ref(6, "0x6") {
		t.Fatalf("finalized head = %#v, want block 6", buf.Finalized())
	}
	want := []client.BlockRef{ref(7, "0x7"), ref(8, "0x8")}
	if !sameBlockRefs(buf.GetChain(), want) {
		t.Fatalf("rollback chain = %#v, want %#v", buf.GetChain(), want)
	}
}

func TestRingBufferApplyBatchDoesNotJumpFinalizedBeyondReturnedBlocks(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.ApplyBatch(client.Head{Finalized: refPtr(100, "0x100")}, []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	if buf.Finalized() == nil || *buf.Finalized() != ref(6, "0x6") {
		t.Fatalf("finalized head = %#v, want last returned block 6", buf.Finalized())
	}
	if buf.Head() == nil || *buf.Head() != ref(6, "0x6") {
		t.Fatalf("head = %#v, want block 6", buf.Head())
	}
}

func TestRingBufferHandleForkWithoutFinalizedHeadCanDropAllUnfinalizedHeads(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.Init(nil, []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
	})

	err := buf.HandleFork([]client.BlockRef{
		ref(5, "0x5a"),
		ref(6, "0x6a"),
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(buf.GetChain()) != 0 {
		t.Fatalf("unfinalized heads = %#v, want empty", buf.GetChain())
	}
}

func TestRingBufferHandleForkErrorsWhenForkGoesBelowFinalizedHead(t *testing.T) {
	buf := NewBlockRingBuffer()
	buf.Init(refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
	})

	err := buf.HandleFork([]client.BlockRef{
		ref(4, "0x4a"),
		ref(5, "0x5a"),
	})

	if err == nil {
		t.Fatal("expected fork below finalized head to fail")
	}
}

func TestReplayPreservesForkCursorState(t *testing.T) {
	// Create a ReplayBuffer
	rb := NewReplayBuffer(10)

	// Write some blocks to it
	rb.Write(1, 10, "0x10", time.Now(), nil, nil)
	rb.Write(1, 11, "0x11", time.Now(), nil, nil)
	rb.Write(1, 12, "0x12", time.Now(), nil, nil)

	// Verify seeking and replaying
	rb.Seek(9)
	var replayed []client.BlockRef
	_, err := rb.ReadFrom(
		func(events []parser.DecodedEvent, blockRow database.BlockRow) error {
			replayed = append(replayed, client.BlockRef{
				Number: blockRow.BlockNumber,
				Hash:   blockRow.BlockHash,
			})
			return nil
		},
		func(logs []CustomLog) error {
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ReadFrom failed: %v", err)
	}

	if len(replayed) != 3 {
		t.Fatalf("replayed length = %d, want 3", len(replayed))
	}

	// Verify ForkTracker advancement with the replayed BlockRefs
	tracker := NewForkTracker(config.ForkModeDefault)
	tracker.Init(refPtr(9, "0x9"), refPtr(9, "0x9"), nil)

	// Apply the replayed refs
	tracker.ApplyBatch(refPtr(9, "0x9"), replayed)

	// Tracker current block should be advanced to block 12
	current := tracker.Current()
	if current == nil || current.Number != 12 || current.Hash != "0x12" {
		t.Fatalf("advanced tracker current = %#v, want block 12/0x12", current)
	}

	// Tracker unfinalized blocks queue should contain blocks 10, 11, 12
	unfinalized := tracker.RecentUnfinalizedBlocks()
	if len(unfinalized) != 3 {
		t.Fatalf("recent unfinalized blocks count = %d, want 3", len(unfinalized))
	}
	if unfinalized[0].Number != 10 || unfinalized[2].Number != 12 {
		t.Fatalf("unexpected unfinalized range: %#v", unfinalized)
	}
}

func TestRollbackOnFailureRangeComputation(t *testing.T) {
	blockRefs := []client.BlockRef{
		{Number: 10, Hash: "0x10"},
		{Number: 11, Hash: "0x11"},
		{Number: 12, Hash: "0x12"},
	}

	if len(blockRefs) == 0 {
		t.Fatal("blockRefs is empty")
	}

	// We calculate rollback to: blockRefs[0].Number - 1
	rollbackTo := blockRefs[0].Number - 1
	if rollbackTo != 9 {
		t.Fatalf("rollbackTo = %d, want 9", rollbackTo)
	}
}
