# Eval #9: PnL Accumulator + Write-Through Cache — Reflection

**Score: 98.80** | **Commit: 711789ac** | **Engine: FlatLookup + DirectCache + DirtySlot**

## What Changed vs Prior

| Attempt | Engine | Throughput | Memory | Score | Key Change |
|---------|--------|-----------|--------|-------|------------|
| #6 (4e446f7b) | FlatLook | 255.1k tx/s | 30.1MB | 91.82 | Flat lookup engine baseline |
| #7 (de801164) | FlatLook+DC | 268.8k tx/s | 30.1MB | 96.78 | Direct-mapped cache replaces Go map |
| #8 (8d59af39) | FlatLook+DC | 253.1k tx/s | 32.1MB | 89.36 | 32 rec/page (2KB) — regression |
| #9 (711789ac) | **FlatLook+DC+DS** | **268.8k tx/s** | **28.1MB** | **98.80** | DirtySlot (16B) replaces DirtyEntry (~88B), write-through cache |

Score progression: 45.02 → 71.90 → 91.82 → 96.78 → **89.36** → **98.80**

## Why Eval #9 Worked

### DirtySlot: 16 bytes vs 88 bytes per entry

The old `DirtyEntry` stored a full `Position` struct (64 bytes) plus `pageIdx` (8) and `localIdx` (8) plus map overhead (~8 bytes for the key). That's ~88 bytes per dirty position. With ~16,384 entries per block, that's ~1.4MB of dirty map overhead.

The new `DirtySlot` stores only `initialPnl0` and `initialPnl1` (2 × uint64 = 16 bytes). That's 256KB per block — saving ~1.2MB.

**How it works:**
- On `Update()`: read position from cache, compute initial PnL, apply update, write-through to cache. Store only the initial PnL in the dirty map.
- On `BlockPnL()`: re-read each dirty position from cache (via GetPage), compute current PnL, delta from initial PnL = block contribution. Then flush dirty pages to disk.
- The position data always lives in the cache — we never hold a stale copy in Go heap.

### Write-through cache eliminates write-back on Flush

The old `HashEngine.BlockPnL()` iterated dirty entries and called `page.WriteRecord()` for each. With the new approach, the page already has the updated position data because `Update()` writes through. BlockPnL just reads the fresh data from cache, computes the delta, and flushes dirty pages to disk.

### Memory savings: 30.1MB → 28.1MB (2MB reduction)

The 2MB comes from:
- Dirty map: ~1.4MB → ~256KB (-1.2MB)
- Go heap pressure: The old approach allocated a `DirtyEntry` per Update, which caused more GC pressure and heap fragmentation. The smaller map is more GC-friendly.
- Incidental: some object header overhead eliminated by the smaller struct.

### Throughput unchanged at 268.8k tx/s

The write-through pattern adds a cache read+write per Update (which was already happening in the old code via the dirty entry), and the BlockPnL re-reads from cache instead of iterating the dirty map's Position structs. These roughly cancel: the old code had to write back through cache in BlockPnL; the new code reads from cache in BlockPnL. Both are cache operations, so throughput is equivalent.

## Surprises

### Eval #8 regression: larger pages hurt, not helped

Page size increase was a **blind alley**. 32 rec/page (2KB) regressed from 96.78 to 89.36 despite my prediction of improvement. Why?

- **More I/O per miss**: 2KB read per miss instead of 1KB. With random access, the extra bytes are wasted because positions within the same page are not accessed together in shuffled order.
- **Same page count**: Going from 16→32 rec/page means each cache page holds more positions. But with 2048 cache pages × 32 = 65,536 positions cached vs 2048 × 16 = 32,768. Even with more positions cached, the read amplification penalty (2× bytes per miss) outweighs the small hit-rate improvement.
- **Memory up to 32.1MB**: Extra 2MB from the larger page buffers (2048 × 2KB = 4MB cache vs 2048 × 1KB = 2MB).

The lesson: **smaller pages are strictly better for shuffled random access** because read amplification dominates cache hit rate.

### Eval #9 throughput didn't improve despite cleaner architecture

I expected some throughput gain from write-through (no separate write-back pass), but the BlockPnL re-read from cache costs about the same as the old write-back. The net throughput is identical — the benefit is exclusively in memory.

## Current Bottleneck Analysis

The FlatLookup engine at 268.8k tx/s and 28.1MB is approaching fundamental limits:

1. **Lookup table**: 20MB irreducible for 5M × uint32
2. **Go heap overhead**: ~5MB for GC metadata, goroutine stacks, internal bookkeeping
3. **Cache pages**: 2MB for 2048 × 1KB
4. **Dirty map**: negligible (~256KB for ~16K DirtySlots)
5. **Bitmap**: 625KB for 5M-bit membership check

Total floor: ~28MB. To get to 100 score, we need either:
- **Throughput > 289k** at current memory (289 / 28.1^0.3 = 289 / 2.74 = 105.5)
- **Memory to 25MB** at current throughput (268 / 25^0.3 = 268 / 2.63 = 101.9)

### Throughput ceiling: ~290-300k?

With shuffled 40-byte binary transfers and Go's io.ReadFull loop, the raw CPU-bound decode + lookup + cache update runs at some maximum. Let's estimate:
- Transfer decode: ~10ns (fixed bytes, no branches)
- Bitmap check: ~5ns (word + bit)
- Lookup: ~5ns (array access)
- Cache GetPage: ~50-100ns (direct-mapped: branch, maybe page read)
- Position update: ~20ns (arithmetic)
- Dirty map store: ~30ns (Go map insert)
- Loop overhead: ~10ns

Total per transfer: ~130-180ns = 5.5-7.7M tx/s per core. But wait — those are CPU cycles. The real bottleneck is **I/O or cache misses**. At 268k tx/s, we're using only ~3-5% CPU. The remaining time is:

Wait, 500,000 tx in 1.9s = 268k tx/s. That's 3.73µs per transfer. In CPU terms: ~32 cycles at 8.6GHz? No — deepseek's benchmark probably runs on a normal x86 machine at ~2-3GHz, so 3.73µs = ~7,500-11,000 cycles per transfer. That's way more than the CPU cost would suggest. What's taking the time?

**It's ReadAt syscalls.** Each cache miss triggers a 1KB ReadAt. With 8,778 first-touches per block × 50 blocks = 438,900 cache misses. Each ReadAt on a file opened O_RDWR with no O_DIRECT probably takes ~2-5µs (seek + wait for data — but the file is small enough to be in OS page cache after the first block). 

Actually, on a modern Linux system with the file in the kernel's page cache:
- ReadAt on a cached file: ~0.5-1µs per 4KB page (memcpy from kernel to user space)
- A 1KB ReadAt reads at most 1 4KB kernel page, but may be partial

With 438,900 misses × 0.5µs = 220ms total read time. Plus 500,000 loop iterations × 0.5µs CPU = 250ms. Total ~470ms. But we see 1.9s wall time. Where's the other 1.4s?

- Go GC: with 28MB heap, Go's GC runs periodically, causing STW pauses
- OS scheduling: we're competing with other processes
- BlockPnL iteration: ~16K dirty entries × cache reads per block × 50 blocks = 80K GetPage calls in BlockPnL. Each GetPage on a cached page is just a check and index (% cap). Should be ~50ns. 80K × 50ns = 4ms. Negligible.
- Cache Flush: 2048 pages per block, only dirty ones get written back. With ~8K dirty positions spread across ~8K pages (assuming low density per page), that's ~8K writePage calls × 0.5µs = 4ms per block × 50 = 200ms.
- ReadAt for the cache misses: maybe each costs 1-2µs due to kernel overhead

Wait — the 438,900 first-touches don't all cause misses. Many positions are in the same page (16 per page), and with direct-mapped cache, a page can survive eviction if its slot isn't contested. But in shuffled order, there's little reuse within a block.

Let me reconsider: 438,900 positions touched across 50 blocks = 8,778 per block. Each block: first touches go to ~8,778 different positions. With 16 rec/page and shuffled positions, each first-touch is likely on a different page. So ~8,778 page misses per block. Cache is 2048 pages. By the end of a block, most pages have been evicted and re-read. The net: ~8,778 ReadAt calls per block × 50 = 438,900 total.

At 1µs per ReadAt (cached file): 439ms read time.

What accounts for the rest of wall time? Could be GC. Let me check if `runtime.GC()` is being called mid-stream.

Looking at `main.go`: `runtime.GC()` is called once before processing starts and once at the end. Not mid-stream. So GC runs periodically during the 500K transfers.

With a 28MB heap, Go's concurrent GC runs ~every few hundred allocations. The main loop does:
- Allocate Transfer struct? No — it's on the stack
- Dirty map insert (when not already dirty): each insert allocates a map entry
- First-touches: 438,900 map inserts over 50 blocks, cleared each BlockPnL

Each map insert allocates ~40-50 bytes for the key-value pair. That's ~20MB of allocation over the run. GC scans this periodically, but with a small heap, STW pauses should be <1ms.

1.9s for 500K tx at 268k tx/s = ~3.73µs/tx. With zero allocation in the hot path (except dirty map inserts on first touch), the per-tx time includes:

For a first-touch position (87.9% are first-touch):
- `e.exists[word]` bitmap check — fast
- `e.lookup[posID]` — array access
- `e.cache.GetPage(pageIdx)` — direct-mapped: modulus, array access, branch (hit: just check and return, miss: ReadAt + fill)
- `page.ReadRecord(localIdx)` — memcpy Position from page
- `UpdatePosition` — arithmetic
- `page.WriteRecord(localIdx)` — memcpy + dirty bit set
- `e.dirty[posID] = DirtySlot{...}` — map insert (alloc)

For a repeat-hit position (12.1%):
- Same minus the map insert (already dirty) and the cache miss path

Map insert allocates, but Go's GC is concurrent. The cost is not STW but allocation throughput.

**My hunch: the bottleneck is the combination of map inserts (allocations) + cache miss ReadAt calls.** At ~1µs per ReadAt and ~1µs per allocation+overhead, we get ~2µs per first-touch + ~0.5µs per repeat-hit = weighted average ~1.8µs. 500K × 1.8µs = 900ms. Plus cache Flush: ~200ms. Plus BlockPnL iteration: ~200ms (16K GetPage per block × 50 blocks = 800K GetPage calls). 

That totals ~1.3s. We see 1.9s. The 0.6s gap is likely Go GC, scheduling, and temperature effects (CPU throttling, NUMA locality, etc.).

## Headroom Analysis

### Throughput headroom

The biggest cost item is the **cache miss ReadAt syscall** (~1µs each, 439K of them = 439ms). There's no way to avoid reading position data from disk — the data must come from somewhere. But we can:
1. **Reduce ReadAt cost** — batch nearby reads (but positions are shuffled, so no spatial locality)
2. **Eliminate redundant ReadAt** — the cache already prevents this (direct-mapped)
3. **Optimize the file access pattern** — mmap? O_DIRECT? memory-mapped I/O has different semantics

### Memory headroom

We're at 28.1MB. The floor is:
- Lookup: 20MB (5M × uint32) — irreducible
- Cache: 2MB (2048 × 1KB) — could go to 1MB (1024 pages)
- Bitmap: 0.6MB (78K × uint64) — irreducible
- Go runtime: ~5MB — irreducible
- **Total floor: ~27.6MB** (we're basically there)

Going to 1024 cache pages would save 1MB but likely hurt throughput (more misses, more ReadAt).

### Score ceiling at current architecture

Best realistic score: 275k / 28^0.3 = 275 / 2.74 = 100.4
Stretch score: 280k / 28^0.3 = 280 / 2.74 = 102.2

## Next Steps

### Candidate 1: Remove dirty map entirely with incremental PnL (HIGH confidence, highest impact)

The dirty map stores `initialPnl0` and `initialPnl1` for each touched position. But what if we don't need it at all?

**Idea**: Accumulate PnL incrementally during Update(). On BlockPnL, just commit and flush. No dirty iteration needed.

How: Instead of recording initial PnL and computing the delta at block end, compute the PnL delta per-update:

```
delta0 = PnL_after - PnL_before  (from cache)
blockPnL0 += delta0
```

But `PnL()` requires both `Amount` and `InitialAmt` fields. After the first update in a block, `Amount` changes but `InitialAmt` stays the same. So PnL before the first update vs PnL after the first update gives us the delta. For subsequent updates in the same block, we also accumulate the delta.

The problem: `PnL = (Amount + Fees) - InitialAmt`. After update 1: `Amount' = Amount + transfer1`. After update 2: `Amount'' = Amount' + transfer2`. The PnL after update 2 is `(Amount'' + Fees'') - InitialAmt`, and the total block delta is `PnL'' - initial_PnL`. If we just accumulate `PnL_after - PnL_before` per update, we get:

```
sum( (Amount_i + Fees_i) - InitialAmt - ((Amount_{i-1} + Fees_{i-1}) - InitialAmt) )
= sum( (Amount_i - Amount_{i-1}) + (Fees_i - Fees_{i-1}) )
= (Amount_last - Amount_0) + (Fees_last - Fees_0)
= (Amount_last + Fees_last) - (Amount_0 + Fees_0)
≠ (Amount_last + Fees_last - InitialAmt) - (Amount_0 + Fees_0 - InitialAmt)  ← these are the same!
```

Yes! The `InitialAmt` cancels out in the per-update delta. So we can accumulate per-update deltas without storing initial PnL at all.

**Implementation:**
1. Remove `dirty` map entirely
2. Add `blockPnL0, blockPnL1` to the engine struct
3. In `Update()`, before applying the transfer: read position from cache, compute current PnL, apply update, write to cache, compute new PnL, add delta to `blockPnL0/1`
4. `BlockPnL()` just returns `blockPnL0, blockPnL1, count` and calls `cache.Flush()`

Wait — but we need the per-position first-touch to compute this delta. For positions already dirty in this block (second+ touch), we need to track the previous PnL. But the dirty map stored initial PnL, not deltas. Second+ touches still update from cache, so we'd need to track the previous PnL for each dirty entry to compute the delta correctly.

Actually, reconsider: The PnL computation is:
```
PnL = (Amount + Fees) - InitialAmt
```

Each `Update()` modifies Amount and Fees. Between updates on the same position in a block:
```
PnL_before = (Amount_old + Fees_old) - InitialAmt
PnL_after  = (Amount_new + Fees_new) - InitialAmt
delta = PnL_after - PnL_before = (Amount_new - Amount_old) + (Fees_new - Fees_old)
```

The delta is just the change in Amount + the change in Fees. That's determined solely by the transfer! We don't need to read the position at all to compute the per-transfer delta.

```
delta0 = transfer_amount (if token==0) + fee_added (which is transfer_amount/300)
delta1 = transfer_amount (if token==1) + fee_added
```

Wait no — Direction matters. If direction is "out" (pool sends), the amount *decreases* the pool's amount.

Let me trace through:
- If token==0, direction=="in": Amount0 increases by transfer.Amount, Fees0 increases by transfer.Amount/300
  - PnL delta = Amount0_delta + Fees0_delta = transfer.Amount + transfer.Amount/300
- If token==0, direction=="out": Amount0 decreases by transfer.Amount, Fees0 increases by transfer.Amount/300
  - PnL delta = -transfer.Amount + transfer.Amount/300
- Same for token==1.

This means the PnL delta per transfer is purely a function of the transfer bytes — no need to read from cache, no need to store dirty state!

**We can compute PnL delta without any cache read at all.**

```go
func transferPnLDelta(t Transfer) (int64, int64) {
    fee := t.Amount / 300
    if fee == 0 { fee = 1 }
    if t.Token == 0 {
        if t.Direction == 0 { // in
            return int64(t.Amount + fee), 0
        } else { // out
            return int64(fee) - int64(t.Amount), 0
        }
    } else {
        if t.Direction == 0 {
            return 0, int64(t.Amount + fee)
        } else {
            return 0, int64(fee) - int64(t.Amount)
        }
    }
}
```

We still need to apply the update to the position in cache (so write-through works), but the PnL accumulation is free — we just add the transfer's inherent delta.

**This eliminates the dirty map entirely.** No map alloc, no iteration, no BlockPnL computation.

**Expected impact:**
- Memory: 28.1MB → ~27MB (save ~1MB from dirty map hash table + DirtySlot structs + map metadata)
- Throughput: 268.8k → 275-285k (save ~30ns per first-touch from no map insert, plus BlockPnL is now just Flush)
- Score: ~98.8 → maybe up to ~102

But wait — the map insert is ~30ns per first-touch, and with 439K first-touches, that's ~13ms saved. The BlockPnL iteration of 800K GetPage calls is maybe ~40ms. Total CPU savings ~53ms out of 1.9s = ~3%. That might push throughput from 268k to 276k.

**Score estimate: 276 / 27^0.3 = 276 / 2.64 = 104.5**

Hmm, that's optimistic. Let me be more conservative: removing the dirty map saves ~500KB-1MB of hash table overhead, maybe throughput gain 1-2%.

Conservative: 272k / 27.5^0.3 = 272 / 2.67 = 101.9

Still, **this could break 100**.

### Candidate 2: Inline the dirty bitmap into the lookup table (lower confidence)

Currently: lookup[5M]uint32 = 20MB. The exists bitmap is separate (625KB). 

Idea: Use the high bit of the uint32 as the "exists" flag. The position count is ~3M, which fits in 22 bits. We have 32 bits per entry. Example: `lookup[posID] = arrIdx | 0x80000000` for existing, `lookup[posID] = 0` for non-existent.

Then the exists bitmap disappears:
- Memory: -625KB (negligible for scoring)
- Throughput: saves one word/bit operation on the hot path
- Code complexity: need to mask the high bit

Not worth it — 625KB doesn't move the needle, and it complicates the lookup.

### Candidate 3: No cache at all? (low confidence)

With 268.8k tx/s and ~439K unique touches (87.9% unique), the cache is actually counterproductive? Each first-touch reads from disk (cache miss), loads a page, writes to it, and then the page might be evicted before it's dirtied again. With 12.1% repeat touches and 2048-page cache, maybe only a fraction of repeats hit the cache.

Actually, the cache is essential for the write-back path: positions are dirtied in cache pages, and at BlockPnL the cache is flushed to disk. Without the cache, every Update would need a ReadAt + WriteAt pair (position not cached). The cache batches writes: writes happen only at Flush time.

No — with write-through, every Update already writes to cache. The cache *is* the data store. Without it, we'd need direct file operations per Update, which would be much slower.

### Candidate 4: Different cache policy (medium confidence)

Direct-mapped cache can thrash under certain access patterns. But the existing throughput (268.8k) suggests it doesn't thrash badly. A set-associative cache (2-4 ways) would reduce conflict misses but add complexity.

I think the incremental PnL (Candidate 1) is the next clear win. Then we hit I/O limits, and further improvements require architectural changes.

## Target

- **Candidate 1 (incremental PnL, no dirty map)**: 275k tx/s, ~27MB → **score ~102**
- **Stretch**: 280k tx/s, ~27MB → **score ~106**
- **If that hits I/O ceiling at ~275k**: pivot to mmap or O_DIRECT to squeeze more

