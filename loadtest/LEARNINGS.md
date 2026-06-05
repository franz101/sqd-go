# Loadtest Learnings

No existing loadtest code was deleted while adding the proto Decimal256 position
comparison path.

## JSONL to Proto Column Ring E2E

`proto_column_ring_e2e_bench_test.go` compares three typed-table ingestion
shapes on `sqd/testdata/exchange_events.jsonl`:

- easyjson decode into structs, row ring, then proto insert columns
- raw `jlexer` decode into row ring, then proto insert columns
- generated-style `jlexer` decode directly into preallocated `ch-go/proto`
  column ring slots, then pointer handoff to the DB sink

The parity test hashes the typed order and derived position table contents, so
the benchmark compares equivalent database-facing rows.

Command:

```bash
GOCACHE=/tmp/go-build go test ./loadtest \
  -run '^TestProtoColumnRingE2EParity$' -v

GOCACHE=/tmp/go-build go test ./loadtest \
  -run '^$' -bench 'BenchmarkE2E_' -benchmem -benchtime=20x -count=5
```

Representative results on the local i7-8700:

```text
easyjson -> row ring -> proto DB: 18.9-19.3ms/op, 817-836 MB/s, 1.44MB/op, 21,540 allocs/op
raw lexer -> row ring -> proto DB: 16.9-19.2ms/op, 823-932 MB/s, 0 B/op, 0 allocs/op
lexer -> proto ring pointer DB:   16.5-17.7ms/op, 893-960 MB/s, 0 B/op, 0 allocs/op
```

Profile result: CPU is dominated by JSON token scanning and UInt256 hex decode;
pointer handoff and proto column consumption are not the bottleneck. Allocation
is setup-time ring/column reservation, not per-ingest work. The direct proto
ring used less reserved heap than the row ring in the paired profile because it
does not keep decoded row structs plus insert columns.

Large-load GC confirmation, using repeated passes over the 15.8 MB fixture to
avoid allocating a 2 GB input buffer:

```bash
GOCACHE=/tmp/go-build LOADTEST_E2E_GC=1 \
  LOADTEST_E2E_TARGET_BYTES=2147483648 \
  go test ./loadtest -run '^TestProtoColumnRingE2ELargeLoadGC$' -v

GOCACHE=/tmp/go-build LOADTEST_E2E_TARGET_BYTES=2147483648 \
  go test ./loadtest -run '^$' -bench 'BenchmarkE2E_' -benchmem \
  -benchtime=1x -count=1
```

Results for 2,148,984,552 bytes, 1,984,240 typed order rows, and 1,984,240
derived position rows:

```text
easyjson GC:   4.322s, 196.13MB allocated, 2,929,448 mallocs, 2 GC cycles, 160us GC pause
proto ring GC: 3.783s, 32B allocated, 1 malloc, 0 GC cycles, 0s GC pause

easyjson benchmark:   4.775s/op, 449.99 MB/s, 196.15MB/op, 2,929,466 allocs/op
raw row-ring bench:   4.687s/op, 458.45 MB/s, 0 B/op, 0 allocs/op
proto ring benchmark: 4.649s/op, 462.29 MB/s, 0 B/op, 0 allocs/op
```

## Proto Decimal256 Position Path

The new `positions` loadtest command compares:

- shopspring `decimal.Decimal` position state and conversion to ClickHouse
- pointer-free `protomath.Decimal256` state using ClickHouse-compatible
  two's-complement `Decimal256(18)` coefficients

The workload is split into two tasks:

1. Keyed lookup plus order-event position math.
2. ClickHouse ingest, using either per-batch inserts or one ch-go `OnInput`
   streaming insert.

Native ClickHouse Decimal inserts require the column type to include the scale,
for example `Decimal256(18)`. ch-go's generated `proto.ColDecimal256` reports
`Decimal256` without scale, so the loadtest uses a local raw-column wrapper that
streams 32-byte little-endian Decimal256 coefficients as `Decimal256(18)`.

## Measured CLI Run

Command:

```bash
/tmp/loadtest_bin -port 9003 -db polymarket_loadtest positions \
  -positions 100000 \
  -events 200000 \
  -engine both \
  -insert both \
  -chunk-size 2000
```

Results:

```text
proto batch:      update 49.372ms, ingest 3.023278s, e2e 3.072650s, 442.13KB, 4,858 mallocs
proto stream:     update 49.372ms, ingest 75.191ms,  e2e 124.563ms, 109.32KB, 1,592 mallocs
shopspring batch: update 194.277ms, ingest 3.022249s, e2e 3.216526s, 190.10MB, 5,340,482 mallocs
shopspring stream:update 194.277ms, ingest 154.959ms, e2e 349.236ms, 189.78MB, 5,337,212 mallocs
```

Takeaways:

- ch-go `OnInput` streaming removes per-batch round trips. Proto ingest improved
  from 3.023s to 75ms for 100k rows.
- Proto Decimal256 math removes shopspring's heap churn during keyed position
  updates. Task 1 went from 194ms and 127MB allocated to 49ms and 0B allocated.
- With streaming insert enabled, proto Decimal256 E2E was 2.8x faster than
  shopspring and reduced allocations by roughly 1778x.

## State Get/Save Hot-Cold Load Reproduction

The `state` loadtest command reproduces the slow custom-processor state pattern
without changing production processor code. It models only the hot path needed
for user-position PnL:

- `State.Get(user, token)` checks a hot in-memory map.
- Cold misses resolve from ClickHouse `loadtest_state_positions`.
- `State.Save(position)` updates hot state and marks the key dirty.
- Dirty state is enqueued to a dedicated writer connection and inserted in
  chunks; the main processing path does not write directly to ClickHouse.
- The current-like engine resolves each missing key immediately.
- The improved engine prefetches missing keys in event windows and resolves
  them in ClickHouse chunks before processing the events.

Default-sized repro:

```bash
timeout 120s go run ./loadtest -port 9003 -db polymarket_loadtest_state state \
  -positions 10000 \
  -events 20000 \
  -engine both \
  -prefetch-batch 2000 \
  -resolve-chunk 500 \
  -insert-chunk 2000 \
  -queue-cap 16 \
  -flush-interval 2s
```

Results:

```text
current:  1m17.003s, 20,000 gets, 10,000 cold misses, 10,000 resolve queries,
          39 queued flushes, 43 insert batches, 129.78MB, 1,292,486 mallocs, 23 GC
improved: 1.429s,   20,000 gets, 10,000 cold misses, 20 resolve queries,
          1 queued flush, 5 insert batches, 14.61MB, 43,372 mallocs, 3 GC
latest state parity: rows=10,000 hash=bc16086054abd85c
improvement: 53.88x faster, 8.88x less allocation, 29.80x fewer mallocs,
             500x fewer ClickHouse resolve queries
```

Large improved-only check:

```bash
timeout 120s go run ./loadtest -port 9003 -db polymarket_loadtest_state_large state \
  -positions 100000 \
  -events 200000 \
  -engine improved \
  -prefetch-batch 4000 \
  -resolve-chunk 1000 \
  -insert-chunk 4000 \
  -queue-cap 32 \
  -flush-interval 2s
```

Result:

```text
improved: 8.224s, 200,000 gets, 100,000 cold misses, 100 resolve queries,
          4 queued flushes, 50 insert batches, 154.23MB, 420,226 mallocs, 4 GC
latest state: rows=100,000 hash=7cf0965ef7ae819b
```
