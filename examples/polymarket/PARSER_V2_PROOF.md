# Polymarket parser v2 — byte-scanner, measured on the real generated schema

This is a **direct proof**, not a template round-trip: `parserv2.go` is a
hand-written, zero-allocation byte-scanner for the polymarket `OrderFilled` hot
path, written straight against the generated package. It fills the **exact same**
generated `InsertBatches` (`ExchangeOrderFilled` / `NegRiskExchangeOrderFilled`)
as the codegen-emitted jlexer parser (`generated.ParseJSONLV2`), decoding via the
same `abiunpack` helpers — only the JSONL extraction differs (a single forward
`bytes.Index` pass vs a per-field JSON lexer). The benchmark
(`parserv2_test.go`, `TestOrderFilledV2BeatsGenerated`) asserts row-count
equivalence, then measures both.

It exists because proving a parser win by editing the codegen *template* and
regenerating is slow and confusing — this measures the win on the real generated
multi-event schema directly.

## Measured (real Polygon block-84M OrderFilled corpus, 757,137 events, this machine)

Equivalence: generated v1 and v2 fill **identical** batches — `v1 rows == v2 rows == 757,137`.

| parser | single-thread | alloc/event |
|--------|---------------|-------------|
| **v1 generated (jlexer, full decode+fill+dead-struct)** | 1.08M ev/s | 0 |
| **v2 byte-scanner (single forward pass)** | **1.82M ev/s** | 0 |

| v2 byte-scanner, parallel (private per-worker batches) | ev/s |
|---|---|
| 1 core | 1.78M |
| 2 cores | 3.45M |
| 4 cores | 5.66M |
| **6 cores** | **8.13M** |

**v2 is 1.7× the generated parser single-threaded, and 8.13M ev/s on 6 cores =
7.5× the generated single-thread baseline — on the real schema, zero-alloc,
producing identical batches.**

## Why 1.7× here (not the 2.7× seen on the sparse LBTC extract)

Honest accounting:
- `OrderFilled` uses **nearly every log field** (address for routing, 2 topics for
  maker/taker, data for 5 uint256, txIndex+logIndex for meta) — the only field the
  scanner skips is `transactionHash`. The byte-scanner's biggest lever (skipping
  unneeded fields) barely applies.
- jlexer is already zero-alloc (`UnsafeString`), so there is no allocation gap to
  close (both are 0 B/event); the win is pure CPU: one forward pass + no JSON
  ceremony + no dead-struct materialization.
- A **multi-pass** byte-scanner (one `bytes.Index` over the whole object per field)
  is *par* with jlexer (measured 1.07M vs 1.09M) — re-scanning the ~700-byte object
  per field eats the advantage. The win requires the **single forward pass** in
  this file. That itself is the lesson: the byte-scanner only beats the lexer if it
  is single-pass and field-targeted.

## Where this sits in the real polymarket pipeline

The measured live polymarket@84M profile is **CUSTOM 54% / PARSE 36% / FETCH 7% /
INSERT 3%** (see `experiments/REPORTING/BATCH_PREP_FINDINGS.md`). So a 1.7× parser
shrinks the 36% PARSE slice — worthwhile, but the dominant lever remains the
ordered custom processor (54%) and its cold-tier lookups. The **parallel scaling**
(1→6 cores: 1.78M→8.13M) is the more transferable win: with per-worker private
batches the fill parallelizes for free.

## Reproduce

```bash
# 1. download a pure-OrderFilled corpus from block 84M to /private/tmp/claude-501/pmof
#    (Exchange + NegRiskExchange addresses, OrderFilled topic0; ~10 pages)
# 2. regenerate the polymarket package, then:
go test ./examples/polymarket/ -run TestOrderFilledV2BeatsGenerated -v
```

The benchmark Skips if the corpus is absent. The corpus is real portal data, not
committed (60 MB); the filter (addresses `0x4bfb…` / `0xc5d5…`, topic0 `0xd0a08e8c…`)
is in `parserv2.go`.

## Scope / honesty

- **OrderFilled only.** The dominant block-84M event; clean (no dynamic arrays).
  The other ~20 polymarket events (incl. array-bearing `PositionSplit`) are not
  reimplemented here — the proof is the parse layer on the hot event.
- **v1 baseline is the dev-generated parser**, which still does the per-block
  dead-struct slot fill (no `onBlock` gate on this branch); part of v2's win is
  skipping that. That is fair — v2 is the fast path.
- This is a proof artifact, not a drop-in replacement: to ship, the codegen parser
  template would emit this single-pass scanner for all archetypes.
