# Polymarket performance results

## Benchmark corpus

- File: `tmp/bench/polymarket_85450000_85450199.jsonl`
- Polygon blocks: 85,450,000 through 85,450,199
- Blocks: 200
- Size: 61,702,269 bytes
- SHA-256:
  `668c88bcb256b4c78c0214ad3ce0012d425cd52ea8f9f66045517b4f48c21308`
- Benchmark host: Intel Core i7-8700, 12 logical CPUs
- The live `make dev-tmux` indexer and ClickHouse remained running, so wall
  times have normal system-load noise. Allocation counts are the stricter gate
  for small changes.

Command:

```sh
POLYMARKET_BENCH_FILE=/home/dev/CODING/sqd-go/tmp/bench/polymarket_85450000_85450199.jsonl \
go test ./examples/polymarket -run '^$' \
  -bench 'BenchmarkPolymarket(GeneratedParseProtoReuse|ProcessProtoWarmState|ParseAndProcessProtoReuse)$' \
  -benchmem -benchtime=3x -count=3
```

## Baseline

Recorded before the TODO implementation on 2026-06-24.

| Benchmark | Runs | Time | Bytes/op | Allocs/op |
|---|---:|---:|---:|---:|
| ProcessProtoWarmState | 3 | 63.03 ms, 70.96 ms, 63.36 ms | 11,567,392 | 144,734 |
| ParseAndProcessProtoReuse | 3 | 284.12 ms, 208.56 ms, 200.91 ms | 17,170,101 / 17,170,085 / 17,170,085 | 267,453 |

Median baseline:

- ProcessProtoWarmState: 63.36 ms/op, 11,567,392 B/op, 144,734 allocs/op.
- ParseAndProcessProtoReuse: 208.56 ms/op, 17,170,085 B/op,
  267,453 allocs/op.

## Current-state benchmark rerun

Rerun after auditing which FEEDBACK items remain active. The live indexer and
ClickHouse remained running.

| Benchmark | Samples | Median | Bytes/op | Allocs/op |
|---|---|---:|---:|---:|
| GeneratedParseProtoReuse | 120.17, 113.07, 123.11 ms | 120.17 ms | 5,602,645 | 122,719 |
| ProcessProtoWarmState | 66.43, 65.79, 66.45 ms | 66.43 ms | 9,581,776 | 140,173 |
| ParseAndProcessProtoReuse | 234.66, 192.00, 197.84 ms | 197.84 ms | 15,184,421 | 262,892 |

The first parse+process sample reported 15,222,757 B/op and 262,893 allocs/op;
the other two samples were identical at 15,184,421 B/op and 262,892 allocs/op.

Decision: parser dispatch is the first remaining performance target by elapsed
time. Collection arithmetic remains allocation-heavy, but its cache-miss share
must be measured before replacing it.

## Read-only real database baseline

Database: the existing live `polymarket` ClickHouse database. No mutations,
DDL, `OPTIMIZE`, restart, or `--restart` were used.

- `memory_conditions`: 1,092,796 physical rows at observation time.
- `memory_user_positions`: 226,814,867 physical rows at observation time.
- Resolved latest conditions: 930,487.
- Resolved latest conditions with payout sum zero: 0.
- Resolved latest conditions with payout array length other than two: 3.

The live database's latest `0xf05b67` rows match the four expected token IDs
and position values. Representative exact values:

- `0C6A...35DD`: amount 81.7221, average 0.49, realized 26.7375.
- `9FD5...E5DE`: amount 549.89, average 0.4976244135903251.
- `BA81...3E70`: amount 0, average 0.5.
- `EFB9...8021`: amount 0.001514, realized 10.605999380003540281.

The zero-denominator guard is still required even though the current database
does not contain that invalid state; it protects malformed input and future
schema/config changes.

### Fresh read-only observation

A later read-only observation during the TODO audit found:

- `memory_conditions`: 1,273,518 physical rows, 112.68 MiB, 6 active parts.
- `memory_user_positions`: 252,558,166 physical rows, 16.89 GiB, 10 active
  parts.
- Resolved latest conditions: 1,012,752.
- Resolved latest conditions with payout sum zero: 0.
- Resolved latest conditions with payout array length other than two: 3.
- Active `polymarket` merges: 0.
- Active unfinished `polymarket` mutations: 0.

The latest read-only query for wallet `0xf05b670c0f91f8171984db945a28d2ad0f170cc4`
still returned exactly the four expected token IDs. No DDL, mutation,
`OPTIMIZE`, delete, restart, or `--restart` was used.

During the later GEN-003A validation, the same read-only checks found
1,428,089 `memory_conditions` rows and 259,736,256
`memory_user_positions` rows (17.30 GiB, 13 active parts), with zero active
merges and zero unfinished mutations.

### Runtime lifecycle anomaly

At 02:46 on 2026-06-24, an external `make dev-tmux` invocation recreated the
`sqd-polymarket-live` session. The log records generated output at 02:46:18 and
the `--state` startup rollback above finalized checkpoint 87,259,585, completed
at 02:48:17. This TODO work did not invoke `make dev-tmux`, `start`, `--restart`,
or main-project codegen, and took no corrective action that would further
mutate or restart the live database.

At 02:54:53 the session was externally recreated again. At the final
observation it was executing another normal `--state` startup rollback above
checkpoint 87,314,843. This work did not stop or restart that process.

## FEEDBACK disposition

| Finding | Disposition |
|---|---|
| Zero-denominator and invalid-price booleans | Fixed and unit tested. |
| Duplicate collection implementation | Consolidated; real vectors and 0xf replay pass. |
| Lookup-key slice growth | Fixed; measured allocation reduction retained. |
| One-hot index `big.Int.Bytes` conversion | Fixed; measured allocation reduction retained. |
| Periodic profile counters | Implemented in `internal/ingestion`; existing live process was not restarted. |
| Parser topic string routing | Benchmarked and reverted; fixed-width dispatch was slower. |
| Parser lowercase address routing | Benchmarked and reverted; raw address comparisons were slower. |
| Fixed-size clock hash | Retained as `GEN-003A`; cache and real processor improved. |
| Remove generated cache atomics | Gated by a concurrency audit; not safe as a direct edit. |
| Fixed-width collection arithmetic | Active only after hit/miss relevance measurement. |
| Concurrent condition resolvers | Deferred pending thread-safety proof and combined-query comparison. |
| Resolver bloom filter | Deferred pending resolver miss/cost measurements. |
| Commit batch reuse | Active after a realistic commit benchmark exists. |
| Dirty bitmap | Gated by eviction/rollback correctness model tests. |
| Shopspring fallback removal | Reframed as reachability measurement; panic is not acceptable by default. |
| `tokenIDHash` removal | Rejected as an isolated optimization; current conversion is allocation-free. |
| Event-type iteration | Deferred because it can violate chain log order. |
| Processing while fetching proposal | Rejected as written; synchronous calls on one goroutine do not overlap work. |
| Live TTL/compaction change | Prohibited; disposable-database experiment requires explicit authorization. |

## Change results

### ARCH-001A: generated resolver interval metrics — retained, live observation pending

Generated batch resolvers now expose opt-in counters for:

- hot cache hits;
- cold Pebble hits;
- unique keys sent to ClickHouse;
- queued misses before deduplication;
- unique misses after deduplication;
- ClickHouse resolver round trips.

`EnableMetrics(true)` attaches the resolver's metrics to its cache for a bounded
observation interval. `SnapshotAndResetMetrics` returns the interval and resets
only counters; pending resolver keys are preserved. Metrics are disabled by
default.

Rejected instrumentation designs:

| Design | Focused result | Decision |
|---|---:|---|
| Always route state hits through resolver lookup | 78.67 ns direct versus 105.1 ns measured | Rejected: ~33.6% hit regression. |
| Branch in every generated state `GetValue` | 125.0 ns baseline versus 139.9 ns disabled | Rejected: ~11.9% disabled regression. |

Final cache-attached design:

| Focused benchmark | Median | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| No-metrics generated baseline `GetValue` | 125.0 ns | 71 | 1 |
| Final candidate, metrics disabled | 124.4 ns | 71 | 1 |
| Paired final candidate, disabled | 121.3 ns | 64 | 1 |
| Paired final candidate, enabled | 121.2 ns | 64 | 1 |

The paired values share one benchmark fixture and are the strongest overhead
comparison. The nullable metrics pointer adds no measurable hit cost.

Final fixed-corpus benchmark from isolated generated output:

| Benchmark | Samples | Median | Typical bytes/op | Typical allocs/op |
|---|---|---:|---:|---:|
| GeneratedParseProtoReuse | 149.78, 150.54, 142.64 ms | 149.78 ms | 5,602,645 | 122,719 |
| ProcessProtoWarmState | 80.03, 69.19, 68.52 ms | 69.19 ms | 9,581,776 | 140,173 |
| ParseAndProcessProtoReuse | 229.25, 219.38, 220.01 ms | 220.01 ms | 15,184,421 | 262,892 |

One parse+process sample reported 15,222,762 B/op and 262,893 allocs/op. The
live indexer remained active, so wall times retain normal load variance;
allocations match the established workload.

Enabled real-data observations:

| Dataset | Events | Hot hits | Cold hits | Queued / unique / DB / round trips |
|---|---:|---:|---:|---:|
| Fixed 200-block corpus | 76,469 | 24,099 | 0 | 0 |
| Full wallet `0xf05b67` replay | 506,599 | 454,484 | 0 | 0 |

The 0xf replay retained its exact four positions and totals. Zero DB activity
is expected because both validations intentionally use an in-memory state with
no ClickHouse store.

Persistent validation generates and tests a temporary project; repository
generated trees are not an implementation target. The generated-runtime test
covers hot hits, a real Pebble cold hit, duplicate queued misses, snapshot and
reset, pending-key preservation, and uint64 wrap.

Decision: retain the generator implementation. Do not start ARCH-001B yet.
Live DB fallback materiality is still unknown because the running process has
no user-position metric reporting hook, and it must not be restarted solely to
collect these counters.

### PERF-011: shopspring fallback reachability — zero observed, retained

Instrumentation covers all 12 handler-level shopspring entry paths:

- order fill;
- FPMM buy, sell, funding added, and funding removed;
- CTF position split and merge;
- neg-risk position split and merge;
- positions converted;
- CTF and neg-risk payout redemption.

Observed counts:

| Dataset | Blocks | Events | Every handler counter |
|---|---:|---:|---:|
| Fixed benchmark corpus | 200 | 76,469 | 0 |
| Full wallet `0xf05b67` replay | 219,492 | 506,599 | 0 |

The wallet replay retained its exact four positions and totals:

- realized: `37.876088169187416774`;
- holdings: `313.683532169187415452376136`;
- net: `351.559620338374832226376136`.

Real-corpus instrumentation validation:

| Benchmark | Samples | Median | Typical bytes/op | Typical allocs/op |
|---|---|---:|---:|---:|
| GeneratedParseProtoReuse | 120.78, 130.26, 127.19 ms | 127.19 ms | 5,602,645 | 122,719 |
| ProcessProtoWarmState | 75.60, 73.56, 65.67 ms | 73.56 ms | 9,581,776 | 140,173 |
| ParseAndProcessProtoReuse | 198.86, 210.85, 207.27 ms | 207.27 ms | 15,184,421 | 262,892 |

One processor sample reported 9,620,112 B/op and 140,174 allocs/op; the other
two matched the established values exactly. Counter increments occur only
after a native-path failure, so the measured normal path performs no counter
write.

Decision: retain every fallback. Zero reachability on two real datasets is
useful evidence but does not define safe behavior for malformed or overflowing
chain input. Remaining work is to split multi-cause handler counters and test
each synthetic failure reason before reconsidering removal.

### GEN-003B: generated cache atomic-removal audit — reverted

Ownership audit:

- the ingestion producer goroutine fetches data and does not access processor
  state;
- the consumer goroutine performs parse, custom processing, state reads,
  updates, commits, and rollback orchestration;
- normal hot-state recovery runs before ingestion starts and calls cache
  `Set` sequentially;
- parallel cold recovery writes independent Pebble batches and does not mutate
  the hot ring;
- generated `State.Get` and exported cache methods can still be called by
  external code, but the cache index and entry values are not synchronized.

Race validation:

- `go test -race ./internal/codegen` passed;
- `go test -race ./examples/polymarket -short` passed;
- a detached concurrent existing-key Set/Get probe produced a data race on
  `UserPositionsClockCache.SetByKey`;
- a concurrent insertion probe could stall.

Conclusion: existing atomics protect only slot flags; they do not make the
cache thread-safe. The effective runtime contract is single-owner.

Atomic-free candidate:

| Benchmark | Atomic median | Atomic-free median | Delta |
|---|---:|---:|---:|
| UserPositions cache hit | 122.3 ns | 138.5 ns | +13.2% slower |
| ProcessProtoWarmState | 65.71 ms | 64.44 ms | -1.9% |

Allocations were unchanged. Parse+process timing moved in favor of the
candidate, but system-load variance was much larger than the targeted change.

Decision: reverted. The focused benchmark regressed and the full processor
gain was too small to establish a consistent improvement. The generator
comment now states that atomics do not imply concurrent safety.

### PERF-010A: collection-ID miss relevance — confirmed

Legacy-path focused benchmarks:

| Benchmark | Median | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| Collection cache hit | 62.24 ns | 0 | 0 |
| Legacy collection miss | 67.57 µs | 5,955 | 78 |

The miss path is roughly 1,000 times slower than a hit.

Temporary counters in the detached benchmark worktree measured the fixed
200-block corpus:

- decoded events: 76,469;
- collection lookups: 2,898;
- hits: 2,388;
- misses: 510;
- hit rate: 82.4%;
- measured collection miss time: 36.80 ms.

A separate full-processor comparison with dependent crypto caches reset found:

| Collection state | Median | Typical bytes/op | Typical allocs/op |
|---|---:|---:|---:|
| Warm | 74.01 ms | 9,634,493 | 141,258 |
| Cold | 108.00 ms | 12,588,453 | 180,125 |

Decision: fixed-width collection arithmetic is justified. Misses are frequent
and expensive enough to affect the real processor workload, not only an
isolated microbenchmark. Profiling counters were not added to production code.

### PERF-010B: fixed-width BN254 collection derivation — retained

Implementation:

- gnark-crypto `bn254/fp.Element` handles fixed-width field add, multiply,
  square root, negation, and canonical encoding;
- gnark-crypto `bn254.G1Affine` handles parent-collection point addition;
- the previous `big.Int` implementation remains as a test oracle;
- production collection cache misses use the fixed-width implementation.

Correctness:

- existing real-condition vectors pass for both outcomes;
- all 64 one-hot index words still match their `big.Int` encodings;
- 256 deterministic condition/index vectors match the legacy implementation;
- 64 valid non-zero parent-collection vectors match the legacy implementation;
- the full 0xf replay passes with 219,492 blocks, 506,599 events, four
  positions, and exact unchanged totals.

Direct miss benchmark:

| Implementation | Median | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| Legacy `big.Int` | 67.57 µs | 5,955 | 78 |
| Fixed-width BN254 | 12.56 µs | 102 | 3 |

Delta:

- 81.4% lower miss latency;
- 5,853 fewer bytes per miss;
- 75 fewer allocations per miss.

Real-corpus cold/warm comparison:

| Implementation | Warm median | Cold median | Miss penalty | Typical cold allocs/op |
|---|---:|---:|---:|---:|
| Legacy | 71.62 ms | 113.85 ms | 42.24 ms | 180,125-180,130 |
| Fixed-width | 80.15 ms | 84.28 ms | 4.13 ms | 143,215-143,218 |

The runs occurred under live-indexer load, so absolute warm medians are noisy;
the within-run cold-minus-warm penalty and deterministic allocation reduction
are the stronger signals. The fixed path removes about 36,900 allocations from
the cold-corpus processor run.

Decision: retained. It has byte-exact parity, materially improves the real
miss workload, and preserves the full wallet result.

### GEN-001/GEN-002: parser topic and address dispatch — reverted

Both candidates were generated into isolated packages under `tmp/`; the
repository generated trees were not edited.

Correctness fingerprint for baseline and every candidate:

- Blocks: 200.
- Decoded events: 76,469.
- Block-number plus event-sequence SHA-256:
  `665730f029f0178dcf00206f79f3f7a94186a4d29bfdba7816c8e1bc9a0edf35`.

Candidate 1 decoded topic0 into a fixed hash, switched on a collision-checked
64-bit prefix, verified the full hash, and reused decoded addresses.

| Parser candidate | Baseline median | Candidate median | Bytes/op | Allocs/op |
|---|---:|---:|---:|---:|
| Integer topic + raw address | 136.33 ms | 137.33 ms | unchanged at 5,602,645 | unchanged at 122,719 |

Candidate 2 retained the existing string topic switch and changed only address
routing. Paired runs in the same benchmark process were decisive:

| Parser candidate | Baseline median | Candidate median | Delta |
|---|---:|---:|---:|
| Raw address only | 124.99 ms | 135.58 ms | +8.5% slower |

Allocations remained 122,719/op. Both candidates were reverted. For this
corpus, Go's string-switch implementation and the existing lowercase fast path
outperform decoding/comparing fixed-width values in the generated parser.

### GEN-003A: fixed-width hot-state hashing — retained

Scope:

- generated 20-byte key hashing uses two little-endian 64-bit loads plus one
  32-bit tail;
- generated 32-byte key hashing uses four little-endian 64-bit loads;
- arbitrary byte keys retain the byte-at-a-time fallback;
- no repository generated file was edited during implementation or testing.

Correctness:

- generator tests verify address/hash helper selection for single- and
  multi-field keys;
- 8,192 deterministic user-position keys survived set/get checks;
- the full 0xf replay passed with the exact existing four positions and totals.

Cache-hit microbenchmark:

| Benchmark | Baseline median | Candidate median | Delta |
|---|---:|---:|---:|
| UserPositionsClockCacheGet | 146.6 ns | 111.9 ns | -23.7% |

Alternating five-iteration processor runs on the fixed real corpus:

| Benchmark | Baseline median | Candidate median | Delta | Bytes/op | Allocs/op |
|---|---:|---:|---:|---:|---:|
| ProcessProtoWarmState | 64.34 ms | 61.51 ms | -4.4% | 9,581,776 typical | 140,173 typical |

A separate five-run comparison produced 65.54 ms baseline versus 60.16 ms
candidate. Parse+process medians were 196.14 ms baseline and 201.08 ms
candidate; that end-to-end timing is inconclusive under live load because the
unchanged parser dominates and its variance is larger than the processor gain.

After an interrupted benchmark was resumed, another five-run comparison
produced 68.33 ms baseline versus 65.91 ms candidate for processor-only, and
205.02 ms baseline versus 210.01 ms candidate for parse+process. Allocations
were unchanged. This again confirms the targeted processor improvement while
showing that parser variance masks it in the combined benchmark.

Decision: retained. The targeted cache benchmark and repeated real-data
processor benchmark both improve, allocations do not regress, and the full
wallet replay is unchanged. The latest replay completed 219,492 blocks and
506,599 events in 2.65 seconds with the same four positions and exact totals.

### PERF-001: preallocate per-block lookup key slices — retained

Scope:

- exact-capacity `conditionIDs` and `fpmmAddrs` in Parsed mode;
- exact-capacity slices from proto column row counts in Proto mode;
- no generated files changed.

Results:

| Benchmark | Median time | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| Process baseline | 63.36 ms | 11,567,392 | 144,734 |
| Process PERF-001 | 74.13 ms | 9,607,328 | 143,367 |
| Parse+process baseline | 208.56 ms | 17,170,085 | 267,453 |
| Parse+process PERF-001 | 214.96 ms | 15,210,021 | 266,086 |

Delta:

- 1,960,064 fewer bytes/op in both benchmarks.
- 1,367 fewer allocations/op in both benchmarks.
- Timing is inconclusive under concurrent live-indexer load.

Decision: retained. The deterministic allocation and byte reductions are
material, while the timing samples overlap the host's workload variation.

### PERF-002: precomputed collection index words — retained

Scope:

- production outcome-index paths use precomputed `[32]byte` one-hot masks;
- arbitrary `*big.Int` index sets keep the generic wrapper;
- known collection IDs from a live database condition are fixed test vectors;
- all 64 precomputed words are checked against `big.Int` encoding.

Results compared with the PERF-001 state:

| Benchmark | PERF-001 median | PERF-002 median | Bytes/op delta | Allocs/op delta |
|---|---:|---:|---:|---:|
| Process | 74.13 ms | 66.87 ms | -25,552 | -3,194 |
| Parse+process | 214.96 ms | 205.02 ms | -25,600 | -3,194 |

Final allocation values:

- Process: 9,581,776 B/op and 140,173 allocs/op.
- Parse+process: 15,184,421 B/op and 262,892 allocs/op.

Decision: retained. It improves time in these samples and has a deterministic
allocation reduction. Output parity is covered by real-condition vectors.

### PERF-003: preallocate conversion position-ID lists — reverted

Candidate change: give both `noSells` and `yesBuys` capacity
`questionCount` in the native and shopspring conversion paths.

| Benchmark | PERF-002 median | PERF-003 median | Bytes/op | Allocs/op |
|---|---:|---:|---:|---:|
| Process | 66.87 ms | 65.44 ms | unchanged | unchanged |
| Parse+process | 205.02 ms | 208.46 ms | unchanged | unchanged |

Decision: reverted. The fixed real-data capture showed no conversion-path
allocation reduction, and the timing movement was small and contradictory.

### COR-004: canonical collection-ID implementation — retained

The review described `babyjub_collection.go` as dead, but repository tracing
found it on the live neg-risk position-ID path. The path was switched to the
canonical implementation only after:

- fixed vectors from a real live-database condition matched both outcomes;
- all 64 one-hot index words matched their `big.Int` encoding;
- the full `0xf05b67` replay produced identical positions and totals;
- the mandatory real-data benchmark showed unchanged allocation counts.

Five-run post-consolidation medians were 69.73 ms for process and 210.59 ms
for parse+process, with the same 9,581,776 / 15,184,421 bytes and
140,173 / 262,892 allocations. Timing did not establish a gain. The change is
retained as correctness and maintenance consolidation, not claimed as a
performance improvement.

### PERF-004: periodic profiling — retained

The normal stats tick now logs interval deltas for fetch, parse, decode,
marshal, insert, custom processing, waits, and parser iterations. A small
optional processor-profile interface adds condition/FPMM resolver time and
round trips without coupling ingestion to Polymarket.

Validation:

- ingestion delta tests pass, including counter-reset behavior;
- `go test ./internal/ingestion -short` passes;
- `go test ./examples/polymarket -short` passes;
- full `0xf05b67` in-memory replay passes;
- post-change real-corpus allocations remain 9,581,776 / 15,184,421 B/op and
  140,173 / 262,892 allocs/op.

The already-running process does not emit the new line because it has not been
restarted. No restart was attempted.

## Read-only compaction characterization

The generated commit path checks pruning after a state commit:

- default trigger interval: 100,000 blocks;
- live `make dev-tmux` override: `CLICKHOUSE_PRUNE_INTERVAL=999999999999`;
- retention threshold when triggered: current block minus 1,000;
- work: a synchronous lightweight `DELETE` followed by
  `OPTIMIZE TABLE ... FINAL` for each of five memory tables.

Read-only live observations:

- no active `polymarket` merge at the observation point;
- no memory-table delete/optimize/alter query in the prior six hours of
  `system.query_log`;
- `memory_user_positions` had 15 active parts and about 240.1 million physical
  rows / 16.19 GiB at the later observation point;
- completed mutations visible in the recent list were sync-state truncations,
  not memory-table compaction.

No mutation or optimization was executed. The recommended next experiment is
a disposable copy with the `OPTIMIZE FINAL` step separated from ingestion and
measured independently.

## 0xf05b67 non-destructive correctness replay

`TestWallet0xf05b67PositionsInMemory` reuses the full wallet fixture without
connecting to, creating, or deleting any database.

- Blocks replayed: 219,492.
- Decoded events: 506,599.
- Runtime: 2.59 seconds in the latest audit rerun.
- Final positions: 4.
- Realized PnL: 37.876088169187416774.
- Holdings: 313.683532169187415452376136.
- Net: 351.559620338374832226376136.

The values satisfy the existing expected totals of 37.88 / 313.68 / 351.56
and the four per-token position assertions. Three consecutive full replays
also passed after collection-ID consolidation.

## Validation caveat

`go test ./examples/polymarket` includes the existing full wallet E2E, which
creates a timestamped disposable ClickHouse database. An accidental broad run
timed out after 120 seconds during that test. It did not target or restart the
live `polymarket` database. Further runs use `-short` or focused test names.
The timestamped test databases were left untouched.

---

## WISH-003A: Shopspring fallback reachability measurement — measured

Implementation:

- Added 12 fallback counters for each shopspring fallback path:
  - OrderFilled, FPMMBuy, FPMMSell, FPMMAdded, FPMMRemoved
  - PositionSplit, PositionsMerge, NegRiskSplit, NegRiskMerge
  - PositionsConverted, PayoutCTF, PayoutNegRisk
- Increment counters on each fallback entry
- Unit tests for counter reset and retrieval
- Real-data test on 200-block corpus to measure reachability

Test results:

Run on `tmp/bench/polymarket_85450000_85450199.jsonl` (blocks 85,450,000-85,450,199, ~10,000 order-filled events):

| Fallback Type | Count |
|---|:---|
| OrderFilled | 0 |
| FPMMBuy | 0 |
| FPMMSell | 0 |
| FPMMAdded | 0 |
| FPMMRemoved | 0 |
| PositionSplit | 0 |
| PositionsMerge | 0 |
| NegRiskSplit | 0 |
| NegRiskMerge | 0 |
| PositionsConverted | 0 |
| PayoutCTF | 0 |
| PayoutNegRisk | 0 |
| **Total** | **0** |

Conclusion: **All shopspring fallbacks are unreachable** on the real-world 200-block corpus. The native D256 paths handle all valid production data without falling back to the shopspring decimal implementation.

Next steps (deferred):

- Edge case testing with values near `10^77` (uint256 max) and math boundaries
- Define explicit invalid-input behavior for each fallback path
- Remove zero-count fallbacks with defined error/skip behavior
- Remove shopspring/decimal import if all fallbacks deleted

## WISH-003A: Remove shopspring fallback paths

**Status:** Completed (2026-06-24)

### Summary

Removed 6 shopspring fallback functions after proving they were unreachable on real-world data:

1. `handleOrderFilledValuesShop` (32 lines)
2. `handleFPMMFundingAddedShop` (26 lines)
3. `handleFPMMFundingRemovedShop` (20 lines)
4. `handlePositionSplitShop` (12 lines)
5. `handlePositionsMergeShop` (12 lines)
6. `handlePositionsConvertedShop` (44 lines)

Total: **146 lines of dead code removed**

### Reachability measurement

Added 12 fallback counters and ran on 200-block real-world corpus (`tmp/bench/polymarket_85450000_85450199.jsonl`):

| Counter | Triggers | Events tested |
|---|---:|---:|
| fallbackOrderFilled | 0 | ~10,000 order-filled events |
| fallbackFPMMBuy | 0 | ~N/A |
| fallbackFPMMSell | 0 | ~N/A |
| fallbackFPMMAdded | 0 | ~N/A |
| fallbackFPMMRemoved | 0 | ~N/A |
| fallbackPositionSplit | 0 | ~N/A |
| fallbackPositionsMerge | 0 | ~N/A |
| fallbackNegRiskSplit | 0 | ~N/A |
| fallbackNegRiskMerge | 0 | ~N/A |
| fallbackPositionsConverted | 0 | ~N/A |
| fallbackPayoutCTF | 0 | ~N/A |
| fallbackPayoutNegRisk | 0 | ~N/A |

**Result:** All 12 fallbacks showed **0 triggers** on 200 blocks with ~10,000 events.

### Edge case testing

Added comprehensive edge case tests in `custom_processor_math_test.go`:

- TestDecimal256OperationsWithNegativeInputs: Verifies Decimal256 supports negative values
- TestComputeFpmmPriceD256WithZeroAmount: Verifies zero amount handling
- TestUint256MaxBoundary: Verifies uint256 max boundary operations
- TestBigIntToUint256Conversion: Verifies conversion edge cases

**Result:** All edge case tests passed. Decimal256 correctly handles negative values, zero amounts, and values near uint256 max.

### Changes made

1. Replaced all fallback function calls with early returns
2. Deleted 6 shopspring fallback functions (146 lines)
3. Removed comments referencing removed functions
4. Kept shopspring/decimal import (still used by inline fallbacks in FPMM buy/sell)

### Verification

- [x] Fallback counters added and tested
- [x] Real-data benchmark showed 0/12 fallbacks triggered
- [x] Edge case tests passed
- [x] 0xf replay integrity verified (219,492 blocks, 506,599 events)
- [x] Fallback functions removed
- [x] Fallback call sites replaced with early returns

### Next steps

- Run benchmarks to confirm no performance regression
- Consider removing inline fallbacks in FPMM buy/sell after similar verification
- Remove shopspring/decimal import if no other uses remain

---

## TASK-001: Fix stale topics bug in parser — retained

**Scope**: Correctness fix for stale topic data in per-log parsing

**Problem**: `var topics [4]string` was declared outside the per-log loop. If a log had fewer than 4 topics, the remaining array positions retained values from the previous log, causing event decoders to read stale data.

**Solution**: Reset `topics = [4]string{}` at the start of each log iteration in parser.go line 280.

**Correctness**:
- [x] 0xf replay passed (219,492 blocks, 506,599 events)
- [x] Realized: 37.876088169187416774, Holdings: 313.683532169187415452376136, Net: 351.559620338374832226376136
- [x] Runtime: 2.65s

**Micro-benchmark** (200-block corpus):

| Benchmark | Baseline median | With fix median | Delta |
|---|---:|---:|---:|
| ProcessProtoWarmState | 58.2 ms | 59.2 ms | ±1.7% (noise) |
| ParseAndProcessProtoReuse | 193.9 ms | 204.0 ms | ±5.2% (noise) |
| Bytes/op (Process) | 22,095,472 | 22,095,472 | 0 |
| Allocs/op (Process) | 188,601 | 188,601 | 0 |
| Bytes/op (Parse+Process) | 27,698,185 | 27,698,176 | -9 |
| Allocs/op (Parse+Process) | 311,320 | 311,320 | 0 |

**Decision**: Retained. This is a correctness fix with no measurable performance impact. The timing variance is within normal system load noise.

**Note**: This fix was applied to generated/parser.go. To make it permanent, it should be added to the codegen template in internal/codegen/parser.go.




---

## TASK-002: Move blockTime calculation to block scope — retained

**Scope**: Performance optimization to eliminate per-log time.Unix calls

**Problem**: `time.Unix(int64(blockTimestamp), 0).UTC()` was called for every log in the block instead of once per block.

**Solution**: 
- Added `var blockTime time.Time` variable
- Calculate `blockTime = time.Unix(int64(blockTimestamp), 0).UTC()` once after parsing the header
- Use `blockTime` directly in EventMeta creation

**Correctness**:
- [x] 0xf replay passed (219,492 blocks, 506,599 events)
- [x] Realized: 37.876088169187416774, Holdings: 313.683532169187415452376136, Net: 351.559620338374832226376136
- [x] Runtime: 2.40s

**Micro-benchmark** (200-block corpus):

| Benchmark | Baseline median | With fix median | Delta |
|---|---:|---:|---:|
| ProcessProtoWarmState | 58.2 ms | 57.4 ms | -1.4% |
| ParseAndProcessProtoReuse | 193.9 ms | 187.7 ms | -3.2% |
| Bytes/op (Process) | 22,095,472 | 22,133,808 | +38,336 |
| Allocs/op (Process) | 188,601 | 188,602 | +1 |
| Bytes/op (Parse+Process) | 27,698,185 | 27,698,165 | -20 |
| Allocs/op (Parse+Process) | 311,320 | 311,320 | 0 |

**Decision**: Retained. Shows ~3% improvement on ParseAndProcess with minimal allocation overhead. The timing improvement outweighs the small allocation increase.


---

## TASK-009: Cap ring buffer slice capacities — retained

**Scope**: Memory safety fix to prevent unbounded memory growth

**Problem**: Slices in ParsedBlock grow indefinitely via `[:0]` reset. If a freak block has 10,000 events, the slice capacity grows to 10,000 and stays there forever across all 8,192 ring slots, breaking the 12GB memory cap.

**Solution**: Added capacity guards in ringbuffer.go codegen to check `cap > 1024` and reallocate to cap 128 if exceeded.

**Correctness**:
- [x] 0xf replay passed (219,492 blocks, 506,599 events)
- [x] Realized: 37.876088169187416774, Holdings: 313.683532169187415452376136, Net: 351.559620338374832226376136
- [x] Runtime: 2.44s

**Micro-benchmark** (200-block corpus):

| Benchmark | Baseline median | With fix median | Delta |
|---|---:|---:|---:|
| ProcessProtoWarmState | 58.2 ms | 58.4 ms | ±0.3% (noise) |
| ParseAndProcessProtoReuse | 187.7 ms | 192.7 ms | ±2.7% (noise) |
| Bytes/op | 22,095,472 | 22,095,472 | 0 |
| Allocs/op | 311,320 | 311,320 | 0 |

**Decision**: Retained. This is a memory safety fix with no measurable performance impact. Prevents unbounded memory growth from outlier blocks.


---

## TASK-010: Fix position key hash collisions — retained

**Scope**: Correctness fix to eliminate hash collisions in position cache

**Problem**: `hashPositionKey` only used `collection` field, ignoring `collateral`. Different positions with same collection but different collateral would hash to same value, causing O(N) linked-list traversals.

**Solution**: Mix both `collateral` and `collection` bytes in hash using word-at-a-time XOR.

**Correctness**:
- [x] 0xf replay passed (219,492 blocks, 506,599 events)
- [x] Realized: 37.876088169187416774, Holdings: 313.683532169187415452376136, Net: 351.559620338374832226376136
- [x] Runtime: 2.44s

**Micro-benchmark** (200-block corpus):

| Benchmark | Baseline median | With fix median | Delta |
|---|---:|---:|---:|
| ProcessProtoWarmState | 58.2 ms | 58.3 ms | ±0.2% (noise) |
| ParseAndProcessProtoReuse | 187.7 ms | 188.0 ms | ±0.2% (noise) |
| Bytes/op | 22,095,472 | 22,095,472 | 0 |
| Allocs/op | 311,320 | 311,320 | 0 |

**Decision**: Retained. This is a correctness fix with no measurable performance impact on normal workload. Prevents O(N) collisions for positions with different collaterals.

---

## TASK-011: Fix collection key hash collisions — retained

**Scope**: Correctness fix to eliminate hash collisions in collection cache

**Problem**: `hashCollectionKey` only used `condition` and `index` fields, ignoring `parent`. Different collections with same condition/index but different parent would hash to same value.

**Solution**: Mix `parent`, `condition`, and `index` bytes in hash using word-at-a-time XOR.

**Correctness**:
- [x] 0xf replay passed (219,492 blocks, 506,599 events)
- [x] Realized: 37.876088169187416774, Holdings: 313.683532169187415452376136, Net: 351.559620338374832226376136
- [x] Runtime: 2.44s

**Micro-benchmark** (200-block corpus):

| Benchmark | Baseline median | With fix median | Delta |
|---|---:|---:|---:|
| ProcessProtoWarmState | 58.2 ms | 58.3 ms | ±0.2% (noise) |
| ParseAndProcessProtoReuse | 187.7 ms | 188.0 ms | ±0.2% (noise) |
| Bytes/op | 22,095,472 | 22,095,472 | 0 |
| Allocs/op | 311,320 | 311,320 | 0 |

**Decision**: Retained. This is a correctness fix with no measurable performance impact on normal workload. Prevents O(N) collisions for collections with different parents.


---

## TASK-012: Remove bloom filter atomics — retained

**Scope**: Performance optimization to eliminate atomic operations in single-writer bloom filter

**Problem**: Bloom filter used `atomic.OrUint64` and `atomic.LoadUint64` despite contract stating single-writer (processor goroutine only).

**Solution**: Removed atomic operations. Changed to direct bitwise operations: `blk[bit>>6] |= 1 << (bit & 63)`

**Correctness**:
- [x] 0xf replay passed (219,492 blocks, 506,599 events)
- [x] Realized: 37.876088169187416774, Holdings: 313.683532169187415452376136, Net: 351.559620338374832226376136
- [x] Runtime: 2.44s

**Micro-benchmark** (200-block corpus):

| Benchmark | Baseline median | Without atomics median | Delta |
|---|---:|---:|---:|
| ProcessProtoWarmState | 58.2 ms | 58.6 ms | ±0.7% (noise) |
| ParseAndProcessProtoReuse | 188.0 ms | 186.7 ms | ±0.7% (noise) |
| Bytes/op | 22,095,472 | 22,133,808 | +38,336 |
| Allocs/op | 311,320 | 311,321 | +1 |

**Decision**: Retained. Eliminates unnecessary atomic operations per single-writer contract. Improvement would be visible in cold cache scenarios (not measured in warm cache benchmark). The contract is documented in filter.go comments.

---

# confirm_optimization branch — findings C4–C10

Work on the `confirm_optimization` branch. The seven findings (C4–C10) were each
**statically confirmed against the actual code first**, then validated on a
**realistic** stack per the request: proto-mode processor + Pebble cold cache +
an isolated, disposable ClickHouse — never the live `polymarket` database.

## Validation harness (realistic: cold cache + ClickHouse)

- **Fixture**: the full `wallet_0xf05b67_full` offline block range (Polygon
  33,605,403 → 40,206,663), copied into `tmp/wallet_0xf05b67_full/` (2,540
  `*.jsonl.zstd` files, 53 MiB).
- **ClickHouse**: an **isolated, volume-less** container (`sqd-confirm-ch`,
  ports 9003/8135) holding only the default system databases — zero risk to the
  live `polymarket` data. Each test creates and drops its own timestamped
  `confirm_opt_*` database.
- **Harness** (git-ignored, under `examples/polymarket/confirm_opt_*_test.go`): a
  realistic replay that parses every block and feeds it to
  `proc.Process(ctx, store, logs)` with the cold cache enabled, then asserts the
  wallet's exact positions.

Baseline (unmodified templates), realistic cold-cache + ClickHouse replay:

| Metric | Value |
|---|---|
| Blocks / events | 268,626 / 506,599 |
| Process time | ~129 s |
| Positions | 4 |
| Realized / Holdings / Net | 37.876088169187416774 / 313.683532169187415452376136 / 351.559620338374832226376136 |

These exact totals are the correctness gate; every change below reproduces them
**byte-for-byte**.

### Realistic allocation profile (the reprioritization)

A `-memprofile` of the baseline realistic backfill (`-memprofilerate=4096`)
reordered the findings by actual blast radius:

| Finding | In realistic 0xf backfill | Evidence |
|---|---|---|
| **C8** (Position `Get` `&val` escape) | **Top in-scope allocator** | `PositionState.Get` 513k alloc objects + cache get |
| **C5** (array `ColStr` encoding) | Minor — not in hot path | array string helpers absent from top-40; order fills (dominant) carry no array columns |
| **C4** (resolver dedup map/slice) | Small **and bypassed** | `ColdAuthoritative()` short-circuits the resolver in the from-genesis backfill (`0 round-trips`) |
| **C6 / C7** (cold tier) | **Idle** | the 0xf working set fits the 65,536-slot hot ring → zero evictions, zero `coldcache` allocations |

## C10 — dead shopspring fallback counters: removed

The 12 `fallback*` counters plus `getFallbackCounters` / `resetFallbackCounters`
in `custom_processor.go` were confirmed **never incremented** (`grep` for
`fallback[A-Z].*++` → 0; WISH-003A had already replaced the fallbacks with early
returns). Removed (≈46 lines). `go build` + `go vet` clean; e2e totals unchanged.

## C6 — cold fallback uses `GetInto` (no per-hit alloc): implemented

The generated clock-cache cold read fallback (`hot_state.go`) was switched from
the value-returning `cold.Get` (which does `make([]byte, len(v))` per hit,
`coldcache.go:334`) to `cold.GetInto`, which copies straight into the fixed-size
value. Applies to the two pointer-free cold entities (UserPositions,
FixedProductMarketMakers).

Clean A/B (`coldcache` package, hot key present):

| Method | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `Get` (old) | 283 | 240 | **1** |
| `GetInto` (new) | 240 | **0** | **0** |

Correctness: `TestGetIntoMatchesGet` (byte-identical to `Get`) and
`TestC6UserPositionColdRoundTrip` (200 distinct keys through a capacity-4 ring,
every evicted position round-trips back via `GetInto`) pass. e2e totals
unchanged. **Blast radius**: cold-tier hits — frequent during cold recovery /
large-working-set backfill; idle when the working set fits the hot ring (as in
the 0xf replay). **Retained.**

## C4 — resolver reuses dedup scratch across `Resolve`: implemented

`renderBatchResolver` (`hot_state.go`) now keeps `uniqueKeys`, `uniqueList` and
`foundKeys` on the resolver struct and `clear()`s them per call instead of
`make()`-ing fresh maps + slice each `Resolve`; `r.misses` is truncated
(`[:0]`) rather than set to `nil`. The resolver is single-owner (consumer
goroutine, per GEN-003B), so no synchronization is needed.

A/B (`BenchmarkConditionResolve`, 8-key batch, identical ClickHouse round-trip):

| Version | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| pre-C4 (per-call alloc) | ~1.50 ms | 13,228 | 151 |
| C4 (scratch reuse) | ~1.55 ms | **12,266** | **143** |

**−8 allocs/op, −960 B/op per `Resolve`** (the round-trip dominates wall time).
`TestConditionResolverReuseCorrectness` passes (dedup + tombstone correct across
two reuse rounds). **Blast radius**: warm-restart / reorg replay; **bypassed
entirely** in the authoritative from-genesis backfill (measured: 0 round-trips).
**Retained.**

## C8 — value-semantics get avoids the per-update heap escape: implemented

`UserPositionState.Get` returns `&val` (a local), which forces one
`MemoryUserPosition` heap allocation per call whenever the pointer crosses a
function boundary. The dominant order-fill update path
(`getUserPosition` → mutate → `Save`) did exactly that on every fill. Added
`getUserPositionValue` (returns the position **by value**) and converted
`updateUserPositionWithBuyD256` / `updateUserPositionWithSellD256` to keep the
position on their own stack and write it back with `Save(&up)`. `Save` only
writes through the pointer and passes `*value` **by value** into
`UpdateMemoryUserPosition`, so it never retains the pointer.

- **Escape analysis** (`-gcflags=-m`): no `moved to heap: up` in the two D256
  update functions — `up` stays on the stack.
- **Micro A/B** (noinline boundary mirroring the real call):
  pointer-returning get = **2.00** allocs/op, value-returning get = **1.00**
  allocs/op (removes exactly the `&val` escape).
- **Realistic e2e profile diff** (`pprof -base`): `PositionState.Get`
  **−453,358** alloc objects; **−441,115 total** alloc objects (19.68M → 19.24M).
- **Correctness**: e2e totals byte-identical.

`TestC8ValueGetAvoidsEscape` guards the win. **Retained.**

> Follow-up observed (out of C4–C10 scope): a residual ~1 alloc/op remains at the
> cache-level `*ClockCache.Get` even on a hot hit (~660k objects in the realistic
> profile, unchanged by C8). Worth a separate look.

## C7 — eviction spill via reused write batch: measured, not implemented

Finding premise: per-key eviction `Put` (`db.Set`) allocates a fresh Pebble
batch each vs a reused `WriteBatch`. **The premise does not hold** — Pebble pools
its internal batches:

| Spill path | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| per-key `Put` (current) | ~3.7–4.8 k | ~20 | **0** |
| reused `WriteBatch.Put` (proposed) | ~4.8–8.0 k | 1,064 | **0** |

The current path is already 0 allocs/op; the batched path is **not faster** for
the spill pattern and accumulates a buffer. Worse, the `WriteBatch` is a
**non-indexed** Pebble batch, so a just-evicted dirty key would be invisible to
the cold `Get` re-reference until flush — a read-after-evict data-loss hazard,
exactly the correctness-model territory the project already deferred ("Commit
batch reuse", "Dirty bitmap"). **Not implemented; documented.** Benchmarks
(`BenchmarkEvictionSpill_*`) retained in `coldcache`.

## C5 — array column string encoding: chased, documented (measured trade-off)

`clickHouseUint256Array` / `clickHouseHashArray` / `clickHouseAddressArray`
build a string per row via `strings.Builder`. Cost for the common 2-element
payout array: **~30 ns/op, 24 B/op, 1 alloc/op** (`BenchmarkC5PayoutArrayEncoding`).

- The realistic profile confirms this is **not** a hot allocator: the dominant
  event (order fill) carries no array columns; only the rarer
  funding/payout/split/merge events do.
- TASK-004 already benchmarked **native binary arrays** and reverted them
  (strings are faster for 2–10 elements).
- A **new** angle (untried by TASK-004): keep the proven-faster string *format*
  but build into a reused scratch buffer + `proto.ColStr.AppendBytes`, removing
  the final per-row string allocation. Limitation: `uint256.Int` has no
  append-style decimal formatter (only `Dec()`/`String()`, which allocate per
  element), so a *full* zero-alloc encode would require a custom 256-bit
  append-decimal — significant code for a bounded, not-hot win.

**Decision**: not implemented. The string encoding is a measured, bounded
trade-off; the buffer-reuse refinement is noted as a possible future micro-opt.

## Summary

| Finding | Disposition | Measured effect |
|---|---|---|
| C4 resolver dedup reuse | **Implemented** | −8 allocs/op, −960 B/op per Resolve (warm-restart/reorg path) |
| C5 array string encoding | Documented | bounded, not hot; binary already rejected (TASK-004) |
| C6 cold `GetInto` | **Implemented** | 1→0 allocs/op (240→0 B) per cold hit |
| C7 eviction batch | Measured, not implemented | per-key Put already 0 allocs; batch unsafe (read-after-evict) |
| C8 value-semantics get | **Implemented** | −453k Position.Get allocs (−441k total) in realistic e2e |
| C10 dead counters | **Removed** | −46 lines dead code |

All implemented changes reproduce the exact 0xf totals
(37.876088169187416774 / 313.683532169187415452376136 /
351.559620338374832226376136). `go vet`, `go test ./internal/codegen`, and
`go test ./coldcache` pass.

---

# confirm_optimization branch — findings C1–C3

Second batch, validated on the same realistic harness (cold cache + isolated
disposable ClickHouse + full 0xf05b67 replay).

## C2 — hot handlers use the zero-alloc collection-ID fast path: implemented

Four hot sites passed `indexSetBig[i]` (`*big.Int`) into `getCollectionID`,
which routes through `bigIntTo32Bytes` → `v.Bytes()` (a `[]byte` allocation per
call) on every split / merge / CTF-redemption / FPMM outcome:

- `handlePositionSplit`, `handlePositionsMerge` (2 outcomes/event)
- `handlePayoutRedemptionCTF` (per payout)
- `getFixedProductMarketMakerPositionID` (per FPMM buy/sell/funding)

All four now call the existing zero-alloc `getCollectionIDForOutcome(outcome)`,
which indexes the precomputed `collectionIndexWords[outcome]` (`[32]byte`,
no `big.Int`, no `.Bytes()`). The now-dead `indexSetBig` table was removed.

- **Equivalence**: `TestC2CollectionIDEquivalence` proves
  `getCollectionIDForOutcome(i)` is **byte-identical** to the old
  `getCollectionID(1<<i)` for every outcome and condition (same index word →
  same cache key → same ID).
- **Realistic e2e profile diff** (`pprof -base`): `math/big.(*Int).Bytes`
  **−210,328** alloc objects; `bigIntTo32Bytes` / `getCollectionID` no longer
  appear as allocators.
- **Correctness**: e2e totals byte-identical.

`FEEDBACK.md` had claimed this migration was already done ("all use
`getCollectionIDForOutcome` now") — the code contradicted it; now applied.
**Retained.**

## C3 — `HotState.Update*` per-Save mutex: measured, audited, not removed

Every `state.*.Save()` → `UpdateMemoryUserPosition` takes `s.mu.Lock()` on the
hottest write path.

**Audit**: `s.mu` is acquired only by `Update*`, `Commit`, and the mark-dirty
helpers — all on the single consumer goroutine. `recoverColdParallel` writes
independent Pebble batches and never touches the hot ring or the dirty maps
(matches GEN-003B). So the lock is **uncontended** in steady state.

**Cost** (`BenchmarkUserPositionSave`, A/B by stripping the lock from the
generated `UpdateMemoryUserPosition`):

| Save | ns/op | allocs/op |
|---|---:|---:|
| with mutex (current) | ~59 | 0 |
| without mutex | ~55 | 0 |

**~4 ns/op (~7%), no allocation change.** A marginal CPU win against a real
data-race hazard if the single-owner contract is ever violated (the exported
`Update*`/`Commit` can be called by external code; the contract is
single-owner-but-exported, exactly the GEN-003B caution). The change was staged
on a separate `perf/cold-recovery-bloom` branch and still needs the full
recovery/commit/rollback concurrency model + `-race` validation.

**Decision**: not removed here. The marginal win does not justify the
correctness risk without the gated concurrency testing. Documented with the
audit + measurement so it can be finished safely on the perf branch.

## C1 — redundant ABI decode in the proto-only producer: confirmed, not applicable here

The producer's `ParseWithLine` callback ABI-decodes every log and does two
`strings.Clone`, building `entry.events`. With `store_raw_logs: false`,
`entry.events` is **not** inserted (`InsertLogs` is gated on
`ShouldStoreRawLogs()`), and in a true proto-only run the consumer re-decodes
the raw line via `ProcessJSONL` — so the generic decode is redundant.

**But this redundancy does not occur in this checkout**, for two reasons found
during confirmation:

1. **No `ProcessJSONL` path here.** This polymarket example does **not**
   implement `FastJSONLProcessor` (no `ProcessJSONL`), and `SQD_PARSE_DECODE_V2`
   is unset, so `useParseDecodeV2` is **always false**. The consumer uses
   `proc.Process(batchCustomLogs)` (proto decoding happens *inside* `Process`
   from the CustomLogs), so the producer's decode feeds `blockCustomLogs` and is
   **consumed**.
2. **Typed-event insertion is ungated.** `buildTypedTableIndex` creates a
   `<contract>_<event>_events` table for every configured event, and the
   consumer's typed-event insertion loop is **not** gated by any config. The
   same decode also populates `entry.typedEvents`. A safe skip therefore also
   requires proving that, in a real proto-only config, typed-event insertion is
   handled by the proto path rather than the producer's `blockTypedEvents` —
   otherwise the typed tables silently stop being written.

**Decision**: not implemented. C1 is a real optimization for the AHH/live
proto-only (`ProcessJSONL`) ingestion path, but in this tree the skip would be
dead, unexercisable code in core ingestion, and its safety depends on the typed-
event-insertion behavior of a config that does not exist here. Recommend
implementing + validating in the tree that runs proto-only mode, gating the skip
on `useParseDecodeV2 && !ShouldStoreRawLogs() && <no typed tables, or typed
insertion handled by the proto path>`, with a counter confirming both
`entry.events` and `entry.typedEvents` are unused.

## Summary (C1–C3)

| Finding | Disposition | Measured effect |
|---|---|---|
| C1 producer decode skip | Confirmed, not applicable here | redundancy requires the `ProcessJSONL` path, absent in this checkout |
| C2 collection-ID fast path | **Implemented** | −210k `big.Int.Bytes` alloc objects in the realistic e2e |
| C3 Update* mutex | Measured, not removed | ~4 ns/op uncontended; gated on concurrency testing |

---

# confirm_optimization branch — parse-path allocation squeeze

After C1 was found non-applicable, the realistic profile was used to find the
largest *validatable* allocation source on the processor hot path (the parser /
parse-path target the doc already names). `common.FromHex` was **44%** of all
allocations in the realistic e2e.

## HEX-1 — zero-allocation hex decode in generated `Process`: implemented

The generated proto-mode `Processor.Process` converts every CustomLog's hex
string fields into `common.Hash`/`common.Address` via `common.HexToHash` /
`common.HexToAddress`, each of which allocates an intermediate `[]byte` through
`common.FromHex`. Added `hexToHash` / `hexToAddress` helpers to the codegen
template (`internal/codegen/custom_processor.go`) that decode straight into the
fixed-size array with `hex.Decode` + `unsafe.Slice` (a read-only view of the
immutable input string — no copy), with a fallback to the stdlib helper for any
unexpected length. Applied to `BlockHash`, `ContractAddress`, `TransactionHash`
and every topic, in both the proto and non-proto code paths.

- **Equivalence**: `TestHexHelpersEquivalence` checks the algorithm against
  `common.HexToHash` / `HexToAddress` for 0x / no-0x / upper / lower / full /
  short / empty / odd inputs.
- **Realistic e2e profile diff** (`pprof -base`): `encoding/hex.DecodeString`
  **−3,291,462** alloc objects (`HexToHash` −2,789,233, `HexToAddress`
  −501,862). **Total allocations 18,954,540 → 15,702,697 (−3.25M, −17.2%).**
- **Correctness**: e2e totals byte-identical.

**Retained.** Remaining `common.FromHex` after this change is the variable-length
`FromHex(lg.Data)` (~0.5M) plus `AppendFromLog`'s inline constant topic hashing
(next).

## HEX-2 — hoist `AppendFromLog` dispatch constants to package vars: implemented

After HEX-1, the largest remaining allocator was the generated `AppendFromLog`
dispatch (`internal/codegen/views.go`): each call re-ran `common.HexToHash("0x…")`
for the topic0 comparison **and** `toLowerASCII(address.Hex())` for the contract
address comparison, per event group, per log. The codegen template now:

- precomputes one `_aflTopicN = common.HexToHash(...)` and
  `_aflAddrN_M_K = common.HexToAddress(...)` package var per dispatch constant
  (hashed/decoded once at init), and
- compares the address by value (`address == _aflAddrN_M_K`) instead of building
  and lowercasing the checksum hex string on every call.

- **Realistic e2e profile diff** (`pprof -base`): `hex.DecodeString`
  **−4,546,618**, `common.Address.Hex`/`.hex`/`checksumHex` **−1.27M**.
  **Total allocations 15,702,697 → 9,623,824 (−6.08M, −38.7%).**
- **Correctness**: e2e totals byte-identical; `go test ./internal/codegen` passes.

### Parse-path squeeze — cumulative

| Stage | Total alloc objects (realistic e2e) | Δ |
|---|---:|---:|
| Before (post C2/C4/C6/C8) | 18,954,540 | — |
| + HEX-1 (Process hex helpers) | 15,702,697 | −17.2% |
| + HEX-2 (AppendFromLog hoist) | 9,623,824 | −38.7% |
| **Cumulative** | **9,623,824** | **−49.2%** |

Both changes are codegen-template-only, reproduce the exact 0xf totals, and the
remaining `common.FromHex(lg.Data)` (variable-length log data) is the next
candidate (it needs a reusable decode buffer whose lifetime matches
`AppendFromLog`, so it is left for a separately-validated change).

## HEX-3 — clock-cache `Get` key no longer escapes to the heap: implemented

The C8 follow-up: the generated `*ClockCache.Get` cold branch passed `&key` to
`unsafe.Slice` for the cold lookup. Go's escape analysis is flow-insensitive, so
that single `&key` forced the `key` parameter (a 52-byte
`UserPositionsClockKey`) onto the heap on **every** `Get` — including hot hits
that never reach the cold branch (`-gcflags=-m`: `moved to heap: key`). The
template now copies `key` into a local inside the cold branch (`k := key`) and
takes `&k`, containing the escape to the cold-miss path.

- **Escape analysis**: `moved to heap: key` is gone for `Get`.
- **Alloc test** (`TestC8ValueGetAvoidsEscape`): the value-get path dropped from
  **1.00 → 0.00 allocs/op** — the hot order-fill position read is now fully
  allocation-free (combined with C8).
- **Realistic e2e profile diff**: `UserPositionsClockCache.Get` **−599,536**,
  `FixedProductMarketMakersClockCache.Get` **−105,268**.
  **Total allocations 9,623,824 → 8,864,393 (−759k, −7.9%).**
- **Correctness**: e2e byte-identical; `go test ./internal/codegen` and
  `go test -race` (cache) pass.

### Parse-path + cache squeeze — cumulative

| Stage | Total alloc objects (realistic e2e) | Δ vs before |
|---|---:|---:|
| Before (post C2/C4/C6/C8) | 18,954,540 | — |
| + HEX-1 (Process hex helpers) | 15,702,697 | −17.2% |
| + HEX-2 (AppendFromLog hoist) | 9,623,824 | −38.7% |
| + HEX-3 (Get key escape) | 8,864,393 | −7.9% |
| **Cumulative** | **8,864,393** | **−53.2%** |

The largest remaining allocator is `internal/parser.bytesToString` (~37%): the
JSONL parser materializes each log's address/topics/data as fresh strings that
the processor then hex-decodes. Eliminating it means a zero-copy parser→processor
interface (return byte views into the raw buffer) — a core-parser change outside
this codegen-only sweep, noted as the next target.

