# Fields that are always present on every event

Every event that sqd-go decodes carries two kinds of data:

1. **ABI-specific args** — the fields declared in the event signature in your
   `config.yaml` (`from`, `to`, `value`, `conditionId`, …). These differ per
   event.
2. **Common metadata** — the chain/block/transaction context that sqd-go
   attaches to *every* event regardless of its ABI. This is what this document
   covers.

There is one important subtlety to internalize up front: **the set of metadata
fields on the Go struct is not the same as the set of metadata columns that get
persisted to ClickHouse.** The Go struct always carries the full metadata; the
database only stores four of those fields by default, and the rest are opt-in.

---

## A) The Go side: `EventMeta`

Every generated event struct **embeds `EventMeta` as its first field** and
**appends `Tombstone bool` as its last field**. So a generated struct looks
like this (from `examples/polymarket/generated/events.go`):

```go
type ConditionalTokensConditionPreparation struct {
	EventMeta                       // <-- always first
	ConditionID      common.Hash    `json:"conditionId"` // indexed
	Oracle           common.Address `json:"oracle"`      // indexed
	QuestionID       common.Hash    `json:"questionId"`  // indexed
	OutcomeSlotCount uint256.Int    `json:"outcomeSlotCount"`
	Tombstone        bool           `json:"-"`           // <-- always last
}
```

`EventMeta` itself is defined in
`internal/template/templates/code/events.go.tmpl` and is generated verbatim into
each project's `generated/events.go`:

```go
type EventMeta struct {
	BlockNumber      uint64         `json:"block_number"`
	BlockTimestamp   time.Time      `json:"block_timestamp"`
	BlockHash        common.Hash    `json:"block_hash"`
	ContractAddress  common.Address `json:"contract_address"`
	TransactionHash  common.Hash    `json:"transaction_hash"`
	TransactionIndex uint64         `json:"transaction_index"`
	LogIndex         uint64         `json:"log_index"`
}
```

Every event also satisfies the `Event` interface via a generated `Meta()`
method, so you can pull the metadata off any decoded event:

```go
func (e ConditionalTokensConditionPreparation) Meta() EventMeta { return e.EventMeta }
```

### Go metadata fields

| Field              | Go type          | JSON tag             | Always present?                          |
| ------------------ | ---------------- | -------------------- | ---------------------------------------- |
| `BlockNumber`      | `uint64`         | `block_number`       | Yes                                      |
| `BlockTimestamp`   | `time.Time`      | `block_timestamp`    | Yes                                      |
| `BlockHash`        | `common.Hash`    | `block_hash`         | Yes                                      |
| `ContractAddress`  | `common.Address` | `contract_address`   | Yes                                      |
| `TransactionHash`  | `common.Hash`    | `transaction_hash`   | Yes                                      |
| `TransactionIndex` | `uint64`         | `transaction_index`  | Yes                                      |
| `LogIndex`         | `uint64`         | `log_index`          | Yes                                      |
| `ChainID`          | `uint64`         | `chain_id`           | **Conditional** — only when configured   |

`ChainID` is the **only** config-conditional Go field. It is emitted into
`EventMeta` only when `chain_id` is listed under `include_metadata` in your
config (see section B). Neither shipped example
(`examples/polymarket`, `examples/uniswap`) enables it, so neither generated
`EventMeta` carries a `ChainID` field. When present, it is inserted as the first
field of `EventMeta`.

---

## B) The ClickHouse side: persisted columns

This is where the Go struct and the database diverge. **By default, only four of
the metadata fields are written to ClickHouse:**

| Column              | ClickHouse type        |
| ------------------- | ---------------------- |
| `block_number`      | `UInt64`               |
| `block_timestamp`   | `DateTime64(3, 'UTC')` |
| `transaction_index` | `UInt64`               |
| `log_index`         | `UInt64`               |

The other metadata fields — `block_hash`, `contract_address`,
`transaction_hash`, and `chain_id` — are **opt-in**. They live on the Go struct
unconditionally but are *not* added as columns unless you ask for them. The
gate is `Config.MetadataIncluded(field)` in `internal/config/config.go`, which
defaults to `false` for every field when `include_metadata` is absent:

```go
func (cfg *Config) MetadataIncluded(field string) bool {
	if cfg == nil {
		return false
	}
	for _, f := range cfg.IncludeMetadata {
		if strings.EqualFold(f, field) ||
			strings.EqualFold(strings.ReplaceAll(f, "_", ""), strings.ReplaceAll(field, "_", "")) {
			return true
		}
	}
	return false
}
```

Both shipped examples leave these off. The practical consequence:

> A SQL query that selects `block_hash`, `contract_address`, `transaction_hash`,
> or `chain_id` will fail with an **`unknown column`** error unless you opt that
> field in via `include_metadata` and re-run codegen.

### Opting in with `include_metadata`

Add the fields you want at the top level of your `config.yaml`, then re-run
`go run . codegen <project>`:

```yaml
name: my_indexer
include_metadata:
  - chain_id
  - block_hash
  - contract_address
  - transaction_hash
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: MyContract
        address: "0x..."
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
```

After regenerating, those fields become real ClickHouse columns and the
`chain_id` field appears on `EventMeta`.

---

## C) Details worth knowing

### Timestamp source and precision

`block_timestamp` comes from the SQD gateway block header's `timestamp` field,
which is **UNIX seconds**. During ingestion it is converted with
`time.Unix(sec, 0).UTC()` (`internal/ingestion/ingestion.go`) and stored in a
`DateTime64(3, 'UTC')` column. That column has millisecond resolution, so
sub-second precision is *structurally* available — but because the source is
whole seconds, **the millisecond portion is always zero**.

### Sort key / primary key

Each per-event table's `ORDER BY` (and primary key) is built as:

```text
(<first 0–2 indexed event args>, block_number, transaction_index, log_index)
```

That is: up to the first two indexed arguments of the event, followed by
`block_number`, `transaction_index`, and `log_index`. Note that
**`block_timestamp` is NOT part of the sort key** — filtering on a time range
does not benefit from the primary index; filter on `block_number` instead when
you can.

### `topic0` is not stored

`topic0` (the keccak hash of the canonical event signature) is used to **route**
a log to the correct event/table during ingestion, but it is **not stored** as a
column in the per-event table. You don't need it once the row has landed in its
typed table.

### Knowing which contract emitted a row

Because there is **no `contract_address` column by default**, a row in a
multi-contract event table does not tell you which address emitted it. To
recover that, either:

- enable `include_metadata: [contract_address]`, or
- rely on single-address tables (where every row is, by construction, from the
  one configured address).

---

## D) A worked query using the four default columns

This works on any freshly generated project with no `include_metadata` config —
it only touches columns that are always persisted. Replace the table name with
one of your generated per-event tables.

```sql
-- Latest 20 ConditionPreparation events, newest first.
-- Uses only the four default-persisted metadata columns.
SELECT
    block_number,
    block_timestamp,
    transaction_index,
    log_index
FROM polymarket.conditional_tokens_condition_preparation
ORDER BY
    block_number      DESC,
    transaction_index DESC,
    log_index         DESC
LIMIT 20;
```

A simple time-bucketed count, again using only default columns:

```sql
SELECT
    toStartOfDay(block_timestamp) AS day,
    count()                       AS events
FROM polymarket.conditional_tokens_condition_preparation
WHERE block_number BETWEEN 30000000 AND 35000000  -- prefer block_number; it's in the sort key
GROUP BY day
ORDER BY day;
```

If you instead wrote `SELECT contract_address, ...` against the same default
project, ClickHouse would reject it with an `unknown column 'contract_address'`
error — that field only exists once you opt it in via `include_metadata`.
