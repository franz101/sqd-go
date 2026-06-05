# Profiler Run: Block 40M, 90 Seconds, `profile_run` Database

## Result Summary

| Metric | Value |
|---|---|
| Blocks processed | 296,497 |
| Wall time | 84s |
| Avg throughput | **3,534 blk/s** |
| Recent 10s windows | 3,600–4,500 blk/s |
| Events processed | 18,076 |
| Memory allocated | 864 MB |

## Built-In Profile Breakdown

```
FETCH:   1m13s  (61%)   ← network-bound, waiting on portal.sqd.dev
PARSE:   494ms  (0%)    ← fast, 3.9ms/iter over 127 batches
DECODE:  92ms   (0%)    ← negligible
INSERT:  2.4s   (2%)    ← ClickHouse async inserts are cheap
CUSTOM:  44.4s  (37%)   ← custom processor (polymarket PnL logic)
WAIT:    34.7s consumer wait, 0s backpressure
```

## CPU Profile Breakdown (pprof, 4s sampled out of 85s)

| Category | % of CPU | Notes |
|---|---|---|
| syscall (network I/O) | 21% | HTTP fetch to portal |
| GC (scanObject) | 20% | Scanning 864MB of live heap |
| runtime.madvise | 11% | OS memory management |
| ClickHouse ch-go client | 13% | Reading/writing protocol frames |
| Custom processor state | 6% | `UserPositionsClockCache.Range`, `saveSnapshotLocked` |
| ReplayBuffer.Write | 2% | Cloning events into ring buffer |
| zstd decompress | 1% | Portal response decompression |

## The 3 Bottlenecks Standing Between You and 10K blk/s

### 1. FETCH is 61% of wall time — you're network-bound on portal.sqd.dev

The portal returns ~2,300–2,700 blocks per HTTP round trip. Each round trip takes 500ms–1.5s including TLS overhead. With page sizes of 5,000 blocks, the portal often returns partial pages. You're doing ~127 fetches for ~296K blocks = ~2,335 blocks/fetch.

**What would 3x this:**
- **Parallel fetches**: The producer goroutine is single-threaded. Fire 2–3 concurrent range requests to the portal (e.g., goroutine pool with non-overlapping ranges). The consumer already handles out-of-order via the ReplayBuffer index. This alone could push to 8–10K blk/s.
- **Larger page sizes**: The adaptive page sizing starts at `minAdaptivePageSize = 5000` and caps at `maxAdaptivePageSize = 100000`. At block 40M the event density is low (~60 events per 5K blocks). You could request 50K–100K block ranges and the portal would likely serve them since log density is sparse here. Pass `--page-size 50000` or raise `minAdaptivePageSize`.
- **HTTP connection reuse / keep-alive**: The client creates a fresh `http.Transport` with `DisableCompression: true` but no explicit connection pooling tuning. The default `MaxIdleConnsPerHost=2` is fine but check if TLS handshakes are repeated (they cost ~100ms each). Pin `MaxConnsPerHost` higher if running parallel fetches.

### 2. CUSTOM processor is 37% — the polymarket PnL logic dominates after fetch

The custom processor runs **synchronously** on the consumer goroutine, blocking the next batch. The pprof shows `UserPositionsClockCache.Range` and `saveSnapshotLocked` as the hot spots — iterating the full position map and snapshotting state every batch.

**What would 3x this:**
- **Batch accumulation**: Right now every `isLastInBatch` triggers the custom processor with that batch's logs. If you buffer 3–5 batches of logs and fire the processor once with ~10K+ blocks of events, the per-invocation overhead (snapshot, range) amortizes dramatically. The processor already handles multi-block batches.
- **Snapshot frequency**: `saveSnapshotLocked` runs a `Range` over all user positions every batch. Profile shows `HashTrieMap.iter` at 5.8% cumulative — iterating the entire concurrent map. Snapshot every N batches or every K seconds instead of every batch.
- **Decouple from critical path**: Run the custom processor in a separate goroutine. The ClickHouse inserts use `async_insert` already — let the processor lag behind by a few batches. The consumer only needs to block on the processor for fork recovery correctness, not for every batch.

### 3. GC pressure — 20% of CPU is scanning 864MB of live objects

5.2M allocations in 84 seconds. The ReplayBuffer clones every event (`cloneDecodedEvent`, `cloneCustomLog`) allocating new slices and maps per block. The `shopspring/decimal` library allocates heavily. `big.Int` operations in the crypto helpers (collection ID, position ID) allocate freely.

**What would 3x this:**
- **Pool/reuse allocations**: The replay buffer clones events into freshly allocated slices. Use `sync.Pool` for the common allocation sizes or pre-allocate a slab of DecodedEvent/CustomLog structs that rotate with the ring buffer.
- **Reduce live heap**: The ReplayBuffer holds 8,192 blocks of cloned data simultaneously. Most of that will never be needed (fork recovery is rare). Consider a tiered approach: keep the last 256 blocks in memory, spill older blocks' raw bytes to a memory-mapped file.
- **Use `GOGC=200` or `GOMEMLIMIT`**: At 864MB allocation, the default GOGC=100 triggers GC too frequently. `GOGC=200` would halve GC frequency at the cost of ~1.7GB peak heap. Since you're not memory-constrained on a dev machine, this is free throughput: `GOGC=200 make dev-e2e`.
- **Avoid `decimal.Decimal` in the hot path**: Every position update does `toDecimal()`/`fromDecimal()` round-trips through `big.Int`. The protomath `Decimal256` type exists specifically to avoid this — the V2 proto path bypasses it but V1 doesn't.

## Quick Wins (No Code Changes)

```bash
# 1. Raise GOGC to reduce GC pressure (~10-15% improvement)
GOGC=200 CLICKHOUSE_DATABASE=profile_run make dev-e2e E2E_START_BLOCK=40000000

# 2. Use larger page sizes (~20% improvement if portal supports it)
CLICKHOUSE_DATABASE=profile_run make dev-e2e E2E_START_BLOCK=40000000 POLYMARKET_ARGS="--page-size 50000"

# 3. Use V3/proto mode if available (bypasses decimal round-trips)
CLICKHOUSE_DATABASE=profile_run make dev-v3

# 4. Set GOMAXPROCS higher if on a multi-core machine
GOMAXPROCS=8 GOGC=200 make dev-e2e E2E_START_BLOCK=40000000
```

## Estimated Impact to Reach 10K blk/s

| Change | Est. Impact | Effort |
|---|---|---|
| Parallel portal fetches (2–3 workers) | 2–3x on fetch-bound batches | Medium — refactor producer goroutine |
| Larger page sizes (50K) | 1.3–1.5x | Trivial — flag change |
| GOGC=200 | 1.1–1.15x | Zero — env var |
| Batch custom processor (5 batches) | 1.2–1.3x on processor time | Small — accumulate logs |
| Reduce snapshot frequency | 1.1x on processor time | Small — add counter |
| Proto mode (V2/V3) | 1.2–1.5x on processor time | Already exists |

**Combined realistic estimate: 3,500 × 2.5 (parallel fetch) × 1.15 (GC) × 1.2 (processor) ≈ 12,000 blk/s**

The single biggest lever is **parallel portal fetches**. Everything else is incremental. If the portal API supports concurrent range requests to the same dataset, 2–3 parallel workers with staggered block ranges would break 10K blk/s without touching anything else in the pipeline.

## Raw Profile Output

CPU profile saved to: `tmp/cpu.prof`
Run log saved to: `tmp/profile_output.log`

Analyze interactively:
```bash
go tool pprof -http=:6060 tmp/cpu.prof
```
