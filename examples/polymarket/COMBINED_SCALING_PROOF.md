# Combining it all: why the single-stage wins don't compound — and the fix

The isolated benchmarks are real (byte-scanner parse 1.8M→8M ev/s on 6 cores;
protomath position math 2.6M ev/s). But on the real polymarket workload — **22
event types, multi-core, the ordered custom processor, and ingest** — they do not
compound. This file measures exactly why, on the real block-84M `OrderFilled`
corpus (757,137 events, 143,330 distinct positions), and shows the architecture
that recovers the scaling. Everything here is measured by `combined_test.go`
(`go test ./examples/polymarket/ -run TestCombined -v`), reproducible, with a
**bit-identical-state correctness gate** on every sharded run.

## Why it doesn't compound today

The production pipeline (mapped in `internal/ingestion/ingestion.go`) is a
producer||consumer split connected by an SPSC ring:

```
producer:  fetch (parallel) -> parse (1 core) ----ring----> consumer: custom processor (1 core, ORDERED) -> insert
```

After the ring **everything is single-threaded**: parse on one core in the
producer, the custom processor on one core in the consumer. The isolated 8M-ev/s
parse used private per-worker batches and no processor — neither holds here. Two
structural taxes follow:

### Tax 1 — the harmonic-sum trap (serial combine)

Combining two stages on one core gives `1/(1/parse + 1/process)`, dominated by
whichever stage you did NOT optimize:

| | ev/s |
|---|---|
| parse alone (1 core) | ~1.8M |
| process alone (1 core) | 2.57M |
| **combined parse+process (1 core)** | **1.08M** |

`1/1.8 + 1/2.57 ≈ 1/1.07M` — measured 1.08M. A 2× faster parser barely moves a
combined number the processor dominates. This is "amazing on single stuff, slow
combining it all," quantified.

### Tax 2 — more than 4 event types

22 event types share the hot path. The generated parser (`generated/parser.go`)
pays, per log, a topic0 switch + (for the 3 `OrderFilled` variants that share a
topic0) 2–3 address compares, an **unconditional dead-struct fill** of every
event into `batches`+`slot` regardless of handlers, and an `interface{}`
type-assert in every `Batch.Append`. These are per-event constants that scale
with event-type count. The byte-scanner (`parserv2.go`) + typed append remove
them — that is the parse-layer lever (see `PARSER_V2_PROOF.md`).

## The fix: shard the ordered processor by entity key

The custom processor's `OrderFilled` handler mutates exactly one entity key,
`Position(maker, tokenID)` (`custom_processor.go` `handleOrderFilledValues` →
`updateUserPositionWithBuy/SellD256`). State is fully keyed, there are **no global
accumulators**, and the only intra-block coupling is same-`(user,tokenID)`
ordering. That is the precondition for ECS/DB-style key-sharding: hash the entity
key to a shard, preserve order WITHIN a shard, run shards in parallel. Same key →
same shard → per-key order preserved; cross-key order is irrelevant.

`combined_test.go` partitions the real corpus by `hash(user,tokenID) % S`, folds
each shard independently, then asserts the merged state is **bit-identical** to
the serial fold (`STATE OK`).

| processor | ev/s | vs serial 1 core | hottest shard |
|---|---|---|---|
| serial (1 core) | 2.57M | 1.00× | — |
| sharded ×2 | 5.57M | 2.17× | 50.3% (ideal 50%) |
| sharded ×4 | 11.00M | 4.28× | 25.5% (ideal 25%) |
| **sharded ×6** | **14.87M** | **5.79×** | 17.4% (ideal 16.7%) |
| sharded ×10 | 14.47M | 5.63× | 10.4% (ideal 10%) |

Two facts that matter:
- **Skew is a non-issue.** Position keys are keccak-derived, so the low bits are
  uniform: the hottest shard is within ~0.7pp of ideal at every level. Load
  imbalance does not bound the speedup.
- **The plateau (~5.8× at ×6) is physical core count**, not skew — this is a
  10-core Apple-Silicon laptop with ~6–8 performance cores; the efficiency cores
  add little. On a server with N uniform cores expect ~N×.

## Combining the parallel stages: don't barrier, pipeline

Making both stages parallel is not enough — *how* you connect them decides the
result.

| combined topology | ev/s | vs serial |
|---|---|---|
| serial parse + serial process (today) | 1.08M | 1.00× |
| **barrier**: parallel parse → then sharded process | 3.83M | 3.56× |
| **pipelined**: parallel parse ‖ router ‖ sharded process | **6.61M** | **6.14×** |

isolated stages: parse(10) 11.22M · router(hash+scatter, 1 core) 20.18M · process(10 shards) 12.76M

- A **barrier** between the two parallel stages re-creates the harmonic-sum trap
  *at the parallel level*: cores idle during whichever stage they are not in. It
  recovers only 3.56× of a possible ~11×.
- **Pipelining** (parse page N+k while routing page N+1 while processing page N)
  removes the barrier. The router does only hash+scatter and runs at 20M ev/s —
  2× the compute — so it is not the bottleneck; the ceiling is
  `min(parse, process) ≈ 11M`. The measured 6.61M is bounded by the single-router
  scatter hop + channel/batch overhead, but already 6.1× serial and well past the
  barrier.
- This topology **is** the existing producer||consumer ring — with both sides made
  internally parallel. The architecture is already right; the levers are (a) shard
  the consumer (custom processor) and (b) parallelize the producer parse.

## Where ingest fits

Insert is ~3% of the live profile and parallelizes for free under sharding: each
shard owns disjoint keys, so each builds its own ch-go columnar (SoA) batches and
flushes them concurrently (ClickHouse accepts concurrent inserts to one
MergeTree; `SQD_ASYNC_INSERT_FLUSH` already decouples the wire round-trip). No new
contention is introduced.

## Honest scope

- **`OrderFilled` only** — the dominant block-84M event (no dynamic arrays). The
  other handlers mutate the same kind of keyed state (`Condition`, `FPMM`,
  `Position`); reference reads (`Condition`/`FPMM`) are idempotent and prefetched
  per block, so they replicate read-only across shards.
- **Compute-only.** This measures the state math, not the cold-tier lookups the
  live `CUSTOM` (54%) also pays (Pebble ~8µs / ClickHouse ~1.9ms on miss).
  Sharding parallelizes those I/O lookups too, so the real-world win is likely
  **larger** than the compute-only 5.8× shown here — I/O parallelizes better than
  CPU.
- Absolute ev/s is thermal-dependent on a laptop; numbers are **best-of-K**
  (peak achievable, the stable estimator). The ratios are the takeaway.

## Bottom line

The single-stage wins are real but get erased by (1) the harmonic-sum of serial
stages and (2) a barrier between parallel stages. The recovery is: **key-shard the
ordered custom processor (proven bit-identical, skew-free, ~5.8× on 6 cores) and
pipeline it behind a parallel producer parse via the existing ring (6.1× combined,
ceiling ~11×).** The custom processor — not the parser — is the lever that matters
once you combine it all.
