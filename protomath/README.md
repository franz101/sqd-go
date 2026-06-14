# Proto UInt256 Math Draft

This draft keeps ClickHouse `proto.UInt256` and `proto.Decimal256` data in a
pointer-free shape and adds math helpers around them.

`uint256` has a 78-digit maximum value, but `10^78` itself does not fit. The
largest valid value is `2^256 - 1`, which is about `1.1579e77`. The tests cover
`1e77` and `2^256 - 1`, and assert that `10^78` is rejected.

ClickHouse `Decimal256` is not `Decimal(78)`. In ClickHouse/ch-go the maximum
Decimal256 precision is 76 digits. `Decimal256(scale)` can still return
fractional fixed-scale results, for example `1 / 3` at scale 18 produces
`0.333333333333333333`.

## API Shape

```go
var raw proto.ColUInt256
col := protomath.WrapProtoCol(&raw)

quotient := make([]protomath.UInt256, col.Rows())
remainder := make([]uint64, col.Rows())
col.Div1e18Into(quotient, remainder)

// Keep using the original ch-go column for inserts/selects.
input := proto.Input{{Name: "amount", Data: col.Proto()}}
```

The `UInt256` hot-path options are:

- `UInt256.DivUint64`: direct four-limb division over the ClickHouse proto value.
- `UInt256.DivBy`: cached `holiman/uint256` divisor path; this is faster for
  repeated `/ 1e18`.
- `ColUInt256.Div1e18Into`: column loop using the cached `/ 1e18` divisor.
- `UInt256.Decimal(scale)`: shopspring decimal conversion for cold paths only.

The unsigned constructors cover `uint8`, `uint16`, `uint32`, `uint64`, `uint`,
`proto.UInt128`, and `proto.UInt256`.

`Decimal256` is a signed scaled integer wrapper over `proto.Decimal256`:

```go
scale := protomath.Decimal256Scale18
amount, _ := protomath.ParseDecimal256("-1,234.50", scale)
price, _ := protomath.ParseDecimal256("0.25", scale)

notional, ok := amount.Mul(price, scale)
_ = ok
_ = notional.Proto()
```

Supported methods include `Add`, `Sub`, `Mul`, `Div`, `Mod`, `Neg`, `Cmp`, and
column loops for add/mul. Parsing supports comma-grouped strings like
`1,234.56` and decimal-comma strings like `1.234,56`.

Constructors cover:

- `ParseDecimal256(string, scale)`
- `FromDecimal256LittleEndianBytes` and `FromDecimal256BigEndianBytes`
- `FromDecimal256BigInt` for whole numbers at a scale
- `FromDecimal256ScaledBigInt` for raw scaled coefficients

## Benchmark Results

Hardware: Intel i7-8700, linux/amd64.

Focused math benchmark:

```text
go test ./drafts/protomath -run '^$' -bench 'BenchmarkDiv1e18' -benchmem -benchtime=10x -count=3
```

Mean result:

| Path | Rows/op | Time | Per row | Allocations |
| --- | ---: | ---: | ---: | ---: |
| Proto subtype, cached Holiman divisor | 100k | 6.89 ms | 68.9 ns | 0 |
| Proto direct limb division | 100k | 6.96 ms | 69.6 ns | 0 |
| Holiman direct | 100k | 5.74 ms | 57.4 ns | 0 |
| `math/big` reused output | 100k | 8.87 ms | 88.7 ns | 20k allocs |
| shopspring decimal | 10k | 7.90 ms | 790 ns | 149.7k allocs |

Real workload shape:

```text
5000 blocks * 2000 tx/block = 10,000,000 rows
go test ./drafts/protomath -run '^$' -bench 'BenchmarkStreamBlocks_(ProtoSubtype|Holiman)$' -benchmem -benchtime=1x -count=3
timeout 30s env PROTO_MATH_DECIMAL_BLOCKS=5000 go test ./drafts/protomath -run '^$' -bench 'BenchmarkStreamBlocks_ShopDecimal$' -benchmem -benchtime=1x -count=1
```

Mean/direct result:

| Path | Rows/op | Time | Allocated/op | Allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Proto subtype, cached Holiman divisor | 10M | 610.7 ms | 0 B | 0 |
| Holiman direct | 10M | 521.0 ms | 0 B | 0 |
| shopspring decimal | 10M | 6.53 s | 4.56 GB | 149.8M |

For the 10M-row stream, the proto subtype is about 11.3x faster than
shopspring decimal and avoids roughly 4.56 GB of per-pass allocation churn.

Decimal256 operator benchmark:

```text
go test ./drafts/protomath -run '^$' -bench 'Benchmark(Decimal256|ShopDecimal)_(Add|Mul|Div)$' -benchmem -benchtime=10x -count=3
```

Mean result:

| Path | Rows/op | Time | Per row | Allocations |
| --- | ---: | ---: | ---: | ---: |
| Decimal256 Add | 100k | 1.53 ms | 15.3 ns | 0 |
| shopspring Add | 10k | 0.93 ms | 92.9 ns | 20k allocs |
| Decimal256 Mul | 100k | 9.16 ms | 91.6 ns | 0 |
| shopspring Mul | 10k | 0.97 ms | 97.0 ns | 20k allocs |
| Decimal256 Div | 100k | 8.99 ms | 89.9 ns | 0 |
| shopspring Div | 10k | 6.33 ms | 633 ns | 130k allocs |

The Decimal256 multiplication fixture is CPU-similar to shopspring, but it still
removes the allocation churn. Add and division are materially faster and
allocation-free.

Position pipeline benchmark:

```text
100k cached positions + 200k incoming order events
Task 1: lookup + update position math
Task 2: build ClickHouse insert columns
go test ./drafts/protomath -run '^$' -bench 'BenchmarkPositions(Task1|Task2|E2E)' -benchmem -benchtime=1x -count=3
```

Mean result:

| Path | Work | Time | Allocated/op | Allocs/op |
| --- | --- | ---: | ---: | ---: |
| Proto math MapSlices | Task 1 update | 52.7 ms | 0 B | 0 |
| Proto math MapId-style | Task 1 update | 52.9 ms | 0 B | 0 |
| shopspring decimal | Task 1 update | 185.2 ms | 133.3 MB | 3.54M |
| Proto math | Task 2 insert columns | 1.57 ms | 128 B | 1 |
| shopspring decimal | Task 2 insert columns | 78.3 ms | 63.4 MB | 1.90M |
| Proto math MapSlices | E2E update + insert columns | 53.5 ms | 128 B | 1 |
| shopspring decimal | E2E update + insert columns | 270.5 ms | 198.9 MB | 5.34M |

This is the intended ECS-style shape: positions are already in ClickHouse proto
columns/slices, order events are applied by keyed lookup, and the updated
columns are ready for ch-go insertion without converting from heap decimals.

Actual ClickHouse insert benchmark is opt-in:

```text
PROTO_MATH_CLICKHOUSE_BENCH=1 go test ./drafts/protomath -run '^$' -bench 'BenchmarkPositionsE2E_(ProtoMath|ShopDecimal)_ClickHouseInsert$' -benchmem -benchtime=1x -count=1
```

Result on the same machine, using a Memory table:

| Path | Work | Time | Allocated/op | Allocs/op |
| --- | --- | ---: | ---: | ---: |
| Proto math | E2E update + raw Decimal256(18) insert | 133.2 ms | 12.2 KB | 132 |
| shopspring decimal | E2E update + conversion + insert | 326.6 ms | 198.9 MB | 5.34M |

Native insert note: ch-go's generated `proto.ColDecimal256` reports type
`Decimal256` without a scale. For actual native inserts into
`Decimal256(18)`, the benchmark sends raw 32-byte little-endian Decimal256 data
through a local `ColRaw` wrapper whose type is `Decimal256(18)`.

## chDB Parity

The chDB parity tests are opt-in because they require the native chDB runtime:

```text
go test -tags chdb ./drafts/protomath
```

These tests compare the wrapper against ClickHouse semantics through
`github.com/chdb-io/chdb-go/chdb`. They cover UInt256 operators, Decimal256
operators, fractional Decimal256 division, and Decimal256 values built from
string, bytes, and scaled `big.Int` constructors.

Important semantic detail: ClickHouse `/` on `UInt256` returns a floating result.
`UInt256.Div` is integer division, so the parity test compares it to ClickHouse
`intDiv`. `%`, bitwise operators, and shifts compare directly.

## Memory Notes

For one `UInt256` column, a 2000-row block is about 64 KiB. A quotient column is
another 64 KiB and the `uint64` remainders are 16 KiB. That is the intended
shape: process one block, or a small batch of blocks, and reuse the columns.

Pointer-free snapshots reduce GC scan cost, but they are not byte-free. A full
ring of large historical snapshots still consumes raw memory, so this should be
paired with block streaming, compact rings, or external ClickHouse storage for
long history.

Arena allocation is not used here because the hot paths do not allocate. Arena
would only move shopspring/big.Int pointer churn elsewhere; it would not remove
GC scan work from retained snapshots.
