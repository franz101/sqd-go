package ingestion

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
)

func chEnv() (host string, port int, password string) {
	host = os.Getenv("CLICKHOUSE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port = 9000
	if p := os.Getenv("CLICKHOUSE_NATIVE_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	password = os.Getenv("CLICKHOUSE_PASSWORD")
	if password == "" {
		password = "sqd-clickhouse"
	}
	return
}

func clickhouseAvailable() bool {
	host, port, password := chEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", host, port),
		Database: "default",
		User:     "default",
		Password: password,
	})
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// TestIntegrationIndexSmallRange indexes 100 blocks of USDC Transfer events on
// Ethereum (blocks 22000000-22000100) through the full pipeline:
// SQD fetch -> JSONL parse -> ABI decode -> ClickHouse insert.
//
// Requires a running ClickHouse (CI provides it via service container).
// Skipped when ClickHouse is not reachable.
func TestIntegrationIndexSmallRange(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}

	host, port, password := chEnv()
	dbName := fmt.Sprintf("integration_test_%d", time.Now().UnixNano())

	endBlock := uint64(22000100)
	cfg := &config.Config{
		Name: dbName,
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: 22000000,
			EndBlock:   &endBlock,
			Contracts: []config.ChainContractConfig{{
				Name:    "USDC",
				Address: config.Address{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"},
				Events: []config.EventConfig{
					{Event: "Transfer(address indexed from, address indexed to, uint256 value)"},
				},
			}},
		}},
	}

	// Create the typed event table manually (normally codegen produces schema.sql).
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer setupCancel()

	store, err := database.NewClickHouse(setupCtx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("setup ClickHouse: %v", err)
	}
	if err := store.EnsureTablesWithOptions(setupCtx, true, database.EnsureTablesOptions{}); err != nil {
		t.Fatalf("ensure base tables: %v", err)
	}

	// Create the typed event table matching what buildTypedTableIndex produces:
	// contract "USDC" + event "Transfer" -> table "usdc_transfer_events"
	createTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.usdc_transfer_events (
		block_number UInt64,
		block_timestamp DateTime64(3, 'UTC'),
		transaction_index UInt64,
		log_index UInt64,
		from FixedString(20),
		to FixedString(20),
		value UInt256
	) ENGINE = MergeTree()
	ORDER BY (block_number, transaction_index, log_index)`, quoteIdentForTest(dbName))

	if err := store.Conn().Do(setupCtx, ch.Query{Body: createTable}); err != nil {
		t.Fatalf("create typed table: %v", err)
	}
	store.Close()

	// Run the ingestion pipeline with a 15-second timeout
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opts := Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     "default",
		ClickHousePassword: password,
		ClickHouseDatabase: dbName,
		Restart:            false,
		CursorMode:         false,
		PageSize:           0,
	}

	err = Run(ctx, cfg, opts)
	if err != nil && ctx.Err() == nil {
		t.Fatalf("ingestion.Run: %v", err)
	}

	// Verify rows were inserted
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer queryCancel()

	verifyStore, err := database.NewClickHouse(queryCtx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("connect ClickHouse for verification: %v", err)
	}
	defer verifyStore.Close()

	var count proto.ColUInt64
	if err := verifyStore.Conn().Do(queryCtx, ch.Query{
		Body: fmt.Sprintf("SELECT count() FROM %s.usdc_transfer_events", quoteIdentForTest(dbName)),
		Result: proto.Results{
			{Name: "count()", Data: &count},
		},
	}); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count.Rows() == 0 {
		t.Fatal("count query returned no result rows")
	}
	n := count.Row(0)
	t.Logf("usdc_transfer_events: %d rows across 100 blocks", n)
	if n == 0 {
		t.Fatal("expected USDC Transfer events in blocks 22000000-22000100")
	}

	// Cleanup
	if err := database.DropClickHouseDatabase(queryCtx, host, port, "default", password, dbName); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

func quoteIdentForTest(s string) string {
	return "`" + s + "`"
}
