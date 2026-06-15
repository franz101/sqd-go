package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/monitoring"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/franz101/sqd-go/internal/parser/abiunpack"
)

// Options configures the ingestion pipeline: ClickHouse connection, page
// sizing, restart behaviour, cold cache tier, and the custom processor.
type Options struct {
	ClickHouseHost     string
	ClickHousePort     int
	ClickHouseUser     string
	ClickHousePassword string
	ClickHouseDatabase string
	PageSize           uint64
	StartBlock         uint64
	BlockCount         uint64
	Restart            bool
	GeneratedSQLDir    string
	CursorMode         bool
	ForkMode           config.ForkMode
	CustomProcessor    CustomProcessor
	StateRestorer      func(blockNumber uint64) error // called before fork replay to roll back processor state
	StateLoader        func(ctx context.Context, blockNumber uint64) error // called on startup to load processor state from database
	Processor          Processor                      // unified processor interface (overrides individual callbacks if set)
	// ColdCache enables a Pebble-backed cold tier under the hot caches (finalized
	// backfill only). It removes the per-miss ClickHouse point-SELECT: an evicted
	// entry is served from local disk, and on a from-genesis run a hot+cold miss is
	// provably new so ClickHouse is skipped entirely. Default off.
	ColdCache    bool
	ColdCacheDir string // base directory for cold-tier files (default os.TempDir()/sqd-coldcache)
}

const (
	cursorPollInterval = 5 * time.Second
	statsInterval      = 10 * time.Second

	// Adaptive page sizing: when pageSize=0, grow page size based on performance
	// Target ~20k blocks/second processing rate
	targetBlocksPerSec  = 20000
	minAdaptivePageSize = 5000
	maxAdaptivePageSize = 100000
)

// CustomLog is a decoded EVM log passed to legacy CustomProcessor callbacks.
type CustomLog struct {
	ChainID          uint64
	BlockNumber      uint64
	BlockTimestamp   time.Time
	BlockHash        string
	ContractAddress  string
	TransactionHash  string
	TransactionIndex uint64
	LogIndex         uint64
	Topics           []string
	Data             string
}

// CustomProcessor is the legacy callback signature for processing decoded logs.
// New code should implement the Processor interface instead.
type CustomProcessor func(ctx context.Context, store *database.Store, logs []CustomLog) error

// Run is the top-level ingestion entry point. It connects to ClickHouse, applies
// generated schemas, and processes each configured chain sequentially. The
// context controls graceful shutdown; a cancelled context drains in-flight work
// and returns ctx.Err().
func Run(ctx context.Context, cfg *config.Config, opts Options) error {
	// Resolve effective processor: use Processor interface if set, otherwise fall back to callbacks
	proc := opts.Processor
	if proc == nil {
		var restoreFn func(uint64) (uint64, error)
		if opts.StateRestorer != nil {
			restoreFn = func(blockNumber uint64) (uint64, error) {
				if err := opts.StateRestorer(blockNumber); err != nil {
					return 0, err
				}
				return blockNumber, nil
			}
		}
		proc = ProcessorFunc{
			ProcessFn:        opts.CustomProcessor,
			RestoreToBlockFn: restoreFn,
			LoadFromDBFn:     opts.StateLoader,
		}
	}
	resetStore := opts.Restart
	if resetStore {
		if err := database.DropClickHouseDatabase(ctx, opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseUser, opts.ClickHousePassword, opts.ClickHouseDatabase); err != nil {
			return fmt.Errorf("drop clickhouse database: %w", err)
		}
	}
	store, err := database.NewClickHouse(ctx, opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseUser, opts.ClickHousePassword, opts.ClickHouseDatabase)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer store.Close()

	// Optional Grafana metrics writer (SQD_METRICS_CH). Off the hot path: it owns
	// its own connection and the ingestion loop only feeds it counters via Observe.
	monitoring.Start(ctx, monitoring.Config{
		Host:     opts.ClickHouseHost,
		Port:     opts.ClickHousePort,
		User:     opts.ClickHouseUser,
		Password: opts.ClickHousePassword,
	})
	defer monitoring.Stop()

	forkMode := opts.ForkMode
	if forkMode == "" {
		forkMode = cfg.ForkMode()
	}
	if !forkMode.Valid() {
		return fmt.Errorf("fork mode must be default")
	}

	if opts.GeneratedSQLDir != "" {
		if err := store.ApplySQLFileWithDatabase(ctx, filepath.Join(opts.GeneratedSQLDir, "schema.sql"), cfg.Name); err != nil {
			return fmt.Errorf("apply generated schema: %w", err)
		}
		customSchemaPath := filepath.Join(opts.GeneratedSQLDir, "custom_schema.sql")
		if _, err := os.Stat(customSchemaPath); err == nil {
			if err := store.ApplySQLFileWithDatabase(ctx, customSchemaPath, cfg.Name); err != nil {
				return fmt.Errorf("apply generated custom schema: %w", err)
			}
		}
		viewsPath := filepath.Join(opts.GeneratedSQLDir, "views.sql")
		if _, err := os.Stat(viewsPath); err == nil {
			if err := store.ApplySQLFileWithDatabase(ctx, viewsPath, cfg.Name); err != nil {
				return fmt.Errorf("apply generated views: %w", err)
			}
		}
	} else if err := store.EnsureTablesWithOptions(ctx, forkMode.UsesCollapsingMergeTree(), database.EnsureTablesOptions{
		StoreBlocks: cfg.ShouldStoreBlocks(),
		StoreLogs:   cfg.ShouldStoreRawLogs(),
	}); err != nil {
		return fmt.Errorf("ensure tables: %w", err)
	}
	log.Printf("ClickHouse connected: %s:%d/%s (fork=%s)", opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseDatabase, forkMode)

	coldDir := opts.ColdCacheDir
	if opts.ColdCache && coldDir == "" {
		coldDir = filepath.Join(os.TempDir(), "sqd-coldcache", cfg.Name)
	}
	for _, chain := range cfg.Chains {
		if err := processChain(ctx, store, cfg, &chain, opts.PageSize, opts.StartBlock, opts.BlockCount, opts.CursorMode, forkMode, opts.Restart, proc, opts.ColdCache, coldDir); err != nil {
			log.Printf("chain %d error: %v", chain.ID, err)
		}
	}
	log.Println("Done.")
	return nil
}

func processChain(ctx context.Context, store *database.Store, cfg *config.Config, chain *config.Chain, pageSize, flagStartBlock, blockCountLimit uint64, cursorMode bool, forkMode config.ForkMode, restart bool, proc Processor, coldCache bool, coldDir string) error {
	if len(chain.Contracts) == 0 {
		return fmt.Errorf("no contracts defined for chain %d", chain.ID)
	}
	log.Printf("Chain %d: building event decoders from %d contracts...", chain.ID, len(chain.Contracts))
	decoders, filters, err := parser.BuildEventDecoder(chain.Contracts)
	if err != nil {
		return fmt.Errorf("build decoders: %w", err)
	}
	typedTables, err := buildTypedTableIndex(chain)
	if err != nil {
		return fmt.Errorf("build typed tables: %w", err)
	}
	log.Printf("Chain %d: %d event types, %d filter(s)", chain.ID, len(decoders), len(filters))
	storeBlocks := cfg.ShouldStoreBlocks()
	currentBlock := chain.StartBlock
	if flagStartBlock > 0 {
		currentBlock = flagStartBlock
	}
	state := NewForkTracker(forkMode)
	if cursorMode {
		if restart {
			log.Printf("Chain %d: restart mode; starting from configured block %d", chain.ID, currentBlock)
		} else {
			saved, hasSaved, err := store.LastSyncState(ctx, chain.ID)
			if err != nil {
				return fmt.Errorf("read sync state: %w", err)
			}
			if recovery, ok := selectRecoveryBase(saved, hasSaved); ok {
				if currentBlock > recovery.Number+1 {
					log.Printf("Chain %d: gap detected between recovery checkpoint %d and requested start block %d; starting a new fork tracker segment", chain.ID, recovery.Number, currentBlock)
					state.Init(nil, nil, nil)
				} else {
					state.Init(recovery.TrackerCurrent, recovery.TrackerFinalized, recovery.TrackerRollbackChain)
				}
				if err := rollbackAfterBlock(ctx, store, forkMode, chain.ID, recovery.Number); err != nil {
					return fmt.Errorf("rollback after recovery checkpoint %d: %w", recovery.Number, err)
				}
				if err := store.SaveSyncState(ctx, chain.ID, recoverySyncState(recovery)); err != nil {
					return fmt.Errorf("save recovery checkpoint %d: %w", recovery.Number, err)
				}
				if recovery.Number >= currentBlock {
					currentBlock = recovery.Number + 1
				}
				if recovery.FromFinalized {
					log.Printf("Chain %d: recovered from finalized checkpoint %d", chain.ID, recovery.Number)
				} else {
					log.Printf("Chain %d: recovered from current checkpoint %d", chain.ID, recovery.Number)
				}
				// Load processor state from database at the recovery checkpoint
				log.Printf("[LOAD STATE] Loading processor state from ClickHouse database at block %d...", recovery.Number)
				if err := proc.LoadFromDatabase(ctx, recovery.Number); err != nil {
					log.Printf("[LOAD STATE] State load from ClickHouse at block %d failed: %v (will rebuild from events)", recovery.Number, err)
				} else {
					log.Printf("[LOAD STATE] Processor state loaded successfully from ClickHouse database at block %d", recovery.Number)
				}
			}
		}
	} else if !restart {
		last, hasLast, err := store.LastBlock(ctx, chain.ID)
		if err != nil {
			return fmt.Errorf("read last block: %w", err)
		}
		if hasLast {
			if err := rollbackAfterBlock(ctx, store, forkMode, chain.ID, last); err != nil {
				return fmt.Errorf("truncate after checkpoint %d: %w", last, err)
			}
			if last >= currentBlock {
				currentBlock = last + 1
			}
		} else if currentBlock > 0 {
			// No durable checkpoint, but a crash may have left committed-ahead hot
			// state from a partially-processed run (hot state commits at a cadence,
			// the checkpoint is written after). Truncate everything >= the start block
			// so re-processing from currentBlock is idempotent and never double-applies.
			if err := rollbackAfterBlock(ctx, store, forkMode, chain.ID, currentBlock-1); err != nil {
				return fmt.Errorf("truncate orphaned state before start %d: %w", currentBlock, err)
			}
		}
	}
	effectiveEndBlock := chain.EndBlock
	if blockCountLimit > 0 {
		end := currentBlock + blockCountLimit - 1
		effectiveEndBlock = minEndBlock(effectiveEndBlock, end)
	}
	// pageSize == 0 means dynamic paging (unrestricted block ranges)
	if cursorMode {
		if effectiveEndBlock != nil {
			log.Printf("Chain %d: starting from block %d (cursor mode, local stop at %d)", chain.ID, currentBlock, *effectiveEndBlock)
		} else {
			log.Printf("Chain %d: starting from block %d (cursor mode)", chain.ID, currentBlock)
		}
	} else {
		log.Printf("Chain %d: starting from block %d", chain.ID, currentBlock)
	}

	sqd := client.New(chainEndpoint(chain.ID, cursorMode))
	defer sqd.Close()
	jsonl := parser.NewFastJSONLParser(1024)
	replayBuf := NewReplayBuffer(8192) // ~8K blocks of replay capacity
	// No-data-loss (Invariant 0): if the processor reports a durable commit
	// horizon, the persisted checkpoint is gated so it never leads that horizon.
	// On crash the run resumes from durable state and re-fetches the cheap gap.
	committedReporter, _ := proc.(CommitHorizonReporter)
	flusher, _ := proc.(Flusher)
	// durableCheckpoint is the highest block number written to sync_state so far.
	var durableCheckpoint uint64
	// Fork-recovery snapshots are only needed near/above the finalized head
	// (reorgs). In cursor mode the cursor often starts far below the finalized
	// head and catches up over millions of blocks — keeping snapshots ON during
	// that backfill phase is the single biggest GC/memory cost. Start disabled
	// and dynamically enable once the consumer approaches the finalized head.
	var snapshotController SnapshotController
	var snapshotsActive bool
	if sc, ok := proc.(SnapshotController); ok {
		snapshotController = sc
		sc.SetSnapshotsEnabled(!cursorMode) // backfill: off; non-cursor: on (no finalized head to track)
	}
	// Cold tier (Pebble): an evicted hot entry is served from local disk instead of
	// a ClickHouse point-SELECT. Authoritative iff ClickHouse holds no rows for this
	// chain at start (fresh / --restart): a hot+cold miss is then provably new and
	// the per-miss SELECT is skipped entirely. On resume-with-data it stays
	// non-authoritative — still serving re-referenced evictions from disk, but never
	// skipping a needed lookup. Reorg-safe: any state recovery (RestoreToBlock /
	// LoadFromDatabase) detaches the cold tier, so post-reorg reads fall back to the
	// rolled-back ClickHouse.
	if coldCache {
		if cc, ok := proc.(ColdCacheProcessor); ok {
			_, hasAny, err := store.LastBlock(ctx, chain.ID)
			if err != nil {
				return fmt.Errorf("cold cache: probe last block: %w", err)
			}
			authoritative := !hasAny
			dir := filepath.Join(coldDir, fmt.Sprintf("chain-%d", chain.ID))
			if err := cc.EnableColdCache(dir, authoritative); err != nil {
				return fmt.Errorf("enable cold cache: %w", err)
			}
			defer func() { _ = cc.CloseColdCache() }()
			log.Printf("Chain %d: cold tier enabled (dir=%s authoritative=%v cursor=%v)", chain.ID, dir, authoritative, cursorMode)
		} else {
			log.Printf("Chain %d: cold cache requested but processor does not implement ColdCacheProcessor", chain.ID)
		}
	}
	fastJSONLProc, fastJSONLOK := proc.(FastJSONLProcessor)
	useParseDecodeV2 := os.Getenv("SQD_PARSE_DECODE_V2") != "" && fastJSONLOK
	if os.Getenv("SQD_PARSE_DECODE_V2") != "" && !fastJSONLOK {
		log.Printf("Chain %d: SQD_PARSE_DECODE_V2 requested, but processor does not implement FastJSONLProcessor; using default processor path", chain.ID)
	}
	if useParseDecodeV2 {
		log.Printf("Chain %d: SQD_PARSE_DECODE_V2 enabled for custom processor", chain.ID)
	}
	totalBlocks, totalEvents := uint64(0), uint64(0)
	startTime := time.Now()
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// profiling accumulators
	var profFetchNanos, profParseNanos, profDecodeNanos, profMarshalNanos, profInsertNanos, profCustomNanos atomic.Int64
	var profConsumerWaitNanos, profProducerBackpressureNanos atomic.Int64
	var profIters atomic.Int64
	defer func() {
		runtime.ReadMemStats(&memAfter)
		printProfile(
			time.Duration(profFetchNanos.Load()),
			time.Duration(profParseNanos.Load()),
			time.Duration(profDecodeNanos.Load()),
			time.Duration(profMarshalNanos.Load()),
			time.Duration(profInsertNanos.Load()),
			time.Duration(profCustomNanos.Load()),
			time.Duration(profConsumerWaitNanos.Load()),
			time.Duration(profProducerBackpressureNanos.Load()),
			int(profIters.Load()),
			atomic.LoadUint64(&totalBlocks),
			atomic.LoadUint64(&totalEvents),
			startTime, memBefore, memAfter,
		)
	}()

	baseInserter := store.NewInserter()
	typedInserters := make(map[string]*database.TypedInserter)
	for _, table := range typedTables.byEvent {
		typedInserters[table.Name] = store.NewTypedInserter(table)
	}
	for _, table := range typedTables.byAddressEvent {
		typedInserters[table.Name] = store.NewTypedInserter(table)
	}

	var currentConsumerBlock atomic.Uint64
	currentConsumerBlock.Store(currentBlock)

	type producerSignal struct {
		forkErr *client.ForkError
		err     error
	}
	type producerAdvance struct {
		nextBlock  uint64
		parentHash string
	}
	errChan := make(chan producerSignal, 1)
	finalizedChan := make(chan *client.BlockRef, 100)

	var prodCancel context.CancelFunc
	var producerDone bool
	var producerAdvanceChan chan producerAdvance

	var startProd func(startBlk uint64, initialParent string)
	startProd = func(startBlk uint64, initialParent string) {
		if prodCancel != nil {
			prodCancel()
		}
		producerDone = false
		advanceChan := make(chan producerAdvance, 1)
		producerAdvanceChan = advanceChan
		var prodCtx context.Context
		prodCtx, prodCancel = context.WithCancel(ctx)

		go func(pCtx context.Context, pBlock uint64, pHash string, advance <-chan producerAdvance) {
			var lastFinalized uint64
			// Adaptive page sizing: when pageSize=0, use this instead of nil
			var adaptivePageSize uint64 = minAdaptivePageSize
			sentSignal := false
			sendSignal := func(sig producerSignal) {
				sentSignal = true
				select {
				case errChan <- sig:
				case <-pCtx.Done():
				}
			}

			defer func() {
				if sentSignal {
					return
				}
				select {
				case errChan <- producerSignal{}:
				case <-pCtx.Done():
				}
			}()

			for {
				select {
				case <-pCtx.Done():
					return
				default:
				}

				// Backpressure check: wait if producer is too far ahead of consumer
				cBlock := currentConsumerBlock.Load()
				if pBlock >= cBlock && pBlock-cBlock >= uint64(replayBuf.capacity)-100 {
					waitStart := time.Now()
					select {
					case <-pCtx.Done():
						profProducerBackpressureNanos.Add(int64(time.Since(waitStart)))
						return
					case <-time.After(10 * time.Millisecond):
					}
					profProducerBackpressureNanos.Add(int64(time.Since(waitStart)))
					continue
				}

				toBlockPtr, rangeLabel, ok := nextProducerRequestRange(pBlock, pageSize, adaptivePageSize, lastFinalized, effectiveEndBlock, cursorMode)
				if !ok {
					return
				}

				t0 := time.Now()
				response, err := sqd.FetchWithParent(pCtx, pBlock, toBlockPtr, pHash, cursorMode, filters)
				profFetchNanos.Add(int64(time.Since(t0)))

				if err != nil {
					var forkErr *client.ForkError
					if cursorMode && errors.As(err, &forkErr) {
						sendSignal(producerSignal{forkErr: forkErr})
						return
					}
					log.Printf("Chain %d: fetch %s error: %v, retrying...", chain.ID, rangeLabel, err)
					select {
					case <-pCtx.Done():
						return
					case <-time.After(5 * time.Second):
					}
					continue
				}

				if response.Head.Finalized != nil && response.Head.Finalized.Number > lastFinalized {
					lastFinalized = response.Head.Finalized.Number
				}

				raw := response.Raw
				if len(raw) == 0 {
					if cursorMode {
						if !shouldWaitForEmptyCursorResponse(effectiveEndBlock) {
							return
						}
						select {
						case finalizedChan <- response.Head.Finalized:
						case <-pCtx.Done():
							return
						}
						select {
						case <-pCtx.Done():
							return
						case <-time.After(cursorPollInterval):
						}
						continue
					}
					return
				}

				type decodedBlock struct {
					number      uint64
					hash        string
					timestamp   time.Time
					events      []parser.DecodedEvent
					logs        []CustomLog
					typedEvents map[string][]parser.DecodedEvent
					raw         []byte
				}

				parseStart := time.Now()
				var decodeDur time.Duration
				var decodedBlocks []decodedBlock
				var dataScratch []byte
				err = jsonl.ParseWithLine(raw, func(block *parser.Block, rawLine []byte) error {
					if effectiveEndBlock != nil && block.Header.Number > *effectiveEndBlock {
						return nil
					}
					blockHash := strings.Clone(block.Header.Hash)
					blockTS := time.Unix(int64(block.Header.Timestamp), 0).UTC()

					var blockEvents []parser.DecodedEvent
					var blockCustomLogs []CustomLog
					blockTypedEvents := make(map[string][]parser.DecodedEvent)

					for _, lg := range block.Logs {
						if len(lg.Topics) == 0 {
							continue
						}
						d0 := time.Now()
						topic0 := abiunpack.DecodeTopicHash(lg.Topics[0])
						def, ok := decoders[topic0]
						if !ok {
							decodeDur += time.Since(d0)
							continue
						}
						if !def.MatchesAddress(lg.Address) {
							decodeDur += time.Since(d0)
							continue
						}
						dataScratch = abiunpack.AppendHexBytes(dataScratch[:0], lg.Data)
						ev, err := def.Decode(lg.Address, lg.Topics, dataScratch)
						decodeDur += time.Since(d0)
						if err != nil {
							continue
						}
						ev.ChainID = chain.ID
						ev.BlockNumber = block.Header.Number
						ev.BlockTimestamp = blockTS
						ev.BlockHash = blockHash
						ev.TxHash = strings.Clone(lg.TransactionHash)
						ev.TxIndex = lg.TransactionIndex
						ev.LogIndex = lg.LogIndex
						ev.Address = strings.Clone(lg.Address)
						blockEvents = append(blockEvents, *ev)

						if !useParseDecodeV2 {
							blockCustomLogs = append(blockCustomLogs, CustomLog{
								ChainID:          chain.ID,
								BlockNumber:      block.Header.Number,
								BlockTimestamp:   blockTS,
								BlockHash:        blockHash,
								ContractAddress:  strings.Clone(lg.Address),
								TransactionHash:  strings.Clone(lg.TransactionHash),
								TransactionIndex: lg.TransactionIndex,
								LogIndex:         lg.LogIndex,
								Topics:           cloneStrings(lg.Topics),
								Data:             strings.Clone(lg.Data),
							})
						}

						if table, ok := typedTables.lookup(ev.Address, ev.EventName); ok {
							blockTypedEvents[table.Name] = append(blockTypedEvents[table.Name], *ev)
						}
					}

					var blockRaw []byte
					if useParseDecodeV2 {
						blockRaw = rawLine
					}
					decodedBlocks = append(decodedBlocks, decodedBlock{
						number:      block.Header.Number,
						hash:        blockHash,
						timestamp:   blockTS,
						events:      blockEvents,
						logs:        blockCustomLogs,
						typedEvents: blockTypedEvents,
						raw:         blockRaw,
					})
					return nil
				})
				if err != nil {
					sendSignal(producerSignal{err: err})
					return
				}

				batchStartBlock := pBlock
				for idx, db := range decodedBlocks {
					// Backpressure check: wait if producer is too far ahead of consumer
					for {
						cBlock := currentConsumerBlock.Load()
						if db.number >= cBlock && db.number-cBlock >= uint64(replayBuf.capacity)-100 {
							waitStart := time.Now()
							select {
							case <-pCtx.Done():
								profProducerBackpressureNanos.Add(int64(time.Since(waitStart)))
								return
							case <-time.After(10 * time.Millisecond):
							}
							profProducerBackpressureNanos.Add(int64(time.Since(waitStart)))
							continue
						}
						break
					}

					isLastInBatch := (idx == len(decodedBlocks)-1)
					replayBuf.Write(chain.ID, db.number, db.hash, db.timestamp, db.events, db.logs, db.typedEvents, response.Head.Finalized, isLastInBatch, rangeLabel, batchStartBlock, db.raw)
					pHash = db.hash
				}

				profParseNanos.Add(int64(time.Since(parseStart)))
				profDecodeNanos.Add(int64(decodeDur))
				profIters.Add(1)

				if len(decodedBlocks) > 0 {
					select {
					case next := <-advance:
						pBlock = next.nextBlock
						pHash = next.parentHash
					case <-pCtx.Done():
						return
					}
				} else {
					// No blocks decoded, advance by 1 to retry
					pBlock++
				}
			}
		}(prodCtx, startBlk, initialParent, advanceChan)
	}

	// Start the initial producer
	parentHash := ""
	if cursorMode {
		if head := state.Head(); head != nil {
			parentHash = head.Hash
		}
	}
	var currentConsumerBlockVal uint64 = currentBlock
	currentConsumerBlock.Store(currentConsumerBlockVal)
	startProd(currentBlock, parentHash)
	defer func() {
		if prodCancel != nil {
			prodCancel()
		}
	}()

	var batchEventBlocks uint64
	var batchEventsCount uint64
	statsTicker := time.NewTicker(statsInterval)
	defer statsTicker.Stop()
	lastStatsTime := startTime
	lastStatsBlocks := uint64(0)
	lastStatsEvents := uint64(0)
	lastCheckpoint := currentBlock
	if lastCheckpoint > 0 {
		lastCheckpoint--
	}
	logStats := func(reason string) {
		now := time.Now()
		totalBlockCount := atomic.LoadUint64(&totalBlocks)
		totalEventCount := atomic.LoadUint64(&totalEvents)
		interval := now.Sub(lastStatsTime)
		if interval <= 0 {
			interval = time.Nanosecond
		}
		elapsed := now.Sub(startTime)
		if elapsed <= 0 {
			elapsed = time.Nanosecond
		}
		deltaBlocks := totalBlockCount - lastStatsBlocks
		deltaEvents := totalEventCount - lastStatsEvents
		log.Printf("Chain %d: stats %s | checkpoint: %d | next: %d | buffered: %d | +%d blocks, +%d events in %s | %.1f blk/s (avg %.1f) | total: %d blocks, %d events",
			chain.ID, reason, lastCheckpoint, currentConsumerBlockVal, replayBuf.Len(),
			deltaBlocks, deltaEvents, interval.Round(time.Millisecond),
			float64(deltaBlocks)/interval.Seconds(), float64(totalBlockCount)/elapsed.Seconds(),
			totalBlockCount, totalEventCount)
		lastStatsTime = now
		lastStatsBlocks = totalBlockCount
		lastStatsEvents = totalEventCount
		monitoring.Observe(chain.ID, totalBlockCount, totalEventCount, currentConsumerBlockVal, lastCheckpoint)
	}

	var batchBlockRows []database.BlockRow
	var batchDecodedEvents []parser.DecodedEvent
	batchTypedEvents := make(map[string][]parser.DecodedEvent)
	var batchCustomLogs []CustomLog
	var batchRawJSONL []byte
	var pendingBatchBlocks uint64
	resetBatch := func() {
		batchBlockRows = batchBlockRows[:0]
		batchDecodedEvents = batchDecodedEvents[:0]
		for k := range batchTypedEvents {
			delete(batchTypedEvents, k)
		}
		batchCustomLogs = batchCustomLogs[:0]
		batchRawJSONL = batchRawJSONL[:0]
		batchEventBlocks = 0
		batchEventsCount = 0
		pendingBatchBlocks = 0
	}
	sendProducerAdvance := func(nextBlock uint64, parentHash string) error {
		if producerAdvanceChan == nil {
			return nil
		}
		select {
		case producerAdvanceChan <- producerAdvance{nextBlock: nextBlock, parentHash: parentHash}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for {
		if entry, ok := replayBuf.GetBlock(currentConsumerBlockVal); ok {
			// Accumulate data for batch insertion
			if len(entry.events) > 0 {
				if storeBlocks {
					batchBlockRows = append(batchBlockRows, entry.blockRow)
				}
				batchDecodedEvents = append(batchDecodedEvents, entry.events...)
			}
			for tableName, events := range entry.typedEvents {
				batchTypedEvents[tableName] = append(batchTypedEvents[tableName], events...)
			}
			if useParseDecodeV2 {
				if len(entry.raw) > 0 {
					batchRawJSONL = append(batchRawJSONL, entry.raw...)
					batchRawJSONL = append(batchRawJSONL, '\n')
				}
			} else {
				batchCustomLogs = append(batchCustomLogs, entry.logs...)
			}

			if cursorMode {
				blockRef := client.BlockRef{Number: entry.number, Hash: entry.hash}
				state.ApplyBatch(entry.finalized, []client.BlockRef{blockRef})

				// Dynamically enable snapshots once the consumer is near the finalized head.
				// Below the finalized head forks can't happen so snapshots are pure waste.
				const snapshotEnableMargin = 128
				if snapshotController != nil && !snapshotsActive && entry.finalized != nil {
					if entry.number+snapshotEnableMargin >= entry.finalized.Number {
						snapshotController.SetSnapshotsEnabled(true)
						snapshotsActive = true
						log.Printf("Chain %d: snapshots enabled — consumer block %d is within %d of finalized head %d",
							chain.ID, entry.number, snapshotEnableMargin, entry.finalized.Number)
					}
				}
			}

			if len(entry.events) > 0 {
				batchEventBlocks++
				batchEventsCount += uint64(len(entry.events))
			}

			atomic.AddUint64(&totalEvents, uint64(len(entry.events)))
			atomic.AddUint64(&totalBlocks, 1)
			pendingBatchBlocks++

			if entry.isLastInBatch {
				insertStart := time.Now()

				// 1. Logs insertion
				if cfg.ShouldStoreRawLogs() && len(batchDecodedEvents) > 0 {
					if err := baseInserter.InsertLogs(ctx, batchDecodedEvents); err != nil {
						return fmt.Errorf("InsertLogs: %w", err)
					}
				}

				// 2. Typed events insertion
				for tableName, events := range batchTypedEvents {
					if len(events) == 0 {
						continue
					}
					inserter := typedInserters[tableName]
					if inserter == nil {
						return fmt.Errorf("missing TypedInserter for %s", tableName)
					}
					if err := inserter.Insert(ctx, events); err != nil {
						return fmt.Errorf("InsertTypedLogs(%s): %w", tableName, err)
					}
				}

				// 3. Optional block ledger insertion
				if storeBlocks && len(batchBlockRows) > 0 {
					if err := baseInserter.InsertBlocks(ctx, batchBlockRows); err != nil {
						return fmt.Errorf("InsertBlocks: %w", err)
					}
				}
				profInsertNanos.Add(int64(time.Since(insertStart)))

				if err := sendProducerAdvance(entry.number+1, entry.hash); err != nil {
					return err
				}

				// 4. Custom Processor
				if useParseDecodeV2 && len(batchRawJSONL) > 0 {
					procStart := time.Now()
					if _, err := fastJSONLProc.ProcessJSONL(ctx, store, batchRawJSONL); err != nil {
						return fmt.Errorf("custom processor v2 error: %w", err)
					}
					profCustomNanos.Add(int64(time.Since(procStart)))
				} else if proc != nil && len(batchCustomLogs) > 0 {
					procStart := time.Now()
					if err := proc.Process(ctx, store, batchCustomLogs); err != nil {
						return fmt.Errorf("custom processor error: %w", err)
					}
					profCustomNanos.Add(int64(time.Since(procStart)))
				}

				if cursorMode {
					current := state.Current()
					if current != nil {
						if err := saveForkState(ctx, store, chain.ID, state, current); err != nil {
							return fmt.Errorf("update sync state %d: %w", entry.number, err)
						}
						if entry.number%10 == 0 {
							if err := store.TruncateSyncState(ctx, chain.ID, current.Number); err != nil {
								log.Printf("Chain %d: truncate sync state error: %v", chain.ID, err)
							}
						}
					}
				} else {
					// Gate the checkpoint to the durable (committed) horizon: it must
					// never lead durable state. With a periodic commit cadence the
					// checkpoint lags by up to the cadence (blocks/time), which a crash
					// re-fetches cheaply and re-processes idempotently (rollbackAfterBlock
					// truncates anything beyond the checkpoint).
					checkpointBlock := entry.number
					if committedReporter != nil {
						if c := committedReporter.CommittedBlock(); c < checkpointBlock {
							checkpointBlock = c
						}
					}
					// No-data-loss invariant (backfill): the durable checkpoint must never
					// lead the finalized head, so a crash always resumes from a finalized
					// block and the re-fetched gap can't be re-orged out from under us. The
					// producer already caps backfill requests at the finalized head; this
					// clamp makes the guarantee local and independent of that logic. The
					// gap (even ~10k blocks) is a cheap HTTP re-fetch on resume.
					if entry.finalized != nil && entry.finalized.Number < checkpointBlock {
						checkpointBlock = entry.finalized.Number
					}
					if checkpointBlock > durableCheckpoint {
						// Make event-table rows for blocks <= checkpointBlock durable before
						// the checkpoint advances past them, so a crash can't drop them.
						if err := store.FlushAsyncInserts(ctx); err != nil {
							return fmt.Errorf("flush async inserts before checkpoint %d: %w", checkpointBlock, err)
						}
						if err := store.UpdateSyncState(ctx, chain.ID, checkpointBlock); err != nil {
							return fmt.Errorf("update sync state %d: %w", checkpointBlock, err)
						}
						durableCheckpoint = checkpointBlock
						if checkpointBlock%10 == 0 {
							if err := store.TruncateSyncState(ctx, chain.ID, checkpointBlock); err != nil {
								log.Printf("Chain %d: truncate sync state error: %v", chain.ID, err)
							}
						}
					}
				}

				scanned := entry.number - entry.requestStartBlock + 1
				elapsed := time.Now().Sub(startTime)
				rate := float64(atomic.LoadUint64(&totalBlocks)) / elapsed.Seconds()
				log.Printf("Chain %d: %s scanned %d blocks, event blocks: %d, events: %d | checkpoint: %d | total: %d blocks, %d events | %.1f blk/s",
					chain.ID, entry.rangeLabel, scanned, batchEventBlocks, batchEventsCount, entry.number, atomic.LoadUint64(&totalBlocks), atomic.LoadUint64(&totalEvents), rate)

				lastCheckpoint = entry.number
				resetBatch()
			}

			currentConsumerBlockVal = entry.number + 1
			currentConsumerBlock.Store(currentConsumerBlockVal)

			select {
			case <-statsTicker.C:
				logStats("periodic")
			default:
			}

			if effectiveEndBlock != nil && currentConsumerBlockVal > *effectiveEndBlock {
				break
			}
			continue
		}

		if producerDone {
			// Checked the buffer, it's empty, and the producer has completed cleanly
			break
		}

		waitStart := time.Now()
		select {
		case <-ctx.Done():
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			log.Printf("Chain %d: interrupted at block %d", chain.ID, currentConsumerBlockVal)
			return ctx.Err()

		case sig := <-errChan:
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			if sig.forkErr != nil {
				forkErr := sig.forkErr
				log.Printf("[FORK DETECTED] Chain %d: fork detected! Previous blocks sent by portal: %v", chain.ID, forkErr.PreviousBlocks)
				safe, ok := state.HandleFork(forkErr.PreviousBlocks)
				if !ok {
					return fmt.Errorf("process fork: unable to find common fork cursor")
				}
				log.Printf("[FORK DETECTED] Common ancestor successfully resolved at safe block %d (hash: %s)", safe.Number, safe.Hash)
				log.Printf("[ROLLBACK] Starting database rollback to safe block %d (mode: %s)...", safe.Number, forkMode)
				if err := rollbackAfterBlock(ctx, store, forkMode, chain.ID, safe.Number); err != nil {
					return fmt.Errorf("rollback after fork cursor %d: %w", safe.Number, err)
				}
				if err := saveForkState(ctx, store, chain.ID, state, &safe); err != nil {
					return fmt.Errorf("save fork cursor %d: %w", safe.Number, err)
				}
				log.Printf("[ROLLBACK] Database rollback completed successfully and fork state saved for block %d", safe.Number)

				log.Printf("[LOAD STATE] Restoring processor state to safe block %d...", safe.Number)
				restoredBlock, err := proc.RestoreToBlock(safe.Number)
				if err != nil {
					log.Printf("[LOAD STATE] Processor state restore to block %d failed: %v. Attempting fallback state load from ClickHouse database...", safe.Number, err)
					if err := proc.LoadFromDatabase(ctx, safe.Number); err != nil {
						return fmt.Errorf("restore processor state after fork at %d: %w", safe.Number, err)
					}
					restoredBlock = safe.Number
				}

				if restoredBlock < safe.Number {
					log.Printf("[ROLLBACK] Replaying blocks %d to %d from replay buffer to catch up custom processor state...", restoredBlock+1, safe.Number)
					for bNum := restoredBlock + 1; bNum <= safe.Number; bNum++ {
						entry, ok := replayBuf.GetBlock(bNum)
						if !ok {
							log.Printf("[ROLLBACK] Replay buffer cache miss for block %d during catch up. Attempting fallback state load from ClickHouse database...", bNum)
							if err := proc.LoadFromDatabase(ctx, safe.Number); err != nil {
								return fmt.Errorf("restore processor state after fork at %d: %w", safe.Number, err)
							}
							break
						}
						// Process through fastJSONLProc or custom processor
						if useParseDecodeV2 && len(entry.raw) > 0 {
							if _, err := fastJSONLProc.ProcessJSONL(ctx, store, append(entry.raw, '\n')); err != nil {
								return fmt.Errorf("custom processor v2 replay error at block %d: %w", bNum, err)
							}
						} else if proc != nil && len(entry.logs) > 0 {
							if err := proc.Process(ctx, store, entry.logs); err != nil {
								return fmt.Errorf("custom processor replay error at block %d: %w", bNum, err)
							}
						}
					}
				}


				replayBuf.PruneAfter(safe.Number)
				if pendingBatchBlocks > 0 {
					log.Printf("[ROLLBACK] Discarding %d uncommitted batch block(s) after fork rollback", pendingBatchBlocks)
					resetBatch()
				}

				currentConsumerBlockVal = safe.Number + 1
				currentConsumerBlock.Store(currentConsumerBlockVal)
				startProd(currentConsumerBlockVal, safe.Hash)
				continue
			}
			if sig.err != nil {
				return fmt.Errorf("producer error: %w", sig.err)
			}

			// Clean exit of producer
			producerDone = true

		case finalized := <-finalizedChan:
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			state.ApplyBatch(finalized, nil)
			if pendingBatchBlocks == 0 {
				current := state.Current()
				if current != nil {
					if err := saveForkState(ctx, store, chain.ID, state, current); err != nil {
						return fmt.Errorf("update sync state: %w", err)
					}
				}
			} else {
				log.Printf("Chain %d: finalized head advanced with %d uncommitted batch block(s); checkpoint delayed until batch commit", chain.ID, pendingBatchBlocks)
			}
			log.Printf("Chain %d: empty response, waiting for new blocks...", chain.ID)

		case <-replayBuf.notifyCh:
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			// A block was written, loop back to GetBlock check

		case <-statsTicker.C:
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			logStats("periodic")
		}
	}

	// Clean completion (backfill): force a durable commit of the tail (the blocks
	// processed since the last periodic commit) and advance the checkpoint to it,
	// so nothing processed is lost on a clean exit. Crash/cancel paths intentionally
	// skip this — they resume from the last durable checkpoint and re-fetch the gap.
	if !cursorMode && flusher != nil && lastCheckpoint > 0 {
		committed, err := flusher.Flush(ctx, store, lastCheckpoint)
		if err != nil {
			return fmt.Errorf("final flush at %d: %w", lastCheckpoint, err)
		}
		if committed > durableCheckpoint {
			if err := store.FlushAsyncInserts(ctx); err != nil {
				return fmt.Errorf("final flush async inserts at %d: %w", committed, err)
			}
			if err := store.UpdateSyncState(ctx, chain.ID, committed); err != nil {
				return fmt.Errorf("final checkpoint %d: %w", committed, err)
			}
			durableCheckpoint = committed
		}
	}

	return nil
}

func printProfile(fetch, parse, decode, marshal, insert, custom, consumerWait, producerBackpressure time.Duration, iters int, totalBlocks, totalEvents uint64, startTime time.Time, memBefore, memAfter runtime.MemStats) {
	total := fetch + parse + decode + marshal + insert + custom
	waitTotal := consumerWait + producerBackpressure
	elapsed := time.Since(startTime)
	log.Println("")
	log.Println("═══ PROFILE ═══")
	log.Printf("  FETCH:  %v (%.0f%%)", fetch, pct(fetch, total))
	if iters > 0 {
		log.Printf("  PARSE:  %v (%.0f%%), %d iterations, avg %v/iter", parse, pct(parse, total), iters, parse/time.Duration(iters))
	} else {
		log.Printf("  PARSE:  %v (%.0f%%), 0 iterations", parse, pct(parse, total))
	}
	log.Printf("  DECODE: %v (%.0f%%)", decode, pct(decode, total))
	log.Printf("  MARSHAL:%v (%.0f%%)", marshal, pct(marshal, total))
	log.Printf("  INSERT: %v (%.0f%%)", insert, pct(insert, total))
	log.Printf("  CUSTOM: %v (%.0f%%)", custom, pct(custom, total))
	log.Printf("  WAIT:   consumer=%v producer_backpressure=%v observed=%v", consumerWait, producerBackpressure, waitTotal)
	log.Printf("  ─────────────────")
	log.Printf("  TOTAL:  %v work (wall %v, %.0f%% observed work)", total, elapsed, pct(total, elapsed))
	log.Printf("  Throughput: %d blocks, %d events, avg %.0f µs/event", totalBlocks, totalEvents, float64(total.Microseconds())/float64(max(totalEvents, 1)))

	allocMegabytes := float64(memAfter.TotalAlloc-memBefore.TotalAlloc) / 1024 / 1024
	log.Printf("  Mem Alloc:  %.2f MB (Mallocs: %d)", allocMegabytes, memAfter.Mallocs-memBefore.Mallocs)
	log.Println("════════════════")
}

func pct(part, total time.Duration) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func waitForNextCursorPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func shouldWaitForEmptyCursorResponse(effectiveEndBlock *uint64) bool {
	return effectiveEndBlock == nil
}

func emptyCursorCheckpoint(currentBlock uint64, head client.Head) (uint64, bool) {
	if head.Finalized == nil || head.Finalized.Number < currentBlock {
		return 0, false
	}
	return head.Finalized.Number, true
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func nextRequestRange(currentBlock, pageSize uint64, effectiveEndBlock *uint64, cursorMode bool) (*uint64, string, bool) {
	if effectiveEndBlock != nil && currentBlock > *effectiveEndBlock {
		return nil, "", false
	}
	if cursorMode {
		if effectiveEndBlock != nil {
			toBlock := *effectiveEndBlock
			return &toBlock, fmt.Sprintf("[%d-%d]", currentBlock, toBlock), true
		}
		return nil, fmt.Sprintf("[%d-tail]", currentBlock), true
	}
	if pageSize == 0 {
		if effectiveEndBlock != nil {
			return nil, fmt.Sprintf("[%d-%d]", currentBlock, *effectiveEndBlock), true
		}
		return nil, fmt.Sprintf("[%d-tail]", currentBlock), true
	}
	toBlock := currentBlock + pageSize - 1
	if effectiveEndBlock != nil && toBlock > *effectiveEndBlock {
		toBlock = *effectiveEndBlock
	}
	if toBlock < currentBlock {
		return nil, "", false
	}
	return &toBlock, fmt.Sprintf("[%d-%d]", currentBlock, toBlock), true
}

func nextProducerRequestRange(currentBlock, pageSize, adaptivePageSize, lastFinalized uint64, effectiveEndBlock *uint64, cursorMode bool) (*uint64, string, bool) {
	if effectiveEndBlock != nil && currentBlock > *effectiveEndBlock {
		return nil, "", false
	}
	effectivePageSize := pageSize
	if effectivePageSize == 0 && cursorMode {
		effectivePageSize = adaptivePageSize
	}
	if effectivePageSize > 0 && cursorMode && (lastFinalized == 0 || currentBlock+effectivePageSize < lastFinalized) {
		toBlock := currentBlock + effectivePageSize - 1
		if effectiveEndBlock != nil && toBlock > *effectiveEndBlock {
			toBlock = *effectiveEndBlock
		}
		if toBlock < currentBlock {
			return nil, "", false
		}
		return &toBlock, fmt.Sprintf("[%d-%d]", currentBlock, toBlock), true
	}
	return nextRequestRange(currentBlock, pageSize, effectiveEndBlock, cursorMode)
}

func minEndBlock(current *uint64, candidate uint64) *uint64 {
	if current != nil && *current < candidate {
		return current
	}
	return &candidate
}

func chainEndpoint(chainID uint64, hot bool) string {
	// Allow overriding the portal endpoint (e.g. a local mock or mirror) for
	// integration testing the full ingestion pipeline against fixture data.
	if v := os.Getenv("SQD_PORTAL_ENDPOINT"); v != "" {
		return v
	}
	suffix := "/finalized-stream"
	if hot {
		suffix = "/stream"
	}
	switch chainID {
	case 1:
		return "https://portal.sqd.dev/datasets/ethereum-mainnet" + suffix
	case 137:
		return "https://portal.sqd.dev/datasets/polygon-mainnet" + suffix
	default:
		return "https://portal.sqd.dev/datasets/polygon-mainnet" + suffix
	}
}

func rollbackAfterBlock(ctx context.Context, store *database.Store, mode config.ForkMode, chainID, lastBlock uint64) error {
	log.Printf("[ROLLBACK] Executing ClickHouse rollback for blocks > %d (mode: %s)...", lastBlock, mode)
	var err error
	if mode.UsesCollapsingMergeTree() {
		err = store.CollapseAfterBlock(ctx, chainID, lastBlock)
	} else {
		err = store.TruncateAfterBlock(ctx, chainID, lastBlock)
	}
	if err != nil {
		log.Printf("[ROLLBACK] ClickHouse rollback failed: %v", err)
		return err
	}
	log.Printf("[ROLLBACK] ClickHouse rollback completed successfully for blocks > %d", lastBlock)
	return nil
}

func saveForkState(ctx context.Context, store *database.Store, chainID uint64, state ForkTracker, current *client.BlockRef) error {
	finalized := state.FinalizedHighWatermark()
	return store.SaveSyncState(ctx, chainID, database.SyncState{
		Current:       blockRefToSyncCursor(*current),
		Finalized:     blockRefPtrToSyncCursor(finalized),
		RollbackChain: blockRefsToSyncCursors(filterUnfinalizedRollbackChain(state.RecentUnfinalizedBlocks(), finalized)),
	})
}

func syncCursorPtrToBlockRef(cursor *database.SyncCursor) *client.BlockRef {
	if cursor == nil || cursor.Hash == "" {
		return nil
	}
	return &client.BlockRef{Number: cursor.Number, Hash: cursor.Hash}
}

func blockRefPtrToSyncCursor(ref *client.BlockRef) *database.SyncCursor {
	if ref == nil {
		return nil
	}
	cursor := blockRefToSyncCursor(*ref)
	return &cursor
}

func blockRefToSyncCursor(ref client.BlockRef) database.SyncCursor {
	return database.SyncCursor{Number: ref.Number, Hash: ref.Hash}
}

func syncCursorsToBlockRefs(cursors []database.SyncCursor) []client.BlockRef {
	out := make([]client.BlockRef, 0, len(cursors))
	for _, cursor := range cursors {
		if cursor.Hash == "" {
			continue
		}
		out = append(out, client.BlockRef{Number: cursor.Number, Hash: cursor.Hash})
	}
	return out
}

func blockRefsToSyncCursors(refs []client.BlockRef) []database.SyncCursor {
	out := make([]database.SyncCursor, 0, len(refs))
	for _, ref := range refs {
		out = append(out, blockRefToSyncCursor(ref))
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.Clone(v)
	}
	return out
}

type typedTableIndex struct {
	byAddressEvent map[string]database.TypedEventTable
	byEvent        map[string]database.TypedEventTable
}

func (i typedTableIndex) lookup(address, eventName string) (database.TypedEventTable, bool) {
	if table, ok := i.byAddressEvent[strings.ToLower(address)+"|"+eventName]; ok {
		return table, true
	}
	table, ok := i.byEvent[eventName]
	return table, ok
}

func buildTypedTableIndex(chain *config.Chain) (typedTableIndex, error) {
	index := typedTableIndex{
		byAddressEvent: make(map[string]database.TypedEventTable),
		byEvent:        make(map[string]database.TypedEventTable),
	}
	used := make(map[string]int)
	for _, contract := range chain.Contracts {
		addresses := configAddresses(contract.Address)
		for _, event := range contract.Events {
			name, args, err := parseEventArgs(event.Event)
			if err != nil {
				return index, fmt.Errorf("%s.%s: %w", contract.Name, event.Event, err)
			}
			viewName := uniqueLower(used, toSnake(contract.Name+"_"+name))

			var filteredArgs []database.TypedEventArg
			for _, arg := range args {
				isOmitted := false
				for _, o := range event.Omit {
					if strings.EqualFold(o, arg.Name) || strings.EqualFold(strings.ReplaceAll(o, "_", ""), strings.ReplaceAll(arg.Name, "_", "")) {
						isOmitted = true
						break
					}
				}
				if !isOmitted {
					filteredArgs = append(filteredArgs, arg)
				}
			}

			table := database.TypedEventTable{
				Name: viewName + "_events",
				Args: filteredArgs,
			}
			if len(addresses) == 0 {
				index.byEvent[name] = table
				continue
			}
			for _, address := range addresses {
				index.byAddressEvent[strings.ToLower(address)+"|"+name] = table
			}
		}
	}
	return index, nil
}

func parseEventArgs(sig string) (string, []database.TypedEventArg, error) {
	sig = strings.TrimSpace(strings.TrimPrefix(sig, "event "))
	open := strings.IndexByte(sig, '(')
	close := strings.LastIndexByte(sig, ')')
	if open <= 0 || close <= open {
		return "", nil, fmt.Errorf("invalid event signature")
	}
	name := strings.TrimSpace(sig[:open])
	inputs := splitEventArgs(sig[open+1 : close])
	parsed, err := abi.JSON(strings.NewReader(eventABIJSON(name, inputs)))
	if err != nil {
		return "", nil, err
	}
	ev, ok := parsed.Events[name]
	if !ok {
		return "", nil, fmt.Errorf("event not found after parsing")
	}
	args := make([]database.TypedEventArg, 0, len(ev.Inputs))
	for idx, input := range ev.Inputs {
		argName := input.Name
		if strings.TrimSpace(argName) == "" {
			argName = fmt.Sprintf("p%d", idx)
		}
		solType := input.Type.String()
		args = append(args, database.TypedEventArg{
			Name:           argName,
			ColumnName:     argName,
			SolidityType:   solType,
			ClickHouseType: clickHouseType(solType),
		})
	}
	return name, args, nil
}

func eventABIJSON(name string, args []string) string {
	inputs := make([]string, 0, len(args))
	for i, arg := range args {
		parts := strings.Fields(strings.TrimSpace(arg))
		if len(parts) == 0 {
			continue
		}
		typ := normalizeSolidityType(parts[0])
		indexed := false
		paramName := fmt.Sprintf("p%d", i)
		for _, part := range parts[1:] {
			if part == "indexed" {
				indexed = true
				continue
			}
			paramName = part
		}
		inputs = append(inputs, fmt.Sprintf(`{"indexed":%t,"name":%q,"type":%q}`, indexed, paramName, typ))
	}
	return fmt.Sprintf(`[{"anonymous":false,"inputs":[%s],"name":%q,"type":"event"}]`, strings.Join(inputs, ","), name)
}

func splitEventArgs(args string) []string {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}
	var out []string
	start := 0
	depth := 0
	for i, r := range args {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(args[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(args[start:]))
	return out
}

func configAddresses(addr config.Address) []string {
	// TODO check for pure string case and validate address length
	if len(addr) == 0 {
		return nil
	}
	return addr
}

func uniqueLower(used map[string]int, base string) string {
	if base == "" {
		base = "event"
	}
	count := used[base]
	used[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}

func toSnake(s string) string {
	parts := identifierParts(s)
	if len(parts) == 0 {
		return "event"
	}
	for i := range parts {
		parts[i] = strings.ToLower(parts[i])
	}
	return strings.Join(parts, "_")
}

func identifierParts(s string) []string {
	var parts []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			parts = append(parts, b.String())
			b.Reset()
		}
	}
	var prevLower bool
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if unicode.IsUpper(r) && prevLower {
				flush()
			}
			b.WriteRune(r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
			continue
		}
		flush()
		prevLower = false
	}
	flush()
	return parts
}

func normalizeSolidityType(typ string) string {
	switch typ {
	case "uint":
		return "uint256"
	case "int":
		return "int256"
	default:
		return typ
	}
}

func clickHouseType(solType string) string {
	if strings.Contains(solType, "[") {
		return "String"
	}
	switch {
	case solType == "bool":
		return "UInt8"
	case solType == "address":
		return "FixedString(20)"
	case solType == "bytes32":
		return "FixedString(32)"
	case isBytesN(solType):
		return fmt.Sprintf("FixedString(%d)", bytesNSize(solType))
	case solType == "bytes", solType == "string":
		return "String"
	case strings.HasPrefix(solType, "uint"):
		return "UInt256"
	case strings.HasPrefix(solType, "int"):
		return "Int256"
	default:
		return "String"
	}
}

func isBytesN(solType string) bool {
	if !strings.HasPrefix(solType, "bytes") || solType == "bytes" {
		return false
	}
	n := bytesNSize(solType)
	return n > 0 && n <= 32
}

func bytesNSize(solType string) int {
	var n int
	fmt.Sscanf(strings.TrimPrefix(solType, "bytes"), "%d", &n)
	return n
}
