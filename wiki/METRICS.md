# Metrics, observability & tuning knobs

A practical guide to what sqd-go tells you while it runs, how to graph it, how to
profile it, and which environment variables actually move the needle.

There are four layers of observability, in increasing order of setup cost:

1. **stdout stats** — always on, zero setup. Read these first.
2. **ClickHouse metrics** — opt-in time series in `monitoring.indexer_metrics`.
3. **Grafana** — dashboards over the ClickHouse data, one `docker compose up`.
4. **CPU profiling** — a `--cpuprofile` file for `go tool pprof`.

> See also [`MONITORING.md`](./MONITORING.md) for the Grafana/ClickHouse setup
> from the dashboard side. This document covers the full picture, including the
> stdout lines and profiling.

---

## 1. stdout stats (always on)

Every `SQD_STATS_INTERVAL` (default **10s**) the ingestion loop prints two — or
three — lines per chain. The cadence is configurable: pass a Go duration
(`5s`, `500ms`) or a bare number of seconds (`30`). Values below the **50ms**
floor, or anything unparseable, fall back to the 10s default
(`internal/envconfig/envconfig.go:179-198`).

### The `stats` line

```text
Chain 137: stats interval | checkpoint: 71240448 | next: 71250000 | buffered: 412 | +9552 blocks, 955 blk/s (avg 612) | +1183 non-empty, +20114 events in 10s | total: 41250000 blocks, 88123456 events
```

| Field | Meaning |
|-------|---------|
| `checkpoint` | Last block whose data is durably committed to ClickHouse. Survives restarts. |
| `next` | The block the **consumer** is about to process (the live cursor). |
| `buffered` | Entries sitting in the replay buffer — fetched/parsed but not yet consumed. Healthy backfill keeps this well above 0; a steady **0** means the consumer is starved (fetch is the bottleneck). A value near the buffer cap means the consumer/insert is the bottleneck and the producer is back-pressured. |
| `+N blocks, X blk/s (avg Y)` | Blocks of **chain coverage** advanced this interval (incl. empty gaps the consumer skipped), the instantaneous rate, and the running average since start. |
| `+N non-empty, +M events in <dur>` | Non-empty blocks and decoded events this interval, over the actual measured interval. |
| `total: ... blocks, ... events` | Cumulative since this process started. |

When a single fetch is taking a long time, a `0 blk/s` tick would otherwise look
like a stall, so the line appends `| fetching from <block> (<elapsed>)` while a
request has been in flight for ≥1s. That is progress, not idleness.

**How to read `blk/s`.** This is the headline throughput number and the right
one to compare across runs and across dense vs. sparse ranges, because it counts
*coverage* (including skipped empty blocks), not just non-empty blocks. The
`avg` smooths out per-tick noise from variable page sizes and fetch latency —
trust it over a handful of instantaneous ticks. A throughput regression shows up
as a falling `avg blk/s`.

### The `profile` line

```text
Chain 137: profile interval | fetch=3.1s parse=4.2s decode=1.0s marshal=210ms insert=900ms custom=1.4s | consumer_wait=120ms producer_backpressure=0s | parse_iterations=9552
```

These are **wall-time deltas accumulated this interval** across pipeline stages.
They overlap (producer and consumer run concurrently), so they do not sum to the
interval — instead, the largest one points at the bottleneck.

| Stage | What it times |
|-------|---------------|
| `fetch` | Network time pulling pages from the SQD portal. |
| `parse` | JSONL parsing of the fetched bytes. Often the wall for single-threaded producers. |
| `decode` | Turning parsed rows into typed events. |
| `marshal` | Building the column batches for insertion. |
| `insert` | ClickHouse insert time (runs on a background goroutine via the pending-insert path). |
| `custom` | Time inside your custom processor (e.g. Polymarket resolution logic). |
| `consumer_wait` | Time the consumer spent blocked waiting for the producer to deliver buffered work. High = fetch/parse-bound. |
| `producer_backpressure` | Time the producer spent blocked because the replay buffer was full. High = insert/consumer-bound. |
| `parse_iterations` | Parse loop iterations this interval (rough proxy for blocks processed by the parser). |

Rule of thumb: compare `consumer_wait` vs. `producer_backpressure`. Whichever is
larger tells you which side of the pipeline is starving the other.

### The optional `processor profile` line

If the active custom processor implements profiling, a third line appears with
processor-specific timings, e.g. for Polymarket:

```text
Chain 137: processor profile interval | condition_resolve=800ms condition_round_trips=42 | fpmm_resolve=600ms fpmm_round_trips=18
```

`*_round_trips` count synchronous ClickHouse lookups the processor made — a
common backfill hot spot. (`internal/ingestion/ingestion.go:1085-1124`.)

---

## 2. ClickHouse metrics (opt-in)

Set `SQD_METRICS_CH` to a truthy value (`1`, `true`, `yes`, `on`) to start a
background goroutine that samples `runtime.MemStats` + `getrusage` and **INSERTs
one row per chain** into `monitoring.indexer_metrics`. It is **off by default**;
explicit falsey values (`0`, `false`, `no`, `off`) also disable it
(`internal/envconfig/envconfig.go` `GetenvBool`).

```sh
SQD_METRICS_CH=1 sqd-go start examples/polymarket
```

The writer owns its **own** ClickHouse connection (a `ch.Client` is not safe for
concurrent use, and the ingestion hot path must never block on a metrics
insert). The database and table are auto-created on start
(`internal/monitoring/recorder.go`).

The ingestion loop only calls `monitoring.Observe(...)` — a cheap, non-blocking
snapshot update — once per stats tick. So the effective sampling cadence is
roughly `min(SQD_STATS_INTERVAL, SQD_METRICS_CH_INTERVAL)`: the writer flushes on
its own ticker, but the per-chain counters it flushes only refresh when the stats
tick fires (`ingestion.go:1129`).

### Tunables

| Env var | Default | Meaning |
|---------|---------|---------|
| `SQD_METRICS_CH` | unset (off) | Truthy enables the writer; falsey/unset disables it. |
| `SQD_METRICS_CH_INTERVAL` | `5s` | Writer flush cadence (Go duration). |
| `SQD_METRICS_CH_TTL_DAYS` | `7` | Retention (`TTL`) on the metrics table. |

### Columns in `monitoring.indexer_metrics`

One row per `(chain_id, ts)`:

| Column | Source |
|--------|--------|
| `blocks_total`, `events_total` | Cumulative counters from the chain. |
| `blocks_per_sec`, `events_per_sec` | Derived from the delta since the previous flush. |
| `head`, `checkpoint`, `checkpoint_lag` | Live cursor, durable cursor, and `head - checkpoint` (0 when caught up). |
| `heap_alloc_bytes`, `heap_sys_bytes`, `heap_objects` | `runtime.MemStats` heap. |
| `sys_bytes`, `stack_sys_bytes` | Total OS memory obtained, stack reservation. |
| `mallocs_per_sec`, `frees_per_sec` | Allocation churn (delta-derived). |
| `num_gc`, `gc_pause_last_ns`, `gc_cpu_fraction`, `next_gc_bytes` | GC count, last pause, fraction of CPU spent in GC, next-GC heap goal. |
| `num_goroutine` | `runtime.NumGoroutine()`. |
| `cpu_cores_used` | CPU-seconds per wall-second from `getrusage` (user+sys), delta-derived. >1 means multi-core. |

The first row after start has zero rates (it primes the delta baseline). Note the
cold-cache budget can push `sys_bytes`/RSS well above `heap_alloc_bytes` — to cut
RSS, lower `SQD_COLDCACHE_MB` and `SQD_PARALLEL_FETCHERS` (see knobs below).

---

## 3. Grafana

`compose.yml` provisions everything from disk — no manual UI setup:

- A **ClickHouse data source** (official `grafana-clickhouse-datasource` plugin),
  auto-installed and pointed at the `clickhouse` service over the native port.
- Two dashboards under the **sqd-go** folder:
  - **Indexer Runtime** (`grafana/dashboards/indexer-runtime.json`) — graphs
    `monitoring.indexer_metrics`: blocks/s, events/s, heap, allocation rate, GC
    CPU% and pause, goroutines, CPU cores, checkpoint lag.
  - **ClickHouse Server** (`grafana/dashboards/clickhouse-server.json`) — graphs
    the server's own `system.*` tables: memory, CPU, query/insert throughput,
    parts, etc.

Bring it up:

```sh
docker compose up -d         # starts clickhouse + grafana
open http://localhost:3001   # default login admin / admin
```

The **ClickHouse Server** dashboard has data immediately. The **Indexer
Runtime** dashboard stays empty until you run the indexer with `SQD_METRICS_CH=1`
(section 2). Grafana connects as a read-only ClickHouse user; override ports and
credentials via `.env` (`GRAFANA_PORT`, `GRAFANA_CH_USER`, `GRAFANA_CH_PASSWORD`).

---

## 4. Profiling (CPU profile → file)

> **Correction to older docs:** sqd-go has **no** `net/http/pprof` server and
> **no** `SQD_PPROF_ADDR` environment variable (despite mentions elsewhere).
> Profiling is **profile-to-file only**, driven by CLI flags.

Pass `--cpuprofile <file>` to `start` (or `dev`). The run wraps execution in
`pprof.StartCPUProfile` / `StopCPUProfile`, writing the profile on exit
(`internal/cli/cli.go:89-94`, `internal/cli/run.go`):

```sh
sqd-go start examples/polymarket --cpuprofile cpu.prof
# ... let it run, then stop it (Ctrl-C). The profile is flushed on shutdown.

go tool pprof cpu.prof
# in pprof:  top   list <func>   web
```

For a quick text view of the hot functions without the interactive shell:

```sh
go tool pprof -top cpu.prof
```

Profiling is captured for the whole process lifetime, so run a representative
window (e.g. a few minutes of steady backfill) before stopping.

**`--cpuprofile` is blind to blocked time.** It samples via SIGPROF, which only
fires on a goroutine that is actively scheduled on a CPU. A goroutine parked on
a channel receive, a mutex, or a network read contributes **zero samples** —
not "low", zero — no matter how long it actually waited. This matters here
specifically: `CompactionPruneState` (`internal/template/templates/code/compaction.go.tmpl`)
runs a sequence of synchronous `DELETE` + `OPTIMIZE TABLE ... FINAL` queries on
`store.Conn()`, called inline from `commitCustomProcessing` on the consumer
goroutine. While it runs, the consumer can't advance to call any pending
`batchFlush`, which starves the bounded (`insertBatchPoolSize = 4`) batch-object
pool the producer's `ParseBatchForInserts` depends on — so the producer also
stalls, blocked on `<-p.batchFree`. Both stalls get folded into the `profile`
line's `custom=` and `parse=` buckets respectively (technically correct wall-clock
deltas, since `profCustomNanos`/`profParseNanos` are `time.Since` spans that
include any blocking inside them) but the bucket names suggest CPU-bound work,
not "blocked on a ClickHouse query elsewhere in the process." A `--cpuprofile`
taken during such a stall shows ~99% in whatever unrelated goroutine happens to
be CPU-bound at the time (e.g. the parser running on other chains) and 0
samples in either blocked goroutine — actively misleading, not just unhelpful.

**`--fgprofile <file>`** uses [fgprof](https://github.com/felixge/fgprof)
instead: it samples `runtime.GoroutineProfile()` at 99Hz, which captures every
goroutine's current stack — running *or* parked — so blocked time gets full
call-stack attribution alongside on-CPU time, in the same pprof-compatible
output `go tool pprof` already reads:

```sh
sqd-go start examples/polymarket --fgprofile wall.pprof
go tool pprof -top wall.pprof
# a goroutine stuck in CompactionPruneState's ch.Client.Do(...), or the
# producer parked on <-p.batchFree, now show up with real sample counts
# instead of vanishing.
```

Verified against an isolated, non-prod repro of exactly this shape (one
goroutine blocked on a channel receive, one blocked on a simulated slow
ClickHouse call, one doing real CPU work): `--cpuprofile`-equivalent sampling
attributed 98.66% of samples to the CPU-bound goroutine and **0** to either
blocked goroutine; fgprof attributed ~21% to the channel receive and ~14% to
the simulated compaction call, each with the actual blocking call stack
(`runtime.gopark <- runtime.selectgo <- ...` / `runtime.gopark <- time.Sleep <-
...`). `--cpuprofile` remains the right tool when you already know the
bottleneck is CPU-bound (e.g. comparing two parsing implementations);
`--fgprofile` is the right one when ingestion looks stalled and you don't yet
know whether that's compute, a lock, or a slow query.

### Granular parse profiling (`-tags sqdparseprof`)

The generated `parser.go` has a second, finer-grained profiler that breaks the
`parse=` bucket above down by sub-stage (line-scan, JSON lex, header parse,
per-log-field parse, event decode, batch append) and prints a `[PARSE 10s]`
summary every 10 seconds. It is opt-in via a build tag because it times every
line/field/event individually — millions of clock reads per batch:

```sh
go build -tags sqdparseprof .
# or: go test -tags sqdparseprof ./...
```

```text
[PARSE 10s] 5.43M ev | 14.2K blk | 541633 ev/s | 1417 blk/s | 71 batches | peak 76.5K ev/batch
[PARSE 10s] jsonFields 41%(12ns/ev)  decode 28%(8ns/ev)  hdr  3%  scan  9%  lex 11%  logs  6%  append  2%
```

The default build (`parseProfilingEnabled = false`, an untyped constant in a
build-tagged companion file) compiles the whole thing away — no clock reads,
no mutex, no log lines (`internal/codegen/codegen.go`, `parser.go.tmpl`'s
`_pnow`/`_psince`).

**The clock is `tsc.UnixNano()` (`github.com/templexxx/tsc`), not `time.Now()`.**
A `time.Now()`-per-field clock was too expensive to leave running by default;
swapping to a calibrated TSC-register read is what makes `-tags sqdparseprof`
viable to run continuously instead of only for one-off debugging. Measured on
an Intel i7-8700 (12 logical CPUs, invariant TSC):

| Clock | Cost/call | Notes |
|-------|-----------|-------|
| `time.Now().UnixNano()` | ~42ns | baseline |
| `tsc.UnixNano()`, unfenced (out-of-order allowed) | ~9.7ns | **not used** — see accuracy below |
| `tsc.UnixNano()`, after `tsc.ForbidOutOfOrder()` | ~18.8ns | what's actually wired in |

RDTSC is not a serializing instruction, so an unfenced start/end pair can be
reordered by the CPU around very short spans. Measured by timing a fixed
~21–25ns span 2M times: unfenced reads averaged **~10.4ns** (a ~50% relative
*undercount*) versus ~21.1ns for fenced reads and ~24.9ns for `time.Now()` on
the same span — i.e. the cheap path would have quietly understated the
cheapest profiled stages (line-scan, per-field parsing) relative to the
heavier ones (decode, append), skewing the `[PARSE 10s]` percentage breakdown.
`_pinit` (a `sync.OnceFunc`) calls `ForbidOutOfOrder()` once per process when
profiling is enabled, paying one extra ~2s recalibration pass at startup in
exchange for correctness — still a net win since it's gated behind the same
build tag as everything else.

End-to-end (`BenchmarkPolymarketGeneratedParseProtoReuse`, a real 61.7MB
Polymarket JSONL fixture, median of 6 runs): the always-on profiling tax fell
from **~32.5%** (old `time.Now()` clock) to **~22.3%** (fenced `tsc` clock)
over the profiling-off baseline — a ~31% relative cut in the cost of leaving
`-tags sqdparseprof` on, without changing what the numbers mean.

TSC clock drift from the single startup calibration (this library does not
auto-recalibrate; `tsc.Calibrate()` would need to be called periodically for
that) measured at roughly **-0.008ppm**, i.e. well under a millisecond of
drift over a 24-hour backfill — irrelevant at the scale `[PARSE 10s]` reports.

On hardware without invariant TSC (or non-amd64), `tsc.UnixNano()` silently
falls back to `time.Now().UnixNano()` — no fallback branch needed in the
generated code, just the original (now redundant) cost.

---

## 5. Tuning knobs

The operationally important environment variables, with their real defaults and
clamps as implemented in `internal/envconfig/envconfig.go`. These are read at
startup.

| Env var | Default | Effect |
|---------|---------|--------|
| `SQD_COLDCACHE_MB` | `0` (auto-size from RAM) | Cold-cache memory budget in MB. **The first lever to cut RSS** — set e.g. `1024` to cap it. Actual usage can be ~2× this for bookkeeping. |
| `SQD_PARALLEL_FETCHERS` | `6` (clamped 1..32) | Number of parallel fetch workers. **The dominant RAM *and* throughput knob** — the look-ahead buffer is roughly `page_size × workers`. Raise for throughput, lower to bound memory. |
| `SQD_PARALLEL_PAGE_SIZE` | `10000` (floor `1000`) | Blocks per worker page. Larger pages amortize request overhead but cost memory per in-flight page. |
| `SQD_PARALLEL_RPS` | `5.0` (≤0 → `5.0`) | Target request rate to the portal. The gateway rate-limits around ~5 req/s; raising this risks throttling. |
| `SQD_TARGET_FETCH_SECONDS` | `6` | Target latency per fetch; the fetcher adapts page sizing toward this. `0` disables the target. |
| `SQD_COMMIT_INTERVAL` | `4096` | Blocks between checkpoint commits to ClickHouse. |
| `SQD_COMMIT_MAX_INTERVAL` | `3s` | Max wall time between commits (Go duration or bare seconds), so sparse ranges still checkpoint. |
| `SQD_RECOVERY_MIN_BLOCK` | unset (auto) | Manual override for the recovery floor (the recency-column value below which positions are loaded keys-only, not with full values). When unset, recovery computes this itself via `quantile(SQD_RECOVERY_QUANTILE)` against the table's recency column — no manual tuning needed; set this only to pin an exact value. |
| `SQD_RECOVERY_QUANTILE` | `0.95` | Quantile level used to auto-derive the recovery floor when `SQD_RECOVERY_MIN_BLOCK` is unset. `0.95` keeps the most recent ~5% of rows in the full value load; the rest get keys-only negative-filter coverage. |
| `SQD_RECOVERY_AUTO_FLOOR` | unset (on) | Set to `0`/`false` to disable the auto-computed floor entirely and restore the old unconditional full-table-scan recovery. |
| `SQD_STATS_INTERVAL` | `10s` | Cadence of the stdout stats/profile lines (duration or seconds; floor 50ms). |
| `SQD_PORTAL_ENDPOINT` | unset (per-chain default) | Override the SQD portal dataset URL. When unset, the chain ID selects a built-in default (e.g. `https://portal.sqd.dev/datasets/ethereum-mainnet`, `.../polygon-mainnet`). |
| `SQD_API_TOKEN` | unset | API token for the portal, written into a project's `.env` by `init`. |
| `SQD_METRICS_CH` | unset (off) | Enable the ClickHouse metrics writer (section 2). |
| `SQD_METRICS_CH_INTERVAL` | `5s` | Metrics writer flush cadence. |
| `SQD_METRICS_CH_TTL_DAYS` | `7` | Retention on `monitoring.indexer_metrics`. |

> Memory tuning recipe: if RSS is the problem, start with `SQD_COLDCACHE_MB`
> (cap it) and `SQD_PARALLEL_FETCHERS` (lower it), then watch `sys_bytes` and RSS
> on the **Indexer Runtime** / **ClickHouse Server** dashboards. If throughput
> (`avg blk/s`) is the problem, raise `SQD_PARALLEL_FETCHERS` / page size and
> check whether `consumer_wait` (fetch-bound) or `producer_backpressure`
> (insert-bound) dominates the `profile` line.

---

## See also

- [`MONITORING.md`](./MONITORING.md) — Grafana/ClickHouse provisioning details,
  the read-only ClickHouse user, and the list of provisioned files.
- `internal/monitoring/recorder.go` — the metrics writer and table schema.
- `internal/ingestion/ingestion.go` — the stdout stats/profile lines.
- `internal/envconfig/envconfig.go` — all `SQD_*` defaults and clamps.
