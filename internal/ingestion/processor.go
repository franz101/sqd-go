package ingestion

import (
	"context"
	"time"

	"github.com/franz101/sqd-go/internal/database"
)

// Processor is the interface for custom event processing, state snapshots,
// and database-backed state recovery. Implementations handle domain-specific
// logic while the ingestion layer manages fetching, parsing, insertion, and
// fork recovery.
type Processor interface {
	// Process is called with a batch of custom logs from a fetch response.
	// It runs after ClickHouse insertion and may update derived state.
	Process(ctx context.Context, store *database.Store, logs []CustomLog) error

	// RestoreToBlock rolls back processor state to the given block number.
	// Called during fork recovery before replaying events from the ring buffer.
	// Returns the actual block number restored and an error.
	RestoreToBlock(blockNumber uint64) (uint64, error)

	// LoadFromDatabase restores processor state from persistent storage at the
	// given block height. Called on startup when a saved checkpoint exists.
	// A nil return is valid if loading is not supported or not configured.
	LoadFromDatabase(blockNumber uint64) error
}

// FastJSONLProcessor is implemented by processors that support parsing and
// inserting events directly from raw JSONL bytes without reflection or maps.
type FastJSONLProcessor interface {
	Processor
	ProcessJSONL(ctx context.Context, store *database.Store, data []byte) (uint64, error)
}

// FastJSONLInsertProcessor is an optional extension of FastJSONLProcessor: the
// single parse that runs the custom processor also captures the event-table
// rows into preallocated native columns, removing the producer-side generic
// ABI decode entirely. The returned flush inserts the captured rows on the
// store's dedicated insert connection; it may run concurrently with the NEXT
// ProcessJSONLWithInserts call (the processor double-buffers), but flushes
// must be invoked one at a time. A nil flush means there is nothing to insert.
type FastJSONLInsertProcessor interface {
	FastJSONLProcessor
	ProcessJSONLWithInserts(ctx context.Context, store *database.Store, data []byte) (uint64, func(context.Context) error, error)
}

// BatchParsedBlock carries one parsed block from the producer's parse stage to
// the consumer. Block is the generated columnar block (type-erased; nil when
// the line had no logs key). The replay buffer's mutex is the happens-before
// edge that publishes the producer's writes to the consumer, so no further
// synchronization is needed despite the shared preallocated ring slots.
type BatchParsedBlock struct {
	Number    uint64
	Hash      string
	Timestamp time.Time
	RawLine   []byte
	Block     any
}

// FastBatchParseProcessor moves the single parse onto the PRODUCER goroutine:
// ParseBatchForInserts parses a whole fetch response once — filling the
// preallocated block ring and a pooled insert batch — and streams one
// BatchParsedBlock per line (in order) through onParsed. The consumer then
// runs only ProcessParsedBlock (state math) per block, plus the returned
// batch flush, which must be invoked serially after all the batch's blocks.
//
// Concurrency contract: ParseBatchForInserts is called by exactly one
// goroutine at a time; ProcessParsedBlock by exactly one other; a parsed
// block is handed to ProcessParsedBlock only after an intervening
// happens-before edge (the replay buffer handoff). Taking a pooled insert
// batch blocks when all are in flight, so producer lead stays bounded.
type FastBatchParseProcessor interface {
	FastJSONLInsertProcessor
	// SupportsBatchParse reports whether the processor's mode allows the
	// producer-side parse (generated processors support it in proto mode).
	SupportsBatchParse() bool
	ParseBatchForInserts(store *database.Store, data []byte, endBlock uint64, onParsed func(BatchParsedBlock) error) (uint64, func(context.Context) error, error)
	ProcessParsedBlock(ctx context.Context, store *database.Store, block any) error
	// ReclaimParseBatches refills the insert-batch pool after fork recovery
	// discards replay-buffer entries whose batch flush was never invoked.
	// Only call when no parse and no flush is in flight.
	ReclaimParseBatches()
}

// BatchParsedBlockPrefetcher is optionally implemented by generated
// FastBatchParseProcessor implementations that can collect state keys across a
// whole producer-parsed page and resolve them in a small number of ClickHouse
// queries before per-block custom processing starts.
type BatchParsedBlockPrefetcher interface {
	PrefetchParsedBlocks(ctx context.Context, store *database.Store, blocks []any) error
}

// CommitHorizonReporter is optionally implemented by processors that durably
// commit derived state at intervals. When implemented, the ingestion checkpoint
// is gated so it never leads this horizon: a crash resumes from durable state
// and re-fetches the (cheap) gap rather than losing un-committed updates.
type CommitHorizonReporter interface {
	// CommittedBlock returns the highest block whose derived state is durable.
	CommittedBlock() uint64
}

// Flusher is optionally implemented by processors that can force a durable commit
// of all state processed up to a block. Called on clean completion/shutdown so
// the tail (blocks since the last periodic commit) is persisted and the
// checkpoint can advance to it. Returns the new committed horizon.
type Flusher interface {
	Flush(ctx context.Context, store *database.Store, blockNumber uint64) (uint64, error)
}

// SnapshotController is optionally implemented by processors that keep in-memory
// fork-recovery snapshots. The ingestion layer disables them during finalized
// backfill (no reorgs) to remove their GC/memory cost, and enables them in cursor
// mode where reorg recovery may need them.
type SnapshotController interface {
	SetSnapshotsEnabled(enabled bool)
}

// ColdCacheProcessor is optionally implemented by processors that keep a
// Pebble-backed cold tier under their hot caches. On a hot miss an evicted entry
// is served from local disk (~8µs) instead of a ClickHouse point-SELECT (~1.9ms);
// when authoritative (the cold tier was opened against an empty ClickHouse, i.e.
// a from-genesis backfill), a hot+cold miss is provably new and the ClickHouse
// lookup is skipped entirely. The ingestion layer only enables it in finalized
// backfill (not cursor mode, for reorg safety) and closes it on exit.
type ColdCacheProcessor interface {
	EnableColdCache(dir string, authoritative bool) error
	CloseColdCache() error
}

// ProcessorFunc adapts individual callback functions to the Processor interface.
// Any nil callbacks are treated as no-ops.
type ProcessorFunc struct {
	ProcessFn        CustomProcessor
	RestoreToBlockFn func(blockNumber uint64) (uint64, error)
	LoadFromDBFn     func(blockNumber uint64) error
}

func (p ProcessorFunc) Process(ctx context.Context, store *database.Store, logs []CustomLog) error {
	if p.ProcessFn == nil {
		return nil
	}
	return p.ProcessFn(ctx, store, logs)
}

func (p ProcessorFunc) RestoreToBlock(blockNumber uint64) (uint64, error) {
	if p.RestoreToBlockFn == nil {
		return blockNumber, nil
	}
	return p.RestoreToBlockFn(blockNumber)
}

func (p ProcessorFunc) LoadFromDatabase(blockNumber uint64) error {
	if p.LoadFromDBFn == nil {
		return nil
	}
	return p.LoadFromDBFn(blockNumber)
}
