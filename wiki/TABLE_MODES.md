# Event-Table Generation Modes

sqd-go generates one set of ClickHouse **raw-event tables** per `config.yaml`.
There are three ways those tables can be laid out. **Only Mode 1 is implemented
today, and it is the default — it does not change.** Modes 2 and 3 are opt-in
behaviours layered on top of it.

| Mode | Config knob | One table per… | `contract_address` column | Status |
|------|-------------|----------------|---------------------------|--------|
| **1. Shared** *(DEFAULT — current behaviour, unchanged)* | *(none)* | `(chain, contract, event)` | Opt-in via `include_metadata: [contract_address]` | **Shipped** |
| **2. Address as extra field** | `address_field: always` | `(chain, contract, event)` — same as default | **Mandatory** (always emitted) | Documented below; small change |
| **3. Per-address table** | `table_mode: per_address` | `(chain, contract, address, event)` — finer than default | Opt-in (inherits the Mode-2 knob) | **PROPOSED** — see [PER_ADDRESS_TABLES_PLAN.md](../PER_ADDRESS_TABLES_PLAN.md) |

Modes 2 and 3 are **orthogonal** — `address_field` (column) and `table_mode`
(grouping) compose as a clean 2×2.

---

## Mode 1 — Shared (default, unchanged)

One physical table **and** one SQL view per `(chain, contract, event)` triplet.
The table name is `{contract}_{event}_events`
([internal/codegen/codegen.go:488](internal/codegen/codegen.go:488),
[:514](internal/codegen/codegen.go:514)).

A contract's `address` is **already a `[]string`**
([internal/config/config.go:109](internal/config/config.go:109)): *all* addresses
listed under one contract entry share **one** table, and the generated view
filters them with `address IN (...)`
([codegen.go:776-794](internal/codegen/codegen.go:776),
[views.sql.tmpl:16-18](internal/template/templates/sql/views.sql.tmpl:16)).

Because the grouping key includes the **contract name**, two contracts that emit
an identically-named event still get separate tables today — e.g. polymarket's
`Exchange.OrderFilled` and `NegRiskExchange.OrderFilled` become
`exchange_order_filled_events` and `neg_risk_exchange_order_filled_events`.

The contract address is a **row-level** value: the parser sets
`meta.ContractAddress = abiunpack.AddressFromHex(address)` on every log
unconditionally ([parser.go.tmpl:265](internal/template/templates/code/parser.go.tmpl:265)),
but it is only *stored as a column* when the config opts in:

```yaml
include_metadata:
  - contract_address
```

Resolved case-/underscore-insensitively via `cfg.MetadataIncluded("contract_address")`
([config.go:302-312](internal/config/config.go:302)) and threaded into codegen as
`IncludeContractAddress`. Every touchpoint is guarded by
`{{- if .IncludeContractAddress}}`. **This is the byte-for-byte behaviour that
ships today and remains the default.**

---

## Mode 2 — Address as an Extra Field

### Summary

Same table grouping as the default; the **only** difference is that the
row-level `contract_address` `FixedString(20)` column is **forced on for every
event table** instead of requiring an `include_metadata` opt-in.

Use this when several addresses share one table (the default grouping already
bundles a contract's `address: [...]` list into a single table) and you need to
attribute each row to the address that emitted it — but you do **not** want the
finer per-address table split of Mode 3.

### Config

A single new top-level knob, orthogonal to `table_mode`:

```yaml
name: polymarket
address_field: always      # opt_in (default) | always
chains:
  - id: 137
    contracts:
      - name: Exchange
        address: "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint256 makerAssetId, uint256 takerAssetId, uint256 makerAmountFilled, uint256 takerAmountFilled, uint256 fee)
```

- `address_field: opt_in` (or omitted) → **exactly today's behaviour**:
  `contract_address` appears only if `include_metadata` lists it.
- `address_field: always` → `IncludeContractAddress` is forced `true` for the
  whole project, regardless of `include_metadata`.

Implementation is a one-line effective-value resolver —
`IncludeContractAddress = cfg.MetadataIncluded("contract_address") || cfg.AddressColumnAlways()` —
substituted at the handful of call sites that read
`cfg.MetadataIncluded("contract_address")` directly. **No template changes are
required**; the existing `{{- if .IncludeContractAddress}}` guards simply become
true.

### What the generated output gains

When `IncludeContractAddress` becomes true, these already-existing conditional
blocks activate:

**Schema** — a column is added to every `CREATE TABLE`
([clickhouse.go.tmpl:36](internal/template/templates/sql/clickhouse.go.tmpl:36),
driven by [codegen.go:639,653,663,703](internal/codegen/codegen.go:663)):

```sql
contract_address FixedString(20),
```

It is **not** part of the sort/primary key
([codegen.go:723-730](internal/codegen/codegen.go:723)), so row ordering is
unchanged.

**Inserter** — six guarded sites turn on
([inserter.go.tmpl](internal/template/templates/code/inserter.go.tmpl), wired from
[inserter.go:44,51](internal/codegen/inserter.go:51)): the column name in
`ClickHouseCommonColumnNames`, a `colContractAddress proto.ColFixedStr` field,
`SetSize(20)` in `init()`, `Reset()`, `Append(meta.ContractAddress.Bytes())`,
and the `Inputs()` entry.

**Parser** — **no change needed**: the parser already sets
`meta.ContractAddress` on every log
([parser.go.tmpl:265](internal/template/templates/code/parser.go.tmpl:265)). In
opt-in-off mode that assignment is simply never consumed because the column does
not exist; in this mode it flows into the now-present column.

**Views** (when raw logs are stored) — each view's SELECT gains
`concat('0x', lower(hex(address))) AS contract_address`
([codegen.go:764-766](internal/codegen/codegen.go:764)). The `address IN (...)`
WHERE filter is unchanged.

### Semantics recap

| Aspect | Default (`opt_in`) | `address_field: always` |
|--------|--------------------|--------------------------|
| Tables generated | `{contract}_{event}_events`, one per `(chain,contract,event)` | **same** |
| `contract_address` column | Only if `include_metadata: [contract_address]` | **Always present** |
| Column type | `FixedString(20)` | `FixedString(20)` |
| Sort / primary key | unaffected | unaffected |
| Parser assignment | already always runs | already always runs |
| Hot-state `memory_*` tables | untouched | untouched |

Mode 2 is genuinely orthogonal to everything else — it only flips one boolean and
adds one already-templated column. It does **not** touch table grouping, Go type
names, the custom processor, or hot-state.

---

## Mode 3 — Per-Address Table (PROPOSED)

Splits the grouping one level finer: **one table per individual address**, even
for a single multi-address contract entry, with human-readable names via aliases
(and a zero-config hex-suffix fallback). This is the only mode that changes the
grouping key, and — unlike Mode 2 — it has real coupling to the custom processor
and the ingestion table index that must be handled carefully.

The full design, config syntax, and the confirmed implementation blockers live in
**[PER_ADDRESS_TABLES_PLAN.md](../PER_ADDRESS_TABLES_PLAN.md)**.

---

## Not affected by any mode: hot-state / `memory_*` overlap tables

Hot-state tables (`memory_conditions`, `memory_user_positions`, …) are a
**separate concern** from raw-event tables. They are declared under `state:`
([config.go:85-91](internal/config/config.go:85)) and keyed purely by **entity
fields** (`ID`, or `(User, TokenID)`) with **no contract address and no chain id
in the key** — they are *already overlapping/shared across contracts by design*:
many different contracts write into the same `memory_*` entity.

- Cache / dirty-set / snapshot restore use only entity-derived `keyType`
  ([hot_state.go:15-22,878-898](internal/codegen/hot_state.go:15)).
- Compaction/pruning `DELETE`s operate on the entity primary key only
  ([compaction.go.tmpl:73-76](internal/template/templates/code/compaction.go.tmpl:73)).
- The cold-tier negative filter extracts only entity PK fields
  ([hot_state_filter_keys.go:46-97](internal/codegen/hot_state_filter_keys.go:46)).

The **state templates** are entity-keyed and therefore unaffected by table modes.
⚠️ **However**, for Mode 3 the *data source* that feeds a hot-state entity is
bound to events by name/type — see the plan's "Hot-state coupling" section, which
is precisely why Mode 3 must keep generated Go type names stable.
