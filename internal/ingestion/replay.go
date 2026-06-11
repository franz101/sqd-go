package ingestion

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/parser"
)

// blockEntry stores all data needed to replay a single block through the full pipeline.
// Events and logs are cloned on write so the ring buffer owns its data.
type blockEntry struct {
	number            uint64
	hash              string
	timestamp         time.Time
	events            []parser.DecodedEvent
	logs              []CustomLog
	blockRow          database.BlockRow
	typedEvents       map[string][]parser.DecodedEvent
	finalized         *client.BlockRef
	isLastInBatch     bool
	rangeLabel        string
	requestStartBlock uint64
	raw               []byte

	// Producer-parse handoff (FastBatchParseProcessor): proto is the parsed
	// columnar block (slot of the processor's preallocated ring; valid until
	// the ring recycles it, which the producer backpressure keeps strictly
	// behind the consumer). batchFlush/batchEvents ride on the batch's last
	// entry: the flush inserts the batch's captured event rows.
	proto       any
	batchFlush  func(context.Context) error
	batchEvents uint64
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
	index    map[uint64]int
	// writePos is the next slot to write into (monotonically increasing modulo capacity).
	writePos int
	// count is the number of valid entries currently in the buffer.
	count int

	mu       sync.Mutex
	notifyCh chan struct{}
	// latestBlock is the highest block number written (updated atomically).
	// GetBlock uses it as a lock-free fast-reject: if the requested block
	// exceeds this value it can't be in the buffer yet and the mutex is skipped.
	latestBlock atomic.Uint64

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
		index:    make(map[uint64]int, capacity),
		notifyCh: make(chan struct{}, 1),
	}
}

// Write stores a block and its events in the ring buffer.
// The caller transfers ownership of events/logs/typedEvents/raw — they must not
// be modified after this call. The producer already builds fresh slices per
// block (strings.Clone at parse time, fresh append-grown slices), so a deep
// copy is unnecessary and was the second-largest allocation source in backfill.
func (rb *ReplayBuffer) Write(chainID uint64, blockNumber uint64, blockHash string, blockTimestamp time.Time, events []parser.DecodedEvent, logs []CustomLog, typedEvents map[string][]parser.DecodedEvent, finalized *client.BlockRef, isLastInBatch bool, rangeLabel string, requestStartBlock uint64, raw []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	idx := rb.writePos % rb.capacity
	if rb.count == rb.capacity {
		delete(rb.index, rb.slots[idx].number)
	}

	rb.slots[idx] = blockEntry{
		number:            blockNumber,
		hash:              blockHash,
		timestamp:         blockTimestamp,
		events:            events,
		logs:              logs,
		typedEvents:       typedEvents,
		finalized:         finalized,
		isLastInBatch:     isLastInBatch,
		rangeLabel:        rangeLabel,
		requestStartBlock: requestStartBlock,
		blockRow: database.BlockRow{
			ChainID:        chainID,
			BlockNumber:    blockNumber,
			BlockTimestamp: blockTimestamp,
			BlockHash:      blockHash,
		},
		raw: raw,
	}
	rb.index[blockNumber] = idx
	rb.latestBlock.Store(blockNumber)

	rb.writePos++
	if rb.count < rb.capacity {
		rb.count++
	}

	select {
	case rb.notifyCh <- struct{}{}:
	default:
	}
}

// WriteParsed stores a producer-parsed block. Same slot lifecycle as Write;
// the mutex pair (this lock, GetBlock's lock) is also the happens-before edge
// that publishes the parsed ring slot's contents to the consumer.
func (rb *ReplayBuffer) WriteParsed(chainID uint64, b BatchParsedBlock, finalized *client.BlockRef, isLastInBatch bool, rangeLabel string, requestStartBlock uint64, batchFlush func(context.Context) error, batchEvents uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	idx := rb.writePos % rb.capacity
	if rb.count == rb.capacity {
		delete(rb.index, rb.slots[idx].number)
	}

	rb.slots[idx] = blockEntry{
		number:            b.Number,
		hash:              b.Hash,
		timestamp:         b.Timestamp,
		finalized:         finalized,
		isLastInBatch:     isLastInBatch,
		rangeLabel:        rangeLabel,
		requestStartBlock: requestStartBlock,
		blockRow: database.BlockRow{
			ChainID:        chainID,
			BlockNumber:    b.Number,
			BlockTimestamp: b.Timestamp,
			BlockHash:      b.Hash,
		},
		raw:         b.RawLine,
		proto:       b.Block,
		batchFlush:  batchFlush,
		batchEvents: batchEvents,
	}
	rb.index[b.Number] = idx
	rb.latestBlock.Store(b.Number)

	rb.writePos++
	if rb.count < rb.capacity {
		rb.count++
	}

	select {
	case rb.notifyCh <- struct{}{}:
	default:
	}
}

// Seek sets the replay start point. ReadFrom will return blocks > blockNumber.
func (rb *ReplayBuffer) Seek(blockNumber uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
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
	rb.mu.Lock()
	if !rb.seekSet {
		rb.mu.Unlock()
		return 0, fmt.Errorf("replay buffer: seek not set")
	}
	if rb.readDone {
		rb.mu.Unlock()
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
		rb.mu.Unlock()
		return 0, nil
	}

	// Copy the entries we need to replay so we can release the lock
	// and run the callbacks concurrently/without blocking writes!
	entries := make([]blockEntry, scanned)
	for i := 0; i < scanned; i++ {
		pos := (startSlot + i) % rb.capacity
		entries[i] = rb.slots[pos]
	}
	rb.readDone = true
	rb.mu.Unlock()

	// Replay from copied entries
	replayed := 0
	for _, entry := range entries {
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
	return replayed, nil
}

// GetBlock returns a copy of the block entry for blockNumber if it exists in the buffer.
// It returns ok=false if the block is not present.
func (rb *ReplayBuffer) GetBlock(blockNumber uint64) (blockEntry, bool) {
	if blockNumber > rb.latestBlock.Load() {
		return blockEntry{}, false
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.count == 0 {
		return blockEntry{}, false
	}

	if pos, ok := rb.index[blockNumber]; ok && pos >= 0 && pos < rb.capacity && rb.slots[pos].number == blockNumber {
		return rb.slots[pos], true
	}
	return blockEntry{}, false
}

// WaitBlock blocks until the requested blockNumber is present in the buffer, or the context is cancelled.
func (rb *ReplayBuffer) WaitBlock(ctx context.Context, blockNumber uint64) (blockEntry, error) {
	for {
		if entry, ok := rb.GetBlock(blockNumber); ok {
			return entry, nil
		}

		rb.mu.Lock()
		ch := rb.notifyCh
		rb.mu.Unlock()

		select {
		case <-ctx.Done():
			return blockEntry{}, ctx.Err()
		case <-ch:
			// A write occurred, retry GetBlock.
		}
	}
}

// HasBlocks returns true if the buffer contains blocks > the given number.
func (rb *ReplayBuffer) HasBlocks(blockNumber uint64) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.count == 0 {
		return false
	}
	newestEntry := rb.slots[(rb.writePos-1+rb.capacity)%rb.capacity]
	return newestEntry.number > blockNumber
}

// PruneBefore removes all entries with block numbers <= finalizedBlock.
// This keeps the buffer from growing unbounded and is safe because finalized
// blocks will never need replay (forks cannot go beyond finalized blocks).
func (rb *ReplayBuffer) PruneBefore(finalizedBlock uint64) int {
	rb.mu.Lock()
	defer rb.mu.Unlock()

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
		delete(rb.index, entry.number)
		rb.slots[oldestPos] = blockEntry{}
		oldestPos = (oldestPos + 1) % rb.capacity
		rb.count--
		pruned++
	}
	return pruned
}

// PruneAfter removes all entries with block numbers > blockNumber.
// This is called during fork recovery to invalidate any optimistic/pre-fork blocks.
func (rb *ReplayBuffer) PruneAfter(blockNumber uint64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.count == 0 {
		return
	}

	newCount := 0
	oldestPos := (rb.writePos - rb.count + rb.capacity) % rb.capacity
	if rb.count == 1 {
		oldestPos = (rb.writePos - 1 + rb.capacity) % rb.capacity
	}

	for i := 0; i < rb.count; i++ {
		pos := (oldestPos + i) % rb.capacity
		if rb.slots[pos].number <= blockNumber {
			newCount++
		} else {
			// Clear slot to help GC
			rb.slots[pos] = blockEntry{}
		}
	}
	rb.count = newCount
	rb.writePos = (oldestPos + newCount) % rb.capacity
	rb.rebuildIndexLocked()
	if rb.count > 0 {
		newestPos := (rb.writePos - 1 + rb.capacity) % rb.capacity
		rb.latestBlock.Store(rb.slots[newestPos].number)
	} else {
		rb.latestBlock.Store(0)
	}
}

// Len returns the number of blocks currently in the buffer.
func (rb *ReplayBuffer) Len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.count
}

func (rb *ReplayBuffer) rebuildIndexLocked() {
	clear(rb.index)
	if rb.count == 0 {
		return
	}
	oldestPos := (rb.writePos - rb.count + rb.capacity) % rb.capacity
	if rb.count == 1 {
		oldestPos = (rb.writePos - 1 + rb.capacity) % rb.capacity
	}
	for i := 0; i < rb.count; i++ {
		pos := (oldestPos + i) % rb.capacity
		entry := rb.slots[pos]
		rb.index[entry.number] = pos
	}
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
