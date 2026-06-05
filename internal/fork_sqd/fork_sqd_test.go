package fork_sqd

import "testing"

func cursor(number uint64, hash string) BlockCursor {
	return BlockCursor{Number: number, Hash: hash}
}

func cursorPtr(number uint64, hash string) *BlockCursor {
	c := cursor(number, hash)
	return &c
}

func TestFindRollbackIndexReturnsLastCommonBlockBeforeMismatch(t *testing.T) {
	idx := FindRollbackIndex(
		[]BlockCursor{cursor(5, "0x5"), cursor(6, "0x6"), cursor(7, "0x7")},
		[]BlockCursor{cursor(5, "0x5"), cursor(6, "0x6a"), cursor(7, "0x7a")},
	)

	if idx != 0 {
		t.Fatalf("rollback index = %d, want 0", idx)
	}
}

func TestFindRollbackIndexSkipsNonOverlappingHeights(t *testing.T) {
	idx := FindRollbackIndex(
		[]BlockCursor{cursor(5, "0x5"), cursor(6, "0x6"), cursor(7, "0x7")},
		[]BlockCursor{cursor(4, "0x4"), cursor(5, "0x5"), cursor(8, "0x8")},
	)

	if idx != 0 {
		t.Fatalf("rollback index = %d, want 0", idx)
	}
}

func TestFindRollbackIndexReturnsMinusOneWhenNoCommonBlock(t *testing.T) {
	idx := FindRollbackIndex(
		[]BlockCursor{cursor(5, "0x5"), cursor(6, "0x6")},
		[]BlockCursor{cursor(5, "0x5a"), cursor(6, "0x6a")},
	)

	if idx != -1 {
		t.Fatalf("rollback index = %d, want -1", idx)
	}
}

func TestTrackerAddBatchAppendsPrunesAndCapsLikeDocs(t *testing.T) {
	tracker := New(3)
	tracker.AddBatch(cursorPtr(10, "0x10"), []BlockCursor{
		cursor(8, "0x8"),
		cursor(9, "0x9"),
		cursor(10, "0x10"),
		cursor(11, "0x11"),
		cursor(12, "0x12"),
		cursor(13, "0x13"),
	})

	want := []BlockCursor{
		cursor(11, "0x11"),
		cursor(12, "0x12"),
		cursor(13, "0x13"),
	}
	got := tracker.RecentUnfinalizedBlocks()
	if !sameCursors(got, want) {
		t.Fatalf("recent = %#v, want %#v", got, want)
	}
	if got := tracker.Current(); got == nil || *got != cursor(13, "0x13") {
		t.Fatalf("current = %#v, want block 13", got)
	}
}

func TestTrackerDoesNotRegressFinalizedHighWatermark(t *testing.T) {
	tracker := New(0)
	tracker.AddBatch(cursorPtr(12, "0x12"), []BlockCursor{cursor(13, "0x13")})
	tracker.AddBatch(cursorPtr(10, "0x10"), []BlockCursor{cursor(14, "0x14")})

	got := tracker.FinalizedHighWatermark()
	if got == nil || *got != cursor(12, "0x12") {
		t.Fatalf("finalized high watermark = %#v, want block 12", got)
	}
}

func TestTrackerHandleForkReturnsCommonAncestorAndTruncates(t *testing.T) {
	tracker := New(0)
	tracker.Init(cursorPtr(7, "0x7"), cursorPtr(4, "0x4"), []BlockCursor{
		cursor(5, "0x5"),
		cursor(6, "0x6"),
		cursor(7, "0x7"),
	})

	safe, ok := tracker.HandleFork([]BlockCursor{
		cursor(5, "0x5"),
		cursor(6, "0x6a"),
		cursor(7, "0x7a"),
	})

	if !ok {
		t.Fatal("fork should resolve")
	}
	if safe != cursor(5, "0x5") {
		t.Fatalf("safe = %#v, want block 5", safe)
	}
	want := []BlockCursor{cursor(5, "0x5")}
	if got := tracker.RecentUnfinalizedBlocks(); !sameCursors(got, want) {
		t.Fatalf("recent = %#v, want %#v", got, want)
	}
	if got := tracker.Current(); got == nil || *got != cursor(5, "0x5") {
		t.Fatalf("current = %#v, want safe cursor", got)
	}
}

func TestTrackerHandleForkCanResolveToFinalizedCommonBase(t *testing.T) {
	tracker := New(0)
	tracker.Init(cursorPtr(6, "0x6"), cursorPtr(4, "0x4"), []BlockCursor{
		cursor(5, "0x5"),
		cursor(6, "0x6"),
	})

	safe, ok := tracker.HandleFork([]BlockCursor{
		cursor(4, "0x4"),
		cursor(5, "0x5a"),
	})

	if !ok {
		t.Fatal("fork should resolve to finalized common base")
	}
	if safe != cursor(4, "0x4") {
		t.Fatalf("safe = %#v, want block 4", safe)
	}
	if got := tracker.RecentUnfinalizedBlocks(); len(got) != 0 {
		t.Fatalf("recent = %#v, want empty", got)
	}
}

func TestTrackerHandleForkFallsBackToFinalizedHighWatermark(t *testing.T) {
	tracker := New(0)
	tracker.Init(cursorPtr(12, "0x12"), cursorPtr(10, "0x10"), []BlockCursor{
		cursor(11, "0x11"),
		cursor(12, "0x12"),
	})

	safe, ok := tracker.HandleFork([]BlockCursor{
		cursor(8, "0x8"),
		cursor(9, "0x9"),
	})

	if !ok {
		t.Fatal("fork should resolve to finalized high watermark")
	}
	if safe != cursor(10, "0x10") {
		t.Fatalf("safe = %#v, want block 10", safe)
	}
	if got := tracker.RecentUnfinalizedBlocks(); len(got) != 0 {
		t.Fatalf("recent = %#v, want empty", got)
	}
}

func TestTrackerHandleForkReturnsFalseWhenNoSafeCursor(t *testing.T) {
	tracker := New(0)
	tracker.Init(cursorPtr(11, "0x11"), cursorPtr(10, "0x10"), []BlockCursor{cursor(11, "0x11")})

	safe, ok := tracker.HandleFork([]BlockCursor{cursor(11, "0x11a")})

	if ok {
		t.Fatalf("fork resolved to %#v, want unresolved", safe)
	}
	if got := tracker.RecentUnfinalizedBlocks(); len(got) != 0 {
		t.Fatalf("recent = %#v, want cleared", got)
	}
}

func TestTrackerApplyBatchTracksEffectiveFinalizedAndRollbackChain(t *testing.T) {
	tracker := New(0)
	tracker.ApplyBatch(cursorPtr(6, "0x6"), []BlockCursor{
		cursor(5, "0x5"),
		cursor(6, "0x6"),
		cursor(7, "0x7"),
		cursor(8, "0x8"),
	})

	if got := tracker.FinalizedHighWatermark(); got == nil || *got != cursor(6, "0x6") {
		t.Fatalf("finalized high watermark = %#v, want block 6", got)
	}
	want := []BlockCursor{cursor(7, "0x7"), cursor(8, "0x8")}
	if got := tracker.RecentUnfinalizedBlocks(); !sameCursors(got, want) {
		t.Fatalf("recent = %#v, want %#v", got, want)
	}
	if got := tracker.Current(); got == nil || *got != cursor(8, "0x8") {
		t.Fatalf("current = %#v, want block 8", got)
	}
}

func TestTrackerApplyBatchDoesNotFinalizeBeyondReturnedBlocks(t *testing.T) {
	tracker := New(0)
	tracker.ApplyBatch(cursorPtr(100, "0x100"), []BlockCursor{
		cursor(5, "0x5"),
		cursor(6, "0x6"),
	})

	if got := tracker.FinalizedHighWatermark(); got == nil || *got != cursor(6, "0x6") {
		t.Fatalf("finalized high watermark = %#v, want last returned block 6", got)
	}
	if got := tracker.Head(); got == nil || *got != cursor(6, "0x6") {
		t.Fatalf("head = %#v, want block 6", got)
	}
}

func TestTrackerApplyEmptyBatchDoesNotMoveCurrentOnceInitialized(t *testing.T) {
	tracker := New(0)
	tracker.Init(cursorPtr(20, "0x20"), cursorPtr(18, "0x18"), nil)

	tracker.ApplyBatch(cursorPtr(25, "0x25"), nil)

	if got := tracker.Current(); got == nil || *got != cursor(20, "0x20") {
		t.Fatalf("current = %#v, want unchanged block 20", got)
	}
	if got := tracker.FinalizedHighWatermark(); got == nil || *got != cursor(25, "0x25") {
		t.Fatalf("finalized high watermark = %#v, want block 25", got)
	}
}

func sameCursors(a, b []BlockCursor) bool {
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
