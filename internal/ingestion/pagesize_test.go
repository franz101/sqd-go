package ingestion

import (
	"math/rand"
	"testing"
)

const (
	tMin = 200
	tMax = 100000
)

// --- direct behavioural cases -------------------------------------------------

func TestNextPageSizeProbesUpWhenUnderCap(t *testing.T) {
	// span == requested => we got the whole window, cap is higher, double.
	got := nextPageSize(1000, 1000, 1000, false, tMin, tMax)
	if got != 2000 {
		t.Fatalf("under-cap probe: got %d, want 2000", got)
	}
}

func TestNextPageSizeTracksCapWhenCapped(t *testing.T) {
	// Requested 8000 but the server only returned a 360-block span => cap≈360.
	got := nextPageSize(8000, 8000, 360, false, tMin, tMax)
	if got != 360+360/4 {
		t.Fatalf("cap tracking: got %d, want %d", got, 360+360/4)
	}
}

func TestNextPageSizeBacksOffOnFailure(t *testing.T) {
	got := nextPageSize(8000, 8000, 0, true, tMin, tMax)
	if got != 4000 {
		t.Fatalf("failure backoff: got %d, want 4000", got)
	}
}

func TestNextPageSizeHoldsOnEmpty(t *testing.T) {
	got := nextPageSize(1234, 5000, 0, false, tMin, tMax)
	if got != 1234 {
		t.Fatalf("empty span should hold steady: got %d, want 1234", got)
	}
}

func TestNextPageSizeRespectsClamps(t *testing.T) {
	if got := nextPageSize(tMax, tMax, tMax, false, tMin, tMax); got != tMax {
		t.Fatalf("probe-up must clamp to max: got %d", got)
	}
	if got := nextPageSize(tMin, tMin, 0, true, tMin, tMax); got != tMin {
		t.Fatalf("backoff must clamp to min: got %d", got)
	}
	// A cap below min must still clamp up to min.
	if got := nextPageSize(tMin, tMin, 10, false, tMin, tMax); got != tMin {
		t.Fatalf("tiny cap must clamp to min: got %d", got)
	}
}

func TestNextPageSizeRepeatedFailuresWalkToMin(t *testing.T) {
	p := uint64(tMax)
	for i := 0; i < 40; i++ {
		p = nextPageSize(p, p, 0, true, tMin, tMax)
	}
	if p != tMin {
		t.Fatalf("repeated failures should reach min %d, got %d", tMin, p)
	}
}

// --- simulated portal: prove convergence + "always full after convergence" ----

// fakePortal returns the block span it serves for a request of `requested`
// blocks starting at `from`: min(requested, capAt(from)). capAt models density.
type fakePortal struct {
	capAt func(from uint64) uint64
	fail  func(from, requested uint64) bool // optional: model too-large rejects
}

func (fp fakePortal) serve(from, requested uint64) (span uint64, failed bool) {
	if fp.fail != nil && fp.fail(from, requested) {
		return 0, true
	}
	cap := fp.capAt(from)
	if requested < cap {
		return requested, false
	}
	return cap, false
}

// runController drives nextPageSize against a fake portal for n requests and
// returns the per-request fill ratio (span/cap) once converged, plus a
// continuity check (cursor advances by exactly the served span, no gaps/overlap).
func runController(fp fakePortal, start, page, n uint64) (fills []float64, contiguous bool, cursor uint64) {
	contiguous = true
	cursor = start
	for i := uint64(0); i < n; i++ {
		requested := page
		span, failed := fp.serve(cursor, requested)
		if !failed && span > 0 {
			fills = append(fills, float64(span)/float64(fp.capAt(cursor)))
			cursor += span // advance by exactly what was served — no gap, no overlap
		}
		page = nextPageSize(page, requested, span, failed, tMin, tMax)
	}
	return fills, contiguous, cursor
}

func TestControllerConvergesFromBelow(t *testing.T) {
	fp := fakePortal{capAt: func(uint64) uint64 { return 360 }}
	fills, _, _ := runController(fp, 1_000_000, tMin, 30)
	// After a few probes the controller should be requesting >= the cap, so the
	// last several responses are full (fill ratio == 1.0).
	tail := fills[len(fills)-5:]
	for i, f := range tail {
		if f < 0.999 {
			t.Fatalf("not converged from below: tail[%d] fill=%.3f (want 1.0)", i, f)
		}
	}
}

func TestControllerConvergesFromAbove(t *testing.T) {
	fp := fakePortal{capAt: func(uint64) uint64 { return 360 }}
	// Start with a huge page (the user's "unbound") — first response reveals the
	// cap and the controller settles just above it.
	fills, _, _ := runController(fp, 1_000_000, tMax, 30)
	tail := fills[len(fills)-5:]
	for i, f := range tail {
		if f < 0.999 {
			t.Fatalf("not converged from above: tail[%d] fill=%.3f", i, f)
		}
	}
}

func TestControllerAdaptsToDensityShift(t *testing.T) {
	// Sparse early (cap 5000), dense later (cap 300): the controller must track
	// both without stalling or chronically under/over-filling.
	fp := fakePortal{capAt: func(from uint64) uint64 {
		if from < 1_100_000 {
			return 5000
		}
		return 300
	}}
	fills, _, cursor := runController(fp, 1_000_000, tMin, 200)
	if cursor <= 1_100_000 {
		t.Fatalf("controller stalled, cursor only reached %d", cursor)
	}
	// Average fill across the whole run should stay high despite the shift.
	var sum float64
	for _, f := range fills {
		sum += f
	}
	avg := sum / float64(len(fills))
	if avg < 0.80 {
		t.Fatalf("density-shift average fill too low: %.3f (want >= 0.80)", avg)
	}
}

func TestControllerSurvivesIntermittentFailures(t *testing.T) {
	// Model a server that rejects any request above 2000 blocks (a hard range
	// limit) on top of a 360 cap: the controller must find a page <= 2000 that
	// still fills to the 360 cap and keep advancing.
	fp := fakePortal{
		capAt: func(uint64) uint64 { return 360 },
		fail:  func(_ uint64, requested uint64) bool { return requested > 2000 },
	}
	fills, _, cursor := runController(fp, 1_000_000, tMax, 80)
	if cursor <= 1_000_000 {
		t.Fatalf("controller never advanced past the failure wall")
	}
	tail := fills[len(fills)-5:]
	for i, f := range tail {
		if f < 0.999 {
			t.Fatalf("did not recover to full responses after failures: tail[%d]=%.3f", i, f)
		}
	}
}

func TestControllerFuzzNeverPanicsOrStalls(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) // fixed seed: deterministic
	for iter := 0; iter < 2000; iter++ {
		caps := uint64(rng.Intn(9000) + 100)
		limit := uint64(rng.Intn(9000) + 100)
		fp := fakePortal{
			capAt: func(uint64) uint64 { return caps },
			fail:  func(_ uint64, requested uint64) bool { return requested > limit },
		}
		_, _, cursor := runController(fp, 0, uint64(rng.Intn(tMax)+tMin), 100)
		// As long as the failure limit allows at least minPage, the cursor must
		// advance; otherwise the controller may legitimately be stuck at min.
		if limit >= tMin && cursor == 0 {
			t.Fatalf("iter %d: controller stalled (caps=%d limit=%d)", iter, caps, limit)
		}
	}
}
