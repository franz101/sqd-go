package ingestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

func TestNextRequestRangeCursorOmitsToBlockWithLocalEnd(t *testing.T) {
	end := uint64(20)

	toBlock, label, ok := nextRequestRange(10, 0, &end, true)

	if !ok {
		t.Fatal("range should be fetchable")
	}
	if toBlock != nil {
		t.Fatalf("cursor request toBlock = %d, want nil", *toBlock)
	}
	if label != "[10-tail]" {
		t.Fatalf("label = %q, want [10-tail]", label)
	}
}

func TestNextRequestRangeBoundedUsesPageSizeAndEnd(t *testing.T) {
	end := uint64(20)

	toBlock, label, ok := nextRequestRange(10, 250, &end, false)

	if !ok {
		t.Fatal("range should be fetchable")
	}
	if toBlock == nil || *toBlock != 20 {
		t.Fatalf("bounded request toBlock = %v, want 20", toBlock)
	}
	if label != "[10-20]" {
		t.Fatalf("label = %q, want [10-20]", label)
	}
}

func TestWaitForNextCursorPollReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := waitForNextCursorPoll(ctx, time.Hour)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wait returned after %v, want immediate context cancellation", elapsed)
	}
}

func TestShouldWaitForEmptyCursorResponseWithoutEndBlock(t *testing.T) {
	if !shouldWaitForEmptyCursorResponse(nil) {
		t.Fatal("empty cursor response without end block should wait for new blocks")
	}
}

func TestShouldWaitForEmptyCursorResponseWithEndBlock(t *testing.T) {
	end := uint64(20)

	if shouldWaitForEmptyCursorResponse(&end) {
		t.Fatal("empty cursor response with end block should stop")
	}
}

func TestEmptyCursorCheckpointUsesFinalizedHead(t *testing.T) {
	checkpoint, ok := emptyCursorCheckpoint(10, client.Head{
		Finalized: &client.BlockRef{Number: 12, Hash: "0x12"},
	})

	if !ok {
		t.Fatal("checkpoint should be available")
	}
	if checkpoint != 12 {
		t.Fatalf("checkpoint = %d, want 12", checkpoint)
	}
}

func TestEmptyCursorCheckpointIgnoresFinalizedHeadBeforeCurrentBlock(t *testing.T) {
	checkpoint, ok := emptyCursorCheckpoint(10, client.Head{
		Finalized: &client.BlockRef{Number: 9, Hash: "0x9"},
	})

	if ok {
		t.Fatalf("checkpoint = %d, want no checkpoint", checkpoint)
	}
}
