# Three levers for "combining it all" — implemented, proven, and scoped

Response to the measured reality that the single-stage wins (byte-scanner parse,
protomath fold) don't compound on the real 22-event polymarket workload, and to
the two correct objections: (a) most CUSTOM cost is ClickHouse lookup latency,
(b) sharding interferes with condition/position creation.

All claims here are measured by tests in this package (`go test ./examples/polymarket/`)
and the latency report in `experiments/REPORTING/LOOKUP_LATENCY_MEASURED.md`.

## The reframing that drove the work

The 20ms lookup is **per-query, not per-key**, and is driven by **part count, not
table size**: a 1000-key `WHERE IN` ≈ a 1-key query (~4ms at 1 part), and ~20ms
== ~240 unmerged parts on a `ReplacingMergeTree` during active backfill (measured,
`LOOKUP_LATENCY_MEASURED.md`). So the CUSTOM stage has two regimes:
**lookup-bound** (resume/cold-warmup) and **compute-bound** (warm burst). Each
wants a different lever.

## Lever 1 — kill/amortize the 20ms lookups (SHIPPED)

The hand-written `ProcessProto` batched conditions + FPMM per block but **not
positions**, so every position hot-miss fell to the inline synchronous resolve
(`generated/state.go:95`) — the dominant lookup-bound cost.

- **`ensurePositionsLoaded`** (`custom_processor.go`) batches the block's
  OrderFilled position keys (all four variants → maker + outcome tokenID) into one
  resolver round-trip, mirroring `ensureConditionsLoaded`. Covers the dominant
  block-84M event. **Cache-warm only**: a missing/wrong key falls back to the
  inline resolve, never corrupts; honors `ColdAuthoritative` (from-genesis = zero
  round-trips). Composes with the framework's `--prefetch` cross-block path (which
  already batches OrderFilled positions; my pass then no-ops on hot-hits).
- Status: **shipped, builds, existing correctness tests pass.** Other position
  events (split/merge/converted/redemption/FPMM) still fall back to inline until
  their key derivations are added here too — a safe, incremental extension.

### Lever 1b — async-resolve: investigated, folds into Lever 3

`StartResolveAllPending` (`hotstate.go:4215`) exists and is unit-tested but has
**no live caller**; only the synchronous `ResolveAllPending` runs (in the
generated two-pass prefetch). `SQD_ASYNC_RESOLVE` is therefore inert in the hot
path. And async-resolve **cannot help within a block** — handlers need the
resolved data immediately. Its only payoff is overlapping block N+1's resolve with
block N's processing, i.e. **pipelining = Lever 3**. So it is not a standalone
change.

## Lever 2 — shard the ordered fold, by the CORRECT key (PROVEN)

The fold parallelizes by entity key, but the key must be **User**, not
`(user, tokenID)`. `handlePositionsConverted` (`custom_processor.go:1065/1092`)
reads the user's NO-token `AvgPrice` and writes the user's YES-token positions —
different tokenIDs, same user. Under `(user, tokenID)` sharding NO and YES land on
different shards → the YES shard can't see the NO value → silent corruption.

- **`TestShardKeyCorrectness`** reproduces it: `(user, tokenID)` sharding
  **corrupts 500/1000** positions vs serial; **User-keyed sharding is
  bit-identical**. This is the regression gate for the shard key.
- **`TestUserKeyedShardThroughput`**: the correct (User) key keeps the speedup —
  serial 3.10M → sharded ×10 **15.71M ev/s (5.07×)**, hottest shard 12.8% vs ideal
  10% (slightly more skew than user+tokenID's 10.4%, still balanced). **Correctness
  is ~free.**
- Dimension/fact split: reference entities (`Condition`/`FPMM`/`NegRiskEvent`,
  low-cardinality, created-then-read) go in a serial pre-pass; only the Position
  facts shard, by User. No global accumulators (only diagnostic counters).

## Lever 3 — combine: pipeline parallel parse with the sharded fold (PROVEN)

`combined_test.go` measures the topology on the real corpus:

| combined | ev/s | vs serial |
|---|---|---|
| serial parse+process (today) | 1.08M | 1× |
| barrier (parse → then shard) | 3.83M | 3.56× |
| **pipelined (parse ‖ router ‖ sharded)** | **6.61M** | **6.14×** |

A barrier between parallel stages re-creates the harmonic-sum trap; pipelining
(router does hash+scatter at 20M ev/s, not the bottleneck) removes it. This is the
existing producer‖consumer ring with both sides parallel — and it absorbs Lever 1b
(block N+1 resolve overlaps block N processing).

## What is deliberately NOT shipped, and why

The **live** sharded consumer (partitioning the generated HotState per shard and
fanning `ProcessProto` out by User inside the correctness-critical ingestion loop)
is built-ready but **not landed**, because the measured analysis says it is not the
current bottleneck: the dominant cost is lookup **latency**, which Lever 1 +
existing batching + `ColdAuthoritative` + low part-count already address, and which
sharding does **not** reduce (it parallelizes CPU, a different regime). Sharding
should be landed behind a default-off flag (`SQD_PROCESSOR_SHARDS`, User-keyed,
with the dimension pre-pass and this correctness gate) **only when profiling proves
the warm fold is CPU-bound** — at which point everything needed (correct key,
topology, regression gate) is already proven here.

## First action for the 30GB latency, today

Cut part count: bigger insert batches / merge-cadence tuning / periodic `OPTIMIZE`.
Measured 20ms → 4ms (5×) with zero architecture change, because the latency is
parts, not bytes.
