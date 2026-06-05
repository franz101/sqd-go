# ClickHouse Target — onRollback

Store pipe output in ClickHouse

**Source:** https://docs.sqd.dev/en/sdk/pipes-sdk/evm/guides/basic-development/targets/clickhouse#onrollback

## Overview

Install the ClickHouse Node.js client:

```
npm install @clickhouse/client
```

At a glance, the pipeline looks like this:

```typescript
import { createClient } from '@clickhouse/client'
import { clickhouseTarget } from '@subsquid/pipes/targets/clickhouse'

await evmPortalSource({ ... }).pipeTo(
  clickhouseTarget({
    client: createClient({ url: 'http://localhost:8123' }),
    onData: async ({ store, data }) => {
      store.insert({ table: 'transfers', values: data.transfers.map(...), format: 'JSONEachRow' })
    },
    onRollback: async ({ store, safeCursor }) => {
      await store.removeAllRows({ tables: ['transfers'], where: `block_number > ${safeCursor.number}` })
    },
  }),
)
```

## Table Design

Use CollapsingMergeTree with a sign Int8 DEFAULT 1 column. This engine enables efficient fork rollbacks: to cancel rows, the target re-inserts them with sign = -1 and ClickHouse merges the pair during background processing.

```sql
CREATE TABLE IF NOT EXISTS transfers (
  block_number     UInt32  CODEC(DoubleDelta, ZSTD),
  transaction_hash String,
  log_index        UInt16,
  from_address     LowCardinality(FixedString(42)),
  to_address       LowCardinality(FixedString(42)),
  value            UInt256,
  sign             Int8 DEFAULT 1
) ENGINE = CollapsingMergeTree(sign)
  ORDER BY (block_number, transaction_hash, log_index);
```

**Design notes:**
- Apply DoubleDelta + ZSTD codecs to monotonically increasing columns such as block numbers and timestamps.
- Use LowCardinality for columns with low cardinality like addresses to reduce storage and speed up filtering.
- Store 256-bit integers as UInt256; serialize JavaScript BigInt values to strings before insertion.

Create the table in onStart using store.command():

```typescript
onStart: async ({ store }) => {
  await store.command({ query: `CREATE TABLE IF NOT EXISTS transfers ( ... )` })
}
```

## onData

Call store.insert() to queue an insert. The call is non-blocking — inserts fire concurrently and are fully flushed when the target closes:

```typescript
onData: async ({ store, data }) => {
  store.insert({
    table: 'transfers',
    values: data.transfers.map((t) => ({
      block_number:     t.block.number,
      transaction_hash: t.rawEvent.transactionHash,
      log_index:        t.rawEvent.logIndex,
      from_address:     t.event.from,
      to_address:       t.event.to,
      value:            t.event.value.toString(),
    })),
    format: 'JSONEachRow',
  })
}
```

## onRollback

Implement onRollback to handle blockchain forks. It is invoked in two situations:

- **type: 'offset_check'** — on startup, when a saved cursor is found, to discard writes from a previous crashed or partial run
- **type: 'blockchain_fork'** — when the stream detects a chain reorganisation

Use store.removeAllRows() to cancel rows past the safe point. For CollapsingMergeTree tables this re-inserts matching rows with sign = -1:

```typescript
onRollback: async ({ store, safeCursor }) => {
  await store.removeAllRows({
    tables: ['transfers'],
    where: `block_number > ${safeCursor.number}`,
  })
}
```

## Complete Example

```typescript
import { commonAbis, evmDecoder, evmPortalSource } from '@subsquid/pipes/evm'
import { clickhouseTarget } from '@subsquid/pipes/targets/clickhouse'
import { createClient } from '@clickhouse/client'

const client = createClient({ url: 'http://localhost:8123' })

await evmPortalSource({
  portal: 'https://portal.sqd.dev/datasets/ethereum-mainnet',
  outputs: evmDecoder({
    range: { from: 'latest' },
    events: { transfers: commonAbis.erc20.events.Transfer },
  }),
}).pipeTo(
  clickhouseTarget({
    client,
    onStart: async ({ store }) => {
      await store.command({
        query: `
          CREATE TABLE IF NOT EXISTS transfers (
            block_number     UInt32  CODEC(DoubleDelta, ZSTD),
            transaction_hash String,
            log_index        UInt16,
            from_address     LowCardinality(FixedString(42)),
            to_address       LowCardinality(FixedString(42)),
            value            UInt256,
            sign             Int8 DEFAULT 1
          ) ENGINE = CollapsingMergeTree(sign)
            ORDER BY (block_number, transaction_hash, log_index)
        `,
      })
    },
    onData: async ({ store, data }) => {
      store.insert({
        table: 'transfers',
        values: data.transfers.map((t) => ({
          block_number:     t.block.number,
          transaction_hash: t.rawEvent.transactionHash,
          log_index:        t.rawEvent.logIndex,
          from_address:     t.event.from,
          to_address:       t.event.to,
          value:            t.event.value.toString(),
        })),
        format: 'JSONEachRow',
      })
    },
    onRollback: async ({ store, safeCursor }) => {
      await store.removeAllRows({
        tables: ['transfers'],
        where: `block_number > ${safeCursor.number}`,
      })
    },
  }),
)
```

## Docker Setup

docker-compose.yml:

```yaml
services:
  clickhouse:
    image: clickhouse/clickhouse-server:latest
    ports:
      - "8123:8123"
      - "9000:9000"
    environment:
      CLICKHOUSE_DB: default
      CLICKHOUSE_USER: default
      CLICKHOUSE_PASSWORD: default
    volumes:
      - clickhouse-data:/var/lib/clickhouse

volumes:
  clickhouse-data:
```

```
docker compose up -d
```

See the clickhouseTarget reference for the full API.
