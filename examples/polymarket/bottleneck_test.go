package polymarket

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/klauspost/compress/zstd"
)

// groupLogsByBlock groups logs by their block number for batch processing
func groupLogsByBlockBottleneck(logs []ingestion.CustomLog) map[uint64][]ingestion.CustomLog {
	groups := make(map[uint64][]ingestion.CustomLog)
	for _, lg := range logs {
		groups[lg.BlockNumber] = append(groups[lg.BlockNumber], lg)
	}
	return groups
}

// TestBottleneck reproduces the speed bottleneck using the full offline
// dataset (wallet_0xf05b67_full). It simulates exactly what ingestion.Run
// does (Producer -> ReplayBuf -> Consumer -> DB Insert + Custom Processor),
// but feeds the local offline data to eliminate network variance.
func TestBottleneck(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	projectRoot := findProjectRoot()
	dataDir := filepath.Join(projectRoot, "debugger/data/wallet_0xf05b67_full")

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Skipf("full data directory not found: %s", dataDir)
	}

	files, err := filepath.Glob(filepath.Join(dataDir, "*.jsonl.zstd"))
	if err != nil || len(files) == 0 {
		t.Skipf("no zstd files found in %s", dataDir)
	}

	// Restrict to first 5 files to keep the test runtime reasonable (~15s)
	if len(files) > 5 {
		files = files[:5]
	}

	t.Log("==========================================")
	t.Log("SPEED BOTTLENECK REPRODUCTION TEST (OFFLINE + DB)")
	t.Log("==========================================")
	t.Logf("Processing %d offline chunks with full DB insertion...", len(files))

	// Setup ClickHouse once for both runs
	ctx := context.Background()
	testDB := "bottleneck_test_db"
	
	// Drop & Create the Database
	_ = database.DropClickHouseDatabase(ctx, "127.0.0.1", 9003, "default", "sqd-clickhouse", testDB)
	store, err := database.NewClickHouse(ctx, "127.0.0.1", 9003, "default", "sqd-clickhouse", testDB)
	if err != nil {
		t.Fatalf("failed to connect to ClickHouse: %v", err)
	}
	defer store.Close()

	// Ensure tables are created (just like ingestion.Run)
	err = store.EnsureTablesWithOptions(ctx, false, database.EnsureTablesOptions{
		StoreBlocks: true,
		StoreLogs:   true,
	})
	if err != nil {
		t.Fatalf("ensure tables failed: %v", err)
	}

	// 1. Run the Synchronized (Bottleneck) version
	t.Log("\n--- RUN 1: Synchronized (Current Bottleneck) ---")
	procSync, _ := generated.NewProcessor(true)
	durSync, statsSync := runOfflinePipeline(t, ctx, store, files, procSync, true)
	speedSync := float64(statsSync.blocks) / durSync.Seconds()
	t.Logf("Synchronized Time: %v (%.2f blocks/sec)", durSync, speedSync)

	// Clear out DB state for fair comparison
	store.TruncateSyncState(ctx, 137, 0)
	procSync.State = generated.NewState()

	// 2. Run the Pipelined (Concurrent) version
	t.Log("\n--- RUN 2: Pipelined (Fixed) ---")
	procPipe, _ := generated.NewProcessor(true)
	durPipe, statsPipe := runOfflinePipeline(t, ctx, store, files, procPipe, false)
	speedPipe := float64(statsPipe.blocks) / durPipe.Seconds()
	t.Logf("Pipelined Time:    %v (%.2f blocks/sec)", durPipe, speedPipe)

	// Compare
	t.Log("\n--- RESULTS ---")
	speedup := speedPipe / speedSync
	t.Logf("Synchronized Speed: %.2f blocks/sec", speedSync)
	t.Logf("Pipelined Speed:    %.2f blocks/sec", speedPipe)
	t.Logf("Bottleneck Factor: Pipelining the offline data + DB inserts is %.2fx faster!", speedup)
}

// runOfflinePipeline simulates ingestion.go perfectly using offline data
// and real ClickHouse Database inserts.
func runOfflinePipeline(t *testing.T, ctx context.Context, store *database.Store, files []string, proc *generated.Processor, synchronized bool) (time.Duration, *processingStats) {
	stats := &processingStats{}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("create zstd decoder: %v", err)
	}
	defer decoder.Close()

	baseInserter := store.NewInserter()

	type pipelineBatch struct {
		logs       []ingestion.CustomLog
		blockRows  []database.BlockRow
	}

	// Replay Buffer (Capacity decouples the Producer and Consumer)
	replayBuf := make(chan pipelineBatch, 100)
	
	// advanceChan is what completely bottlenecks ingestion.go
	advanceChan := make(chan struct{}, 1)

	start := time.Now()

	// PRODUCER GOROUTINE (Simulates network fetch + parse)
	go func() {
		defer close(replayBuf)
		jsonlParser := parser.NewFastJSONLParser(100)

		for _, filePath := range files {
			compressed, _ := os.ReadFile(filePath)
			decompressed, _ := decoder.DecodeAll(compressed, nil)

			var batchLogs []ingestion.CustomLog
			var batchBlocks []database.BlockRow
			
			// Decompressing and JSON parsing is CPU heavy. This is the "Producer" work.
			_ = jsonlParser.Parse(decompressed, func(block *parser.Block) error {
				blockTime := time.Unix(int64(block.Header.Timestamp), 0).UTC()
				
				batchBlocks = append(batchBlocks, database.BlockRow{
					ChainID:        137,
					BlockNumber:    block.Header.Number,
					BlockHash:      block.Header.Hash,
					BlockTimestamp: blockTime,
				})

				for _, lg := range block.Logs {
					topics := make([]string, len(lg.Topics))
					copy(topics, lg.Topics)
					batchLogs = append(batchLogs, ingestion.CustomLog{
						ChainID:          137,
						BlockNumber:      block.Header.Number,
						BlockTimestamp:   blockTime,
						BlockHash:        block.Header.Hash,
						ContractAddress:  lg.Address,
						TransactionHash:  lg.TransactionHash,
						TransactionIndex: lg.TransactionIndex,
						LogIndex:         lg.LogIndex,
						Topics:           topics,
						Data:             lg.Data,
					})
				}
				return nil
			})
			
			if len(batchBlocks) > 0 {
				// Send parsed batch to consumer
				replayBuf <- pipelineBatch{
					logs:      batchLogs,
					blockRows: batchBlocks,
				}
				
				if synchronized {
					// BOTTLENECK: The producer yields completely and idles until the
					// consumer is entirely finished writing this batch to the DB.
					<-advanceChan
				}
			}
		}
	}()

	// CONSUMER LOOP (Simulates DB Insertion + Process)
	for batch := range replayBuf {
		if len(batch.blockRows) > 0 {
			stats.blocks += uint64(len(batch.blockRows))
			stats.events += uint64(len(batch.logs))
			
			// 1. Insert into ClickHouse (simulating exactly what ingestion.go does)
			_ = baseInserter.InsertBlocks(ctx, batch.blockRows)

			// 2. Custom Processor
			if len(batch.logs) > 0 {
				byBlock := groupLogsByBlockBottleneck(batch.logs)
				for _, blockLogs := range byBlock {
					_ = proc.Process(ctx, store, blockLogs)
				}
			}
		}

		if synchronized {
			// Unblock the producer ONLY AFTER DB insertion and processing are fully complete
			advanceChan <- struct{}{}
		}
	}

	dur := time.Since(start)
	return dur, stats
}
