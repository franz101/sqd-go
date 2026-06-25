package ingestion

import (
	"time"

	"github.com/franz101/sqd-go/internal/envconfig"
)

// Portal page sizing.
//
// The SQD portal caps each response at a "cursor size": the largest block SPAN
// it returns for one request, bounded by an internal byte/row budget — so the
// block count of a full response VARIES with data density (a sparse range packs
// thousands of blocks into one response; a dense range only a few hundred).
//
// Requesting fewer blocks than the cap under-fills the round-trip — the live
// indexer's 200-block floor was the dominant reason deep backfill ran at ~40
// blk/s while an uncapped single stream sustains ~180+. Requesting far MORE than
// the cap is harmless — the server simply caps the response — until the range is
// so large the request is rejected or times out.
//
// nextPageSize is the binary-search-style feedback controller the user asked
// for: "set the page size unbound; if you hit the cursor size, make it smaller."
// Starting from a large page, the first response reveals the cap (span <
// requested) and the controller tracks it; a failure (too large / transient)
// halves the page. It re-derives the cap from every response, so it stays
// converged as density shifts across history.
//
//   - span == requested -> did NOT hit the cap; probe higher (x2)
//   - span <  requested -> hit the cap at `span`; request just above it (+25%)
//   - failed             -> too large / transient; binary-search down (/2)
//
// `span` is the block span the response covered (lastBlock - fromBlock + 1),
// NOT the number of rows returned — so the controller behaves identically with
// includeAllBlocks on or off.
func nextPageSize(current, requested, span uint64, failed bool, minPage, maxPage uint64) uint64 {
	if minPage == 0 {
		minPage = 1
	}
	if maxPage < minPage {
		maxPage = minPage
	}
	if current == 0 {
		current = minPage
	}
	switch {
	case failed:
		// Request was too large for the server (reject/timeout) or hit a
		// transient error — halve and retry smaller. This is the binary-search
		// "back off" leg; repeated failures walk it down to minPage.
		return clampUint64(current/2, minPage, maxPage)
	case span == 0:
		// No blocks advanced (a gap with includeAllBlocks=false, or end of data).
		// Hold steady — sizing can't be inferred from an empty response.
		return clampUint64(current, minPage, maxPage)
	case span >= requested:
		// Got the entire requested window: the cap is at least this big, so we
		// under-asked. Probe higher to find the real cap.
		return clampUint64(current*2, minPage, maxPage)
	default:
		// span < requested: the server capped us at `span`. Aim just above the
		// cap so the next response is full without a wasteful over-ask; the +25%
		// headroom absorbs density wobble without oscillating.
		return clampUint64(span+span/4, minPage, maxPage)
	}
}

// clampUint64 returns v bounded to the inclusive range [lo, hi].
func clampUint64(v, lo, hi uint64) uint64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// defaultTargetFetchSeconds is the wall-clock budget a single producer fetch
// should aim to fit inside. nextPageSize alone grows the page toward the portal's
// byte/row cap; on a dense range that cap can still be tens of thousands of blocks,
// so one /finalized-stream request (read whole-response before any block is
// emitted) can take many seconds, starving the consumer between bursts. Capping by
// latency keeps each request short so progress looks continuous rather than bursty.
const defaultTargetFetchSeconds = 6.0

// clampPageForLatency shrinks `page` so a single request finishes in roughly
// targetDur. If the just-completed fetch covered `span` blocks in `fetchDur`, the
// page that would fetch in ~targetDur is span*targetDur/fetchDur. It ONLY ever
// shrinks (never grows) and never below minPage. A non-positive targetDur, a fetch
// already within target, or a zero span leaves `page` unchanged — so a fast fetch
// (e.g. a sparse pre-deployment range) is never throttled.
func clampPageForLatency(page, span uint64, fetchDur, targetDur time.Duration, minPage uint64) uint64 {
	if targetDur <= 0 || fetchDur <= targetDur || span == 0 {
		return page
	}
	scaled := uint64(float64(span) * targetDur.Seconds() / fetchDur.Seconds())
	if scaled >= page {
		return page // never grow via the latency path
	}
	return clampUint64(scaled, minPage, page)
}

// resolveTargetFetchDuration returns the per-fetch latency budget for the dense
// adaptive path. SQD_TARGET_FETCH_SECONDS overrides the default; a value <= 0
// disables the latency clamp entirely (pure byte/row-cap sizing).
func resolveTargetFetchDuration() time.Duration {
	return envconfig.TargetFetchDuration()
}

// resolveStatsInterval returns the periodic-stats cadence used by the consumer's
// stats ticker. SQD_STATS_INTERVAL (a Go duration such as "200ms") overrides the
// default — handy for verbose operation and for tests whose runs are far shorter
// than the 10s default. Unparseable values or anything below 50ms are ignored.
func resolveStatsInterval() time.Duration {
	return envconfig.StatsIntervalDuration()
}
