package ingestion

import (
	"context"
	"testing"
	"time"
)

func TestParallelPrefetchWindowIsBoundedByBlockSpan(t *testing.T) {
	p := newParallelPrefetcher("unused", nil, false, 100, 100_000, 1_000, 4, newRateLimiter(1000, 1))
	if got, want := p.maxAhead, uint64(4_000); got != want {
		t.Fatalf("maxAhead = %d, want %d", got, want)
	}
	p.nextEmit = 100

	done := make(chan bool, 1)
	go func() {
		done <- p.waitForWindow(context.Background(), 5_001)
	}()

	select {
	case <-done:
		t.Fatal("window admitted work more than one worker wave ahead")
	case <-time.After(20 * time.Millisecond):
	}

	p.mu.Lock()
	p.nextEmit = 1_001
	p.cond.Broadcast()
	p.mu.Unlock()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("window rejected work after the consumer advanced")
		}
	case <-time.After(time.Second):
		t.Fatal("window did not wake after the consumer advanced")
	}
}
