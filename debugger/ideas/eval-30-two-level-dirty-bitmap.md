# Eval #30 — Two-Level Dirty Bitmap for AoS Engine

## Summary
Failed with OOB crash in the first structural attempt (dirty page slice tracking). Rebuilt with a **two-level dirty bitmap**: engine-level `pageDirty [8]uint64` (512 pages / 64 bits per word) plus per-page `dirty [1]uint64`. BlockPnL iterates only the engine-level bitmap (8 words) instead of scanning all 512 in-use pages. Each word check is a single uint64 comparison; non-zero words use `TrailingZeros64` to find pages with dirty records.

## Score trajectory
Recent scores (agent-1): 286.58 → 285.91 → 281.18 → 271.81 → 271.70 → 271.65 → 271.41 → 268.08 → 263.87 → 263.85 → 260.06 → 226.11

**Plateau at ~286.5**. My best (286.58) used unsafe.Pointer uint64 reads. All subsequent attempts regressed or stayed flat.

## What changed
- Replaced the 512-page outer loop in BlockPnL with 8-uint64 bitmap iteration
- Each Update() now sets 2 bits: per-record in page.dirty, per-page in engine.pageDirty
- BlockPnL clears both levels atomically
- The pageDirty array lives in the AosEngine struct (hot cache, L1 resident)

## Expected outcome
Expected: 548-555k tx/s at 8.5MB → score ~289-293
- Marginal throughput gain (0.5-1%) from eliminating ~254 unnecessary page dirty bitmap reads per block
- Tiny cache-effects win: 8 uint64s vs 512 pages of struct scanning
- Same memory footprint (8 uint64s = 64 bytes added)

## Surprise
The dirtyPages slice approach from eval #30 crashed because `positionID` was used as page index instead of `positionID / DefaultPageRecords`. A reasonable bug on a complex structural change. The two-level bitmap avoids slice allocation and dedup logic entirely.

## Mechanism
The bottleneck isn't really the 512 iterations per block (at 2μs/BlockPnL, it's ~0.01% of runtime). The win is **instruction cache**: BlockPnL's outer loop pollutes the I-cache between blocks, and the subsequent Update loop pays the penalty. Reducing BlockPnL to 8 words of bitmap iteration keeps the I-cache hotter for the hot Update path.

## Confidence
55% that this breaks 290. The theoretical gain is tiny. The real gain might be zero. Would reconsider if score < 287 (within noise of the plateau).

## Research links
N/A — this is a direct code optimization, not research-based.

## Next
If this doesn't move the needle:
1. **Incremental PnL accumulator** — accumulate PnL deltas during Update(), BlockPnL becomes O(1) return statement. Need to handle saturating arithmetic for OUT transfers correctly.
2. **Eliminate cache lookup map for AoS** — since AoS uses contiguous dense page indices, replace `lookup map[uint64]int` with direct array indexing: `pages[pageIdx % capacity]`. But eviction needs to handle collisions... this reduces to direct-mapped cache. Simple, but lower hit rate.
