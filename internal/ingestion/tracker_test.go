package ingestion

import (
	"testing"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
)

func TestForkTrackerDefaultsToRingBuffer(t *testing.T) {
	tracker := NewForkTracker("")
	tracker.Init(refPtr(7, "0x7"), refPtr(4, "0x4"), []client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6"),
		ref(7, "0x7"),
	})

	safe, ok := tracker.HandleFork([]client.BlockRef{
		ref(5, "0x5"),
		ref(6, "0x6a"),
		ref(7, "0x7a"),
	})

	if !ok {
		t.Fatal("fork should resolve")
	}
	if safe != ref(5, "0x5") {
		t.Fatalf("safe = %#v, want block 5", safe)
	}
}

func TestRingBufferTrackerKeepsCurrentAcrossEmptyFinalizedUpdate(t *testing.T) {
	tracker := NewForkTracker(config.ForkModeDefault)
	tracker.Init(refPtr(20, "0x20"), refPtr(18, "0x18"), nil)

	tracker.ApplyBatch(refPtr(25, "0x25"), nil)

	if got := tracker.Current(); got == nil || *got != ref(20, "0x20") {
		t.Fatalf("current = %#v, want unchanged block 20", got)
	}
	if got := tracker.FinalizedHighWatermark(); got == nil || *got != ref(25, "0x25") {
		t.Fatalf("finalized high watermark = %#v, want block 25", got)
	}
}
