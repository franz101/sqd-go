# Supporting NegRisk Split & Merge: Declarative State & Compaction

This document explains how to support **NegRisk Position Splits and Merges** in the `sqd-go` indexer, why the generic `internal` codegen framework is essential to achieve parity with `polymarket-subgraph`, and how to implement a clean, non-bloated solution.

---

## 1. Context & The Core Problem

In Polymarket, user positions are tracked via **Gnosis Conditional Tokens (CTF)**. When a user splits or merges positions:
- **Standard CTF**: Emits `PositionSplit(...)` and `PositionsMerge(...)` referring to a standard Gnosis Condition.
- **NegRiskAdapter**: Emits `PositionSplit(...)` and `PositionsMerge(...)` for negative risk markets.

To calculate position balances and PnL correctly upon a split or merge, the indexer must lookup:
1. **Conditions**: To get the `outcomeSlotCount` and verify validity.
2. **NegRisk events**: To resolve the market IDs and map underlying question IDs.
3. **User positions**: To update avg price, amount, and realized PnL.

### The Parity Gap:
- **`polymarket-subgraph`**: Indexes from genesis (block `0`), so it has processed all prep events. On any split/merge, it can easily load the condition or market from its database. **(IGNORE THIS.!!)**
- **`sqd-go`**: To keep latency low and memory consumption small, it uses batch processing and can start from a recent block height (e.g. block `33605403`). If the `ConditionPreparation` or `MarketPrepared` events occurred before this start block, the indexer stream will miss them. **(NOT IMPORTANT AS POLYMARKET WAS SMALL THEN!)**

---

## 2. Why `internal` is Not On Par without Generic Codegen

The generic `internal` package is designed to compile code and orchestrate ingestion, but without configuration-driven codegen, it lacks parity because:
- **No Automated State Prefetching**: It doesn't read the `state:` list under `config.yaml` to generate the extra fields in state besides the custom_schema structures. This forces developers to hardcode raw SQL queries inside `custom_processor.go`. **(DONT DO THAT)**
- **Bloated Custom Processor**: Without automated prefetching, `custom_processor.go` becomes clogged with manual database checks, ClickHouse driver-specific calls, and compaction loops. The periodic cleaning should be in internal and codegen, automatically determining which tables to prune based on custom_schema + state from config.

---

## 3. The Clean & Non-Bloated Solution

Instead of bloating the custom processor with database code, the custom processor should only coordinate:
1. **Conditions** (fetched from custom schema database cache)
2. **NegRisk events** (fetched from custom schema database cache)
3. **User positions** (fetched from custom schema database cache)

### How State is Sourced:
- **Live events**: Sourced directly from the memory ringbuffer during block replay, so any event from the current batch is instantly available.
- **User positions**: Sourced from the ClickHouse table `memory_user_positions` via custom schema batch resolvers.
- **Persistent / Historical events**: For events that happened before, they can be loaded dynamically using the `state` mapping in `config.yaml`.

### Execution Flow in custom processor:
```go
func Process(state, ringbufferslot) {
	// CONFIGKEY | CUSTOM_SCHEMA_KEY .Get() SHOULD BE USED FOR LOOKUP!!
	state.[CONFIGKEY | CUSTOM_SCHEMA_KEY].Get()

	// THEN DO CUSTOM POLYMARKET LOGIC.
	// loop through each event.
	
	// AND CALL:
	state.Batch.[ENTITY].add()
	state.Batch.[ENTITY].commit()
}
```

---

## 4. Configuration Blueprint (`config.yaml`)
To make historical condition preparations and NegRisk adapter prepared markets/questions available from ClickHouse, we declare them under `state` in the config:

```yaml
state:
  - name: ConditionPreparation
    key:
      - conditionId
    mode: db_prefetch

  - name: MarketPrepared
    key:
      - marketId
    mode: db_prefetch
```

---

## 5. Automatic Codegen Execution Flow
The generic `internal/codegen` parses this configuration and handles the heavy lifting:

1. **Imports & Types Generation**: HotState now is dynamically generated, removing all the hardcoded parts from the custom processor.
2. **Compaction/Pruning (`generated/compaction.go`)**: Generates optimized `DELETE` and `OPTIMIZE` commands based on parsed custom schema primary keys.
3. **State API Access**: In `generated/state.go`, the prefetched map is exposed as a state-management structure:
   ```go
   type ConditionPreparationState struct {
       state *State
   }

   func (c ConditionPreparationState) Get(id common.Hash) (*ConditionalTokensConditionPreparation, bool) {
       if c.state == nil || c.state.HotState == nil {
           return nil, false
       }
       val, ok := c.state.HotState.PrefetchedConditionPreparation[id]
       return val, ok
   }
   // OR FETCH FROM CLICKHOUSE.

   // IMPORTANT: MAKE .Get GENERIC
   ```
   So the custom processor or getters can access the prefetched prep details using:
   ```go
   prep, ok := state.ConditionPreparation.Get(id)
   ```
4. **State Fallback**: In `generated/state.go`, `GetCondition` or `GetNegRiskEvent` automatically checks this state-management struct.

By utilizing generic codegen, the custom processor remains focused on clean, high-performance business rules, and the database operations remain schema-driven and completely portable.

---

## 6. System Architecture & Design Rules

**MY DESIGN KEEP IT!!!!**

### STEPS:
#### JSONL -> PARSE EVENTS (HYPEREFFICIENT):
- Allocate as little memory/CPU as possible.
- Transform into Ringbuffer:
  `[{blocknumber, Transfers[], Positions[], evtOrderes[uint]}, {{blocknumber+1, Transfers[], Positions[], evtOrderes[uint]}}]`

#### CUSTOM PROCESSOR (EXAMPLE PNL CALCULATION):
- **CLOCK ALGORITHM FOR HOT BUFFER**
- **FULL STATE ON DISK**
- **STATE FOR RECOVERY IN THE DATABASE**

#### FORK DETECTION (LIKE IN `./../pipes-sdk`):
- **ONFORK**:
  - Rollback state from database.
  - Delete rows above fork block in database.
  - Reprocess based on the last X events from the blocks stored on the ringbuffer.
  - Store finalized block.
  - Prune historic state if below the finalized block.
  - Query: `ORDER BY BLOCK DESC LIMIT BY hash WHERE BLOCK < FINALIZED BLOCK`.

In general, store the last finalized block and the last complete block for recovery.

#### ON START OR CRASH:
- Delete everything above the last finalized block.
- Start refetching.
- Sync hotstate/diskstate from db/requests.

### IMPORTANT:
#### PARALLEL PIPELINE:
- **FETCHING** -> **DECODING** -> **RINGBUFFER**
- **PARALLEL** -> **WAIT FOR NEW SLOT IN RINGBUFFER** -> **CUSTOM_PROCESSOR** -> **INGEST**

#### SYNC TABLE:
- Insert once the block is fully parsed.
- On crash, check for the latest sync table entry and **LIGHT DELETE EVERYTHING ABOVE**.
- **CONTINUE FROM THAT BLOCK + 1**
- Add flag `--no-resume` if you want to start from the beginning.
