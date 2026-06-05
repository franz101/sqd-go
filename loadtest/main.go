package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"time"

	_ "github.com/franz101/sqd-go/examples/polymarket" // Registers custom processing callback
	"github.com/franz101/sqd-go/internal/database"
)

func main() {
	// Parse general flags
	chHost := flag.String("host", "127.0.0.1", "ClickHouse host")
	chPort := flag.Int("port", 9003, "ClickHouse native port")
	chUser := flag.String("user", "default", "ClickHouse user")
	chPass := flag.String("pass", "sqd-clickhouse", "ClickHouse password")
	chDB := flag.String("db", "polymarket_loadtest", "ClickHouse database name")
	force := flag.Bool("force", false, "Force running against production database 'polymarket'")

	flag.Parse()
	args := flag.Args()

	if *chDB == "polymarket" && !*force {
		log.Fatalf("Error: Running the loadtest directly against the production database 'polymarket' is blocked by default to prevent data corruption. Please specify a separate database via the -db flag (e.g. -db polymarket_loadtest) or set -force to override this check.")
	}

	if len(args) < 1 {
		printUsageAndExit()
	}

	command := args[0]

	// Define command subflags
	populateCmd := flag.NewFlagSet("populate", flag.ExitOnError)
	popCount := populateCmd.Uint64("count", 20000000, "Number of user positions to generate & insert")
	popBatchSize := populateCmd.Int("batch-size", 50000, "ClickHouse batch insertion size")

	runCmd := flag.NewFlagSet("run", flag.ExitOnError)
	runBlocks := runCmd.Uint64("blocks", 1000, "Number of blocks to run")
	runTxs := runCmd.Int("txs", 500, "Transactions per block")
	runHotPct := runCmd.Float64("hot-pct", 0.05, "Hot user transaction percentage (0.0 to 1.0) for cache hit simulation")
	runTPS := runCmd.Int("tps", 0, "Target TPS throttle, 0 for max speed")
	runQueueCap := runCmd.Int("queue-cap", 5000, "Bounded queue capacity for blocks")
	cpuProfile := runCmd.String("cpu-profile", "", "Path to write CPU profile to")
	memProfile := runCmd.String("mem-profile", "", "Path to write memory profile to")

	positionsCmd := flag.NewFlagSet("positions", flag.ExitOnError)
	posPositions := positionsCmd.Int("positions", 100000, "Number of cached positions")
	posEvents := positionsCmd.Int("events", 200000, "Number of incoming order events")
	posEngine := positionsCmd.String("engine", "both", "Math engine: proto, shopspring, or both")
	posInsert := positionsCmd.String("insert", "stream", "Insert mode: none, batch, stream, or both")
	posChunkSize := positionsCmd.Int("chunk-size", 2000, "Rows per ClickHouse insert chunk")

	stateCmd := flag.NewFlagSet("state", flag.ExitOnError)
	statePositions := stateCmd.Int("positions", 10000, "Number of cold ClickHouse user positions")
	stateEvents := stateCmd.Int("events", 20000, "Number of incoming user position events")
	stateEngine := stateCmd.String("engine", "both", "State engine: current, improved, or both")
	statePrefetchBatch := stateCmd.Int("prefetch-batch", 2000, "Events per improved prefetch window")
	stateResolveChunk := stateCmd.Int("resolve-chunk", 500, "Keys per ClickHouse resolve query")
	stateInsertChunk := stateCmd.Int("insert-chunk", 2000, "Rows per queued ClickHouse insert chunk")
	stateQueueCap := stateCmd.Int("queue-cap", 16, "Queued write batch capacity")
	stateFlushInterval := stateCmd.Duration("flush-interval", 2*time.Second, "Dirty state flush interval")

	switch command {
	case "populate":
		_ = populateCmd.Parse(args[1:])
		ctx := context.Background()

		// Ensure schema is created in target database before populating
		store, err := database.NewClickHouse(ctx, *chHost, *chPort, *chUser, *chPass, *chDB)
		if err != nil {
			log.Fatalf("Failed to connect to ClickHouse: %v", err)
		}
		log.Printf("Ensuring schema in ClickHouse database: %s", *chDB)
		if err := store.ApplySQLFileWithDatabase(ctx, "examples/polymarket/generated/schema.sql", "polymarket"); err != nil {
			log.Fatalf("Failed to apply schema.sql: %v", err)
		}
		if err := store.ApplySQLFileWithDatabase(ctx, "examples/polymarket/generated/custom_schema.sql", "polymarket"); err != nil {
			log.Fatalf("Failed to apply custom_schema.sql: %v", err)
		}
		store.Close()

		err = PopulateUserPositions(ctx, *chHost, *chPort, *chUser, *chPass, *chDB, *popCount, *popBatchSize)
		if err != nil {
			log.Fatalf("Population failed: %v", err)
		}

	case "positions":
		_ = positionsCmd.Parse(args[1:])
		ctx := context.Background()
		cfg := PositionCompareConfig{
			Host:      *chHost,
			Port:      *chPort,
			User:      *chUser,
			Password:  *chPass,
			Database:  *chDB,
			Positions: *posPositions,
			Events:    *posEvents,
			Engine:    *posEngine,
			Insert:    *posInsert,
			ChunkSize: *posChunkSize,
		}
		if err := RunPositionCompare(ctx, cfg); err != nil {
			log.Fatalf("Position comparison failed: %v", err)
		}

	case "state":
		_ = stateCmd.Parse(args[1:])
		ctx := context.Background()
		cfg := StateCompareConfig{
			Host:          *chHost,
			Port:          *chPort,
			User:          *chUser,
			Password:      *chPass,
			Database:      *chDB,
			Positions:     *statePositions,
			Events:        *stateEvents,
			Engine:        *stateEngine,
			PrefetchBatch: *statePrefetchBatch,
			ResolveChunk:  *stateResolveChunk,
			InsertChunk:   *stateInsertChunk,
			QueueCap:      *stateQueueCap,
			FlushInterval: *stateFlushInterval,
		}
		if err := RunStateCompare(ctx, cfg); err != nil {
			log.Fatalf("State comparison failed: %v", err)
		}

	case "run":
		_ = runCmd.Parse(args[1:])

		// CPU profiling setup
		if *cpuProfile != "" {
			f, err := os.Create(*cpuProfile)
			if err != nil {
				log.Fatalf("could not create CPU profile: %v", err)
			}
			defer f.Close()
			if err := pprof.StartCPUProfile(f); err != nil {
				log.Fatalf("could not start CPU profile: %v", err)
			}
			defer pprof.StopCPUProfile()
			log.Printf("Writing CPU profile to %s", *cpuProfile)
		}

		ctx := context.Background()

		// Connect to DB and ensure schema
		store, err := database.NewClickHouse(ctx, *chHost, *chPort, *chUser, *chPass, *chDB)
		if err != nil {
			log.Fatalf("Failed to connect to ClickHouse: %v", err)
		}
		defer store.Close()

		log.Printf("Ensuring schema in ClickHouse database: %s", *chDB)
		if err := store.ApplySQLFileWithDatabase(ctx, "examples/polymarket/generated/schema.sql", "polymarket"); err != nil {
			log.Fatalf("Failed to apply schema.sql: %v", err)
		}
		if err := store.ApplySQLFileWithDatabase(ctx, "examples/polymarket/generated/custom_schema.sql", "polymarket"); err != nil {
			log.Fatalf("Failed to apply custom_schema.sql: %v", err)
		}

		// Initialize Event Generator
		// 2 million total users, 10,000 hot users (with high cache hits)
		generator := NewEventGenerator(137, 2000000, 10000, *runHotPct)

		// Create and start pipeline
		pipeline, err := NewPipeline(generator, store, *runQueueCap)
		if err != nil {
			log.Fatalf("Failed to create pipeline: %v", err)
		}

		pipeline.Start(ctx, *runBlocks, *runTxs, *runTPS)

		// Memory profiling setup
		if *memProfile != "" {
			f, err := os.Create(*memProfile)
			if err != nil {
				log.Fatalf("could not create memory profile: %v", err)
			}
			defer f.Close()
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Fatalf("could not write memory profile: %v", err)
			}
			log.Printf("Written memory profile to %s", *memProfile)
		}

	default:
		printUsageAndExit()
	}
}

func printUsageAndExit() {
	fmt.Println("Usage: loadtest <command> [flags]")
	fmt.Println("Commands:")
	fmt.Println("  populate - Populates ClickHouse database with millions of user positions")
	fmt.Println("  positions - Compares shopspring decimal vs proto Decimal256 position updates and ClickHouse ingest")
	fmt.Println("  state    - Reproduces hot/cold State.Get/Save ClickHouse load and compares batched prefetch")
	fmt.Println("  run      - Runs a high-throughput block stream to measure performance/backpressure")
	fmt.Println("Common flags:")
	fmt.Println("  -host string: ClickHouse host (default \"127.0.0.1\")")
	fmt.Println("  -port int: ClickHouse native port (default 9003)")
	fmt.Println("  -user string: ClickHouse user (default \"default\")")
	fmt.Println("  -pass string: ClickHouse password (default \"sqd-clickhouse\")")
	fmt.Println("  -db string: ClickHouse database name (default \"polymarket\")")
	os.Exit(1)
}
