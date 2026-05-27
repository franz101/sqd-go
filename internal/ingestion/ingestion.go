package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/parser"
)

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
	StateLoader        func(blockNumber uint64) error // called on startup to load processor state from database
	Processor          Processor                      // unified processor interface (overrides individual callbacks if set)
}

const cursorPollInterval = 5 * time.Second

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

type CustomProcessor func(ctx context.Context, store *database.Store, logs []CustomLog) error

func Run(ctx context.Context, cfg *config.Config, opts Options) error {
	// Resolve effective processor: use Processor interface if set, otherwise fall back to callbacks
	proc := opts.Processor
	if proc == nil {
		proc = ProcessorFunc{
			ProcessFn:        opts.CustomProcessor,
			RestoreToBlockFn: opts.StateRestorer,
			LoadFromDBFn:     opts.StateLoader,
		}
	}
	if opts.Restart {
		if err := database.DropClickHouseDatabase(ctx, opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseUser, opts.ClickHousePassword, opts.ClickHouseDatabase); err != nil {
			return fmt.Errorf("drop clickhouse database: %w", err)
		}
	}
	store, err := database.NewClickHouse(ctx, opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseUser, opts.ClickHousePassword, opts.ClickHouseDatabase)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer store.Close()

	forkMode := opts.ForkMode
	if forkMode == "" {
		forkMode = cfg.ForkMode()
	}
	if !forkMode.Valid() {
		return fmt.Errorf("fork mode must be default")
	}

	if opts.GeneratedSQLDir != "" {
		if err := store.ApplySQLFile(ctx, filepath.Join(opts.GeneratedSQLDir, "schema.sql")); err != nil {
			return fmt.Errorf("apply generated schema: %w", err)
		}
		if err := store.ApplySQLFile(ctx, filepath.Join(opts.GeneratedSQLDir, "views.sql")); err != nil {
			return fmt.Errorf("apply generated views: %w", err)
		}
	} else if err := store.EnsureTablesWithCollapsing(ctx, forkMode.UsesCollapsingMergeTree()); err != nil {
		return fmt.Errorf("ensure tables: %w", err)
	}
	log.Printf("ClickHouse connected: %s:%d/%s (fork=%s)", opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseDatabase, forkMode)

	for _, chain := range cfg.Chains {
		if err := processChain(ctx, store, &chain, opts.PageSize, opts.StartBlock, opts.BlockCount, opts.CursorMode, forkMode, proc); err != nil {
			log.Printf("chain %d error: %v", chain.ID, err)
		}
	}
	log.Println("Done.")
	return nil
}

func processChain(ctx context.Context, store *database.Store, chain *config.Chain, pageSize, flagStartBlock, blockCountLimit uint64, cursorMode bool, forkMode config.ForkMode, proc Processor) error {
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

	currentBlock := chain.StartBlock
	if flagStartBlock > 0 {
		currentBlock = flagStartBlock
	}
	state := NewForkTracker(forkMode)
	if cursorMode {
		saved, hasSaved, err := store.LastSyncState(ctx, chain.ID)
		if err != nil {
			return fmt.Errorf("read sync state: %w", err)
		}
		if recovery, ok := selectRecoveryBase(saved, hasSaved); ok {
			state.Init(recovery.TrackerCurrent, recovery.TrackerFinalized, recovery.TrackerRollbackChain)
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
			if err := proc.LoadFromDatabase(recovery.Number); err != nil {
				log.Printf("[LOAD STATE] State load from ClickHouse at block %d failed: %v (will rebuild from events)", recovery.Number, err)
			} else {
				log.Printf("[LOAD STATE] Processor state loaded successfully from ClickHouse database at block %d", recovery.Number)
			}
		}
	} else {
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
		}
	}
	effectiveEndBlock := chain.EndBlock
	if blockCountLimit > 0 {
		end := currentBlock + blockCountLimit - 1
		effectiveEndBlock = minEndBlock(effectiveEndBlock, end)
	}
	if cursorMode {
		if effectiveEndBlock != nil {
			log.Printf("Chain %d: starting from block %d (cursor mode, local stop at %d)", chain.ID, currentBlock, *effectiveEndBlock)
		} else {
			log.Printf("Chain %d: starting from block %d (cursor mode)", chain.ID, currentBlock)
		}
	} else {
		if pageSize == 0 {
			pageSize = 1000
		}
		log.Printf("Chain %d: starting from block %d", chain.ID, currentBlock)
	}

	sqd := client.New(chainEndpoint(chain.ID, cursorMode))
	defer sqd.Close()
	jsonl := parser.NewFastJSONLParser(1024)
	replayBuf := NewReplayBuffer(8192) // ~8K blocks of replay capacity
	totalBlocks, totalEvents := uint64(0), uint64(0)
	startTime := time.Now()
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// profiling accumulators
	var profFetch, profParse, profDecode, profMarshal, profInsert time.Duration
	profIters := 0

	baseInserter := store.NewInserter()
	typedInserters := make(map[string]*database.TypedInserter)
	for _, table := range typedTables.byEvent {
		typedInserters[table.Name] = store.NewTypedInserter(table)
	}
	for _, table := range typedTables.byAddressEvent {
		typedInserters[table.Name] = store.NewTypedInserter(table)
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("Chain %d: interrupted at block %d", chain.ID, currentBlock)
			// print profile
			runtime.ReadMemStats(&memAfter)
			printProfile(profFetch, profParse, profDecode, profMarshal, profInsert, profIters, totalBlocks, totalEvents, startTime, memBefore, memAfter)
			return ctx.Err()
		default:
		}

		requestStartBlock := currentBlock
		toBlockPtr, rangeLabel, ok := nextRequestRange(currentBlock, pageSize, effectiveEndBlock, cursorMode)
		if !ok {
			break
		}
		toBlock := uint64(0)
		if toBlockPtr != nil {
			toBlock = *toBlockPtr
		}

		fetchStart := time.Now()
		parentHash := ""
		if cursorMode {
			if head := state.Head(); head != nil {
				parentHash = head.Hash
			}
		}
		response, err := sqd.FetchWithParent(ctx, currentBlock, toBlockPtr, parentHash, cursorMode, filters)
		profFetch += time.Since(fetchStart)
		if err != nil {
			var forkErr *client.ForkError
			if cursorMode && errors.As(err, &forkErr) {
				log.Printf("[FORK DETECTED] Chain %d: fork detected at block %d! Previous blocks sent by portal: %v", chain.ID, currentBlock, forkErr.PreviousBlocks)
				safe, ok := state.HandleFork(forkErr.PreviousBlocks)
				if !ok {
					return fmt.Errorf("process fork at %d: unable to find common fork cursor", currentBlock)
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

				// Restore custom processor state before fetching canonical blocks again.
				log.Printf("[LOAD STATE] Restoring processor state to safe block %d...", safe.Number)
				if err := proc.RestoreToBlock(safe.Number); err != nil {
					log.Printf("[LOAD STATE] Processor state restore to block %d failed: %v. Attempting fallback state load from ClickHouse database...", safe.Number, err)
					if err := proc.LoadFromDatabase(safe.Number); err != nil {
						return fmt.Errorf("restore processor state after fork at %d: %w", safe.Number, err)
					} else {
						log.Printf("[LOAD STATE] Processor state loaded successfully from ClickHouse database at block %d", safe.Number)
					}
				} else {
					log.Printf("[LOAD STATE] Processor state restored successfully to safe block %d", safe.Number)
				}

				currentBlock = safe.Number + 1
				log.Printf("[FORK RECOVERY] Resuming canonical fetch from block %d with parent hash %s", currentBlock, safe.Hash)
				continue
			}
			log.Printf("Chain %d: fetch %s error: %v", chain.ID, rangeLabel, err)
			time.Sleep(5 * time.Second)
			continue
		}
		raw := response.Raw

		var decodedEvents []parser.DecodedEvent
		responseBlockCount := uint64(0)
		eventBlockCount := uint64(0)

		lastProcessed := toBlock
		if len(raw) == 0 {
			if cursorMode {
				if !shouldWaitForEmptyCursorResponse(effectiveEndBlock) {
					endBlock := *effectiveEndBlock
					log.Printf("Chain %d: empty response %s, reached end block %d, stopping", chain.ID, rangeLabel, endBlock)
					break
				}
				state.ApplyBatch(response.Head.Finalized, nil)
				if current := state.Current(); current != nil {
					if err := saveForkState(ctx, store, chain.ID, state, current); err != nil {
						return fmt.Errorf("update sync state %d: %w", current.Number, err)
					}
				}
				log.Printf("Chain %d: empty response %s, waiting for new blocks...", chain.ID, rangeLabel)
				if err := waitForNextCursorPoll(ctx, cursorPollInterval); err != nil {
					log.Printf("Chain %d: interrupted at block %d", chain.ID, currentBlock)
					runtime.ReadMemStats(&memAfter)
					printProfile(profFetch, profParse, profDecode, profMarshal, profInsert, profIters, totalBlocks, totalEvents, startTime, memBefore, memAfter)
					return err
				}
				continue
			}
			if err := store.UpdateSyncState(ctx, chain.ID, lastProcessed); err != nil {
				return fmt.Errorf("update sync state %d: %w", lastProcessed, err)
			}
			log.Printf("Chain %d: empty response %s, advancing", chain.ID, rangeLabel)
		} else {
			typedEvents := make(map[string][]parser.DecodedEvent)
			typedSpecs := make(map[string]database.TypedEventTable)
			var customLogs []CustomLog
			var blockRows []database.BlockRow
			var blockRefs []client.BlockRef
			maxSeenBlock := uint64(0)
			seenBeyondEnd := false

			parseStart := time.Now()
			var decodeDur, marshalDur time.Duration
			err = jsonl.Parse(raw, func(block *parser.Block) error {
				if block.Header.Number > maxSeenBlock {
					maxSeenBlock = block.Header.Number
				}
				if effectiveEndBlock != nil && block.Header.Number > *effectiveEndBlock {
					seenBeyondEnd = true
					return nil
				}
				blockRefs = append(blockRefs, client.BlockRef{
					Number: block.Header.Number,
					Hash:   strings.Clone(block.Header.Hash),
				})
				responseBlockCount++
				blockHash := strings.Clone(block.Header.Hash)
				blockTS := time.Unix(int64(block.Header.Timestamp), 0).UTC()
				blockRow := database.BlockRow{
					ChainID:        chain.ID,
					BlockNumber:    block.Header.Number,
					BlockTimestamp: blockTS,
					BlockHash:      blockHash,
				}
				blockHasEvents := false
				var blockEvents []parser.DecodedEvent
				var blockCustomLogs []CustomLog

				for _, lg := range block.Logs {
					if len(lg.Topics) == 0 {
						continue
					}
					d0 := time.Now()
					topic0 := common.HexToHash(lg.Topics[0])
					def, ok := decoders[topic0]
					if !ok {
						decodeDur += time.Since(d0)
						continue
					}
					if !def.MatchesAddress(lg.Address) {
						decodeDur += time.Since(d0)
						continue
					}
					ev, err := def.Decode(lg.Address, lg.Topics, common.FromHex(lg.Data))
					decodeDur += time.Since(d0)
					if err != nil {
						log.Printf("decode %s log in block %d: %v", def.EventName(), block.Header.Number, err)
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
					decodedEvents = append(decodedEvents, *ev)
					blockEvents = append(blockEvents, *ev)
					{
						customLogs = append(customLogs, CustomLog{
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
					blockHasEvents = true
					if table, ok := typedTables.lookup(ev.Address, ev.EventName); ok {
						typedEvents[table.Name] = append(typedEvents[table.Name], *ev)
						typedSpecs[table.Name] = table
					}
				}
				if blockHasEvents {
					blockRows = append(blockRows, blockRow)
					eventBlockCount++
				}
				replayBuf.Write(chain.ID, block.Header.Number, blockHash, blockTS, blockEvents, blockCustomLogs)
				return nil
			})
			profParse += time.Since(parseStart)
			profDecode += decodeDur
			profMarshal += marshalDur
			profIters++

			if err != nil {
				log.Printf("Chain %d: parse %s error: %v", chain.ID, rangeLabel, err)
				time.Sleep(5 * time.Second)
				continue
			}

			// Sequential ClickHouse insertion using pre-allocated inserter instances
			insertStart := time.Now()
			var firstErr error

			// 1. Logs insertion
			if len(decodedEvents) > 0 {
				if err := baseInserter.InsertLogs(ctx, decodedEvents); err != nil {
					firstErr = fmt.Errorf("InsertLogs: %w", err)
				}
			}

			// 2. Typed logs insertions
			if firstErr == nil {
				for tableName, events := range typedEvents {
					inserter := typedInserters[tableName]
					if inserter == nil {
						firstErr = fmt.Errorf("missing TypedInserter for %s", tableName)
						break
					}
					if err := inserter.Insert(ctx, events); err != nil {
						firstErr = fmt.Errorf("InsertTypedLogs(%s): %w", tableName, err)
						break
					}
				}
			}

			// 3. Blocks insertion
			if firstErr == nil && len(blockRows) > 0 {
				if err := baseInserter.InsertBlocks(ctx, blockRows); err != nil {
					firstErr = fmt.Errorf("InsertBlocks: %w", err)
				}
			}

			if firstErr != nil {
				log.Printf("Chain %d: sequential insert error: %v", chain.ID, firstErr)
				if len(blockRefs) > 0 {
					_ = rollbackAfterBlock(ctx, store, forkMode, chain.ID, blockRefs[0].Number-1)
				}
				time.Sleep(5 * time.Second)
				continue
			}
			profInsert += time.Since(insertStart)
			if proc != nil && len(customLogs) > 0 {
				insertStart := time.Now()
				if err := proc.Process(ctx, store, customLogs); err != nil {
					profInsert += time.Since(insertStart)
					log.Printf("Chain %d: custom processor %s error: %v", chain.ID, rangeLabel, err)
					if len(blockRefs) > 0 {
						_ = rollbackAfterBlock(ctx, store, forkMode, chain.ID, blockRefs[0].Number-1)
					}
					time.Sleep(5 * time.Second)
					continue
				}
				profInsert += time.Since(insertStart)
			}

			if cursorMode {
				lastProcessed = maxSeenBlock
				if effectiveEndBlock != nil && (seenBeyondEnd || lastProcessed > *effectiveEndBlock) {
					lastProcessed = *effectiveEndBlock
				}
				state.ApplyBatch(response.Head.Finalized, blockRefs)
				current := state.Current()
				if current == nil && len(blockRefs) > 0 {
					current = &blockRefs[len(blockRefs)-1]
				}
				if current != nil {
					if err := saveForkState(ctx, store, chain.ID, state, current); err != nil {
						return fmt.Errorf("update sync state %d: %w", lastProcessed, err)
					}
				}
			} else if err := store.UpdateSyncState(ctx, chain.ID, lastProcessed); err != nil {
				return fmt.Errorf("update sync state %d: %w", lastProcessed, err)
			}
			if profIters%10 == 0 {
				if err := store.TruncateSyncState(ctx, chain.ID, lastProcessed); err != nil {
					log.Printf("Chain %d: truncate sync state error: %v", chain.ID, err)
				}
			}

			totalEvents += uint64(len(decodedEvents))
		}

		scanned := uint64(0)
		if lastProcessed >= requestStartBlock {
			scanned = lastProcessed - requestStartBlock + 1
		}
		totalBlocks += scanned
		elapsed := time.Since(startTime)
		rate := float64(totalBlocks) / elapsed.Seconds()
		if len(raw) > 0 {
			log.Printf("Chain %d: %s scanned %d blocks, event blocks: %d, events: %d | checkpoint: %d | total: %d blocks, %d events | %.1f blk/s",
				chain.ID, rangeLabel, scanned, eventBlockCount, len(decodedEvents), lastProcessed, totalBlocks, totalEvents, rate)
		} else {
			log.Printf("Chain %d: %s scanned %d blocks, empty response | checkpoint: %d | total: %d blocks, %d events | %.1f blk/s",
				chain.ID, rangeLabel, scanned, lastProcessed, totalBlocks, totalEvents, rate)
		}

		currentBlock = lastProcessed + 1

		if effectiveEndBlock != nil && currentBlock > *effectiveEndBlock {
			break
		}
	}

	runtime.ReadMemStats(&memAfter)
	printProfile(profFetch, profParse, profDecode, profMarshal, profInsert, profIters, totalBlocks, totalEvents, startTime, memBefore, memAfter)
	return nil
}

func printProfile(fetch, parse, decode, marshal, insert time.Duration, iters int, totalBlocks, totalEvents uint64, startTime time.Time, memBefore, memAfter runtime.MemStats) {
	if iters == 0 {
		return
	}
	total := fetch + parse + decode + marshal + insert
	elapsed := time.Since(startTime)
	log.Println("")
	log.Println("═══ PROFILE ═══")
	log.Printf("  FETCH:  %v (%.0f%%)", fetch, pct(fetch, total))
	log.Printf("  PARSE:  %v (%.0f%%), %d iterations, avg %v/iter", parse, pct(parse, total), iters, parse/time.Duration(iters))
	log.Printf("  DECODE: %v (%.0f%%)", decode, pct(decode, total))
	log.Printf("  MARSHAL:%v (%.0f%%)", marshal, pct(marshal, total))
	log.Printf("  INSERT: %v (%.0f%%)", insert, pct(insert, total))
	log.Printf("  ─────────────────")
	log.Printf("  TOTAL:  %v (wall %v, %.0f%% accounted)", total, elapsed, pct(total, elapsed))
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
	if cursorMode {
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

func minEndBlock(current *uint64, candidate uint64) *uint64 {
	if current != nil && *current < candidate {
		return current
	}
	return &candidate
}

func chainEndpoint(chainID uint64, hot bool) string {
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
			table := database.TypedEventTable{
				Name: viewName + "_events",
				Args: args,
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
