package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// farMarkerPortal models the REAL skip-empties SQD portal (includeAllBlocks=false):
// a request [from, pin] scans forward from `from` to a high-water mark
// hw = min(pin, from+scanCap-1), returning the "present" blocks (multiples of
// `density`) it finds, capped at denseCap per response. When the scanned window
// holds present blocks the marker is the last RETURNED present block (a short,
// dense-style response); when it is empty the body is a SINGLE extent-marker line
// at hw (a far jump the prefetcher counts as empty -> drives beast mode). This is
// the landscape that exposed a pre-existing beast-mode deadlock: pinned-mode walks
// of the dense band leave small pending gaps, then beast re-engages over the empty
// tail and (before the fix) claimed those gaps concurrently, each far-jumping to a
// different marker and racing the shared cursor past nextEmit into a permanent hole.
type farMarkerPortal struct {
	srv      *httptest.Server
	density  uint64
	denseCap uint64
	scanCap  uint64
}

func newFarMarkerPortal(density, denseCap, scanCap uint64, denseFrom, denseTo uint64) *farMarkerPortal {
	fp := &farMarkerPortal{density: density, denseCap: denseCap, scanCap: scanCap}
	fp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "999999999")
		w.Header().Set("X-Sqd-Head-Number", "999999999")

		pin := uint64(1) << 62
		if q.ToBlock != nil {
			pin = *q.ToBlock
		}
		hw := q.FromBlock + scanCap - 1
		if hw > pin {
			hw = pin
		}
		// Collect present blocks (multiples of density) in [from, hw] within the
		// dense band, capped at denseCap.
		var present []uint64
		first := ((q.FromBlock + density - 1) / density) * density
		for n := first; n <= hw && uint64(len(present)) < denseCap; n += density {
			if n >= denseFrom && n <= denseTo {
				present = append(present, n)
			}
		}
		var b strings.Builder
		if len(present) > 0 {
			// Dense response: marker is the last returned present block (short).
			for _, n := range present {
				fmt.Fprintf(&b, `{"header":{"number":%d,"hash":"0x%064x","timestamp":%d}}`+"\n", n, n, 1700000000+n)
			}
		} else {
			// Empty window: single far extent-marker at the scanned high-water mark.
			fmt.Fprintf(&b, `{"header":{"number":%d,"hash":"0x%064x","timestamp":%d}}`+"\n", hw, hw, 1700000000+hw)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
	return fp
}

func (fp *farMarkerPortal) close() { fp.srv.Close() }

// TestParallelPrefetcherBeastReengageNoDeadlock drives a sparse landscape (a dense
// band near the head followed by a long empty tail) through several workers. The
// dense band leaves small pending gaps; the empty tail re-engages beast mode. Before
// beast claims were fully serialized, concurrent gap workers far-jumped to divergent
// markers and orphaned the consumer's nextEmit, hanging the whole pool. This must
// now drain to completion (and stay complete + duplicate-free) well within the
// timeout.
func TestParallelPrefetcherBeastReengageNoDeadlock(t *testing.T) {
	const start, end uint64 = 0, 4_000_000
	const denseFrom, denseTo uint64 = 1000, 6000
	const density, denseCap, scanCap uint64 = 500, 4, 106_000
	fp := newFarMarkerPortal(density, denseCap, scanCap, denseFrom, denseTo)
	defer fp.close()

	p := newParallelPrefetcher(fp.srv.URL, nil, false /*includeAllBlocks*/, start, end, defaultParallelPageSize, 6, noRateLimit())
	p.launch(context.Background())

	type res struct {
		nums []uint64
		err  error
	}
	done := make(chan res, 1)
	go func() {
		var nums []uint64
		for {
			pg, ok := p.Next(context.Background())
			if !ok {
				done <- res{nums: nums}
				return
			}
			if pg.err != nil {
				done <- res{err: pg.err}
				return
			}
			nums = append(nums, blockNumbersOf(t, pg.raw)...)
		}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("unexpected page err: %v", r.err)
		}
		// Every present dense block in [denseFrom,denseTo] must appear exactly once,
		// ascending; far markers may also appear but never a duplicate or a regress.
		var prev uint64
		seen := make(map[uint64]int)
		for _, n := range r.nums {
			if n < prev {
				t.Fatalf("out-of-order block %d after %d", n, prev)
			}
			prev = n
			seen[n]++
		}
		for n := denseFrom; n <= denseTo; n += density {
			if seen[n] == 0 {
				t.Fatalf("dense block %d never emitted (coverage hole)", n)
			}
		}
		for n, c := range seen {
			if c > 1 {
				t.Fatalf("block %d emitted %d times (duplicate)", n, c)
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("prefetcher deadlocked: beast re-engagement orphaned the consumer cursor")
	}
}

// overshootPortal returns blocks up to toBlock+overshoot, i.e. it does NOT strictly
// honour the pinned toBlock (a defensive model of a portal that scans a few blocks
// past the pin). The prefetcher must trim the overshoot tail so no block beyond the
// reported coveredTo is ever emitted or re-fetched by the next unit.
func newOvershootPortal(respCap, overshoot uint64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "999999999")
		last := q.FromBlock + respCap - 1
		if q.ToBlock != nil && last > *q.ToBlock+overshoot {
			last = *q.ToBlock + overshoot // overshoot the pin by a few blocks
		}
		var b strings.Builder
		for n := q.FromBlock; n <= last; n++ {
			fmt.Fprintf(&b, `{"header":{"number":%d,"hash":"0x%064x","timestamp":%d}}`+"\n", n, n, 1700000000+n)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
}

// TestParallelPrefetcherOvershootNoDuplicate proves the raw-trim closes the
// over-emit/duplicate class: even when the portal returns blocks past the pinned
// toBlock, each chunk's raw ends exactly at coveredTo, so [start,end] is emitted
// exactly once, in order, with nothing beyond endBlock.
func TestParallelPrefetcherOvershootNoDuplicate(t *testing.T) {
	const start, end uint64 = 1000, 9000
	srv := newOvershootPortal(137, 5) // 137-block pages that overshoot the pin by 5
	defer srv.Close()

	p := newParallelPrefetcher(srv.URL, nil, true /*includeAllBlocks*/, start, end, 300, 6, noRateLimit())
	p.launch(context.Background())

	var got []uint64
	var prevTo uint64 = start - 1
	for {
		pg, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("unexpected page err [%d-%d]: %v", pg.from, pg.coveredTo, pg.err)
		}
		if pg.from != prevTo+1 {
			t.Fatalf("page from=%d, want %d (gap/overlap)", pg.from, prevTo+1)
		}
		prevTo = pg.coveredTo
		got = append(got, blockNumbersOf(t, pg.raw)...)
	}
	if prevTo != end {
		t.Fatalf("last covered=%d, want %d", prevTo, end)
	}
	if uint64(len(got)) != end-start+1 {
		t.Fatalf("got %d blocks, want %d (duplicates or over-emit past endBlock)", len(got), end-start+1)
	}
	for i, n := range got {
		if want := start + uint64(i); n != want {
			t.Fatalf("block[%d]=%d, want %d (out-of-order/dup)", i, n, want)
		}
	}
}
