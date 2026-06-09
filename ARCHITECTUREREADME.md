# V2 Architecture — Proto-Mode Indexing Pipeline

This document explains the **V2 ("proto-mode") pipeline** step by step: how raw
blockchain history is fetched, decoded, buffered, processed into derived state
(e.g. Polymarket PnL), persisted to ClickHouse, and kept correct across chain
reorgs and crashes.

V2 is the zero-copy, low-GC evolution of the original struct-based pipeline. The
design intent lives in [`ARCHITECTURE.md`](ARCHITECTURE.md); the V2 feature gate
and rollout status live in [`ecs/V2_FLAG.md`](ecs/V2_FLAG.md). This README
describes what the code actually does today.

---

## 0. The one-paragraph version

A **producer** goroutine streams JSONL pages from a SQD portal, decodes each log
into a typed event, and writes per-block entries into an in-memory **replay ring
buffer**. A **consumer** goroutine reads blocks out of that buffer in strict
order, inserts typed events into ClickHouse columnar batches, then feeds the
block to a **custom processor** that maintains derived state in a CLOCK-evicted
**hot cache** backed by ClickHouse. A **sync/checkpoint** mechanism records the
last durable block so crashes resume cleanly, and a **fork detector** rolls back
state and replays from the ring buffer when the chain reorgs. V2's defining trait
is that events live in **columnar proto blocks** with **zero-copy views**, so the
hot path allocates almost nothing and the GC barely runs.

```
                 ┌────────────────────── producer goroutine ──────────────────────┐
  SQD portal ──▶ │ FetchWithParent ─▶ ParseWithLine (JSONL) ─▶ decode logs ─▶ ...  │
                 └─────────────────────────────────────────────────────┬──────────┘
                                                                        ▼
                                                          ┌──────── ReplayBuffer ────────┐
                                                          │  ring of recent blockEntry   │
                                                          └───────────────┬──────────────┘
                                                                          ▼
                 ┌────────────────────── consumer goroutine ──────────────────────┐
                 │ GetBlock(n) ─▶ batch ─▶ ClickHouse insert ─▶ custom processor   │
                 │            ─▶ ProtoRingBuffer + hot CLOCK cache ─▶ checkpoint    │
                 └─────────────────────────────────────────────────────────────────┘
```

The producer and consumer run concurrently and are decoupled by the ring buffer,
exactly as called for in `ARCHITECTURE.md` ("PARALLEL: FETCHING → DECODING →
RINGBUFFER" running alongside "WAIT FOR NEW SLOT → CUSTOM_PROCESSOR → INGEST").

---

## 1. Entry point & proto mode (default)

- `main.go` registers the Polymarket project and calls `cli.Run` ([main.go](main.go)).
- `sqd-go dev <project>` resolves to `runDev` → `runStartPipelineInternal`
  ([internal/cli/run.go:19](internal/cli/run.go)). This loads the project config,
  runs codegen, brings up `docker compose` (ClickHouse), and starts ingestion.
- **Proto mode is on by default.** The `--no-proto` flag disables it, falling
  back to the V1 legacy parsed mode (struct-based JSON decode). Proto mode flows
  through `applyOverrides` → `cfg.ProtoMode`
  ([internal/cli/run.go:79](internal/cli/run.go)) and is also pushed into a
  process-global via `SetProtoMode`
  ([internal/cli/processor_registry.go:19](internal/cli/processor_registry.go)).
- When the processor is constructed, it reads that global:
  `generated.NewProcessor(cli.GetProtoMode())`
  ([internal/cli/init.go:783](internal/cli/init.go)). Proto mode therefore
  selects the **columnar proto ring + zero-copy view** code path inside the
  processor rather than the legacy struct path.
- **Cold cache is on by default.** The Pebble cold tier (§6.5) is enabled
  unless `--no-cold-cache` is passed or the config sets `cold_cache: false`.
  `--cold-cache` force-enables it. Resolution logic: `resolveColdCache`
  ([internal/cli/run.go:296](internal/cli/run.go)).
- Useful related flags: `--restart` (drop DB and re-index from scratch),
  `--start-block` / `--end-block`, `--pagesize`, `--cpuprofile`.

Everything below happens inside `ingestion.Run` →
`processChain` ([internal/ingestion/ingestion.go:70](internal/ingestion/ingestion.go),
[:142](internal/ingestion/ingestion.go)).

---

## 2. Stage 1 — FETCH (producer)

The producer is an anonymous goroutine started by `startProd`
([internal/ingestion/ingestion.go:316](internal/ingestion/ingestion.go)). Its loop:

1. **Backpressure check.** If the producer is more than `capacity-100` blocks
   ahead of the consumer, it sleeps 10ms and retries. This prevents the producer
   from overrunning the ring buffer
   ([:359](internal/ingestion/ingestion.go)).
2. **Range selection.** `nextProducerRequestRange` computes the next
   `[from, to]` request window using an adaptive page size that grows over time
   and is capped at the finalized head during backfill
   ([:1028](internal/ingestion/ingestion.go)).
3. **Fetch.** `sqd.FetchWithParent(ctx, from, to, parentHash, cursorMode, filters)`
   pulls a JSONL page from the SQD portal
   (`chainEndpoint`, [:1056](internal/ingestion/ingestion.go); client in
   `internal/client`). In **cursor mode** the request carries the expected parent
   hash so the portal can signal a reorg by returning a `client.ForkError`.
4. **Finalized head tracking.** The response header's finalized block is recorded
   so checkpoints can be gated to it (see §8).

Empty responses at the chain head wait `cursorPollInterval` and retry; empty
responses during backfill end the producer.

---

## 3. Stage 2 — PARSE & DECODE (producer)

Each fetched page is raw JSONL (one JSON object per block).
`jsonl.ParseWithLine` (a reusable `parser.FastJSONLParser`,
[:240](internal/ingestion/ingestion.go)) walks the bytes line by line and invokes
a callback per block ([:434](internal/ingestion/ingestion.go)):

- For each log in the block, the first topic is hashed
  (`abiunpack.DecodeTopicHash`) and matched against the registered event
  `decoders`. Non-matching logs and address mismatches are skipped cheaply.
- Matching logs are decoded into a `parser.DecodedEvent` carrying chain/block/tx
  metadata. Strings that outlive the parse buffer (`hash`, `txHash`, `address`)
  are explicitly `strings.Clone`d so no decoded event aliases the transient page
  buffer.
- Events are also bucketed by **typed table** (`blockTypedEvents[table.Name]`)
  for columnar ClickHouse insertion.
- A `decodedBlock{number, hash, timestamp, events, typedEvents, raw}` is
  accumulated per block.

The result is a per-page slice of decoded blocks, each grouped by block number —
matching the `ARCHITECTURE.md` shape
`[{blocknumber, Transfers[], Positions[], ...}, ...]`.

> **Note on the experimental raw-JSONL fast path.** When the env var
> `SQD_PARSE_DECODE_V2` is set *and* the processor implements
> `FastJSONLProcessor` ([internal/ingestion/processor.go:31](internal/ingestion/processor.go)),
> the consumer skips re-materializing `CustomLog`s and instead forwards the raw
> JSONL bytes straight to `ProcessJSONL`
> ([internal/ingestion/ingestion.go:255](internal/ingestion/ingestion.go)). This
> is an optional optimization layered on top of the normal proto path; the
> default Polymarket processor uses the standard path described below.

---

## 4. Stage 3 — REPLAY RING BUFFER (handoff)

Decoded blocks are written into the **`ReplayBuffer`**, a fixed-capacity circular
buffer (8192 blocks) of recent blocks
([internal/ingestion/replay.go:34](internal/ingestion/replay.go),
constructed at [ingestion.go:241](internal/ingestion/ingestion.go)).

- `Write(...)` clones each block's events/logs/typed-events into a `blockEntry`
  so the buffer owns its data independent of the transient parse buffers
  ([replay.go:71](internal/ingestion/replay.go)). It records `isLastInBatch`
  (the last block of a fetch page → triggers a DB flush) and the request's
  finalized head.
- A `map[blockNumber]slotIndex` index gives O(1) `GetBlock(n)`
  ([replay.go:222](internal/ingestion/replay.go)). When the ring wraps, the
  oldest entry is evicted from the index.
- `notifyCh` wakes the consumer whenever a block lands.

This buffer is the **fork-recovery cache**: on a reorg, recent blocks are
replayed from here instead of re-fetched from the network (§7). A fork deeper
than 8192 blocks falls back to a database state reload.

> This is distinct from the processor's `ProtoRingBuffer` (§6). The `ReplayBuffer`
> lives at the ingestion layer and holds whole-block decoded entries for replay;
> the `ProtoRingBuffer` lives inside the processor and holds columnar proto event
> blocks for state computation.

---

## 5. Stage 4 — CONSUME, BATCH & INGEST into ClickHouse (consumer)

The consumer is the main `for` loop in `processChain`
([internal/ingestion/ingestion.go:642](internal/ingestion/ingestion.go)). It
pulls blocks strictly in order via `GetBlock(currentConsumerBlockVal)`; if the
block isn't buffered yet it blocks on `replayBuf.notifyCh`, the error channel,
the finalized channel, or a stats ticker.

For each block it:

1. **Accumulates** the block's decoded events, typed events, and block-ledger row
   into batch slices, and applies the block to the fork-tracking state
   (`state.ApplyBatch`).
2. On the **last block of a fetch page** (`entry.isLastInBatch`), it flushes the
   batch ([:680](internal/ingestion/ingestion.go)):
   - `InsertLogs` — optional raw log rows.
   - For each typed table, `TypedInserter.Insert(ctx, events)` — the **columnar**
     ClickHouse path. Each typed event maps to dense columns
     (`proto.ColUInt64`, `proto.ColFixedStr`, `proto.ColUInt256`, …) as defined
     in the generated `ProtoEventBlock`
     ([examples/polymarket/generated/views.go:15](examples/polymarket/generated/views.go)).
   - `InsertBlocks` — optional block ledger.
3. **Advances the producer** by sending `{nextBlock, parentHash}` on
   `producerAdvanceChan` so the next page is fetched with the correct parent for
   fork detection.
4. **Runs the custom processor** (§6) over the batch.
5. **Updates the checkpoint** (§8).

Ingestion is columnar throughout: events are appended to typed column builders
and inserted as blocks, never row-by-row.

---

## 6. Stage 5 — CUSTOM PROCESSOR (derived state, e.g. PnL)

The processor is generated per project
([examples/polymarket/generated/custom_processor.go](examples/polymarket/generated/custom_processor.go)).
`Process(ctx, store, logs)` ([:162](examples/polymarket/generated/custom_processor.go))
groups incoming logs by block and, in **proto mode**, does the following per
block:

1. `protoRing.NextProtoSlot(blockNum, hash)` claims and resets a columnar slot in
   the **`ProtoRingBuffer`** ([generated/ringbuffer.go:367](examples/polymarket/generated/ringbuffer.go)).
   The ring is power-of-two sized; indexing is a bit-mask (`& bitWiseLength`), and
   slots are reused (`Reset`) rather than reallocated — this is the pointer-free,
   GC-light columnar store referenced in the hot-cache GC notes.
2. `block.AppendFromLog(addr, topics, data, meta)` decodes the raw log **directly
   into columns** with no per-event heap allocation
   ([generated/views.go:695](examples/polymarket/generated/views.go)).
3. `processProtoBlocks` → `CustomProcessingProto`
   ([generated/custom_processor.go:44](examples/polymarket/generated/custom_processor.go))
   invokes the domain logic `CustomProcessProtoFn`, which iterates events via
   **zero-copy proto views**:

   ```go
   block.QueryConditionalTokensPositionSplit().Map(func(ev ...ProtoView) { ... })
   ```

   ([examples/polymarket/custom_processor.go:448](examples/polymarket/custom_processor.go)).
   A `...ProtoView` is a tiny struct `{block, index}`; accessor methods like
   `ev.Maker()` / `ev.ConditionID()` read straight from the columnar arrays
   ([generated/views.go:1151+](examples/polymarket/generated/views.go)) — no
   event struct is ever materialized.

### Hot state, disk/DB state — the CLOCK cache

Derived entities (UserPositions, Conditions, Markets, NegRiskEvents, FPMMs) live
in **CLOCK-evicted hot caches** in `state.HotState`
([examples/polymarket/generated/hotstate.go:51](examples/polymarket/generated/hotstate.go)):

- Each cache is a flat ring of entries plus an integer index map (`idxLookup` /
  `idxInsert`). Capacity defaults to 100k.
  Lookups and inserts are O(1) over pointer-free arrays — this is the "A2
  pointer-free ring + A3 flat index" layout that makes cache GC O(1).
- Eviction is the **CLOCK / second-chance algorithm**: each entry has a
  `referenced` bit; `Set` advances a `hand`, clearing reference bits until it
  finds an unreferenced slot to reuse ([hotstate.go:127](examples/polymarket/generated/hotstate.go)).
  `Get` sets the reference bit ([:168](examples/polymarket/generated/hotstate.go)).
- On a **cache miss**, the entity is lazily loaded from ClickHouse via a
  per-entity **Resolver** (`Queue` the key, `Resolve` against the DB, re-`Get`),
  e.g. `UserPositionState.Get`
  ([examples/polymarket/generated/state.go:58](examples/polymarket/generated/state.go)).
  So the base path is **hot cache → ClickHouse on miss**, with full state durable
  in ClickHouse. With the optional **cold tier** enabled the path becomes **hot
  cache → cold tier (Pebble) → ClickHouse**, and the **V3** negative filter can
  short-circuit a provably-new key before either — see §6.5.

### Hybrid commit cadence (durability)

After each block, `commitCustomProcessing`
([examples/polymarket/generated/custom_processor.go:60](examples/polymarket/generated/custom_processor.go))
decides whether to make derived state durable. It commits when **either** bound
trips:

- `SQD_COMMIT_INTERVAL` blocks (default 5000) — bounds the crash re-fetch budget;
  dominates during fast backfill.
- `SQD_COMMIT_MAX_INTERVAL` (default 3s) — bounds durability latency; dominates at
  the live head where only a few blocks/sec arrive.

On commit it `SaveSnapshot(blockNumber)`, `state.Commit(ctx, store)` (flush hot
state to ClickHouse), periodically prunes historic state
(`CompactionPruneState`, `CLICKHOUSE_PRUNE_INTERVAL`), and advances
`state.LastSyncBlock`. The highest durably-committed block is exposed via
`CommittedBlock()` (the `CommitHorizonReporter` interface) so the ingestion
checkpoint can be gated to it.

> **Correctness gate:** the Wallet A79 PnL value (−$13.93 / $3.00) is the
> end-to-end check that V2 is at parity with V1. See
> `examples/polymarket/wallet_a79_*_test.go`.

---

## 6.5 Cold tier (V2) and the V3 negative-lookup filter

The `hot cache → ClickHouse on miss` path above has one pathological case: a
**from-genesis backfill**, where almost every hot miss is a brand-new key whose
lazy `Resolve` is a ClickHouse point-SELECT (~1.9 ms) that returns *nothing*. That
SELECT storm caps the indexer at ~500 blk/s.

**V2 — Pebble cold tier** ([`internal/coldcache`](internal/coldcache/coldcache.go),
**on by default**; disable with `--no-cold-cache`). A bounded, off-heap Pebble KV
sits *under* the CLOCK caches and *above* ClickHouse for the pointer-free entities
(UserPositions, FPMMs):

- On CLOCK **eviction**, the victim is **spilled** to Pebble
  ([hotstate.go `SetByKey`](examples/polymarket/generated/hotstate.go)).
- On a hot **miss**, the entry is served from Pebble (~8 µs) and promoted back
  ([hotstate.go `Get`](examples/polymarket/generated/hotstate.go)).
- When the tier is opened against an **empty** ClickHouse (from-genesis /
  `--restart`) it is **authoritative**: a hot+cold miss is *provably* new, so the
  ClickHouse SELECT is skipped entirely (`coldAuthoritative`,
  [state.go:67](examples/polymarket/generated/state.go)).

This removes the SELECT storm and is the big V1→V2 win at the head of a backfill.

**V3 — in-RAM negative-lookup filter** (opt-in via `--dev-ch-type`, implies proto + cold).
V2 still pays *one negative Pebble `Get` per brand-new key* — and during a
from-genesis backfill that is nearly every event. V3 puts a fixed-size **blocked
Bloom filter** in front of Pebble
([`internal/coldcache/filter.go`](internal/coldcache/filter.go)):

- `Put` (on spill) adds the key; `Get` tests it first. If the filter says
  **absent**, the key was never spilled, so the Pebble probe (and, when
  authoritative, the ClickHouse SELECT) is skipped — resolved from a **single
  64-byte cache-line test in RAM**.
- **Correctness is structural:** keys are only ever added, so the filter has **no
  false negatives** — "absent" is always truthful. A false positive merely falls
  through to a correct Pebble `Get`; if the filter ever saturates it degrades to
  V2 behaviour, never to a wrong answer. The A79 gate (−$13.93 / $3.00) holds.
- **Blocked, not classic:** all k probes land in one cache line (one cache miss),
  so the filter is actually faster than a hot Pebble negative `Get`. A classic
  scattered Bloom (k random probes) is *slower* once the bitset exceeds L2 — that
  was measured and rejected.
- **Bounded memory:** a power-of-two bitset allocated once at startup (default
  ~64 MiB/cache, [ingestion.go `coldNegativeFilterBits`](internal/ingestion/ingestion.go)).

The CLI wires this through `ColdNegativeFilterProcessor`
([processor.go](internal/ingestion/processor.go) →
[hotstate.go `EnableColdNegativeFilter`](examples/polymarket/generated/hotstate.go)).

### Synthetic-load result (V2 vs V3)

`TestLoadV2VsV3` ([wallet_a79_loadtest_test.go](examples/polymarket/wallet_a79_loadtest_test.go))
replays a deterministic stream of distinct-wallet `OrderFilled` logs (each a new
UserPosition, growing the ring past its capacity so the cold tier is exercised)
with the real A79 events sprinkled in, through V2 and V3 on separate ClickHouse
DBs, and takes the median over alternating-order rounds so neither version gets a
warm-cache slot advantage. Both must reproduce A79 = −$13.93 / $3.00.

```bash
LOAD_TEST=1 LOAD_TEST_BYTES=1073741824 LOAD_TEST_ROUNDS=3 \
  go test ./examples/polymarket/ -run '^TestLoadV2VsV3$' -v -count=1 -timeout 900s
```

Representative local run (Apple silicon, 1 GiB workload, ClickHouse on :9003):

| Version | Cold path | Median throughput | A79 PnL |
|---------|-----------|------------------:|---------|
| V2 | proto + Pebble cold tier | ~302k blk/s | −$13.9310 / $3.00 ✅ |
| V3 | + in-RAM negative filter | ~325k blk/s (**1.07×**) | −$13.9310 / $3.00 ✅ |

V3 skipped ~1.3M negative Pebble `Get`s/run at 1 GiB; the margin **grows with the
cold-store size** (the larger Pebble gets, the more a negative `Get` costs), to
~1.08× at 2 GiB and more across a full 26M→40M backfill where the on-disk DB far
exceeds Pebble's block cache. Same harness covers V1→V2 (`TestLoadV1VsV2`).

---

## 7. FORK DETECTION & ROLLBACK (reorg recovery)

Fork handling runs in **cursor mode** (live head), driven by the fork-tracking
state machine and ring buffer in
[internal/ingestion/fork.go](internal/ingestion/fork.go) /
[fork_go.go](internal/ingestion/fork_go.go).

1. **Detect.** The portal returns a `client.ForkError` from `FetchWithParent`
   when the supplied parent hash no longer matches the canonical chain. The
   producer forwards it on `errChan`
   ([internal/ingestion/ingestion.go:382](internal/ingestion/ingestion.go)).
2. **Find common ancestor.** `state.HandleFork(previousBlocks)` walks the
   unfinalized chain held in the `BlockRingBuffer` to find the safe common block
   ([fork_go.go:95](internal/ingestion/fork_go.go), consumer at
   [ingestion.go:828](internal/ingestion/ingestion.go)).
3. **Roll back ClickHouse.** `rollbackAfterBlock(ctx, store, mode, chainID,
   safe.Number)` deletes rows above the safe block
   ([ingestion.go:1076](internal/ingestion/ingestion.go)) — the
   "DELETE ROWS ABOVE FORK BLOCK" step from the design.
4. **Restore processor state.** `proc.RestoreToBlock(safe.Number)` rewinds
   derived state from its in-memory snapshot; if that fails it falls back to
   `proc.LoadFromDatabase(safe.Number)` (reload hot state from ClickHouse)
   ([ingestion.go:846](internal/ingestion/ingestion.go)).
5. **Replay forward.** For any gap between the restored block and the safe block,
   blocks are re-read from the **`ReplayBuffer`** and re-processed — no network
   re-fetch ([ingestion.go:858](internal/ingestion/ingestion.go)). A buffer miss
   falls back to a DB state load.
6. **Resume.** `replayBuf.PruneAfter(safe.Number)`, discard any uncommitted
   batch, set the consumer to `safe.Number+1`, and restart the producer from the
   safe hash ([ingestion.go:888](internal/ingestion/ingestion.go)).

Finalized blocks are pruned out of the fork buffer (`pruneFinalized`) and reorg
snapshots are disabled entirely during finalized backfill
(`SetSnapshotsEnabled(false)`, [ingestion.go:253](internal/ingestion/ingestion.go))
since reorgs can't reach below the finalized head — removing their GC cost where
they aren't needed.

---

## 8. CHECKPOINT, SYNC TABLE & CRASH RECOVERY

The pipeline records progress in a `sync_state` table so a crash resumes without
data loss or duplication.

**Cursor mode (live head):** after each committed batch, `saveForkState` writes
the current unfinalized head, and `TruncateSyncState` trims old sync rows every
10 blocks ([ingestion.go:732](internal/ingestion/ingestion.go)).

**Backfill mode:** the checkpoint is deliberately **gated to the durable
horizon** ([ingestion.go:746](internal/ingestion/ingestion.go)):

- It is clamped to the processor's `CommittedBlock()` so the checkpoint never
  leads un-committed derived state.
- It is clamped to the **finalized head** so a crash always resumes from a
  finalized block and the re-fetched gap can't be reorged out (the
  "no-data-loss" invariant noted in the checkpoint design).
- Before advancing, `FlushAsyncInserts` forces event rows durable, then
  `UpdateSyncState(chainID, checkpointBlock)` records it.

The lag introduced by the periodic commit cadence (§6) is intentional: a crash
re-fetches that bounded gap over HTTP and re-processes it idempotently, which is
cheap. On clean completion the tail is force-flushed
(`flusher.Flush`, [ingestion.go:917](internal/ingestion/ingestion.go)) so nothing
processed is lost.

**On startup / crash recovery** (per `ARCHITECTURE.md`): read the latest
`sync_state` entry, treat everything above it as un-durable, and continue from
`lastBlock + 1`. `--restart` ignores the saved checkpoint and starts from the
configured start block. `rollbackAfterBlock` guarantees any rows beyond the
checkpoint are truncated before reprocessing, so resume is idempotent.

---

## 9. Parallelism summary

| Goroutine | Responsibility | Decoupled by |
|-----------|----------------|--------------|
| **Producer** | fetch → parse JSONL → decode → `ReplayBuffer.Write` | `ReplayBuffer` (ring) |
| **Consumer** | `GetBlock` → batch → ClickHouse insert → custom processor → checkpoint | `producerAdvanceChan`, `notifyCh`, `errChan`, `finalizedChan` |

- **Backpressure:** the producer stalls when it gets within ~100 blocks of the
  ring capacity, so memory stays bounded
  ([ingestion.go:359](internal/ingestion/ingestion.go),
  [:535](internal/ingestion/ingestion.go)).
- **Ordering:** the consumer only ever reads `currentConsumerBlockVal` and
  increments by one, so blocks commit in strict ascending order even though
  fetching/decoding ran ahead in parallel.
- **Profiling:** per-stage nanosecond accumulators (`profFetchNanos`,
  `profParseNanos`, `profDecodeNanos`, `profInsertNanos`, `profCustomNanos`,
  `profConsumerWaitNanos`, `profProducerBackpressureNanos`) are summarized by
  `printProfile` on exit ([ingestion.go:939](internal/ingestion/ingestion.go)).

---

## 10. Why V2 is fast (the GC story)

| Mechanism | Effect |
|-----------|--------|
| Columnar `ProtoEventBlock` + reused ring slots | events stored as dense typed columns, no per-event struct allocation |
| `AppendFromLog` decodes straight into columns | hot path is allocation-free |
| Zero-copy `...ProtoView` (`{block, index}`) | iterating events materializes nothing |
| Pointer-free CLOCK hot cache + flat int index | O(1) lookup/insert/eviction and **O(1) GC scan** of derived state |
| Snapshots disabled during finalized backfill | removes the largest in-memory churn where reorgs are impossible |
| Bounded ring buffers (replay + proto) | working-set memory is constant regardless of chain length |
| Pebble cold tier + authoritative-skip (V2, §6.5) | evicted keys served from local disk, provably-new keys skip ClickHouse — kills the from-genesis SELECT storm |
| In-RAM blocked-Bloom negative filter (V3, §6.5) | provably-new keys skip even the Pebble probe (one cache-line test); ~1.07× over V2 on the synthetic load, A79-correct |

Expected order-of-magnitude wins (parser allocation, ring memory, GC scan time,
position math, ClickHouse ingest) are tabulated in
[`ecs/V2_FLAG.md`](ecs/V2_FLAG.md).

---

## File map

| Concern | File |
|---------|------|
| CLI entry / flags | [main.go](main.go), [internal/cli/run.go](internal/cli/run.go), [internal/cli/processor_registry.go](internal/cli/processor_registry.go) |
| Pipeline orchestration | [internal/ingestion/ingestion.go](internal/ingestion/ingestion.go) |
| Replay ring buffer | [internal/ingestion/replay.go](internal/ingestion/replay.go) |
| Fork detection / rollback | [internal/ingestion/fork.go](internal/ingestion/fork.go), [internal/ingestion/fork_go.go](internal/ingestion/fork_go.go) |
| Processor interfaces | [internal/ingestion/processor.go](internal/ingestion/processor.go) |
| Generated processor | [examples/polymarket/generated/custom_processor.go](examples/polymarket/generated/custom_processor.go) |
| Proto columns & zero-copy views | [examples/polymarket/generated/views.go](examples/polymarket/generated/views.go) |
| Proto ring buffer | [examples/polymarket/generated/ringbuffer.go](examples/polymarket/generated/ringbuffer.go) |
| CLOCK hot cache | [examples/polymarket/generated/hotstate.go](examples/polymarket/generated/hotstate.go) |
| Hot/DB state + resolvers | [examples/polymarket/generated/state.go](examples/polymarket/generated/state.go) |
| Cold tier (V2) + negative filter (V3) | [internal/coldcache/coldcache.go](internal/coldcache/coldcache.go), [internal/coldcache/filter.go](internal/coldcache/filter.go) |
| V2/V3 synthetic load + A79 gate | [examples/polymarket/wallet_a79_loadtest_test.go](examples/polymarket/wallet_a79_loadtest_test.go) |
| Domain logic (PnL) | [examples/polymarket/custom_processor.go](examples/polymarket/custom_processor.go) |
| Design intent | [ARCHITECTURE.md](ARCHITECTURE.md), [ecs/V2_FLAG.md](ecs/V2_FLAG.md), [COLD.md](COLD.md) |
