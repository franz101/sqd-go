# Polymarket ECS and Proto-Math Simulation Learnings

This document summarizes the design, implementation, and benchmark results of a high-performance, low-RAM Entity Component System (ECS) architecture using pointer-free `protomath.Decimal256` structures.

The goal is to eliminate GC pause overhead and memory scaling bottlenecks when processing massive prediction market workloads (e.g., a **50 GB** log stream of events with millions of user positions).

---

## 1. Architectural Design

To support high-frequency event ingestion and rollback/recovery (such as blockchain reorgs), we modeled a zero-allocation state engine around the following components:

### A. ConditionInput (Incoming Events)
Incoming blocks are represented as contiguous `ch-go/proto` column bundles (`entityID`, `delta`, `price`, `block`, `logIndex`). Rather than creating heap-allocated structures per event, we process them sequentially or in slices directly from raw ClickHouse/Redpanda buffer columns.

### B. PositionStore (ECS/SoA State Engine)
Instead of a pointer-heavy mapping like `map[uint64]*Position` where `Position` wraps `decimal.Decimal` (which allocates `*big.Int` pointers on the heap), we store user positions as contiguous flat arrays in a Structure-of-Arrays (SoA) layout:
- `rowByEntity map[uint64]int`: An entity-index lookup map mapping user ID to a contiguous row index.
- `amount []protomath.Decimal256`
- `totalBought []protomath.Decimal256`
- `avgPrice []protomath.Decimal256`

All math uses the pointer-free `Decimal256` implementation from `drafts/protomath`, which operates directly on 32-byte structures.

### C. MutationRing (Rollback Cache)
To support block reorgs and rollback state without allocating new memory, we built a circular ring buffer containing flat, pointer-free `Mutation` structures:
```go
type Mutation struct {
    EntityID       uint64
    RowIdx         int
    OldAmount      protomath.Decimal256
    OldTotalBought protomath.Decimal256
    OldAvgPrice    protomath.Decimal256
    NewAmount      protomath.Decimal256
    NewTotalBought protomath.Decimal256
    NewAvgPrice    protomath.Decimal256
}
```
Pre-allocating a `MutationRing` with a fixed capacity ensures that rollback recording is fully allocation-free (0 bytes of heap memory requested during execution).

### D. InsertStream
Exposing the in-memory ECS state for ClickHouse insertion is done by pointing `proto.Input` column pointers directly to the `PositionStore` backing columns. This enables zero-copy streaming of updated positions directly to ClickHouse via `OnInput`.

---

## 2. Benchmark Results

We compared the pointer-free ECS/SoA Proto-Math model against the standard pointer-heavy Shopspring decimal model. 

### Simulation Parameters
- **Active User Positions**: 100,000 cached positions
- **Incoming Stream**: 1,000,000 events (scaled to simulate a **50 GB** stream of 250,000,000 events)

### E2E Simulation Test Performance (1 Million Events)

| Metric | Shopspring (Traditional) | ProtoMath ECS (Optimized) | Improvement |
| :--- | :---: | :---: | :---: |
| **Duration** | 1.356s | 0.461s | **2.9x faster** |
| **Throughput** | 737,294 events/sec | 2,169,660 events/sec | **2.9x throughput** |
| **Memory Allocated** | 808.66 MB | 48.11 MB | **16.8x less allocation** |

### Micro-Benchmarks (`go test -bench`)

- **`BenchmarkSimulation_ProtoMathECS`**:
  - Speed: **78.6 ms** per 200,000 events (~393 ns/event)
  - Heap Allocations: **128 B/op** (exactly **1 alloc/op**)
- **`BenchmarkSimulation_ShopspringDecimal`**:
  - Speed: **391.6 ms** per 200,000 events (~1,958 ns/event)
  - Heap Allocations: **226,884,989 B/op** (~1,134 B/event)
  - Allocs: **6,189,131 allocs/op** (~31 allocs/event)

---

## 3. Extrapolated 50 GB Workload Metrics (250 Million Events)

Applying the throughput and allocation profiles of both systems to a full **50 GB / 250M event** log ingestion yields the following comparison:

- **Duration (Shopspring)**: **339.1 seconds** (~5.65 minutes)
- **Duration (ProtoMath ECS)**: **115.2 seconds** (~1.92 minutes)
- **Heap Allocations (Shopspring)**: **197.43 GB** of total allocated heap memory
- **Heap Allocations (ProtoMath ECS)**: **11.75 GB** of total allocated memory
- **Garbage Saved**: **185.68 GB** of pointer-churn and GC scan overhead avoided!

---

## 4. Block-Level Rollback Performance (5,000 Blocks)

To support blockchain reorgs, we track the circular `MutationRing` head index at the start of each block:
`blockHeads[bIdx] = ring.head`

To roll back exactly $N$ blocks, we lookup the target block's head index and traverse backward, reverting mutations:
`ring.RollbackTo(blockHeads[targetBlock], store)`

### Ingestion & Rollback Metrics
- **Block Simulation**: 5,000 blocks processed (20,000 transactions).
- **Rollback Window**: Reverting the last 500 blocks (2,000 transactions) to block 4500.
- **Rollback Duration**: **439 microseconds** (sub-millisecond recovery).
- **Memory Cost**: **0 bytes** of heap allocations (completely allocation-free).

### RAM Overhead for Circular History (Preallocated)
Because `Mutation` is a flat structure containing only primitive value types (8-byte ID, 8-byte index, and 32-byte Decimal256 old states), the memory cost per mutation is only **112 bytes**.
- **Small Scale (100 tx/block, 5,000 blocks history)**: Requires 500,000 preallocated mutations -> **56 MB** of RAM.
- **Large Scale (2,000 tx/block, 5,000 blocks history)**: Requires 10,000,000 preallocated mutations (rounds to 16.7M capacity) -> **1.87 GB** of RAM.

This confirms that **5,000 blocks of rollback history is fully supported** and extremely cost-effective.

---

## 5. Key Takeaways

1. **Memory Efficiency**: Keeping in-memory states as contiguous arrays of `protomath.Decimal256` values (which are value types wrapping `[32]byte`) avoids generating million-pointer structures that force the Go garbage collector into long scan phases.
2. **Zero-Copy Streaming**: Because `PositionStore` stores variables in flat arrays matching ClickHouse columns, generating `proto.Input` for streaming requires no data structure conversions or string serializations.
3. **Rollback Stability**: The `MutationRing` circular cache records rollback snapshots at zero CPU allocation cost, ensuring maximum safety against reorgs without degrading ingestion throughput.

