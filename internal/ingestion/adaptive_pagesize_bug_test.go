package ingestion

import (
	"testing"
	"time"
)

// TestAdaptivePageSizeSaturationBug proves, at unit scale, the root cause of the
// page being pinned at the floor (200) during deep backfill:
//
//   - ReplayBuffer.Len() saturates at capacity and NEVER decreases (the ring
//     overwrites in place; PruneBefore is never called at runtime).
//   - adjustAdaptivePageSize halves the page whenever buffered >= 3/4 capacity.
//   - The producer feeds replayBuf.Len() as `buffered` (ingestion.go:448).
//
// So after the first `capacity` blocks, the page halves on EVERY tick regardless
// of how fast the producer is or how far behind the consumer is — collapsing to
// the floor and throttling fetch. The fix is to feed the TRUE in-flight depth
// (producer latest - consumer position), which this test also demonstrates.
func TestAdaptivePageSizeSaturationBug(t *testing.T) {
	capacity := 8
	rb := NewReplayBuffer(capacity)
	write := func(n uint64) {
		rb.Write(137, n, "h", time.Unix(0, 0), nil, nil, nil, nil, false, "", n, nil)
	}

	// Fill far past capacity. The consumer is irrelevant to Len(); the ring just
	// overwrites its oldest slot, so count saturates at capacity.
	for n := uint64(1); n <= 1000; n++ {
		write(n)
	}
	if rb.Len() != capacity {
		t.Fatalf("Len()=%d, want saturated at capacity %d", rb.Len(), capacity)
	}

	// A FAST producer (returned 50000 blocks in 10ms) wants to grow the page, but
	// the saturated Len() forces a halving every call → collapse to the floor.
	page := uint64(50000)
	for i := 0; i < 30; i++ {
		page = adjustAdaptivePageSize(page, 50000, 10*time.Millisecond, rb.Len(), rb.capacity)
	}
	if page != minAdaptivePageSize {
		t.Fatalf("expected page to collapse to floor %d under saturated Len(); got %d", minAdaptivePageSize, page)
	}
	t.Logf("BUG CONFIRMED: saturated Len()=%d pins adaptive page to floor %d despite a fast producer", rb.Len(), page)

	// THE FIX: feed the true in-flight depth = producer's latest block - consumer
	// position. If the consumer keeps up (here at 995 vs latest 1000 → depth 5),
	// the signal is far below the 3/4-capacity threshold, so the page GROWS.
	trueDepth := int(rb.latestBlock.Load() - 995)
	if trueDepth < 0 || trueDepth >= (rb.capacity*3)/4 {
		t.Fatalf("sanity: trueDepth=%d should be small (consumer keeping up)", trueDepth)
	}
	grown := adjustAdaptivePageSize(5000, 50000, 10*time.Millisecond, trueDepth, rb.capacity)
	if grown <= 5000 {
		t.Fatalf("with true in-flight depth=%d the page should grow; got %d", trueDepth, grown)
	}
	t.Logf("FIX VALIDATED: true in-flight depth=%d lets the adaptive page grow 5000 -> %d", trueDepth, grown)
}
