package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/envconfig"
)

func decodeJSONBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// createUSDCSchema provisions the base tables plus the typed
// usdc_transfer_events table (contract "USDC" + event "Transfer"), matching the
// full-pipeline integration setup.
func createUSDCSchema(t *testing.T, host string, port int, password, dbName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := database.NewClickHouse(ctx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("setup ClickHouse: %v", err)
	}
	if err := store.EnsureTablesWithOptions(ctx, true, database.EnsureTablesOptions{}); err != nil {
		t.Fatalf("ensure base tables: %v", err)
	}
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
	if err := store.Conn().Do(ctx, ch.Query{Body: createTable}); err != nil {
		t.Fatalf("create typed table: %v", err)
	}
	store.Close()
}

func dropDB(t *testing.T, host string, port int, password, dbName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.DropClickHouseDatabase(ctx, host, port, "default", password, dbName); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

// TestParallelFetchSparseGapDeadlock reproduces the production stall seen on
//
//	make uniswap-fast --parallel-fetch
//
// The LBTC Transfer filter is extremely sparse: blocks are empty for the first
// ~20M blocks, then events appear near the chain tip. In cursor mode the
// sequential phase fetches DENSE (includeAllBlocks=true) and engages the
// parallel prefetcher SPARSE (includeAllBlocks=false). After the dense head
// region is consumed, the next event block is much further ahead than the
// replay-buffer capacity (8192). Two coupled gates then deadlock:
//
//   - Producer backpressure (ingestion.go) halts the producer once
//     pBlock-cBlock >= capacity-100, even though the skipped-empty blocks
//     wrote nothing to the buffer — so the producer never reaches the far
//     event block.
//   - Consumer gap-skip (ingestion.go) only advances over empties while
//     c <= replayBuf.LatestBlock(), and LatestBlock never advances through an
//     empty region — so the consumer waits forever for a block that was
//     skipped.
//
// Symptom: checkpoint frozen, "buffered" frozen, consumer_wait≈100%,
// producer_backpressure≈100% — exactly the production profile.
//
// This test drives the real ingestion pipeline against a fake portal that
// models the sparsity. On the buggy code it never completes (times out). It is
// the regression guard for the fix.
//
// Requires a running ClickHouse (skipped otherwise).
func TestParallelFetchSparseGapDeadlock(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}

	const (
		denseRespCap uint64 = 600   // first dense fetch covers [0,599], then parallel engages
		eventBlockA  uint64 = 25000 // first event — far beyond denseRespCap + bufferCapacity(8192)
		eventBlockB  uint64 = 25001 // second event
		endBlock     uint64 = 26000 // a little past the events
		lbtcAddr            = "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
		transferSig         = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	)

	var denseReqs, sparseReqs atomic.Int64

	// eventLine emits one block carrying a single LBTC Transfer log.
	eventLine := func(n uint64) string {
		from := "0x0000000000000000000000001111111111111111111111111111111111111111"
		to := "0x0000000000000000000000002222222222222222222222222222222222222222"
		value := "0x0000000000000000000000000000000000000000000000000000000000000064" // 100
		return fmt.Sprintf(
			`{"header":{"number":%d,"hash":"0x%064x","timestamp":%d},"logs":[{"address":"%s","transactionHash":"0x%064x","data":"%s","transactionIndex":0,"logIndex":0,"topics":["%s","%s","%s"]}]}`+"\n",
			n, n, 1700000000+n, strings.ToLower(lbtcAddr), n, value, transferSig, from, to,
		)
	}
	headerLine := func(n uint64) string {
		return fmt.Sprintf(`{"header":{"number":%d,"hash":"0x%064x","timestamp":%d}}`+"\n", n, n, 1700000000+n)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock        uint64  `json:"fromBlock"`
			ToBlock          *uint64 `json:"toBlock"`
			IncludeAllBlocks bool    `json:"includeAllBlocks"`
		}
		_ = decodeJSONBody(r, &q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", "9000000")
		w.Header().Set("X-Sqd-Head-Number", "9000000")

		hi := endBlock
		if q.ToBlock != nil && *q.ToBlock < hi {
			hi = *q.ToBlock
		}

		if q.IncludeAllBlocks {
			// Dense sequential phase: contiguous block headers, capped.
			denseReqs.Add(1)
			last := q.FromBlock + denseRespCap - 1
			if last > hi {
				last = hi
			}
			var b strings.Builder
			for n := q.FromBlock; n <= last; n++ {
				b.WriteString(headerLine(n))
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(b.String()))
			return
		}

		// Sparse parallel phase: only event-bearing blocks exist.
		sparseReqs.Add(1)
		var b strings.Builder
		for _, e := range []uint64{eventBlockA, eventBlockB} {
			if e >= q.FromBlock && e <= hi {
				b.WriteString(eventLine(e))
			}
		}
		if b.Len() == 0 {
			// No matching blocks in range: portal returns an empty body, which
			// drives the prefetcher's empty-skip path.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	t.Setenv(envconfig.PortalEndpoint, srv.URL)
	t.Setenv(envconfig.EnvParallelFetchers, "2")
	t.Setenv(envconfig.EnvParallelPageSize, "1000") // minSpan = 2*1000 = 2000
	t.Setenv(envconfig.EnvParallelRPS, "1000000")   // no throttle in the hermetic test

	host, port, password := chEnv()
	dbName := fmt.Sprintf("stall_test_%d", time.Now().UnixNano())
	createUSDCSchema(t, host, port, password, dbName)
	defer dropDB(t, host, port, password, dbName)

	end := endBlock
	cfg := &config.Config{
		Name: dbName,
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: 0,
			EndBlock:   &end,
			Contracts: []config.ChainContractConfig{{
				Name:    "USDC",
				Address: config.Address{lbtcAddr},
				Events: []config.EventConfig{
					{Event: "Transfer(address indexed from, address indexed to, uint256 value)"},
				},
			}},
		}},
	}

	opts := Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     "default",
		ClickHousePassword: password,
		ClickHouseDatabase: dbName,
		Restart:            false,
		CursorMode:         true,
		ParallelFetch:      true,
		PageSize:           0,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, cfg, opts) }()

	select {
	case err := <-runErr:
		if err != nil && ctx.Err() == nil {
			t.Fatalf("ingestion.Run: %v", err)
		}
		t.Logf("Run completed: dense_requests=%d sparse_requests=%d", denseReqs.Load(), sparseReqs.Load())
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatalf("DEADLOCK reproduced: parallel sparse fetch stalled in the empty gap "+
			"(dense_requests=%d sparse_requests=%d). The producer backpressured before reaching "+
			"the event block at %d, and the consumer cannot gap-skip past LatestBlock.",
			denseReqs.Load(), sparseReqs.Load(), eventBlockA)
	}

	// Correctness: the two LBTC events past the empty gap must have been indexed.
	// On the deadlocked code the producer never reached them.
	got := countRows(t, host, port, password, dbName, "usdc_transfer_events")
	if got != 2 {
		t.Fatalf("indexed %d transfer events across the empty gap, want 2", got)
	}
}

func countRows(t *testing.T, host string, port int, password, dbName, table string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := database.NewClickHouse(ctx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("connect ClickHouse for verification: %v", err)
	}
	defer store.Close()
	var count proto.ColUInt64
	if err := store.Conn().Do(ctx, ch.Query{
		Body:   fmt.Sprintf("SELECT count() FROM %s.%s", quoteIdentForTest(dbName), table),
		Result: proto.Results{{Name: "count()", Data: &count}},
	}); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count.Rows() == 0 {
		return 0
	}
	return count.Row(0)
}
