# ClickHouse Indexer Load Testing Framework

This load testing framework is located in the [loadtest/](file:///home/dev/CODING/polymarket_lowram/sqd-go-v2/loadtest) directory. It is designed to simulate a high-load production environment to inspect indexing performance, database throughput, memory consumption, and backpressure behavior under conditions such as:
- **20+ Million User Positions** stored in the ClickHouse database.
- **100k+ Transactions per Second (TPS)** event log stream.
- **5000 Blocks** queue buffering capacity in-memory.

The framework reuses the exact same codegen types and parser decoders as the real indexing application, ensuring realistic simulations.

## File Structure

- [main.go](file:///home/dev/CODING/polymarket_lowram/sqd-go-v2/loadtest/main.go): The CLI entrypoint parsing inputs and setting up profiling.
- [populator.go](file:///home/dev/CODING/polymarket_lowram/sqd-go-v2/loadtest/populator.go): Multi-threaded ClickHouse batch populator that fills `memory_user_positions` with up to 20 million user/token combinations in parallel.
- [generator.go](file:///home/dev/CODING/polymarket_lowram/sqd-go-v2/loadtest/generator.go): Encodes synthetic `ExchangeOrderFilled` logs using real event topic/data ABI specifications. Simulates configurable cache hits via a hot-to-cold user ratio.
- [pipeline.go](file:///home/dev/CODING/polymarket_lowram/sqd-go-v2/loadtest/pipeline.go): Bounded-queue streaming pipeline connecting the generator (producer) and custom state processor (consumer), measuring queue utilization and backpressure wait times.

---

## Getting Started

### 1. Build the Load Test Binary
From the root directory:
```bash
go build -o loadtest_bin ./loadtest
```

### 2. Populate the Database (20M User Positions)
Populate the database `polymarket_loadtest` with 20 million positions using 8 threads and a batch insert size of 50,000.
```bash
./loadtest_bin -port 9003 populate -count 20000000 -batch-size 50000
```
*Note: The script automatically targets port 9003 as configured in the `.env` file, and uses the separate database `polymarket_loadtest` by default.*

### 3. Run the Streaming & Backpressure Simulation
To run the high-throughput test with a 5% hot-user distribution (simulating cache misses 95% of the time, forcing database resolution):
```bash
./loadtest_bin -port 9003 run -blocks 10000 -txs 1000 -hot-pct 0.05 -queue-cap 500 -cpu-profile cpu.prof -mem-profile mem.prof
```

### 4. Compare Position Math Engines
Compare shopspring decimal position updates against pointer-free ClickHouse
`Decimal256(18)` math, and compare per-batch ClickHouse inserts with one
ch-go `OnInput` streaming insert:

```bash
./loadtest_bin -port 9003 -db polymarket_loadtest positions \
  -positions 100000 \
  -events 200000 \
  -engine both \
  -insert both \
  -chunk-size 2000
```

Flags:
- `-engine`: `proto`, `shopspring`, or `both`.
- `-insert`: `none`, `batch`, `stream`, or `both`.
- `-positions`: Number of cached positions to update.
- `-events`: Number of incoming order events.
- `-chunk-size`: Rows per ClickHouse insert block.

The command reports two tasks:
- Task 1: keyed lookup plus position math.
- Task 2: ClickHouse ingest.

Recent local run against ClickHouse native port `9003`:

| Engine | Insert | Task 1 update | Task 2 ingest | E2E | Allocated | Mallocs |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| proto Decimal256 | batch | 49.372 ms | 3.023 s | 3.073 s | 442 KB | 4,858 |
| proto Decimal256 | stream | 49.372 ms | 75.191 ms | 124.563 ms | 109 KB | 1,592 |
| shopspring decimal | batch | 194.277 ms | 3.022 s | 3.217 s | 190 MB | 5.34M |
| shopspring decimal | stream | 194.277 ms | 154.959 ms | 349.236 ms | 190 MB | 5.34M |

Streaming benefit:
- proto insert task: `3.023s -> 75ms`, about `40x` faster.
- shopspring insert task: `3.022s -> 155ms`, about `19.5x` faster.

Proto-vs-shopspring with streaming:
- E2E speedup: `2.8x`.
- Allocation reduction: about `1778x`.
- Malloc reduction: about `3353x`.

#### Run Parameters:
- `-db`: ClickHouse database name (default `polymarket_loadtest`). Running directly on the production database `polymarket` is blocked by default. Pass `-force` to override.
- `-force`: Allows running against the production database `polymarket` (default `false`).
- `-blocks`: Number of blocks to simulate (default `1000`).
- `-txs`: Transactions (events) per block (default `500`).
- `-hot-pct`: Simulates cache hit rates from `0.0` (all cache misses) to `1.0` (all cache hits) (default `0.05`).
- `-tps`: Target TPS limit. Use `0` for unthrottled maximum speed (default `0`).
- -queue-cap: Capacity of the memory queue (default `5000` blocks). Keeps memory usage bounded.
- -cpu-profile: File path to output CPU profiling (e.g. `cpu.prof`).
- -mem-profile: File path to output memory heap profiling (e.g. `mem.prof`).

---

### Run Parameters:
- `-db`: ClickHouse database name (default `polymarket_loadtest`). Running directly on the production database `polymarket` is blocked by default. Pass `-force` to override.
- `-force`: Allows running against the production database `polymarket` (default `false`).
- `-blocks`: Number of blocks to simulate (default `1000`).
- `-txs`: Transactions (events) per block (default `500`).
- `-hot-pct`: Simulates cache hit rates from `0.0` (all cache misses) to `1.0` (all cache hits) (default `0.05`).
- `-tps`: Target TPS limit. Use `0` for unthrottled maximum speed (default `0`).
- -queue-cap: Capacity of the memory queue (default `5000` blocks). Keeps memory usage bounded.
- -cpu-profile: File path to output CPU profiling (e.g. `cpu.prof`).
- -mem-profile: File path to output memory heap profiling (e.g. `mem.prof`).

---

## State Prefetch Benchmark

The state compare benchmark demonstrates the power of **prefetch with batch fetching** for stateful processing. The concept is simple:

### Prefetch Concept

Instead of fetching state on-demand (which causes N point queries for N events), we:
1. **Dry run**: Collect all unique state keys needed for a batch of events
2. **Batch fetch**: Resolve all keys in a single ClickHouse query
3. **Process**: Process events with all state already in cache

This maintains the simple custom_processor syntax:
```go
// User code stays simple - just call state.Position.Get()
up, ok := state.Position.Get(user, tokenID)
```

The framework handles the prefetch transparently in the background.

### Results

Recent run with 10,000 cold positions and 20,000 events:

| Metric | Current (Point Queries) | Improved (Prefetch) | Improvement |
|---|---|---|---|
| **Duration** | 45.3s | 1.2s | **37.7x faster** |
| **Alloc** | 129.8 MB | 14.5 MB | **8.9x reduction** |
| **Mallocs** | 1,291,244 | 43,368 | **29.8x reduction** |
| **Queries** | 10,000 | 20 | **500x fewer** |
| **Hash** | bc16086054abd85c | bc16086054abd85c | ✅ Match |

The improved approach processes 20,000 events in just 1.2 seconds with only 20 ClickHouse queries (vs 10,000 point queries).

### Running the State Prefetch Benchmark

```bash
./loadtest_bin -port 9003 -db polymarket_loadtest state \
  -positions 10000 \
  -events 20000 \
  -engine both \
  -prefetch-batch 2000 \
  -resolve-chunk 500 \
  -insert-chunk 2000 \
  -queue-cap 16 \
  -flush-interval 2s
```

Flags:
- `-positions`: Number of cold ClickHouse state positions to create
- `-events`: Number of incoming events to process
- `-engine`: `current`, `improved`, or `both`
- `-prefetch-batch`: Events per prefetch window (improved engine)
- `-resolve-chunk`: Keys per ClickHouse resolve query
- `-insert-chunk`: Rows per ClickHouse insert batch
- `-queue-cap`: Queued write batch capacity
- `-flush-interval`: Dirty state flush interval

---

## Analyzing Bottlenecks

### Log Stats Output Format
Every 2 seconds, the pipeline outputs performance indicators:
```
[STATS] Elapsed: 10s | Blk: 450 | TPS: 90000.0 (avg 89500.0) | Queue: 1240/5000 (24.8%) | BP Wait: 15ms | Consumer Proc: 820ms | Alloc: 120 MB | Sys: 340 MB | GC: 15
```
- `Queue`: Number of blocks currently in memory waiting to be processed (capped at `-queue-cap` to bound memory).
- `BP Wait`: Time the producer spent blocking on queue insertion (direct indicator of backpressure).
- `Consumer Proc`: CPU time consumed processing logs.
- `Alloc / Sys`: Go garbage collector and heap stats.

### Profiling
After running the test, inspect CPU bottlenecks using standard Go pprof:
```bash
go tool pprof -http=:8080 cpu.prof
```
This is useful to analyze GC pauses, `sync.Map` mutex contention, or ClickHouse client serialization overhead.
