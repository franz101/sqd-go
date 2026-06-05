package generated

import (
	"testing"

	"github.com/franz101/sqd-go/internal/coldcache"
)

// TestColdTierSpillConsultPromote exercises the generated clock cache's cold-tier
// integration directly (white-box): an entry evicted from the tiny hot ring must
// be spilled to Pebble and served back on a later Get, preserving its value
// across an update→evict→re-read cycle. This is the #0-correctness core that the
// A79-under-load test does not reach (A79 positions stay hot and never evict).
func TestColdTierSpillConsultPromote(t *testing.T) {
	c := NewUserPositionsClockCache(4) // tiny capacity to force eviction
	cs, err := coldcache.Open(t.TempDir()+"/cc", 0, 0)
	if err != nil {
		t.Fatalf("open cold: %v", err)
	}
	defer cs.Close()
	c.cold = cs

	mk := func(i byte) UserPositionsClockKey {
		var k UserPositionsClockKey
		k.User[19] = i
		k.TokenID[31] = i
		return k
	}

	k1 := mk(1)
	c.SetByKey(k1, MemoryUserPosition{User: k1.User, TokenID: k1.TokenID, BlockNumber: 111, UpdatedAtBlock: 111})

	// Flood the cap-4 ring with distinct keys to evict k1 (spilled to cold).
	for i := byte(2); i <= 9; i++ {
		k := mk(i)
		c.SetByKey(k, MemoryUserPosition{User: k.User, TokenID: k.TokenID, BlockNumber: uint64(i)})
	}

	got, ok := c.Get(k1)
	if !ok {
		t.Fatal("k1 not recovered from cold tier after eviction")
	}
	if got.BlockNumber != 111 || got.User != k1.User || got.TokenID != k1.TokenID {
		t.Fatalf("cold round-trip mismatch: %+v", got)
	}

	// Update k1 (now promoted to hot), evict again, re-read → updated value wins.
	got.BlockNumber = 222
	c.SetByKey(k1, got)
	for i := byte(10); i <= 18; i++ {
		k := mk(i)
		c.SetByKey(k, MemoryUserPosition{User: k.User, TokenID: k.TokenID, BlockNumber: uint64(i)})
	}
	got2, ok := c.Get(k1)
	if !ok || got2.BlockNumber != 222 {
		t.Fatalf("updated cold value not preserved: ok=%v val=%+v", ok, got2)
	}
}

// TestColdTierDisabledUnchanged confirms a cache with no cold tier behaves exactly
// as before: an evicted key is simply gone (the default-off path).
func TestColdTierDisabledUnchanged(t *testing.T) {
	c := NewUserPositionsClockCache(4) // c.cold stays nil
	mk := func(i byte) UserPositionsClockKey {
		var k UserPositionsClockKey
		k.User[19] = i
		k.TokenID[31] = i
		return k
	}
	k1 := mk(1)
	c.SetByKey(k1, MemoryUserPosition{User: k1.User, TokenID: k1.TokenID, BlockNumber: 111})
	for i := byte(2); i <= 9; i++ {
		k := mk(i)
		c.SetByKey(k, MemoryUserPosition{User: k.User, TokenID: k.TokenID, BlockNumber: uint64(i)})
	}
	if _, ok := c.Get(k1); ok {
		t.Fatal("evicted key should be gone when cold tier is disabled")
	}
}
