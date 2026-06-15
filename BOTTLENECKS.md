# Indexer throughput bottlenecks — line by line

Deep-backfill resume of the polymarket indexer ran at **~40 blocks/s**. This
documents every line that held it there, how it was measured, what was fixed,
and what still caps it.

Throughput trajectory: **40 → ~85 sustained (peaks 113–122) blk/s** after the
page-size fix; now **consumer-bound on Pebble cold reads**.

The pipeline: one **producer** goroutine fetches+parses portal pages into a ring
buffer; one **consumer** goroutine drains the ring, applies state, inserts to
ClickHouse. Steady-state rate = `min(producer fill, consumer drain)`.

---

## How it was measured

1. **Built-in phase profiler** — `printProfile`, [internal/ingestion/ingestion.go:1466](internal/ingestion/ingestion.go:1466).
   Prints on shutdown (SIGINT). Accumulators: `profFetchNanos` [:530](internal/ingestion/ingestion.go:530),
   `profCustomNanos` (state apply) [:1014](internal/ingestion/ingestion.go:1014),
   `profConsumerWaitNanos` / `profProducerBackpressureNanos`.
   The 40 blk/s run:
   ```
   FETCH:  12m8s (65%)           <- network fetch dominates
   CUSTOM: 5m14s (28%)           <- state apply (hot/cold/ring)
   INSERT: 38s   (3%)
   WAIT:   consumer=7m3s  producer_backpressure=0s   <- consumer STARVED
   Throughput: avg 79 µs/event
   ```
   Reading: **producer never blocked, consumer idle 55% of wall** → fetch-bound.

2. **Live pprof** — `SQD_PPROF_ADDR=localhost:6060`, wired at [internal/cli/run.go:135](internal/cli/run.go:135).
   CPU profile *after* the page fix (now consumer-bound):
   ```
   pebble.(*getIter).Next                 42.67% cum   <- cold-tier reads
   generated.parseJSONL                   24.05%
   pebble manifest/sstable seek+blockcache ~13%        <- more cold-tier
   abiunpack + math/big (event decode)    ~8%
   hot-ring idxLookup + clockHash64        ~5% (cheap, O(1))
   ```

---

## BOTTLENECK 1 — Fetch: page pinned at 200  [FIXED ✓]

**The single binding constraint at 40 blk/s.** The portal caps each response at a
*cursor budget* holding **370–2900 blocks** (varies with density); requesting only
200 under-filled every round-trip.

| line | role |
|---|---|
| [ingestion.go:438](internal/ingestion/ingestion.go:438) | **ONE** producer goroutine — all fetches serialized |
| [ingestion.go:528](internal/ingestion/ingestion.go:528) | `FetchWithParent(...)` — 4.19 s per 200-block page = **48 blk/s**, 65% of wall |
| [ingestion.go:441](internal/ingestion/ingestion.go:441) | page starts at `minAdaptivePageSize` and was driven *down* by the bug below |

**Root cause — the page-sizing signal was permanently saturated:**

| line | the bug |
|---|---|
| (old) `adjustAdaptivePageSize` [ingestion.go:1576](internal/ingestion/ingestion.go:1576) | halves the page when `buffered >= capacity*3/4` ([:1584](internal/ingestion/ingestion.go:1584)) |
| fed `replayBuf.Len()` | `Len()` [replay.go:375](internal/ingestion/replay.go:375) returns `count`, which **saturates at capacity (8192)** ([replay.go:95](internal/ingestion/replay.go:95)) and is only decremented by `PruneBefore` [replay.go:314](internal/ingestion/replay.go:314) — **never called at runtime** |

So after the first 8192 blocks the signal read 8192 forever → halve every tick →
page floored at 200. Proof: [internal/ingestion/adaptive_pagesize_bug_test.go](internal/ingestion/adaptive_pagesize_bug_test.go).

> Note: `buffered: 8192` in the stats log ([ingestion.go:851](internal/ingestion/ingestion.go:851),
> fed by `replayBuf.Len()` [:852](internal/ingestion/ingestion.go:852)) is therefore **not** a
> lag gauge — it saturates. The *real* producer backpressure gate uses true lag
> `pBlock - cBlock >= capacity-100` at [ingestion.go:495](internal/ingestion/ingestion.go:495)
> (which never fired → consumer-starved, matching the profiler).

**The fix — binary-search page controller** ([internal/ingestion/pagesize.go](internal/ingestion/pagesize.go)):

| line | role |
|---|---|
| [pagesize.go](internal/ingestion/pagesize.go) `nextPageSize` | probe up (`span==requested`), track cap (`span<requested → span*1.25`), halve on `failed` |
| [ingestion.go:453](internal/ingestion/ingestion.go:453) | wired in place of `adjustAdaptivePageSize` |
| [ingestion.go:542](internal/ingestion/ingestion.go:542) | binary-search backoff on fetch error |
| [ingestion.go:57](internal/ingestion/ingestion.go:57) | bounds `minAdaptivePageSize=200 .. maxAdaptivePageSize=100000` |

**Validated:** 11 unit tests ([pagesize_test.go](internal/ingestion/pagesize_test.go)) +
real-portal stage suite ([fetch_stages_integration_test.go](internal/ingestion/fetch_stages_integration_test.go)):

| stage | density (event-blocks) | adaptive | fixed-200 | speedup |
|---|---|---|---|---|
| 23M | 13% | 3026 blk/s (page→2378) | 784 | 3.86× |
| 58M | 22% | 3193 blk/s (page→2912) | 673 | 4.74× |
| 85M | 100% | 210 blk/s (page→438) | 175 | 1.20× |

Live effect: page 200→~480, **40 → ~120 blk/s fetch; producer overtook consumer.**

---

## BOTTLENECK 2 — Consumer: Pebble cold-tier reads  [CURRENT WALL, ~55% CPU]

Once fetch was unblocked, the consumer became the wall at ~85 blk/s. The live CPU
profile shows **Pebble cold reads = ~55% of CPU** — the O(log n) LSM lookups at
100M+ keys (your "small scale O(1), big scale log n").

| line | role |
|---|---|
| [ingestion.go:975](internal/ingestion/ingestion.go:975) | consumer pulls next block `replayBuf.GetBlock(currentConsumerBlockVal)` |
| `generated.parseJSONL` | 24% — producer-parse path decodes the page |
| [generated/hotstate.go:927](examples/polymarket/generated/hotstate.go:927) | `UserPositionsClockCache.Get` — hot miss falls to cold |
| [generated/hotstate.go:937](examples/polymarket/generated/hotstate.go:937) | `c.cold.GetInto(...)` on hot miss |
| [coldcache.go:298](internal/coldcache/coldcache.go:298) → [:302](internal/coldcache/coldcache.go:302) | `Store.GetInto` → `db.Get` — **Pebble LSM read = `getIter.Next` 42.67%** |
| [generated/hotstate.go:364](examples/polymarket/generated/hotstate.go:364), [:1451](examples/polymarket/generated/hotstate.go:1451), [:1975](examples/polymarket/generated/hotstate.go:1975), [:2500](examples/polymarket/generated/hotstate.go:2500) | same cold-consult for the other entities |
| [generated/hotstate.go:846](examples/polymarket/generated/hotstate.go:846) `idxLookup` + [:3557](examples/polymarket/generated/hotstate.go:3557) `clockHash64` | hot-ring lookup — **O(1), cheap (~5%)**, NOT the problem |

**Why:** after recovery the hot ring is empty for the 100M+ `UserPositions`
working set; every access misses hot → Pebble read → promote. Once the ring fills
it evicts, pushing reads back to cold — which is why the live rate warmed to ~113
then settled to ~85.

**Levers toward 200 (cold reads are concurrency-safe → parallelizable):**
- Batch + parallelize per-block cold lookups via `PrefetchParsedBlocks` [ingestion.go:1000](internal/ingestion/ingestion.go:1000).
- sstable-per-entity so entities fetch in parallel (task #9).
- Parallel `custom` (shard state apply by key hash).
- Larger hot ring / better retention to cut the miss rate.

---

## BOTTLENECK 3 — Client buffers the whole 60 MB response  [secondary, 1.35×]

| line | issue |
|---|---|
| [client.go:147](internal/client/client.go:147) | `io.ReadAll(resp.Body)` — reads the entire ~60 MB body before decoding |
| [client.go:163](internal/client/client.go:163) | `zstdDecoder.DecodeAll(raw, ...)` — no receive/decode overlap, 60 MB alloc/page → GC |
| [client.go:85](internal/client/client.go:85) / [:124](internal/client/client.go:124) | compression is fine (zstd, manual) — not the issue |

Measured: a **streaming** decoder (decode as bytes arrive) gives **1.35× per
stream** (78 vs 57 blk/s single-stream). Use `zstd.WithDecoderConcurrency(1)` to
avoid a goroutine explosion under parallel fetch.

---

## BOTTLENECK 4 — `includeAllBlocks=true` in deep backfill  [secondary, sparse stages]

| line | issue |
|---|---|
| [ingestion.go:528](internal/ingestion/ingestion.go:528) | passes `cursorMode` as `includeAllBlocks` — always true |

At 23M/58M **77–87% of blocks are empty** (no polymarket events). Fetching them is
pure transfer waste for a *full re-sync from 23M*. `includeAllBlocks=false` skips
them, but returns **non-contiguous** block numbers → the consumer's
`GetBlock(prev+1)` at [ingestion.go:975](internal/ingestion/ingestion.go:975) must
advance to the next *available* block (safe: skipped blocks carry no events; only
skip when `N < latestBlock`). At 85M (100% event-blocks) there is no gap, so no
benefit there. This is the "backfill" of the three stages (backfill /
backfill-unfinalized / live).

---

## BOTTLENECK 5 — Dead hardcoded prefetch in the consumer  [minor + codegen smell]

| line | issue |
|---|---|
| [internal/codegen/custom_processor.go:1072-1090](internal/codegen/custom_processor.go:1072) | codegen emits a hardcoded per-event `UserPositionsResolver.Queue(...)` loop |
| [custom_processor.go:1165](internal/codegen/custom_processor.go:1165), [:1176](internal/codegen/custom_processor.go:1176) | same in the proto path |

Runs for every `ExchangeOrderFilled` / `NegRiskExchangeOrderFilled` event even when
the cold tier is authoritative (the resolver Resolve is then a no-op) — wasted
queue/alloc work inside `CUSTOM`. Also a **codegen genericity violation**
(polymarket entity names hardcoded in `internal/codegen`) tracked separately.

---

## Summary

| # | bottleneck | where | status |
|---|---|---|---|
| 1 | page pinned at 200 (saturated `Len()` signal) | [ingestion.go:453](internal/ingestion/ingestion.go:453), [replay.go:375](internal/ingestion/replay.go:375) | **FIXED** → 40→~120 fetch |
| 2 | Pebble cold-tier reads (55% CPU) | [hotstate.go:937](examples/polymarket/generated/hotstate.go:937), [coldcache.go:302](internal/coldcache/coldcache.go:302) | **current wall** ~85 blk/s |
| 3 | whole-response buffering | [client.go:147](internal/client/client.go:147) | open (1.35×) |
| 4 | includeAllBlocks in sparse backfill | [ingestion.go:528](internal/ingestion/ingestion.go:528) | open (full re-sync) |
| 5 | dead hardcoded prefetch | [custom_processor.go:1080](internal/codegen/custom_processor.go:1080) | open (minor + codegen) |
