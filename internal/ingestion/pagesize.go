package ingestion

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
