package ingestion

import (
	"context"

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
