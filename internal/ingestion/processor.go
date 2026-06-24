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
	// The context must be honored so a slow load (e.g. a large ClickHouse
	// recover) can be aborted by SIGINT instead of blocking shutdown.
	LoadFromDatabase(ctx context.Context, blockNumber uint64) error
}

// FastJSONLProcessor is implemented by processors that support parsing and
// inserting events directly from raw JSONL bytes without reflection or maps.
type FastJSONLProcessor interface {
	Processor
	ProcessJSONL(ctx context.Context, store *database.Store, data []byte) (uint64, error)
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

// ProcessorProfileReporter optionally exposes processor-specific cumulative
// timings. The ingestion loop samples it at the normal stats interval and logs
// deltas, keeping domain instrumentation out of the generic hot path.
type ProcessorProfileReporter interface {
	ProcessorProfile() ProcessorProfile
}

type ProcessorProfile struct {
	ConditionResolveDuration time.Duration
	ConditionRoundTrips      int64
	FPMMResolveDuration      time.Duration
	FPMMRoundTrips           int64
}

func (p ProcessorProfile) Delta(previous ProcessorProfile) ProcessorProfile {
	return ProcessorProfile{
		ConditionResolveDuration: nonNegativeDurationDelta(p.ConditionResolveDuration, previous.ConditionResolveDuration),
		ConditionRoundTrips:      maxInt64(p.ConditionRoundTrips-previous.ConditionRoundTrips, 0),
		FPMMResolveDuration:      nonNegativeDurationDelta(p.FPMMResolveDuration, previous.FPMMResolveDuration),
		FPMMRoundTrips:           maxInt64(p.FPMMRoundTrips-previous.FPMMRoundTrips, 0),
	}
}

// PrefetchProcessor is optionally implemented by processors that support the
// two-pass batch prefetch (--prefetch): each block is dispatched once to collect
// its hot-state read-set, the misses are resolved in one ClickHouse round-trip per
// entity, then the block is dispatched again for real against the warm cache. This
// collapses the lazy path's one-SELECT-per-missing-key into one SELECT per entity
// per block — a large win in resume/cursor mode against a populated ClickHouse,
// where per-key cold misses otherwise dominate. Opt-in: off by default.
type PrefetchProcessor interface {
	EnablePrefetch(enabled bool)
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

// ColdNegativeFilterProcessor is optionally implemented by cold-tier processors
// that also support the V3 in-RAM negative-lookup Bloom filter: a hot+cold miss
// for a provably-new key skips even the Pebble probe (and, when authoritative,
// the ClickHouse SELECT), resolving from a single cache-line test in RAM. This is
// what distinguishes V3 from V2 on a from-genesis backfill, where nearly every
// hot miss is a brand-new key. Enabled by the ingestion layer when V3 is selected.
type ColdNegativeFilterProcessor interface {
	EnableColdNegativeFilter(bits uint64)
}

// ProcessorFunc adapts individual callback functions to the Processor interface.
// Any nil callbacks are treated as no-ops.
type ProcessorFunc struct {
	ProcessFn        CustomProcessor
	RestoreToBlockFn func(blockNumber uint64) (uint64, error)
	LoadFromDBFn     func(ctx context.Context, blockNumber uint64) error
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

func (p ProcessorFunc) LoadFromDatabase(ctx context.Context, blockNumber uint64) error {
	if p.LoadFromDBFn == nil {
		return nil
	}
	return p.LoadFromDBFn(ctx, blockNumber)
}
