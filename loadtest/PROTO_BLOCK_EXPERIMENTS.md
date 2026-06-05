# Proto Block Optimization Experiments

## Goal

Test whether Polymarket hot-state snapshots and loadtest insert paths can use ClickHouse-native `ch-go/proto` column types as an ECS-style struct-of-arrays layout:

- `proto.ColDecimal128` for `Decimal(38, 18)` storage.
- `proto.ColUInt256` for EVM uint256 columns.
- `proto.ColFixedStr` for address/hash fixed bytes.
- `proto.Input` as the block-shaped column list used directly by ClickHouse inserts.

`proto.Block` is mostly metadata (`Rows`, `Columns`, `BlockInfo`). The row data lives in the column objects. So the in-memory "ClickHouse block" is the set of proto columns plus the `proto.Input` column descriptor list.

The production loadtest shape is:

```text
5000 blocks * 2000 tx/block = 10,000,000 tx rows
```

The key design implication is to process one block, or a small group of blocks, as proto columns. Do not retain all 10M row objects as pointer-rich Go structs.

## Access Pattern

The columns are slices or contiguous byte buffers:

```go
type ProtoPositionBlock struct {
  User        proto.ColFixedStr   // contiguous []byte, 20 bytes per row
  TokenID     proto.ColFixedStr   // contiguous []byte, 32 bytes per row
  Amount      proto.ColDecimal128 // []proto.Decimal128, two uint64 per row
  AvgPrice    proto.ColDecimal128
  RealizedPnL proto.ColDecimal128
  TotalBought proto.ColDecimal128
}

amount := block.Amount[i]
block.Amount[i] = updatedAmount
userBytes := block.User.Row(i) // slice into User.Buf, no row allocation
```

This is the same idea as the ECS `MapSlices` path: systems operate over whole typed columns and index `i`, instead of touching pointer-rich structs one entity at a time.

## Experiment 1: Snapshot GC Scan Cost

Benchmark command:

```bash
LOADTEST_BENCH_ROWS=50000 LOADTEST_BENCH_SNAPSHOTS=32 \
go test ./loadtest -run '^$' -bench 'BenchmarkExp1' -benchmem -benchtime=10x -count=3
```

Shape:

- Decimal snapshots: `[]decimalPositionBench`, four `decimal.Decimal` fields per row.
- Proto snapshots: block-shaped columns, four `proto.ColDecimal128` columns per row.
- Retained snapshots: 32.
- Rows per snapshot: 50k.
- Decimal pointer graph: 50k * 32 * 4 = 6.4M decimal pointers for GC to scan.

Results:

| Benchmark | Mean |
| --- | ---: |
| GC scan, decimal snapshots | 28.78 ms |
| GC scan, proto block snapshots | 0.257 ms |

Improvement: about **112x faster GC scan**, or about **99.1% less GC scan time**.

Snapshot copying itself is roughly a memcpy problem. Warmed copy runs were in the same sub-millisecond range for both layouts. The real win is not copy time; it is eliminating millions of row-level pointers from the snapshot ring.

## Experiment 2: Math Directly On Proto Decimal Columns

Benchmark command:

```bash
LOADTEST_BENCH_ROWS=50000 \
go test ./loadtest -run '^$' -bench 'BenchmarkExp2' -benchmem -benchtime=10x -count=3
```

Shape:

- Decimal path: current-style `decimal.Decimal` arithmetic for average-price update.
- Proto path: direct fixed-point arithmetic on `proto.Decimal128` storage, using the low 64-bit lane for this benchmark's bounded micro-unit values.

Results:

| Benchmark | Mean | Allocations |
| --- | ---: | ---: |
| `decimal.Decimal` position update | 45.75 ms | ~35.2 MB, ~950k allocs |
| proto-column position update | 0.596 ms | 0 B, 0 allocs |

Improvement: about **76.8x faster**, with allocation pressure removed.

Caveat: this benchmark uses a low-64-bit fast path to prove the storage and access model. Production `Decimal(38, 18)` codegen needs overflow-safe generated Int128/Int256 helpers for `avg * amount` style intermediates. That still keeps the no-pointer column layout, but the math helper must be wider than this micro-benchmark fast path.

### Direct `proto.ColUInt256 / 1e8`

Unit test:

```bash
go test ./loadtest -run TestProtoColUInt256Div1e8 -v
```

Result: passed. The test divides values read from `proto.ColUInt256` directly and compares quotient/remainder against `math/big`, including values above 64 bits and `2^256 - 1`.

Benchmark:

```bash
LOADTEST_BENCH_ROWS=50000 \
go test ./loadtest -run '^$' -bench 'BenchmarkExp2_UInt256Div1e8_ProtoCol' -benchmem -benchtime=20x -count=3
```

Results:

| Rows | Mean | Allocations |
| ---: | ---: | ---: |
| 50k | 3.44 ms | 0 B, 0 allocs |

That is about 68.8 ns/row. At the real loadtest shape:

| Workload | Estimated time for `UInt256 / 1e8` pass |
| --- | ---: |
| 1 block, 2000 tx | ~138 us |
| 5000 blocks, 10M tx | ~688 ms |

The math is done by iterating the column slice:

```go
for i, value := range col {
  quotient[i], remainder[i] = protoUInt256DivUint64(value, 100_000_000)
}
```

Internally, `proto.UInt256` is four little-endian `uint64` limbs. Division uses base-2^64 long division from the high limb to the low limb with `bits.Div64`.

## Experiment 3: Per-Batch Insert vs `OnInput` Streaming

Benchmark command:

```bash
LOADTEST_CLICKHOUSE_BENCH=1 LOADTEST_INSERT_ROWS=1000000 LOADTEST_INSERT_CHUNK=10000 \
CLICKHOUSE_NATIVE_PORT=9003 CLICKHOUSE_PASSWORD=sqd-clickhouse \
go test ./loadtest -run '^$' -bench 'BenchmarkExp3' -benchmem -benchtime=1x -count=1
```

Shape:

- Same order-event columns in both paths.
- Table engine: `Memory`, to isolate client encoding and query round-trip overhead.
- Per-batch path: one `conn.Do` per 10k rows.
- Streaming path: one `conn.Do` with repeated `OnInput` blocks.

Results:

| Rows | Per-batch | `OnInput` streaming | Improvement |
| ---: | ---: | ---: | ---: |
| 200k | 1.215 s | 27.7 ms | 43.8x |
| 1M | 6.060 s | 134 ms | 45.2x |

This is a direct loadtest/backend improvement: for large inserts, `OnInput` turns many query round trips into one streaming insert while still sending ClickHouse-native blocks.

## Recommendation

Use proto-column blocks for hot-state snapshots and write codegen around the column layout:

1. Keep active state as columns plus a key-to-row index.
2. Snapshot by copying column buffers, not slices of structs containing `decimal.Decimal`.
3. Generate column-oriented update loops for hot paths where possible.
4. Use `proto.Input`/`OnInput` for large inserts and population.

This matches the ECS lesson: a monomorphic, columnar `MapSlices` layout is the win. For this codebase, the natural column format is already the ClickHouse `proto` format.
