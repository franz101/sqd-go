# Fork Handling

Handle blockchain forks and rollbacks in real-time streams

**Source:** https://docs.sqd.dev/en/sdk/pipes-sdk/evm/guides/architecture-deep-dives/fork-handling

## Overview

When consuming a real-time stream near the chain head, the portal can detect that the client's view of the chain has diverged from the canonical chain — a situation known as a fork or reorg. The portal signals this with an HTTP 409 response containing a sample of blocks from the new canonical chain. Your code must find the highest block that both chains agree on, roll back any state written after that point, and replay from there.

**Fork handling is only needed for real-time streams (range.from: 'latest').** Historical streams consume already-finalized data and never produce forks. See Fork detection scope below.

The SDK provides two patterns for consuming a stream. Both use the same state-tracking logic; they differ in how the fork signal is delivered.

If your pipeline includes a stateful transformer, it must also implement a fork callback to roll back its own state in lockstep with the target.

### Via pipeTo / targets

The pipeTo(createTarget({write, fork})) pattern keeps fork handling completely separate from batch processing. The SDK catches the 409 internally and calls fork() with the portal's consensus block sample; write() never sees the interruption and continues iterating batches without restarting.

#### Step 1: Declare state

Two variables span the lifetime of the stream:

```
let recentUnfinalizedBlocks: BlockCursor[] = []
let finalizedHighWatermark: BlockCursor | undefined
```

recentUnfinalizedBlocks is the local history of unfinalized blocks used to find the common ancestor during a fork. finalizedHighWatermark tracks the highest finalized block ever seen — stored as a full BlockCursor (number and hash) so it can double as a rollback cursor when needed. Both must be declared outside pipeTo so fork() can access them.

#### Step 2: Collect rollback history

Inside write(), append each batch's unfinalized blocks to the local history:

```
ctx.stream.state.rollbackChain.forEach((bc) => {
  recentUnfinalizedBlocks.push(bc)
})
```

ctx.stream.state.rollbackChain contains only the blocks from this batch that are above the current finalized head — it is a per-batch delta, not a full snapshot. Always append to the end; never replace or reorder.

#### Step 3: Prune finalized blocks

After collecting history, prune blocks that are now finalized and cap the queue:

```
if (ctx.stream.head.finalized) {
  if (!finalizedHighWatermark || ctx.stream.head.finalized.number > finalizedHighWatermark.number) {
    finalizedHighWatermark = ctx.stream.head.finalized
  }
  recentUnfinalizedBlocks = recentUnfinalizedBlocks.filter(b => b.number >= finalizedHighWatermark!.number)
}
recentUnfinalizedBlocks = recentUnfinalizedBlocks.slice(recentUnfinalizedBlocks.length - 1000)
```

Portal instances behind a load balancer can report different finalized heads. Using the maximum seen so far (the high-water mark) prevents the pruning threshold from moving backwards when the stream reconnects to a lagging instance. See consideration 6 for details.

#### Step 4: Implement fork()

fork() receives previousBlocks — the portal's current-chain sample — and must return the last good block cursor, or null if recovery is impossible:

```
fork: async (newConsensusBlocks) => {
  const rollbackIndex = findRollbackIndex(recentUnfinalizedBlocks, newConsensusBlocks)
  if (rollbackIndex >= 0) {
    recentUnfinalizedBlocks.length = rollbackIndex + 1
    return recentUnfinalizedBlocks[rollbackIndex]
  }
  if (finalizedHighWatermark &&
      newConsensusBlocks.every(b => b.number < finalizedHighWatermark!.number)) {
    recentUnfinalizedBlocks = recentUnfinalizedBlocks.filter(b => b.number <= finalizedHighWatermark!.number)
    return finalizedHighWatermark
  }
  return null
}
```

Three cases:
1. A common ancestor is found in local history — truncate and return it
2. All previousBlocks fall below the finalized high-water mark, meaning the portal's sample doesn't reach local history — return the high-water mark cursor
3. No recovery possible — return null, which surfaces a ForkCursorMissingError

### Via async iteration (workaround)

Alternative pattern for consuming the stream directly.

## The Common-Ancestor Search

Both approaches use the same merge-sort scan. Given two ascending-sorted arrays of BlockCursor — local history and the portal's previousBlocks — findRollbackIndex returns the index in local history of the last entry that both chains agree on (same block number and hash):

```typescript
function findRollbackIndex(chainA: BlockCursor[], chainB: BlockCursor[]): number {
  let aIndex = 0, bIndex = 0, lastCommonIndex = -1
  while (aIndex < chainA.length && bIndex < chainB.length) {
    const a = chainA[aIndex], b = chainB[bIndex]
    if (a.number < b.number) { aIndex++; continue }
    if (a.number > b.number) { bIndex++; continue }
    if (a.hash !== b.hash) return lastCommonIndex   // chains diverged here
    lastCommonIndex = aIndex; aIndex++; bIndex++
  }
  return lastCommonIndex
}
```

The scan advances the pointer for the lower-numbered entry until both point to the same block number. A hash mismatch means the chains diverged at this number; lastCommonIndex holds the last agreement point. Returning -1 means no common ancestor was found in the sample.

## Edge Cases and Considerations

1. **Rollback history bootstrap** — On process start, recentUnfinalizedBlocks is empty. The stream begins from range.from. If state were persisted across restarts (e.g. in a database), restoring recentUnfinalizedBlocks and passing its last entry would resume from the correct position.

2. **The previousBlocks payload from the 409** — The portal returns a sample of blocks from the new canonical chain in the 409 response body.

3. **Rollback depth and history limits** — The queue is capped at 1000 blocks, sufficient for all networks SQD supports.

4. **State rollback atomicity** — The rollback must be atomic with data processing to maintain consistency.

5. **Cursor semantics** — Fork handling is transparent to write(): when the portal returns a 409 the SDK catches the ForkException inside the read() iterator, calls fork() to determine the rollback cursor, then resumes the stream from that cursor — write() keeps iterating batches without interruption.

6. **Load-balanced portals and a non-monotonic finalized head** — Portal instances can be load-balanced: a reconnected stream may land on a lagging instance whose X-Sqd-Finalized-Head-Number is lower than what was previously seen. Treat the finalized head as a monotonically increasing high-water mark so the pruning threshold never moves backwards. The full cursor (number + hash) is kept so it can serve as a fallback rollback point when a lagging instance's 409 sample doesn't overlap our history.

7. **Fork detection scope (real-time streams only)** — Historical streams consume already-finalized data and never produce forks.

8. **The rollbackChain field contract** — Not all data streams contain information on recent blocks. Always use ctx.stream.state.rollbackChain which contains cursor values for all unfinalized blocks of the batch.

9. **Algorithm correctness for common-ancestor search** — The merge-sort scan requires both arrays to be sorted ascending by block number.

10. **Concurrency and ordering invariants** — Fork handling must maintain ordering guarantees across the pipeline.
