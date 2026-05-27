package ingestion

import (
	"context"
	"fmt"
	"time"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/parser"
)

// blockEntry stores all data needed to replay a single block through the full pipeline.
// Events and logs are cloned on write so the ring buffer owns its data.
type blockEntry struct {
	number    uint64
	hash      string
	timestamp time.Time
	events    []parser.DecodedEvent
	logs      []CustomLog
	blockRow  database.BlockRow
}

// ReplayBuffer is a circular buffer of recent blocks that enables fork recovery
// without re-fetching from the network. On a fork, events are replayed from
// the buffer through ClickHouse inserts and the custom processor.
//
// Capacity is fixed at creation time; older entries are silently overwritten.
// A fork deeper than the buffer capacity falls back to network re-fetch.
type ReplayBuffer struct {
	slots    []blockEntry
	capacity int
	// writePos is the next slot to write into (monotonically increasing modulo capacity).
	writePos int
	// count is the number of valid entries currently in the buffer.
	count int

	// Seek point for replay — when set, ReadFrom returns the first block > seekBlock.
	seekBlock uint64
	seekSet   bool
	// readPos tracks the current read position during replay.
	readPos  int
	readDone bool
}

// NewReplayBuffer creates a replay buffer with the given capacity.
// Capacity must be at least 1. A value of 8192 holds ~8K blocks.
func NewReplayBuffer(capacity int) *ReplayBuffer {
	if capacity < 1 {
		capacity = 8192
	}
	return &ReplayBuffer{
		slots:    make([]blockEntry, capacity),
		capacity: capacity,
	}
}

// Write stores a block and its events in the ring buffer.
// Events and logs are cloned so the caller retains ownership of the originals.
func (rb *ReplayBuffer) Write(chainID uint64, blockNumber uint64, blockHash string, blockTimestamp time.Time, events []parser.DecodedEvent, logs []CustomLog) {
	idx := rb.writePos % rb.capacity

	// Clone events
	clonedEvents := make([]parser.DecodedEvent, len(events))
	for i, ev := range events {
		clonedEvents[i] = cloneDecodedEvent(ev)
	}

	// Clone logs
	clonedLogs := make([]CustomLog, len(logs))
	for i, lg := range logs {
		clonedLogs[i] = cloneCustomLog(lg)
	}

	rb.slots[idx] = blockEntry{
		number:    blockNumber,
		hash:      blockHash,
		timestamp: blockTimestamp,
		events:    clonedEvents,
		logs:      clonedLogs,
		blockRow: database.BlockRow{
			ChainID:        chainID,
			BlockNumber:    blockNumber,
			BlockTimestamp: blockTimestamp,
			BlockHash:      blockHash,
		},
	}

	rb.writePos++
	if rb.count < rb.capacity {
		rb.count++
	}
}

// Seek sets the replay start point. ReadFrom will return blocks > blockNumber.
func (rb *ReplayBuffer) Seek(blockNumber uint64) {
	rb.seekBlock = blockNumber
	rb.seekSet = true
	rb.readDone = false
	rb.readPos = 0
}

// ReadFrom replays blocks starting from the seek point. It calls the provided
// callbacks for each block in ascending order. Returns the number of blocks replayed.
//
// The inserter callback receives decoded events (for ClickHouse insertion).
// The processor callback receives custom logs (for custom processor).
func (rb *ReplayBuffer) ReadFrom(inserter func(events []parser.DecodedEvent, blockRow database.BlockRow) error, processor func(logs []CustomLog) error) (int, error) {
	if !rb.seekSet {
		return 0, fmt.Errorf("replay buffer: seek not set")
	}
	if rb.readDone {
		return 0, nil
	}

	// Find the starting slot. The buffer has up to `count` entries, stored
	// contiguously from (writePos - count) to (writePos - 1) modulo capacity.
	// We scan forward to find the first entry > seekBlock.
	startSlot := -1
	scanned := 0
	oldestPos := (rb.writePos - rb.count + rb.capacity) % rb.capacity
	if rb.count == 1 {
		oldestPos = (rb.writePos - 1 + rb.capacity) % rb.capacity
	}

	// Linear scan from the oldest entry
	for i := 0; i < rb.count; i++ {
		pos := (oldestPos + i) % rb.capacity
		entry := rb.slots[pos]
		if entry.number > rb.seekBlock {
			if startSlot < 0 {
				startSlot = pos
			}
			scanned++
		}
	}

	if startSlot < 0 || scanned == 0 {
		rb.readDone = true
		return 0, nil
	}

	// Replay from startSlot, reading scanned entries
	replayed := 0
	for i := 0; i < scanned; i++ {
		pos := (startSlot + i) % rb.capacity
		entry := rb.slots[pos]
		if entry.number <= rb.seekBlock {
			continue
		}
		if err := inserter(entry.events, entry.blockRow); err != nil {
			return replayed, fmt.Errorf("replay insert at block %d: %w", entry.number, err)
		}
		if processor != nil && len(entry.logs) > 0 {
			if err := processor(entry.logs); err != nil {
				return replayed, fmt.Errorf("replay processor at block %d: %w", entry.number, err)
			}
		}
		replayed++
	}
	rb.readDone = true
	return replayed, nil
}

// HasBlocks returns true if the buffer contains blocks > the given number.
func (rb *ReplayBuffer) HasBlocks(blockNumber uint64) bool {
	if rb.count == 0 {
		return false
	}
	oldestPos := (rb.writePos - rb.count + rb.capacity) % rb.capacity
	if rb.count == 1 {
		oldestPos = (rb.writePos - 1 + rb.capacity) % rb.capacity
	}
	newestEntry := rb.slots[(rb.writePos-1+rb.capacity)%rb.capacity]
	oldestEntry := rb.slots[oldestPos]
	_ = oldestEntry
	return newestEntry.number > blockNumber
}

// PruneBefore removes all entries with block numbers <= finalizedBlock.
// This keeps the buffer from growing unbounded and is safe because finalized
// blocks will never need replay (forks cannot go beyond finalized blocks).
func (rb *ReplayBuffer) PruneBefore(finalizedBlock uint64) int {
	if rb.count == 0 {
		return 0
	}
	oldestPos := (rb.writePos - rb.count + rb.capacity) % rb.capacity
	pruned := 0
	for rb.count > 0 {
		entry := rb.slots[oldestPos]
		if entry.number > finalizedBlock {
			break
		}
		// Clear the slot to help GC
		rb.slots[oldestPos] = blockEntry{}
		oldestPos = (oldestPos + 1) % rb.capacity
		rb.count--
		pruned++
	}
	return pruned
}

// Len returns the number of blocks currently in the buffer.
func (rb *ReplayBuffer) Len() int {
	return rb.count
}

// cloneDecodedEvent deep-copies a DecodedEvent so the buffer owns the data.
func cloneDecodedEvent(ev parser.DecodedEvent) parser.DecodedEvent {
	cloned := ev
	if ev.Params != nil {
		cloned.Params = make(map[string]any, len(ev.Params))
		for k, v := range ev.Params {
			cloned.Params[k] = v // Params values are immutable (uint256, string, []byte)
		}
	}
	// Strings are immutable in Go, no need to clone.
	return cloned
}

// cloneCustomLog deep-copies a CustomLog.
func cloneCustomLog(lg CustomLog) CustomLog {
	cloned := lg
	if len(lg.Topics) > 0 {
		cloned.Topics = make([]string, len(lg.Topics))
		copy(cloned.Topics, lg.Topics)
	}
	return cloned
}

// ReplayFromBuffer replays blocks from the buffer through the full ingestion
// pipeline: ClickHouse insert + custom processor. Called during fork recovery
// after ClickHouse has been rolled back to the safe block.
//
// Parameters:
//   - store: ClickHouse connection
//   - chainID: chain identifier for insertion
//   - customProcessor: the user's custom processor (may be nil)
//   - typedInserters: map of pre-allocated inserters for typed event tables
//   - safeBlock: the fork recovery safe block number
func ReplayFromBuffer(ctx context.Context, rb *ReplayBuffer, store *database.Store, chainID uint64, customProcessor CustomProcessor, typedTables typedTableIndex, baseInserter *database.Inserter, typedInserters map[string]*database.TypedInserter, safeBlock uint64) ([]client.BlockRef, error) {
	rb.Seek(safeBlock)

	var replayedRefs []client.BlockRef

	_, err := rb.ReadFrom(
		func(events []parser.DecodedEvent, blockRow database.BlockRow) error {
			if len(events) > 0 {
				if err := baseInserter.InsertLogs(ctx, events); err != nil {
					return fmt.Errorf("insert logs: %w", err)
				}
				// Insert typed events
				typedEvents := make(map[string][]parser.DecodedEvent)
				typedSpecs := make(map[string]database.TypedEventTable)
				for _, ev := range events {
					if table, ok := typedTables.lookup(ev.Address, ev.EventName); ok {
						typedEvents[table.Name] = append(typedEvents[table.Name], ev)
						typedSpecs[table.Name] = table
					}
				}
				for tableName, tevs := range typedEvents {
					inserter := typedInserters[tableName]
					if inserter == nil {
						return fmt.Errorf("missing TypedInserter for %s", tableName)
					}
					if err := inserter.Insert(ctx, tevs); err != nil {
						return fmt.Errorf("insert typed %s: %w", tableName, err)
					}
				}
				if err := baseInserter.InsertBlocks(ctx, []database.BlockRow{blockRow}); err != nil {
					return fmt.Errorf("insert block: %w", err)
				}
			}
			replayedRefs = append(replayedRefs, client.BlockRef{
				Number: blockRow.BlockNumber,
				Hash:   blockRow.BlockHash,
			})
			return nil
		},
		func(logs []CustomLog) error {
			if customProcessor != nil {
				return customProcessor(ctx, store, logs)
			}
			return nil
		},
	)

	if err != nil {
		return replayedRefs, err
	}

	return replayedRefs, nil
}
