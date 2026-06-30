package polymarket

import (
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
)

// TestPrefetchQueueSkipsProvablyNewUnderAuthoritative pins the speedup: with an
// authoritative cold tier and a complete negative filter, a key the filter has
// never seen is provably new (it cannot exist in ClickHouse), so the prefetch
// resolver must NOT enqueue it — the lazy Get returns zero and a CH resolve would
// be a wasted round-trip. This is the gate that collapses the resolve storm.
func TestPrefetchQueueSkipsProvablyNewUnderAuthoritative(t *testing.T) {
	s := generated.NewState()
	s.HotState = generated.NewHotState(1 << 12)
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, true /*authoritative*/, 0, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	newKey := generated.UserPositionsClockKey{
		User:    common.HexToAddress("0xc000000000000000000000000000000000000003"),
		TokenID: tokenIDHash(*uint256.NewInt(0xC1)),
	}
	s.HotState.UserPositionsResolver.Queue(newKey)
	if p := s.HotState.UserPositionsResolver.Pending(); p != 0 {
		t.Fatalf("authoritative + filter-negative key was queued (Pending=%d), want 0 (skipped)", p)
	}
}

// TestPrefetchQueueEnqueuesWhenNotAuthoritative pins the safety side: in
// non-authoritative mode (resume/cursor against a populated ClickHouse) the skip
// must NOT fire — every hot+cold miss has to fall back to ClickHouse, so the key
// is queued.
func TestPrefetchQueueEnqueuesWhenNotAuthoritative(t *testing.T) {
	s := generated.NewState()
	s.HotState = generated.NewHotState(1 << 12)
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, false /*authoritative*/, 0, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	key := generated.UserPositionsClockKey{
		User:    common.HexToAddress("0xc000000000000000000000000000000000000004"),
		TokenID: tokenIDHash(*uint256.NewInt(0xC2)),
	}
	s.HotState.UserPositionsResolver.Queue(key)
	if p := s.HotState.UserPositionsResolver.Pending(); p != 1 {
		t.Fatalf("non-authoritative miss was not queued (Pending=%d), want 1", p)
	}
}
