package ingestion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/franz101/sqd-go/abiunpack"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/envconfig"
	"github.com/franz101/sqd-go/internal/monitoring"
	"github.com/franz101/sqd-go/internal/parser"
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
	StateRestorer      func(blockNumber uint64) error                      // called before fork replay to roll back processor state
	StateLoader        func(ctx context.Context, blockNumber uint64) error // called on startup to load processor state from database
	Processor          Processor                                           // unified processor interface (overrides individual callbacks if set)
	// ColdCache enables a Pebble-backed cold tier under the hot caches (finalized
	// backfill only). It removes the per-miss ClickHouse point-SELECT: an evicted
	// entry is served from local disk, and on a from-genesis run a hot+cold miss is
	// provably new so ClickHouse is skipped entirely. Default off.
	ColdCache    bool
	ColdCacheDir string // base directory for cold-tier files (default os.TempDir()/sqd-coldcache)

	// Prefetch enables the two-pass batch prefetch (see PrefetchProcessor): each
	// block is dispatched once to collect its read-set, resolved in one round-trip
	// per entity, then dispatched again against a warm cache. Collapses the lazy
	// path's one-SELECT-per-missing-key into one SELECT per entity per block. Default
	// off; most useful in resume/cursor mode against a populated ClickHouse.
	Prefetch bool

	// ParallelFetch fetches the finalized backfill range with concurrent range
	// workers (cursor mode only). The immutable finalized region is fetched out
	// of order and re-serialized for the in-order consumer; see parallel_fetch.go.
	ParallelFetch bool

	// ReindexFrom is the block number to reindex from. If set, all blocks with
	// numbers greater than this value are deleted from ClickHouse using lightweight
	// DELETE, and indexing starts from this block. Data at or below this block is
	// preserved.
	ReindexFrom uint64
}

const (
	cursorPollInterval = 5 * time.Second
	statsInterval      = 10 * time.Second

	// Adaptive page sizing: when pageSize=0, grow page size based on performance
	// Target ~20k blocks/second processing rate
	targetBlocksPerSec  = 20000
	minAdaptivePageSize = 5000
	maxAdaptivePageSize = 100000

	// Cursor-mode producers fetch through /finalized-stream while at least
	// this many blocks below the finalized head, switching to the paced live
	// /stream endpoint only for the final approach. Anything at or below the
	// finalized head is immutable, so the margin just prevents endpoint
	// flapping near the boundary.
	finalizedCatchupMargin = 512
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
		if err := processChain(ctx, store, cfg, &chain, opts.PageSize, opts.StartBlock, opts.BlockCount, opts.CursorMode, forkMode, opts.Restart, proc, opts.ColdCache, coldDir, opts.ParallelFetch, opts.ReindexFrom, opts.Prefetch); err != nil {
			log.Printf("chain %d error: %v", chain.ID, err)
		}
	}
	log.Println("Done.")
	return nil
}

func processChain(ctx context.Context, store *database.Store, cfg *config.Config, chain *config.Chain, pageSize, flagStartBlock, blockCountLimit uint64, cursorMode bool, forkMode config.ForkMode, restart bool, proc Processor, coldCache bool, coldDir string, parallelFetch bool, reindexFrom uint64, prefetch bool) error {
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
	if reindexFrom > 0 {
		log.Printf("[REINDEX] Deleting all blocks > %d using lightweight DELETE...", reindexFrom)
		if err := rollbackAfterBlock(ctx, store, forkMode, chain.ID, reindexFrom); err != nil {
			return fmt.Errorf("reindex rollback: %w", err)
		}
		log.Printf("[REINDEX] Lightweight DELETE completed for blocks > %d", reindexFrom)
	}
	currentBlock := chain.StartBlock
	if flagStartBlock > 0 {
		currentBlock = flagStartBlock
	}
	if reindexFrom > 0 && reindexFrom > currentBlock {
		currentBlock = reindexFrom
		log.Printf("[REINDEX] Starting from block %d (from --reindex-from)", currentBlock)
	}
	state := NewForkTracker(forkMode)
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
	if prefetch {
		if pp, ok := proc.(PrefetchProcessor); ok {
			pp.EnablePrefetch(true)
			log.Printf("Chain %d: --prefetch enabled (two-pass batch read-set prefetch)", chain.ID)
		} else {
			log.Printf("Chain %d: prefetch requested but processor does not implement PrefetchProcessor", chain.ID)
		}
	}
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
	// The hot /stream endpoint paces its responses for real-time consumption —
	// fine at the head, but a deep catch-up read through it trickles in at
	// ~200 KB/s (multi-minute fetches, ~26 blk/s). Blocks at or below the
	// finalized head are immutable, so cursor mode fetches them from the
	// un-paced /finalized-stream endpoint and only switches to /stream once
	// the producer is within finalizedCatchupMargin of the finalized head.
	var sqdFinalized *client.Client
	if cursorMode {
		sqdFinalized = client.New(chainEndpoint(chain.ID, false))
		defer sqdFinalized.Close()
	}
	jsonl := parser.NewFastJSONLParser(1024)
	replayBuf := NewReplayBuffer(8192) // ~8K blocks of replay capacity
	// No-data-loss (Invariant 0): if the processor reports a durable commit
	// horizon, the persisted checkpoint is gated so it never leads that horizon.
	// On crash the run resumes from durable state and re-fetches the cheap gap.
	committedReporter, _ := proc.(CommitHorizonReporter)
	flusher, _ := proc.(Flusher)
	processorProfileReporter, _ := proc.(ProcessorProfileReporter)
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
	backfillGC := cursorMode && !snapshotsActive
	var previousGOGC int
	if backfillGC {
		previousGOGC = debug.SetGCPercent(200)
	}
	defer func() {
		if backfillGC {
			debug.SetGCPercent(previousGOGC)
		}
	}()
	fastJSONLProc, fastJSONLOK := proc.(FastJSONLProcessor)
	useParseDecodeV2 := envconfig.ParseDecodeV2Enabled() && fastJSONLOK
	if envconfig.ParseDecodeV2Enabled() && !fastJSONLOK {
		log.Printf("Chain %d: SQD_PARSE_DECODE_V2 requested, but processor does not implement FastJSONLProcessor; using default processor path", chain.ID)
	}
	if useParseDecodeV2 {
		log.Printf("Chain %d: SQD_PARSE_DECODE_V2 enabled for custom processor", chain.ID)
	}
	fastInsertProc, fastInsertOK := proc.(FastJSONLInsertProcessor)
	singleParse := useParseDecodeV2 && fastInsertOK && !storeBlocks && !cfg.ShouldStoreRawLogs() &&
		os.Getenv("SQD_SINGLE_PARSE") != "0"
	batchProc, batchProcOK := proc.(FastBatchParseProcessor)
	batchParse := singleParse && batchProcOK && batchProc.SupportsBatchParse() &&
		os.Getenv("SQD_PRODUCER_PARSE") != "0"
	if batchParse {
		log.Printf("Chain %d: producer-parse pipeline enabled (one generated parse; consumer runs state math)", chain.ID)
	} else if singleParse {
		log.Printf("Chain %d: single-parse pipeline enabled (generated decode feeds state and event inserts)", chain.ID)
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

	// Producer fetch instrumentation, read by logStats for honest reporting. A
	// single dense /finalized-stream request is fetched+parsed whole before any
	// block is emitted, so the consumer can sit idle for the length of one request;
	// surfacing the in-flight fetch keeps a "0 blk/s" stats tick from looking like a
	// stall. producerFetchSinceNanos is the start time of the in-flight sequential
	// fetch (0 = none); producerFetchFrom is the block it started from.
	var producerFetchFrom atomic.Uint64
	var producerFetchSinceNanos atomic.Int64

	type producerSignal struct {
		forkErr *client.ForkError
		err     error
	}
	errChan := make(chan producerSignal, 1)
	finalizedChan := make(chan *client.BlockRef, 100)

	var prodCancel context.CancelFunc
	var producerDone bool

	// Parallel fetch concurrently pulls the finalized backfill range with N range
	// workers, paced by a shared rate limiter (the portal caps ~5 req/s). It only
	// engages in cursor mode, and only for blocks at/below the finalized head:
	// those are immutable, so workers fetch disjoint pages out of order without
	// parent-hash fork detection and a reorder buffer re-serializes them. The
	// producer self-advances across the parallel region just as it does the
	// sequential one. parallelBound is the highest block the prefetcher produces;
	// the consumer uses it to gate the empty-gap skip below.
	//
	// parallelSkipEmpties fetches with includeAllBlocks=false (sparse: matching
	// blocks + the portal's marker), which transfers far less data but leaves
	// block-number gaps. The consumer skips those gaps (see CeilBlock below). It is
	// only safe when the project does not need every block — i.e. it stores neither
	// the raw blocks table nor the raw logs table; otherwise we fetch every block.
	parallelEnabled := parallelFetch && cursorMode
	if parallelFetch && !cursorMode {
		log.Printf("Chain %d: --parallel-fetch requires cursor mode; using sequential fetch", chain.ID)
	}
	parallelWorkers, parallelPageSize, parallelRPS := ParallelFetchSettings()
	parallelLimiter := newRateLimiter(parallelRPS, defaultParallelBurst)
	parallelSkipEmpties := parallelEnabled && !storeBlocks && !cfg.ShouldStoreRawLogs()
	parallelEndpoint := chainEndpoint(chain.ID, false)
	var parallelBound atomic.Uint64
	// Per-fetch latency budget for the dense adaptive path (sequential cursor mode).
	// Bounds how big a single /finalized-stream request can grow so progress stays
	// continuous instead of arriving in multi-second bursts. 0 disables (see
	// SQD_TARGET_FETCH_SECONDS).
	targetFetchDur := resolveTargetFetchDuration()

	var startProd func(startBlk uint64, initialParent string)
	startProd = func(startBlk uint64, initialParent string) {
		if prodCancel != nil {
			prodCancel()
		}
		producerDone = false
		var prodCtx context.Context
		prodCtx, prodCancel = context.WithCancel(ctx)

		go func(pCtx context.Context, pBlock uint64, pHash string) {
			var lastFinalized uint64
			// Adaptive page sizing: when pageSize=0, use this instead of nil
			var adaptivePageSize uint64 = minAdaptivePageSize
			onCatchupEndpoint := false
			// Parallel-fetch state (this producer instance only). prefetch is lazily
			// started once the finalized head is known and the remaining region is
			// large enough; parallelDone latches once it has run (or was declined) so
			// the producer never re-engages it near the head.
			var prefetch *parallelPrefetcher
			var parallelDone bool
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

				// Lazily engage parallel fetch once the finalized head is known and
				// the remaining finalized region is large enough to amortize the
				// coordination. parallelBound is published before any parallel page is
				// produced, so the consumer's gap-skip gate is stable.
				if parallelEnabled && prefetch == nil && !parallelDone {
					bound, ok := parallelFinalizedBound(cursorMode, pBlock, lastFinalized, effectiveEndBlock)
					switch {
					case ok && bound >= pBlock+parallelMinSpan(parallelPageSize, parallelWorkers):
						prefetch = newParallelPrefetcher(parallelEndpoint, filters, !parallelSkipEmpties, pBlock, bound, uint64(parallelPageSize), parallelWorkers, parallelLimiter)
						parallelBound.Store(bound)
						prefetch.launch(pCtx)
						log.Printf("Chain %d: parallel fetch engaged for finalized backfill [%d-%d] (%d workers, ~%.0f req/s, skipEmpties=%v)", chain.ID, pBlock, bound, parallelWorkers, parallelRPS, parallelSkipEmpties)
					case cursorMode && lastFinalized == 0:
						// Finalized head not learned yet — the first sequential fetch
						// will populate it. Retry on the next iteration; don't latch.
					default:
						// Region too small, or unbounded/non-engageable: a permanent
						// decision, so stop checking and stay sequential.
						parallelDone = true
					}
				}

				var response client.Response
				var rangeLabel string
				var fromPrefetch bool
				var prefetchedTo uint64
				var requestedTo uint64
				var err error
				var lastFetchDur time.Duration
				if prefetch != nil {
					// Attribute the consumer's wait for the prefetcher to deliver the next
					// page to fetch time: ~0 when prefetch keeps up (fetch not limiting), >0
					// when it stalls. Previously invisible (fetch=0s) since background prefetch
					// work never touched profFetchNanos.
					fetchT0 := time.Now()
					page, ok := prefetch.Next(pCtx)
					profFetchNanos.Add(int64(time.Since(fetchT0)))
					if !ok {
						// Region fully emitted (or cancelled/stalled): resume sequential
						// from the current cursor, which the last consumed page advanced.
						prefetch = nil
						parallelDone = true
						if pCtx.Err() != nil {
							return
						}
						log.Printf("Chain %d: parallel fetch finished; resuming sequential fetch at block %d", chain.ID, pBlock)
						continue
					}
					if page.err != nil {
						if pCtx.Err() != nil {
							return
						}
						log.Printf("Chain %d: parallel fetch error at [%d-%d]: %v; resuming sequential fetch", chain.ID, page.from, page.coveredTo, page.err)
						prefetch = nil
						parallelDone = true
						pBlock = page.from
						pHash = ""
						continue
					}
					pBlock = page.from
					pHash = "" // finalized region: no parent-hash chaining
					response = client.Response{Raw: page.raw, Head: page.head}
					rangeLabel = fmt.Sprintf("[%d-%d]", page.from, page.coveredTo)
					prefetchedTo = page.coveredTo
					fromPrefetch = true
				} else {
					toBlockPtr, label, ok := nextProducerRequestRange(pBlock, pageSize, adaptivePageSize, lastFinalized, effectiveEndBlock, cursorMode)
					if !ok {
						return
					}
					rangeLabel = label
					if toBlockPtr != nil {
						requestedTo = *toBlockPtr
					}

					// Catch-up fetches go through /finalized-stream; the request shape
					// (parentBlockHash, includeAllBlocks) stays identical, so fork
					// detection and the dense replay buffer are unaffected.
					fetchClient := sqd
					if sqdFinalized != nil && lastFinalized > 0 && pBlock+finalizedCatchupMargin <= lastFinalized {
						fetchClient = sqdFinalized
						if !onCatchupEndpoint {
							onCatchupEndpoint = true
							log.Printf("Chain %d: catch-up fetch via finalized-stream (block %d, finalized head %d)", chain.ID, pBlock, lastFinalized)
						}
					} else if onCatchupEndpoint {
						onCatchupEndpoint = false
						log.Printf("Chain %d: within %d of finalized head %d — fetching via live stream", chain.ID, finalizedCatchupMargin, lastFinalized)
					}

					t0 := time.Now()
					producerFetchFrom.Store(pBlock)
					producerFetchSinceNanos.Store(t0.UnixNano())
					response, err = fetchClient.FetchWithParent(pCtx, pBlock, toBlockPtr, pHash, cursorMode, filters)
					lastFetchDur = time.Since(t0)
					producerFetchSinceNanos.Store(0)
					profFetchNanos.Add(int64(lastFetchDur))

					if err != nil {
						var forkErr *client.ForkError
						if cursorMode && errors.As(err, &forkErr) {
							sendSignal(producerSignal{forkErr: forkErr})
							return
						}
						log.Printf("Chain %d: fetch %s error: %v, retrying...", chain.ID, rangeLabel, err)
						// Adaptive paging back-off: a too-large range can reject/time out, so
						// halve the page before retrying (binary-search down). Only affects the
						// request ceiling — pBlock is unchanged, so no block is skipped.
						if pageSize == 0 {
							adaptivePageSize = nextPageSize(adaptivePageSize, adaptivePageSize, 0, true, minAdaptivePageSize, maxAdaptivePageSize)
						}
						select {
						case <-pCtx.Done():
							return
						case <-time.After(5 * time.Second):
						}
						continue
					}
				}

				if response.Head.Finalized != nil && response.Head.Finalized.Number > lastFinalized {
					lastFinalized = response.Head.Finalized.Number
				}

				raw := response.Raw
				if len(raw) == 0 {
					if fromPrefetch {
						// Empty page (region ran past available data): self-advance;
						// the next Next() drains and finishes the prefetcher.
						pBlock = prefetchedTo + 1
						continue
					}
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
				if batchParse {
					// Finalized pages cannot participate in fork replay, so their
					// raw lines need not survive this parse call. Near the live
					// head retain one owned page for replay safety.
					parseRaw := raw
					retainRawLines := true
					if response.Head.Finalized != nil {
						var requestedEnd uint64
						switch {
						case fromPrefetch:
							requestedEnd = prefetchedTo
						default:
							requestedEnd = requestedTo
						}
						if requestedEnd > 0 && requestedEnd <= response.Head.Finalized.Number {
							retainRawLines = false
						}
					}
					if retainRawLines {
						parseRaw = retainReplayJSONLPage(raw)
					}
					var endBlock uint64
					if effectiveEndBlock != nil {
						endBlock = *effectiveEndBlock
					}
					batchStartBlock := pBlock
					var pending BatchParsedBlock
					var havePending bool
					eventCount, batchFlush, parseErr := batchProc.ParseBatchForInserts(store, parseRaw, endBlock, func(parsed BatchParsedBlock) error {
						if !retainRawLines {
							parsed.RawLine = nil
						}
						for {
							consumerBlock := currentConsumerBlock.Load()
							if parsed.Number >= consumerBlock && parsed.Number-consumerBlock >= uint64(replayBuf.capacity)-100 {
								waitStart := time.Now()
								select {
								case <-pCtx.Done():
									profProducerBackpressureNanos.Add(int64(time.Since(waitStart)))
									return pCtx.Err()
								case <-time.After(10 * time.Millisecond):
								}
								profProducerBackpressureNanos.Add(int64(time.Since(waitStart)))
								continue
							}
							break
						}
						if havePending {
							replayBuf.WriteParsed(chain.ID, pending, response.Head.Finalized, false, rangeLabel, batchStartBlock, nil, 0)
							pHash = pending.Hash
						}
						pending = parsed
						havePending = true
						return nil
					})
					if parseErr != nil {
						if pCtx.Err() != nil {
							return
						}
						sendSignal(producerSignal{err: parseErr})
						return
					}
					profParseNanos.Add(int64(time.Since(parseStart)))
					profIters.Add(1)
					if havePending {
						replayBuf.WriteParsed(chain.ID, pending, response.Head.Finalized, true, rangeLabel, batchStartBlock, batchFlush, eventCount)
						pHash = pending.Hash
						if fromPrefetch {
							pBlock = prefetchedTo + 1
							continue
						}
						next := pending.Number + 1
						if pageSize == 0 && next > batchStartBlock {
							span := next - batchStartBlock
							adaptivePageSize = nextPageSize(adaptivePageSize, adaptivePageSize, span, false, minAdaptivePageSize, maxAdaptivePageSize)
							adaptivePageSize = clampPageForLatency(adaptivePageSize, span, lastFetchDur, targetFetchDur, minAdaptivePageSize)
						}
						pBlock = next
					} else {
						if batchFlush != nil {
							_ = batchFlush(pCtx)
						}
						if fromPrefetch {
							pBlock = prefetchedTo + 1
						} else {
							pBlock++
						}
					}
					continue
				}
				var decodeDur time.Duration
				var decodedBlocks []decodedBlock
				var dataScratch []byte
				rawForParse := raw
				if useParseDecodeV2 {
					// Portal/client response buffers may be reused as soon as the
					// producer starts its next fetch. Replay entries outlive that
					// fetch, so retain one owned copy per page and let each rawLine
					// slice reference it. Copying the page once avoids both
					// corruption and one allocation per block.
					rawForParse = retainReplayJSONLPage(raw)
				}
				if singleParse {
					err = jsonl.ScanHeadersWithLine(rawForParse, func(number, timestamp uint64, hash string, rawLine []byte) error {
						if effectiveEndBlock != nil && number > *effectiveEndBlock {
							return nil
						}
						decodedBlocks = append(decodedBlocks, decodedBlock{
							number:    number,
							hash:      strings.Clone(hash),
							timestamp: time.Unix(int64(timestamp), 0).UTC(),
							raw:       rawLine,
						})
						return nil
					})
				} else {
					err = jsonl.ParseWithLine(rawForParse, func(block *parser.Block, rawLine []byte) error {
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
				}
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

				if fromPrefetch {
					// Parallel mode advances to the grid-page boundary (the prefetcher
					// controls paging, so skip the sequential adaptive-page feedback). The
					// last decoded block may be below prefetchedTo on a sparse tail; the
					// page boundary is authoritative. pHash was tracked by the parse loop
					// above for the eventual sequential handoff.
					pBlock = prefetchedTo + 1
					continue
				}
				if len(decodedBlocks) > 0 {
					// Self-advance: the producer just wrote every block of this page to
					// the replay buffer and tracked the last block's hash in pHash, so
					// it can fetch the next page immediately instead of blocking on a
					// per-page handshake with the consumer. Goroutine sampling on the v2
					// line showed that handshake left the producer idle ~50% of wall
					// time (fetch and consume fully serialized at page boundaries). The
					// per-block replay-buffer backpressure above already bounds how far
					// ahead the producer can run, so it can never lap the consumer.
					next := decodedBlocks[len(decodedBlocks)-1].number + 1
					// Adaptive paging: next - batchStartBlock is the block span this
					// request actually covered. Feed it back so the page grows toward the
					// portal's cursor cap on a sparse range (far fewer round-trips across
					// the empty pre-deployment prefix) and tracks it on a dense one. Only
					// the request ceiling moves; pBlock is authoritative, so no block is
					// ever skipped.
					if pageSize == 0 && next > batchStartBlock {
						span := next - batchStartBlock
						adaptivePageSize = nextPageSize(adaptivePageSize, adaptivePageSize, span, false, minAdaptivePageSize, maxAdaptivePageSize)
						// Bound the page by how long the request actually took: a fetch
						// that ran past the latency budget gets a smaller next page so the
						// consumer doesn't starve between multi-second bursts. Only shrinks,
						// and only on the slow dense path (a fast sparse fetch is untouched).
						adaptivePageSize = clampPageForLatency(adaptivePageSize, span, lastFetchDur, targetFetchDur, minAdaptivePageSize)
					}
					pBlock = next
				} else {
					// No blocks decoded, advance by 1 to retry
					pBlock++
				}
			}
		}(prodCtx, startBlk, initialParent)
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

	statsTicker := time.NewTicker(resolveStatsInterval())
	defer statsTicker.Stop()
	lastStatsTime := startTime
	lastStatsBlocks := uint64(0)
	lastStatsEvents := uint64(0)
	lastProfileTotals := profileTotals{}
	lastProcessorProfile := ProcessorProfile{}
	// Chain-coverage tracking. The consumer cursor (currentConsumerBlockVal)
	// advances on every block AND every confirmed-empty gap-skip (sparse
	// --parallel-fetch), so its delta is the honest, mode-independent progress
	// rate; totalBlocks counts only blocks physically present in the buffer and
	// thus under-reports sparse mode by the empty-block ratio.
	startConsumerBlock := currentBlock
	lastStatsConsumer := currentBlock
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
		// Chain blocks covered this interval (and overall), including empty gaps the
		// consumer skipped over. This is the "blk/s" an operator should compare across
		// dense and sparse modes.
		var coverage uint64
		if currentConsumerBlockVal > lastStatsConsumer {
			coverage = currentConsumerBlockVal - lastStatsConsumer
		}
		coverageRate := float64(coverage) / interval.Seconds()
		avgCoverageRate := float64(currentConsumerBlockVal-startConsumerBlock) / elapsed.Seconds()
		// A "0 covered" tick during an in-flight fetch is a long single request, not
		// idleness — surface it so a deep dense backfill doesn't look stuck.
		fetchNote := ""
		if since := producerFetchSinceNanos.Load(); since != 0 {
			if inflight := now.Sub(time.Unix(0, since)); inflight >= time.Second {
				fetchNote = fmt.Sprintf(" | fetching from %d (%s)", producerFetchFrom.Load(), inflight.Round(time.Second))
			}
		}
		log.Printf("Chain %d: stats %s | checkpoint: %d | next: %d | buffered: %d | +%d blocks, %.0f blk/s (avg %.0f) | +%d non-empty, +%d events in %s%s | total: %d blocks, %d events",
			chain.ID, reason, lastCheckpoint, currentConsumerBlockVal, replayBuf.Len(),
			coverage, coverageRate, avgCoverageRate,
			deltaBlocks, deltaEvents, interval.Round(time.Millisecond), fetchNote,
			totalBlockCount, totalEventCount)
		currentProfileTotals := profileTotals{
			fetch:                time.Duration(profFetchNanos.Load()),
			parse:                time.Duration(profParseNanos.Load()),
			decode:               time.Duration(profDecodeNanos.Load()),
			marshal:              time.Duration(profMarshalNanos.Load()),
			insert:               time.Duration(profInsertNanos.Load()),
			custom:               time.Duration(profCustomNanos.Load()),
			consumerWait:         time.Duration(profConsumerWaitNanos.Load()),
			producerBackpressure: time.Duration(profProducerBackpressureNanos.Load()),
			iterations:           profIters.Load(),
		}
		profileDelta := currentProfileTotals.delta(lastProfileTotals)
		log.Printf("Chain %d: profile %s | fetch=%s parse=%s decode=%s marshal=%s insert=%s custom=%s | consumer_wait=%s producer_backpressure=%s | parse_iterations=%d",
			chain.ID, reason,
			profileDelta.fetch.Round(time.Millisecond),
			profileDelta.parse.Round(time.Millisecond),
			profileDelta.decode.Round(time.Millisecond),
			profileDelta.marshal.Round(time.Millisecond),
			profileDelta.insert.Round(time.Millisecond),
			profileDelta.custom.Round(time.Millisecond),
			profileDelta.consumerWait.Round(time.Millisecond),
			profileDelta.producerBackpressure.Round(time.Millisecond),
			profileDelta.iterations)
		lastProfileTotals = currentProfileTotals
		if processorProfileReporter != nil {
			currentProcessorProfile := processorProfileReporter.ProcessorProfile()
			processorDelta := currentProcessorProfile.Delta(lastProcessorProfile)
			log.Printf("Chain %d: processor profile %s | condition_resolve=%s condition_round_trips=%d | fpmm_resolve=%s fpmm_round_trips=%d",
				chain.ID, reason,
				processorDelta.ConditionResolveDuration.Round(time.Millisecond),
				processorDelta.ConditionRoundTrips,
				processorDelta.FPMMResolveDuration.Round(time.Millisecond),
				processorDelta.FPMMRoundTrips)
			lastProcessorProfile = currentProcessorProfile
		}
		lastStatsTime = now
		lastStatsBlocks = totalBlockCount
		lastStatsEvents = totalEventCount
		lastStatsConsumer = currentConsumerBlockVal
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
		pendingBatchBlocks = 0
	}
	type pendingCursorInsert struct {
		done      chan error
		syncState database.SyncState
		block     uint64
	}
	var pendingInsert *pendingCursorInsert
	drainPendingInsert := func() error {
		if pendingInsert == nil {
			return nil
		}
		pending := pendingInsert
		pendingInsert = nil
		if err := <-pending.done; err != nil {
			return err
		}
		if err := store.SaveSyncState(ctx, chain.ID, pending.syncState); err != nil {
			return fmt.Errorf("update sync state %d: %w", pending.block, err)
		}
		if pending.block%10 == 0 {
			if err := store.TruncateSyncState(ctx, chain.ID, pending.block); err != nil {
				log.Printf("Chain %d: truncate sync state error: %v", chain.ID, err)
			}
		}
		lastCheckpoint = pending.block
		return nil
	}
	kickPendingInsert := func(flush func(context.Context) error, syncState database.SyncState, block uint64) {
		done := make(chan error, 1)
		pendingInsert = &pendingCursorInsert{done: done, syncState: syncState, block: block}
		go func() {
			insertStart := time.Now()
			var err error
			if flush != nil {
				err = flush(ctx)
			}
			profInsertNanos.Add(int64(time.Since(insertStart)))
			done <- err
		}()
	}
	defer func() {
		if pendingInsert != nil {
			<-pendingInsert.done
		}
	}()
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
			if batchParse {
				if entry.proto != nil {
					procStart := time.Now()
					if err := batchProc.ProcessParsedBlock(ctx, store, entry.proto); err != nil {
						return fmt.Errorf("producer-parsed processor error at block %d: %w", entry.number, err)
					}
					profCustomNanos.Add(int64(time.Since(procStart)))
				}
			} else if useParseDecodeV2 {
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
						if backfillGC {
							debug.SetGCPercent(previousGOGC)
							backfillGC = false
						}
						log.Printf("Chain %d: snapshots enabled — consumer block %d is within %d of finalized head %d",
							chain.ID, entry.number, snapshotEnableMargin, entry.finalized.Number)
					}
				}
			}

			atomic.AddUint64(&totalEvents, uint64(len(entry.events)))
			atomic.AddUint64(&totalBlocks, 1)
			pendingBatchBlocks++

			if entry.isLastInBatch {
				if batchParse {
					atomic.AddUint64(&totalEvents, entry.batchEvents)
					if cursorMode {
						if err := drainPendingInsert(); err != nil {
							return fmt.Errorf("previous producer-parse event insert: %w", err)
						}
						current := state.Current()
						if current != nil {
							kickPendingInsert(entry.batchFlush, forkSyncState(state, current), entry.number)
						}
					} else if entry.batchFlush != nil {
						insertStart := time.Now()
						if err := entry.batchFlush(ctx); err != nil {
							return fmt.Errorf("producer-parse event insert: %w", err)
						}
						profInsertNanos.Add(int64(time.Since(insertStart)))
					}
				} else if singleParse {
					if len(batchRawJSONL) > 0 {
						procStart := time.Now()
						eventCount, flush, err := fastInsertProc.ProcessJSONLWithInserts(ctx, store, batchRawJSONL)
						profCustomNanos.Add(int64(time.Since(procStart)))
						if err != nil {
							return fmt.Errorf("single-parse processor error: %w", err)
						}
						atomic.AddUint64(&totalEvents, eventCount)
						if flush != nil {
							insertStart := time.Now()
							if err := flush(ctx); err != nil {
								return fmt.Errorf("single-parse event insert: %w", err)
							}
							profInsertNanos.Add(int64(time.Since(insertStart)))
						}
					}
				} else {
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

					// The producer self-advances (it tracked the last block's hash and
					// kept fetching), so there is no per-page advance handshake to send
					// here — the consumer just proceeds to the custom processor.

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
				}

				if cursorMode && !batchParse {
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

				if !cursorMode || !batchParse {
					lastCheckpoint = entry.number
				}
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

		// Sparse parallel backfill (includeAllBlocks=false) leaves block-number
		// gaps: blocks with no matching logs are absent. When the missing block is
		// within the parallel region and at or below the scanned high-water mark
		// (latestBlock — the portal returns a marker block at the end of every
		// scanned response), the gap is confirmed-empty and was never fetched, so
		// jump to the next present block instead of waiting forever for it.
		if parallelSkipEmpties {
			c := currentConsumerBlockVal
			if pb := parallelBound.Load(); pb != 0 && c <= pb && c <= replayBuf.LatestBlock() {
				if next, ok := replayBuf.CeilBlock(c); ok {
					currentConsumerBlockVal = next
					currentConsumerBlock.Store(currentConsumerBlockVal)
					continue
				}
			}
		}

		if producerDone {
			// Checked the buffer, it's empty, and the producer has completed cleanly
			if err := drainPendingInsert(); err != nil {
				return fmt.Errorf("final producer-parse event insert: %w", err)
			}
			break
		}

		waitStart := time.Now()
		select {
		case <-ctx.Done():
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			log.Printf("Chain %d: interrupted at block %d", chain.ID, currentConsumerBlockVal)
			_ = drainPendingInsert()
			return ctx.Err()

		case sig := <-errChan:
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			if sig.forkErr != nil {
				if err := drainPendingInsert(); err != nil {
					return fmt.Errorf("drain producer-parse event insert before fork rollback: %w", err)
				}
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
					if cc, ok := proc.(ColdCacheProcessor); ok {
						_ = cc.CloseColdCache()
					}
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
							if cc, ok := proc.(ColdCacheProcessor); ok {
								_ = cc.CloseColdCache()
							}
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
				if batchParse {
					batchProc.ReclaimParseBatches()
				}
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
				if err := drainPendingInsert(); err != nil {
					return fmt.Errorf("drain producer-parse event insert after producer error: %w", err)
				}
				return fmt.Errorf("producer error: %w", sig.err)
			}

			// Clean exit of producer
			producerDone = true

		case finalized := <-finalizedChan:
			profConsumerWaitNanos.Add(int64(time.Since(waitStart)))
			if err := drainPendingInsert(); err != nil {
				return fmt.Errorf("drain producer-parse event insert at live tail: %w", err)
			}
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

	// Emit a final stats line on clean completion so the last cursor position and
	// full chain coverage are recorded, not just left in the profile below (periodic
	// ticks can miss the final advance on a fast backfill).
	logStats("final")

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

type profileTotals struct {
	fetch                time.Duration
	parse                time.Duration
	decode               time.Duration
	marshal              time.Duration
	insert               time.Duration
	custom               time.Duration
	consumerWait         time.Duration
	producerBackpressure time.Duration
	iterations           int64
}

func (p profileTotals) delta(previous profileTotals) profileTotals {
	return profileTotals{
		fetch:                nonNegativeDurationDelta(p.fetch, previous.fetch),
		parse:                nonNegativeDurationDelta(p.parse, previous.parse),
		decode:               nonNegativeDurationDelta(p.decode, previous.decode),
		marshal:              nonNegativeDurationDelta(p.marshal, previous.marshal),
		insert:               nonNegativeDurationDelta(p.insert, previous.insert),
		custom:               nonNegativeDurationDelta(p.custom, previous.custom),
		consumerWait:         nonNegativeDurationDelta(p.consumerWait, previous.consumerWait),
		producerBackpressure: nonNegativeDurationDelta(p.producerBackpressure, previous.producerBackpressure),
		iterations:           maxInt64(p.iterations-previous.iterations, 0),
	}
}

func nonNegativeDurationDelta(current, previous time.Duration) time.Duration {
	if current < previous {
		return 0
	}
	return current - previous
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
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
	if v := envconfig.PortalEndpointURL(); v != "" {
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
	return store.SaveSyncState(ctx, chainID, forkSyncState(state, current))
}

func forkSyncState(state ForkTracker, current *client.BlockRef) database.SyncState {
	finalized := state.FinalizedHighWatermark()
	return database.SyncState{
		Current:       blockRefToSyncCursor(*current),
		Finalized:     blockRefPtrToSyncCursor(finalized),
		RollbackChain: blockRefsToSyncCursors(filterUnfinalizedRollbackChain(state.RecentUnfinalizedBlocks(), finalized)),
	}
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

func retainReplayJSONLPage(raw []byte) []byte {
	return bytes.Clone(raw)
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
			// Derive the table name exactly like codegen's buildEventSpecs: honor
			// an explicit `name:` override. Without this, an overridden event
			// (e.g. ExchangeV2.OrderFilled named OrderFilledV2) resolves to a table
			// name that schema.sql never created -> UNKNOWN_TABLE on insert (the
			// crash at exchange_v2_order_filled_events). The index itself stays
			// keyed by the base event name, which is what decoded logs carry and
			// what lookup() receives.
			tableNameEvent := name
			if event.Name != nil && strings.TrimSpace(*event.Name) != "" {
				tableNameEvent = strings.TrimSpace(*event.Name)
			}
			viewName := uniqueLower(used, toSnake(contract.Name+"_"+tableNameEvent))

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
