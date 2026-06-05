# COLD.md — Fixed-Memory Hot/Cold Layout, Bounded ≤ 14 GB

> Design evaluation only — **no code changes**. This documents *how* the hot/cold
> caches could be made to fit a hard memory ceiling, given that every hot struct is
> fixed-size and the rings are already pre-allocated.
>
> Convention: **14 GB = 14 GiB = 14 × 2³⁰ = 15,032,385,536 bytes.** MiB/GiB are
> binary (2²⁰ / 2³⁰). Sizes below are 64-bit Go layout (`go vet`/`unsafe.Sizeof`
> rules: each field aligned to its own alignment; struct rounded up to its max
> field alignment).

---

## 0. TL;DR

1. **It works because the layout is closed-form.** Every hot entity is a
   fixed-size struct, stored in a pre-allocated CLOCK ring. So total hot RSS is
   exactly `Σ_i capacity_i × entrySize_i + index arrays` — a number you can solve
   *before* the process starts. There is nothing to "discover at runtime."
2. **Fixed memory (A)** = pick per-entity capacities so the sum ≤ a hot-tier
   budget (~11 GiB), reserve the rest (~3 GiB) for the non-ring growables, and let
   **CLOCK eviction be the ceiling** — once a ring is full it *evicts*, never
   grows. Add `GOMEMLIMIT` as a runtime backstop.
3. **Preallocation (B)** is already 90% done: `NewHotState` does
   `make([]Entry, capacity)` + `make([]int32, …)` for every cache up front. The
   remaining work is (i) **touch** those pages at init so RSS is deterministic
   from t=0, and (ii) pre-size the *off-ring* growables (dirty maps, resolver miss
   slices, batch column builders, replay/proto rings) so nothing silently
   reallocates past the budget.
4. **The cold tier today is ClickHouse** — a hot miss queues a key in a per-entity
   `Resolver` and batch-loads it from CH (`examples/polymarket/generated/hotstate.go`,
   `Resolve()`). That already gives you an unbounded, disk-backed cold store for
   ~0 RAM. An optional **in-process cold arena** (demote-on-evict) is evaluated in
   §6 if CH round-trips become the bottleneck.
5. **The only thing that breaks the closed form** is the three variable-length
   fields (`MemoryCondition.Payouts`, `MemoryMarket.QuestionIDs`,
   `MemoryNegRiskEvent.QuestionIDs`) and the one pointer
   (`ConditionPreparations` value embeds `time.Time` → `*Location`). Inline them as
   bounded fixed arrays and the budget becomes exact and the rings fully
   pointer-free. See §4.4.

---

## 1. Why this is possible: every hot struct is fixed-size

There are **6 hot caches** (matches the GC-layout note), all generated in
[`examples/polymarket/generated/hotstate.go`](examples/polymarket/generated/hotstate.go),
default capacity `DefaultClockCacheCapacity = 100000` (line 19). Each cache is:

```go
type XClockCache struct {
    buckets  []int32          // hash chain heads, len = pow2 ≥ 2·capacity, init -1
    next     []int32          // chain links, len = capacity, init -1
    ring     []XClockEntry    // the backing store, len = capacity   ← preallocated
    mask     uint32
    capacity uint64
    hand     uint64           // CLOCK hand
    size     uint64
}
type XClockEntry struct {
    key        XClockKey
    value      MemoryX
    referenced uint32         // CLOCK second-chance bit (atomic)
    inUse      uint32         // slot state 0/1/2 (atomic)
}
```

### 1.1 Field-level sizes of every `Memory*` value

Component types: `common.Hash`=`[32]byte` (align 1), `common.Address`=`[20]byte`
(align 1), `uint64`/`int64`=8, `uint32`=4, `uint8`/`bool`=1,
`protomath.Decimal256`=`proto.Decimal256`=`Int256`= 4×uint64 = **32 bytes** (align
8), `uint256.Int`=`[4]uint64`= **32 bytes** (align 8), slice header `[]T`= **24
bytes** (align 8, *plus* off-heap backing), `time.Time`= **24 bytes** (align 8,
contains a `*Location`).

| Value struct | Fields (type → bytes) | Inline size | Var-length escape |
|---|---|---|---|
| `MemoryUserPosition` (l.488) | User `Addr`20, TokenID `Hash`32, Amount/AvgPrice/RealizedPnL/TotalBought `Decimal256`32×4, UpdatedAtBlock/UpdatedAt/BlockNumber/TxIndex/LogIndex `u64/i64`8×5, Tombstone `bool`1 | **232** | none ✅ |
| `MemoryCondition` (l.21) | ID `Hash`32, Oracle `Addr`20, QuestionID `Hash`32, OutcomeSlotCount `u8`1, Resolved `bool`1, **Payouts `[]uint256`24**, +5×8, Tombstone 1 | **160** | `Payouts` → N×32 off-ring |
| `MemoryMarket` (l.951) | ID `Hash`32, QuestionCount `u32`4, **QuestionIDs `[]Hash`24**, +5×8, Tombstone 1 | **112** | `QuestionIDs` → N×32 off-ring |
| `MemoryNegRiskEvent` (l.1382) | same shape as Market | **112** | `QuestionIDs` → N×32 off-ring |
| `MemoryFixedProductMarketMaker` (l.1813) | ID `Addr`20, ConditionID `Hash`32, CollateralToken `Addr`20, +5×8, Tombstone 1 | **120** | none ✅ |
| `ConditionalTokensConditionPreparation` (events.go l.39) | EventMeta{u64, **time.Time 24 (*Location!)**, Hash, Addr, Hash, u64, u64}=136, ConditionID `Hash`32, Oracle `Addr`20, QuestionID `Hash`32, OutcomeSlotCount `uint256`32, Tombstone 1 | **264** | embeds a pointer (`*Location`) |

### 1.2 Entry sizes (value + key + 2×u32 flags, with alignment padding)

| Cache | Key | Entry struct size | Notes |
|---|---|---|---|
| **UserPositions** (largest) | {Addr20, Hash32}=52 | **296** | key 0..52, value pad→56..288, flags 288..296 |
| ConditionPreparations | {Hash32} | **304** | not pointer-free (time.Time) |
| Conditions | {Hash32} | **200** | |
| Markets | {Hash32} | **152** | |
| NegRiskEvents | {Hash32} | **152** | |
| FixedProductMarketMakers | {Addr20} | **152** | |

The ring is `make([]XClockEntry, capacity)` (e.g. l.538 for UserPositions) — so
**the entire `capacity × entrySize` block is allocated at construction**, not as
entries arrive. That is the whole basis for both (A) and (B) below.

---

## 2. The memory model today

```
        Set/Get (single writer: the consumer goroutine)
                         │
                         ▼
   ┌──────────── HOT TIER (in RAM, bounded) ────────────┐
   │  6 × CLOCK ring  +  flat int32 chained-arena index  │   ← A2+A3 layout
   │  capacity-bounded, pointer-free (mostly), O(1) GC    │
   └───────────────────────┬─────────────────────────────┘
                            │  miss → Resolver.Queue(key)
                            ▼
   ┌──────────── COLD TIER (on disk) ────────────────────┐
   │  ClickHouse  memory_user_positions / memory_*        │
   │  full durable state; batch SELECT … LIMIT 1 BY key   │
   └──────────────────────────────────────────────────────┘
```

- **There is no in-RAM cold cache today.** `Resolver.Resolve()` (e.g. l.856)
  batches all missed keys into one `… WHERE (user,token_id) IN (…) ORDER BY block
  DESC LIMIT 1 BY key` and re-`Set`s them into the hot ring; misses that find
  nothing are tombstoned so they don't re-query. **ClickHouse *is* the cold tier.**
- Eviction is **CLOCK / second-chance** (`SetByKey`, l.596): the hand sweeps,
  clears `referenced`, and reuses the first unreferenced slot. **A full ring never
  grows — it overwrites.** That is the structural property that makes a ceiling
  possible at all.

---

## 3. The budget equation

For one cache at capacity `C`:

```
bytes(C) = C·entrySize        (ring)
         + C·4                 (next[])
         + 4·pow2(≥ 2C)        (buckets[], smallest power of two ≥ 2C)
```

`pow2(≥2C)` is between `2C` and `4C`, so buckets cost ≈ `8C … 16C` bytes; the ring
dominates. **Total hot RSS = Σ over the 6 caches.** Everything is known at
codegen/startup → solvable in closed form.

### 3.1 Baseline: default 100k caps is tiny (~128 MiB)

| Cache | C | ring | next+buckets | all-in |
|---|--:|--:|--:|--:|
| UserPositions | 100k | 28.2 MiB | 1.4 MiB | **29.6 MiB** |
| ConditionPreparations | 100k | 29.0 | 1.4 | **30.4** |
| Conditions | 100k | 19.1 | 1.4 | **20.5** |
| Markets | 100k | 14.5 | 1.4 | **15.9** |
| NegRiskEvents | 100k | 14.5 | 1.4 | **15.9** |
| FPMMs | 100k | 14.5 | 1.4 | **15.9** |
| | | | **total** | **≈ 128 MiB** |

(`pow2(≥200k)=262144` → buckets = 1.0 MiB; next = 0.38 MiB.) So 14 GB is **~110×
headroom over the default** — the question is purely *how to spend it safely*.

### 3.2 Everything else that must live inside 14 GB

The rings are not the only consumers. A *hard* 14 GB ceiling must budget all of:

| Region | Where | Bounded by | Status |
|---|---|---|---|
| 6 hot rings + indexes | hotstate.go | capacities (this doc) | preallocated ✅ |
| `dirty*` maps (×6) | HotState (l.2569) | distinct keys touched between commits | **grows, not capped** ⚠ |
| Resolver `misses []Key` (×6) | l.834 etc. | events per Resolve, reset each call | transient |
| off-ring var-length backings | Payouts / QuestionIDs | N×32 per entry | **heap, not in ring** ⚠ |
| ReplayBuffer (8192 blocks) | internal/ingestion/replay.go | 8192 × per-block decoded size | separate ring |
| ProtoRingBuffer (columnar) | generated/ringbuffer.go | pow2 slots × columns | separate ring |
| batch column builders | Memory*Batch | commit batch row count | transient |
| Go runtime + GC headroom | — | allocation rate × GC target | `GOGC`/`GOMEMLIMIT` |

**Partition rule:** budget the hot rings at ≈ **0.7–0.8 × 14 GiB** and reserve the
remainder for the table above. The ⚠ rows are the ones that can silently blow the
budget — §4 and §5 pin them down.

---

## 4. (A) Fixed memory — bounding total ≤ 14 GB

### 4.1 Global partition

```
B            = 14 GiB                       (hard ceiling)
B_hot        ≈ 11 GiB   (6 rings + indexes) → solve capacities to hit this
B_reserve    ≈ 3 GiB    (dirty maps + replay + proto + batches + GC headroom)
GOMEMLIMIT   = 13 GiB   (soft backstop below B, so GC gets aggressive before OOM)
```

### 4.2 Per-entity capacity by **cardinality**, not uniformly

Uniform caps waste RAM: FPMMs/Markets have far lower live cardinality than
user-positions, yet a uniform 32M cap would give them 32M slots they never fill.
Size each cache to its working set; give **UserPositions the remainder** (it is the
one the GC note flags as "grows largest").

**Worked 14 GiB plan** (illustrative — set real caps from observed
`SELECT uniqExact(...)`):

| Cache | C | all-in | rationale |
|---|--:|--:|---|
| ConditionPreparations | 2.0M | ~0.60 GiB | ≈ one per condition; hold ~all |
| Conditions | 2.0M | ~0.40 GiB | hold ~all conditions |
| Markets | 1.0M | ~0.16 GiB | hold ~all markets |
| NegRiskEvents | 0.2M | ~0.03 GiB | small set |
| FPMMs | 0.2M | ~0.03 GiB | small set |
| **UserPositions** | **32M** | **~9.2 GiB** | the rest of B_hot |
| | | **≈ 10.4 GiB hot** | + ~3.6 GiB reserve = **~14 GiB** ✅ |

UserPositions @ 32M: ring `296×32M = 8.82 GiB`, next `122 MiB`, buckets
`pow2(64M)=67,108,864 ×4 = 256 MiB` → **9.19 GiB**. Low-cardinality caches sized to
hold ~all their rows means they **effectively never evict** (zero CH round-trips on
the hot path); only UserPositions evicts → cold.

### 4.3 Eviction is the ceiling (the core guarantee)

Because each ring is `make()`'d at `capacity` and `SetByKey` reuses slots via the
CLOCK hand, **a cache's RSS is monotonic up to `capacity × entrySize` and then
flat forever.** No `append`, no `grow`, no rehash-realloc on the hot path. So the
*fixed* part of memory cannot exceed the number you computed in §4.2 — by
construction, not by hope. Overflowing entities don't grow memory; they get
demoted to the cold tier (ClickHouse) and re-loaded on demand.

### 4.4 Kill the variable-length escapes (make the budget exact)

Three fields leave the ring and three facts follow: the budget is no longer exact,
the entries aren't fully pointer-free, and the GC has per-entry work again.

- `MemoryCondition.Payouts []uint256.Int` — N×32 B heap backing per condition.
- `MemoryMarket.QuestionIDs []common.Hash` / `MemoryNegRiskEvent.QuestionIDs` —
  N×32 B heap backing per market/event.
- `ConditionPreparations` value embeds `EventMeta.BlockTimestamp time.Time`, which
  carries a `*Location` pointer → a GC-scanned pointer in **every** slot of that
  ring (the A2 note already calls out that event structs keep `time.Time`).

**Evaluation of fixes (no code, just the trade):**

1. **Inline as a bounded fixed array.** Polymarket conditions are almost always
   binary; outcome slots are capped at 256 on-chain. Replace `Payouts []uint256`
   with `Payouts [K]uint256 + PayoutLen uint8` for a chosen K (e.g. 8). Same for
   `QuestionIDs [K]common.Hash`. Result: the entry is self-contained, the ring is
   pointer-free, and `bytes(C)` is exact. Cost: `K×32` B per slot even when
   unused, and a hard cap of K (overflow must spill the rare wide row to CH-only).
   For UserPositions (the big ring) this is moot — it has *no* var-length field, so
   the dominant 9 GiB is already exact and pointer-free.
2. **Keep slices, budget worst-case N.** Add `Σ C_i × maxN_i × 32` to B_reserve.
   Simpler, looser, still GC-scanned.
3. **For the `time.Time` pointer:** store unix-millis `int64` in the cold/hot
   value (exactly the A2 trick already applied to `MemoryUserPosition.UpdatedAt`)
   and only re-hydrate `time.Time` at the CH boundary. Removes the last per-slot
   pointer from the ConditionPreparations ring.

Recommendation: **(1)+(3)** if you want the closed form to be a literal guarantee
and O(1) GC across *all* rings; **(2)** if condition/market cardinality is low
enough that their backings are noise next to the 9 GiB UserPositions ring.

### 4.5 Structural ceiling to know about

The index uses `int32` ring slots (`buckets []int32`, `next []int32`,
`idxInsert(..., uint32)` cast to `int32`). That caps any single cache at **2³¹ ≈
2.1 billion entries** — ~640 GiB of UserPositions, far above 14 GB, so it is not a
constraint here, but it is the hard structural limit if the budget ever grows.

---

## 5. (B) Preallocation — committing the layout up front

### 5.1 What's already preallocated

`NewHotState(capacity)` (l.2590) constructs all 6 caches; each `New*ClockCache`
does `make([]Entry, capacity)`, `make([]int32, bucketCount)`, `make([]int32,
capacity)` and initializes `buckets`/`next` to `-1`. So **the ring + index virtual
memory is reserved at startup** and the footprint is decided by the capacities, not
by the data. No realloc, no fragmentation, no rehash on the hot path.

### 5.2 Make RSS deterministic from t=0 (touch-at-init)

Subtlety: `make` reserves virtual address space but Linux faults pages in lazily.
`buckets`/`next` are touched immediately (the `-1` init loops, l.76/79), so they're
resident at once; the **ring** pages only fault in as `Set` writes them. So
*virtual* size is fixed at startup but *RSS* climbs as the ring fills — which means
"it fit at startup" doesn't prove "it fits when full."

To make the 14 GB guarantee observable at t=0 rather than discovered under load,
**pre-fault the rings** (a one-time pass that writes each page, or a build that
relies on the `-1` index init already paging the index). Then RSS at startup ≈ the
§4.2 number, and if it boots, it fits. This converts "bounded eventually" into
"committed immediately" — the real point of preallocation.

### 5.3 Preallocate the off-ring growables too

Preallocating only the rings is a half-guarantee; the §3.2 ⚠ rows must also be
pinned:

- **`dirty*` maps:** today `make(map[Key]struct{})` with no size hint; they grow
  with distinct keys touched per commit window (`SQD_COMMIT_INTERVAL`=5000 blocks).
  Pre-size with a capacity hint and/or commit often enough that the working set is
  bounded; budget `≈ maxDirty × ~48 B` into B_reserve. (Alternatively, a fixed
  dirty *ring* of keys instead of a map removes the growth entirely.)
- **Resolver `misses []Key`:** give it an initial `cap` = max events/commit so it
  never re-grows; it already resets each `Resolve`.
- **Batch column builders (`Memory*Batch`):** their `proto.Col*` slices grow to the
  flush batch row count. Pre-size to the commit batch and `Reset()` (already
  called) reuses the backing — bounded.
- **ReplayBuffer (8192) + ProtoRingBuffer:** already fixed-capacity rings; include
  their worst-case per-block size in B_reserve.

### 5.4 Cost of preallocation, and how to avoid waste

Preallocation trades RAM-you-might-not-use for predictability: a 32M UserPositions
ring is 8.8 GiB resident **whether or not** 32M positions are live. That is
acceptable *only* because capacities are cardinality-sized (§4.2) — uniform 32M
across all 6 caches would commit ~57 GiB for no benefit. So **(A) and (B) are
co-dependent:** preallocation is safe precisely because the fixed-memory plan sized
each ring to its working set. If you want pay-as-you-go instead, the only knob is to
*lower* `capacity` (smaller committed ring, more eviction → more cold reads); the
rings themselves never grow.

---

## 6. The cold cache: three designs

Today "cold" = ClickHouse. Whether to add an in-process cold tier is a
latency/RAM trade. Evaluated:

### C0 — ClickHouse is the cold tier (status quo)
- **RAM:** ~0 (just the transient `misses` slice + batch result columns).
- **Cost:** one batched SQL round-trip per commit window of misses
  (`Resolver.Resolve`), `… IN (keys) … LIMIT 1 BY key`. Tombstones suppress repeat
  misses. Fully durable, unbounded.
- **Best when:** UserPositions hit-rate in the hot ring is high (most activity is
  recent), so misses are rare. With a 32M hot ring this is likely.

### C1 — in-process cold arena (demote-on-evict)
- A second, larger, **read-mostly, pointer-free** tier between the ring and CH: a
  single pre-allocated `[]byte` slab (or `[]MemoryUserPosition` flat array) with an
  open-addressing index. On hot eviction, **demote** the entry into the arena
  instead of dropping it; a hot miss checks the arena before CH.
- **RAM:** whatever slab you carve from B (counts against 14 GB). Because the value
  is fixed-size and pointer-free (after §4.4), the arena is itself closed-form:
  `coldCap × entrySize`.
- **Cost:** removes CH round-trips for the "warm but evicted" band; adds a second
  lookup and demotion bookkeeping. Not durable (it's a cache) — CH is still the
  source of truth.
- **Best when:** the working set is larger than fits hot but has a heavy "recently
  evicted, soon re-touched" band, and CH round-trip latency dominates.

### C2 — mmap'd cold file
- Same as C1 but the slab is an `mmap` of a file, so cold pages spill to disk under
  pressure and don't count fully against RSS.
- **RAM:** OS-managed; effectively elastic, but you lose the hard-ceiling guarantee
  (page cache is outside your budget math).
- **Best when:** you want a large cold tier without committing RAM, and a soft
  ceiling is acceptable.

**Recommendation:** start at **C0** (it's already implemented and costs no RAM);
the 14 GB plan in §4.2 puts ~32M positions hot, which should keep miss rate low.
Add **C1** only if profiling shows `profCustom`/resolve time dominated by CH
round-trips. **C1 is the natural fit for "fixed memory + preallocation"** because
its slab is itself a fixed, pre-allocated, closed-form region — it extends the same
discipline rather than breaking it. Avoid **C2** if the 14 GB number must be a hard
guarantee.

---

## 7. Concrete 14 GiB layout (putting A + B together)

```
B = 14 GiB hard ceiling, GOMEMLIMIT = 13 GiB backstop

HOT RINGS (preallocated + pre-faulted at startup):
  UserPositions            C=32M   →  9.19 GiB   (296 B/entry, pointer-free)
  ConditionPreparations    C=2M    →  0.60 GiB   (after §4.4(3): drop time.Time → int64)
  Conditions               C=2M    →  0.40 GiB   (after §4.4(1): inline Payouts)
  Markets                  C=1M    →  0.16 GiB   (inline QuestionIDs)
  NegRiskEvents            C=0.2M  →  0.03 GiB   (inline QuestionIDs)
  FPMMs                    C=0.2M  →  0.03 GiB
                                  ─────────────
                          hot ≈ 10.4 GiB

RESERVE (~3.6 GiB):
  dirty maps (pre-sized) + resolver miss slices
  ReplayBuffer (8192 blocks) + ProtoRingBuffer
  batch column builders (commit-batch sized)
  Go heap churn + GC headroom (GOGC tuned, or rely on GOMEMLIMIT)

COLD: ClickHouse (C0). Optional C1 arena carved from RAM only if CH misses dominate.
```

If it boots (rings pre-faulted), it fits — and it cannot grow, because every ring
evicts at capacity and every off-ring region is pre-sized.

---

## 8. Invariants & how to verify (no code, just the checks)

1. **Closed form holds:** sum the §4.2 numbers; assert ≤ B_hot. Re-derive whenever
   a struct field or a capacity changes (it's all codegen-known).
2. **Caps are cardinality-true:** periodically `SELECT uniqExact(key)` per
   `memory_*` table; a cache whose live cardinality exceeds its cap is silently
   thrashing to cold — raise its cap or accept the miss rate.
3. **RSS at startup ≈ predicted** (after pre-fault). Divergence = a region you
   didn't budget (almost always a §3.2 ⚠ growable).
4. **Pointer-free rings stay pointer-free:** the GC-scan test
   (`hotcache_gcscan_test.go`, `GC_SCAN=1`) must stay flat in N after §4.4 — that
   is the proof the cold/hot split didn't reintroduce per-entry GC work.
5. **Correctness unchanged:** the A79 gate (−$13.93 / $3.00) must still pass — any
   eviction/cold-load change is validated against it, same as every other change.
