package ingestion

import (
	"testing"
	"time"
)

func TestReplayBufferGetBlockAfterWraparound(t *testing.T) {
	rb := NewReplayBuffer(3)

	for block := uint64(10); block <= 14; block++ {
		rb.Write(137, block, "hash", time.Time{}, nil, nil, nil, nil, false, "", block, nil)
	}

	entry, ok := rb.GetBlock(13)
	if !ok {
		t.Fatal("expected block 13 to be present")
	}
	if entry.number != 13 {
		t.Fatalf("entry number = %d, want 13", entry.number)
	}

	if _, ok := rb.GetBlock(11); ok {
		t.Fatal("expected overwritten block 11 to be absent")
	}
}

func TestReplayBufferGetBlockKeepsBlockZero(t *testing.T) {
	rb := NewReplayBuffer(3)

	for block := uint64(0); block <= 2; block++ {
		rb.Write(137, block, "hash", time.Time{}, nil, nil, nil, nil, false, "", block, nil)
	}

	entry, ok := rb.GetBlock(0)
	if !ok {
		t.Fatal("expected block 0 to remain indexed")
	}
	if entry.number != 0 {
		t.Fatalf("entry number = %d, want 0", entry.number)
	}
}

func TestReplayBufferGetBlockFallsBackForSparseEntries(t *testing.T) {
	rb := NewReplayBuffer(5)

	rb.Write(137, 10, "hash", time.Time{}, nil, nil, nil, nil, false, "", 10, nil)
	rb.Write(137, 20, "hash", time.Time{}, nil, nil, nil, nil, false, "", 20, nil)

	entry, ok := rb.GetBlock(20)
	if !ok {
		t.Fatal("expected sparse block 20 to be present")
	}
	if entry.number != 20 {
		t.Fatalf("entry number = %d, want 20", entry.number)
	}
}

func TestReplayBufferGetBlockAfterPrune(t *testing.T) {
	rb := NewReplayBuffer(5)

	for block := uint64(10); block <= 14; block++ {
		rb.Write(137, block, "hash", time.Time{}, nil, nil, nil, nil, false, "", block, nil)
	}

	if pruned := rb.PruneBefore(11); pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	if _, ok := rb.GetBlock(10); ok {
		t.Fatal("expected pruned block 10 to be absent")
	}
	if _, ok := rb.GetBlock(11); ok {
		t.Fatal("expected pruned block 11 to be absent")
	}
	if entry, ok := rb.GetBlock(12); !ok || entry.number != 12 {
		t.Fatalf("block 12 lookup = (%d, %v), want (12, true)", entry.number, ok)
	}

	rb.PruneAfter(13)
	if _, ok := rb.GetBlock(14); ok {
		t.Fatal("expected block 14 pruned after rollback to be absent")
	}
	if entry, ok := rb.GetBlock(13); !ok || entry.number != 13 {
		t.Fatalf("block 13 lookup = (%d, %v), want (13, true)", entry.number, ok)
	}
}

func BenchmarkReplayBufferGetBlockFull(b *testing.B) {
	const capacity = 8192
	rb := NewReplayBuffer(capacity)
	for block := uint64(10_000_000); block < 10_000_000+capacity; block++ {
		rb.Write(137, block, "hash", time.Time{}, nil, nil, nil, nil, false, "", block, nil)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		block := uint64(10_000_000 + (i % capacity))
		entry, ok := rb.GetBlock(block)
		if !ok || entry.number != block {
			b.Fatalf("lookup block %d = (%d, %v)", block, entry.number, ok)
		}
	}
}
