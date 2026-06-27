# Cold-tier negative filter: split-block Bloom (measured improvement)

The negative Bloom filter (`coldcache/filter.go`) sits in front of Pebble/CH: a
`mayContain==false` skips the lookup entirely. Its false-positive rate therefore
directly sets how many lookups are wasted, and `add`/`mayContain` run on every
cold Put and every potential hot-miss. This is a measured, drop-in improvement to
that filter. Reproduce: `go test ./coldcache -run 'TestFilterFPRComparison|TestSplitNoFalseNegatives' -v`
and `go test ./coldcache -bench BenchmarkFilterEndToEnd`.

## The change

Replaced the in-block bit pattern with a **split-block (register-blocked) Bloom**
— the Impala/Parquet design adapted to the existing 512-bit cache line. Each key
sets exactly **one bit in each of the 8 64-bit words** (`bit_j = top 6 bits of
h*salt[j]`), instead of 8 bits along an **arithmetic progression** mod 512
(`bit_i = (h>>9 + i*g) mod 512`). `newNegFilter` now returns `SplitBloom`
(`filter_split.go`); the legacy `BloomFilter`/`AtomicBloom` stay as the baseline.
In-memory only (rebuilt per run) — no migration. Reuses the existing `negHash`
(fastest hash measured: 5.2 ns/52 B vs xxhash 6.2, a wyhash-style candidate 8.8).

## Why the AP pattern was worse

Both set 8 distinct bits per key in one block, so I expected parity — but the AP
bits are **clustered and correlated** (a stride-`g` progression), so an absent
key's 8-bit pattern is far more likely to be fully covered by the union of
inserted patterns. One-bit-per-word is **maximally spread** across 8 disjoint
64-bit regions: a false positive needs one specific bit set in *every* word.

## Measured (`filter_improve_test.go`, 52-byte keys, equal memory)

| keys/block | legacy double-hash FPR | split-block FPR | improvement |
|---|---|---|---|
| 6  | 0.011% | ~0% | (no FPs observed) |
| 10 | 0.021% | 0.0003% | **~75x** |
| 16 | 0.036% | 0.0020% | **~18x** |

| op (atomic, the default) | legacy | split-block |
|---|---|---|
| add | 24.4 ns | **17.1 ns (1.42x)** |
| mayContain hit | 15.2 ns | 15.6 ns (≈) |
| mayContain miss | 12.4 ns | 15.6 ns (+3 ns) |

Each avoided false positive is a skipped **Pebble (~8 µs)** or **ClickHouse
(~20 ms)** lookup, so the 18–75× FPR drop dominates. `add` is faster because the
8 ORs hit 8 *distinct* sequential words (the AP pattern can map two bits to the
same word → serialized atomics). The +3 ns on a filter *miss* is the branchless
all-8-words check (no early-exit) — noise against the µs–ms lookups the lower FPR
avoids.

## Correctness

Bloom invariant preserved: **no false negatives** — every bit set on `add` is
checked on `mayContain`, so a written key always reports present and the
authoritative read gate (`coldAuthoritative && !ColdMightContain`) never drops a
real position. Pinned by `TestNegFilterNoFalseNegatives` (now exercising the
production `SplitBloom` via `newNegFilter`) and `TestSplitNoFalseNegatives`; the
full `coldcache` suite passes. Shipped as the default (strict improvement, no
flag); `SQD_COLDCACHE_FILTER_ATOMIC` still selects atomic vs single-writer.
