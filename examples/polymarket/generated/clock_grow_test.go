package generated

import (
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func upKey(i int) UserPositionsClockKey {
	var k UserPositionsClockKey
	binary.LittleEndian.PutUint64(k.User[:], uint64(i)*0x9e3779b97f4a7c15)
	binary.LittleEndian.PutUint64(k.User[12:], uint64(i)+0x1234)
	binary.LittleEndian.PutUint64(k.TokenID[:], uint64(i)+1)
	binary.LittleEndian.PutUint64(k.TokenID[24:], uint64(i)*2654435761)
	return k
}

var _ = common.Address{}

// TestClockCacheLazyGrow proves the ring starts small, grows on demand, and that
// rebuilding the bucket index across grows never loses or corrupts an entry.
func TestClockCacheLazyGrow(t *testing.T) {
	maxCap := uint64(1 << 20)
	c := NewUserPositionsClockCache(maxCap)
	if c.capacity != initialClockCapacity {
		t.Fatalf("initial capacity = %d, want %d (lazy start)", c.capacity, initialClockCapacity)
	}
	n := int(initialClockCapacity) * 5 // forces several doublings, still < maxCap (no eviction)
	for i := 0; i < n; i++ {
		c.SetByKey(upKey(i), MemoryUserPosition{BlockNumber: uint64(i) + 1})
	}
	if c.capacity <= initialClockCapacity {
		t.Fatalf("ring did not grow: capacity=%d", c.capacity)
	}
	if c.capacity > maxCap {
		t.Fatalf("ring exceeded max: capacity=%d > %d", c.capacity, maxCap)
	}
	// Every key must still be present with its exact value: no eviction (n < maxCap)
	// and no index corruption across the grows.
	for i := 0; i < n; i++ {
		v, ok := c.Get(upKey(i))
		if !ok {
			t.Fatalf("key %d missing after grow (lost entry); capacity=%d size=%d", i, c.capacity, c.size)
		}
		if v.BlockNumber != uint64(i)+1 {
			t.Fatalf("key %d corrupted: BlockNumber=%d want %d", i, v.BlockNumber, uint64(i)+1)
		}
	}
	// Updates after growth must hit the existing slot, not duplicate.
	c.SetByKey(upKey(7), MemoryUserPosition{BlockNumber: 999})
	if v, ok := c.Get(upKey(7)); !ok || v.BlockNumber != 999 {
		t.Fatalf("update after grow failed: ok=%v v=%d", ok, v.BlockNumber)
	}
	t.Logf("lazy-grow OK: %d entries, capacity %d->%d, no loss/corruption", n, initialClockCapacity, c.capacity)
}

// TestClockCacheCapsAtMax proves growth stops at maxCapacity and CLOCK eviction
// takes over (so a tiny cap still bounds memory).
func TestClockCacheCapsAtMax(t *testing.T) {
	maxCap := initialClockCapacity * 2
	c := NewUserPositionsClockCache(maxCap)
	n := int(maxCap) * 2 // exceed the cap -> must cap capacity and evict
	for i := 0; i < n; i++ {
		c.SetByKey(upKey(i), MemoryUserPosition{BlockNumber: uint64(i) + 1})
	}
	if c.capacity != maxCap {
		t.Fatalf("capacity=%d, want capped at %d", c.capacity, maxCap)
	}
	if c.Evictions() == 0 {
		t.Fatalf("expected CLOCK evictions once at max cap")
	}
	if c.size > maxCap {
		t.Fatalf("size=%d exceeds capacity %d", c.size, maxCap)
	}
	t.Logf("cap-at-max OK: capacity capped at %d, %d evictions", c.capacity, c.Evictions())
}
