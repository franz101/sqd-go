package polymarket

import (
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

// TestHotCacheGCScan measures the garbage-collector cost of the hot-state rings
// as a function of how full they are. The signal it isolates is the A2 change:
// whether MemoryUserPosition is pointer-bearing (embeds time.Time, whose *Location
// word forces the GC to scan every one of the N ring slots on every mark) or
// pointer-free (int64 unix-millis -> the whole []MemoryUserPosition backing array
// is a single no-scan span the GC skips entirely).
//
// The cost is driven by the struct's POINTER BITMAP, not the field's runtime
// value, so this benchmark never sets UpdatedAt: a zero time.Time still has a
// scannable pointer word; an int64 does not. That also lets the same test compile
// before and after A2.
//
// Gated behind GC_SCAN=1. Sizes via GC_SCAN_NS (comma-separated ring fills,
// default "100000,250000,500000,1000000") to show how the per-GC mark cost scales
// toward the multi-GB / 20 GB regime.
func TestHotCacheGCScan(t *testing.T) {
	if os.Getenv("GC_SCAN") != "1" {
		t.Skip("set GC_SCAN=1 to run the hot-cache GC-scan benchmark")
	}

	sizes := []uint64{100_000, 250_000, 500_000, 1_000_000}
	if v := os.Getenv("GC_SCAN_NS"); v != "" {
		sizes = sizes[:0]
		for _, part := range splitCSV(v) {
			if n, err := strconv.ParseUint(part, 10, 64); err == nil && n > 0 {
				sizes = append(sizes, n)
			}
		}
	}

	const gcRounds = 8

	t.Logf("[GCSCAN] sizeof(MemoryUserPosition)=%d bytes", memUserPositionSize())

	// Control: a bare []MemoryUserPosition of n elements, held live, with NO
	// sync.Map index. After A2 this backing array is pointer-free, so the GC marks
	// it as a single no-scan span. Comparing this to the full cache below isolates
	// how much of the cache's GC cost is the ring (≈0 now) vs the sync.Map index
	// (the A3 target: ~2-3 boxed objects per entry).
	t.Logf("[GCSCAN][ring-only] %-10s %-12s %-12s", "entries", "avgGC", "liveObjs")
	for _, n := range sizes {
		ring := make([]generated.MemoryUserPosition, n)
		var user common.Address
		var token common.Hash
		for i := uint64(0); i < n; i++ {
			putUint64(user[12:], i/1000+1)
			putUint64(token[24:], i+1)
			ring[i] = generated.MemoryUserPosition{User: user, TokenID: token, BlockNumber: i}
		}
		runtime.GC()
		var durs []time.Duration
		for i := 0; i < gcRounds; i++ {
			start := time.Now()
			runtime.GC()
			durs = append(durs, time.Since(start))
		}
		if len(ring) != int(n) {
			t.Fatal("ring control vanished")
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		avg, _ := avgAndP50(durs)
		t.Logf("[GCSCAN][ring-only] %-10d %-12s %-12d", n, avg.Round(time.Microsecond), ms.HeapObjects)
		ring = nil
		runtime.GC()
	}

	t.Logf("[GCSCAN][full-cache] %-10s %-12s %-12s %-14s %-12s", "entries", "avgGC", "p50GC", "heapInuseMiB", "liveObjs")

	for _, n := range sizes {
		// Build a single large UserPositions ring filled to n distinct keys. We hold
		// it live for the whole GC loop so the mark phase must traverse it each round.
		cache := generated.NewUserPositionsClockCache(n)
		fillUserPositions(cache, n)

		// Warm a GC so the heap is settled, then time gcRounds synchronous GCs.
		runtime.GC()
		var durs []time.Duration
		for i := 0; i < gcRounds; i++ {
			start := time.Now()
			runtime.GC()
			durs = append(durs, time.Since(start))
		}
		// Touch the cache after the loop so the optimizer can't free it early.
		if cache.Len() != n {
			t.Fatalf("ring fill mismatch: got %d want %d", cache.Len(), n)
		}

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		avg, p50 := avgAndP50(durs)
		t.Logf("[GCSCAN][full-cache] %-10d %-12s %-12s %-14.1f %-12d",
			n, avg.Round(time.Microsecond), p50.Round(time.Microsecond),
			float64(ms.HeapInuse)/(1<<20), ms.HeapObjects)

		// Drop the ref so the next size starts clean.
		cache = nil
		runtime.GC()
	}
}

// fillUserPositions inserts n distinct (user, token_id) positions into the ring.
// It never writes UpdatedAt: the GC-scan cost depends on the struct's pointer
// bitmap, not the timestamp value, so leaving it zero keeps the measurement honest
// and the test source compatible across the A2 type change.
func fillUserPositions(cache *generated.UserPositionsClockCache, n uint64) {
	var user common.Address
	var token common.Hash
	for i := uint64(0); i < n; i++ {
		// Distinct user every 1000 positions; distinct token every step -> n keys.
		putUint64(user[12:], i/1000+1)
		putUint64(token[24:], i+1)
		cache.Set(generated.MemoryUserPosition{
			User:           user,
			TokenID:        token,
			UpdatedAtBlock: i,
			BlockNumber:    i,
		})
	}
}

func putUint64(b []byte, v uint64) {
	for k := 0; k < 8 && k < len(b); k++ {
		b[len(b)-1-k] = byte(v >> (8 * k))
	}
}

func memUserPositionSize() uintptr {
	var p generated.MemoryUserPosition
	return unsafe.Sizeof(p)
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func avgAndP50(durs []time.Duration) (avg, p50 time.Duration) {
	if len(durs) == 0 {
		return 0, 0
	}
	var total time.Duration
	cp := append([]time.Duration(nil), durs...)
	for _, d := range cp {
		total += d
	}
	// simple insertion sort (tiny slice)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	return total / time.Duration(len(cp)), cp[len(cp)/2]
}
