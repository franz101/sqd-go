package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedConcurrentResolveRuntime is the correctness proof for
// resolveAllParallel's fix (each concurrent per-entity resolve now gets its
// own dedicated *ch.Client instead of racing on one shared connection, which
// ch-go does not support). It runs against a real, disposable ClickHouse (set
// SQD_RESOLVE_CH_ADDR to run; skips otherwise) with TWO distinct hot-state
// entities so a run with only one job (nothing to cross-contaminate with)
// can't hide a bug: if concurrent resolves ever interleaved requests/
// responses across connections, one entity would come back holding the
// other's data, or an error.
func TestGeneratedConcurrentResolveRuntime(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}

	root := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("config.yaml", `name: concurrent_resolve_fixture
chains:
  - id: 137
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address taker, uint256 makerAssetId)
`)
	writeFile("custom_schema.go", `package concurrent_resolve_fixture

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// pk: User, TokenID
type MemoryUserPositionSchema struct {
	User             common.Address
	TokenID          common.Hash
	Balance          uint64
	BlockNumber      uint64
	TransactionIndex uint64
	LogIndex         uint64
	UpdatedAtBlock   uint64
	UpdatedAt        time.Time
}

// pk: ConditionID
type MemoryMarketSchema struct {
	ConditionID      common.Hash
	Question         uint64
	BlockNumber      uint64
	TransactionIndex uint64
	LogIndex         uint64
	UpdatedAtBlock   uint64
	UpdatedAt        time.Time
}
`)
	writeFile("go.mod", "module concurrentresolvefixture\n\ngo 1.25\n\nrequire github.com/franz101/sqd-go v0.0.0\n\nreplace github.com/franz101/sqd-go => "+filepath.ToSlash(repoRoot(t))+"\n")

	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	testPath := filepath.Join(root, "generated", "concurrent_resolve_test.go")
	if err := os.WriteFile(testPath, []byte(concurrentResolveRuntimeTestSource), 0o644); err != nil {
		t.Fatal(err)
	}

	output, err := runGo(goBin, root, os.Environ(), "test", "./generated", "-run", "^TestConcurrentResolve", "-race", "-count=1", "-v")
	if err != nil {
		t.Fatalf("generated concurrent resolve tests failed: %v\n%s", err, output)
	}
	t.Logf("inner test output:\n%s", output)
}

const concurrentResolveRuntimeTestSource = `package generated

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ethereum/go-ethereum/common"
)

const positionsDDL = "CREATE TABLE IF NOT EXISTS %s.memory_user_positions (" +
	"user FixedString(20), token_id FixedString(32), balance UInt64, " +
	"block_number UInt64, transaction_index UInt64, log_index UInt64, " +
	"updated_at_block UInt64, updated_at DateTime64(3, 'UTC') DEFAULT now64(3)" +
	") ENGINE = MergeTree() PRIMARY KEY (user, token_id) " +
	"ORDER BY (user, token_id, block_number, transaction_index, log_index)"

const marketsDDL = "CREATE TABLE IF NOT EXISTS %s.memory_markets (" +
	"condition_id FixedString(32), question UInt64, " +
	"block_number UInt64, transaction_index UInt64, log_index UInt64, " +
	"updated_at_block UInt64, updated_at DateTime64(3, 'UTC') DEFAULT now64(3)" +
	") ENGINE = MergeTree() PRIMARY KEY (condition_id) " +
	"ORDER BY (condition_id, block_number, transaction_index, log_index)"

func crAddr(i int) common.Address {
	var a common.Address
	a[0] = 0xAA
	a[19] = byte(i)
	return a
}

func crHash(i int) common.Hash {
	var h common.Hash
	h[0] = 0xBB
	h[31] = byte(i)
	return h
}

// TestConcurrentResolveNoCrossContamination seeds distinct, easily
// distinguishable rows into two different hot-state tables, queues misses on
// both entities' resolvers, resolves them concurrently via resolveAllParallel
// against a real 3-connection pool, and asserts each entity comes back with
// exactly its own data — never the other entity's, never an error from two
// goroutines racing one socket.
func TestConcurrentResolveNoCrossContamination(t *testing.T) {
	addr := os.Getenv("SQD_RESOLVE_CH_ADDR")
	if addr == "" {
		t.Skip("set SQD_RESOLVE_CH_ADDR (host:port of a disposable ClickHouse) to run")
	}
	ctx := context.Background()
	db := fmt.Sprintf("resolve_rt_test_%d", time.Now().UnixNano())

	setup, err := ch.Dial(ctx, ch.Options{Address: addr, User: "default", Password: os.Getenv("SQD_RESOLVE_CH_PASSWORD")})
	if err != nil {
		t.Fatalf("dial setup: %v", err)
	}
	defer setup.Close()
	if err := setup.Do(ctx, ch.Query{Body: "CREATE DATABASE " + db}); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = setup.Do(dropCtx, ch.Query{Body: "DROP DATABASE IF EXISTS " + db})
	})
	if err := setup.Do(ctx, ch.Query{Body: fmt.Sprintf(positionsDDL, db)}); err != nil {
		t.Fatalf("create positions table: %v", err)
	}
	if err := setup.Do(ctx, ch.Query{Body: fmt.Sprintf(marketsDDL, db)}); err != nil {
		t.Fatalf("create markets table: %v", err)
	}

	const N = 50
	posBatch := NewMemoryUserPositionBatch()
	for i := 0; i < N; i++ {
		posBatch.Append(MemoryUserPosition{
			User: crAddr(i), TokenID: crHash(i), Balance: uint64(9_000_000 + i),
			BlockNumber: 100, UpdatedAtBlock: 100,
		})
	}
	if err := posBatch.Insert(ctx, setup, db); err != nil {
		t.Fatalf("seed positions: %v", err)
	}
	marketBatch := NewMemoryMarketBatch()
	for i := 0; i < N; i++ {
		marketBatch.Append(MemoryMarket{
			ConditionID: crHash(i), Question: uint64(1_000_000 + i),
			BlockNumber: 100, UpdatedAtBlock: 100,
		})
	}
	if err := marketBatch.Insert(ctx, setup, db); err != nil {
		t.Fatalf("seed markets: %v", err)
	}

	// A dedicated pool, mirroring database.Store.ResolveConns: enough
	// connections that positions and markets genuinely resolve concurrently,
	// not serialized through a single shared socket.
	pool := make([]*ch.Client, 3)
	for i := range pool {
		c, err := ch.Dial(ctx, ch.Options{Address: addr, User: "default", Password: os.Getenv("SQD_RESOLVE_CH_PASSWORD")})
		if err != nil {
			t.Fatalf("dial pool conn %d: %v", i, err)
		}
		defer c.Close()
		pool[i] = c
	}

	for round := 0; round < 5; round++ {
		hot := NewHotState(256)
		for i := 0; i < N; i++ {
			hot.UserPositionsResolver.Queue(UserPositionsClockKey{User: crAddr(i), TokenID: crHash(i)})
			hot.MarketsResolver.Queue(MarketsClockKey{ConditionID: crHash(i)})
		}
		if err := hot.resolveAllParallel(ctx, pool, db); err != nil {
			t.Fatalf("round %d: resolveAllParallel: %v", round, err)
		}
		for i := 0; i < N; i++ {
			pos, ok := hot.UserPositions.Get(UserPositionsClockKey{User: crAddr(i), TokenID: crHash(i)})
			if !ok {
				t.Fatalf("round %d: position %d missing after resolve", round, i)
			}
			if pos.Balance != uint64(9_000_000+i) {
				t.Errorf("round %d: position %d balance = %d, want %d (cross-contamination or wrong row)", round, i, pos.Balance, 9_000_000+i)
			}
			mkt, ok := hot.Markets.Get(MarketsClockKey{ConditionID: crHash(i)})
			if !ok {
				t.Fatalf("round %d: market %d missing after resolve", round, i)
			}
			if mkt.Question != uint64(1_000_000+i) {
				t.Errorf("round %d: market %d question = %d, want %d (cross-contamination or wrong row)", round, i, mkt.Question, 1_000_000+i)
			}
		}
	}
}

// TestConcurrentResolveFallsBackWithoutPool verifies resolveAllParallel
// itself refuses to run with zero connections (the caller-side fallback to
// the sequential ResolveAllPending lives in resolvePendingState, not tested
// here since it needs the full Store type — this only proves the primitive
// fails safely rather than silently no-op'ing or panicking).
func TestConcurrentResolveFallsBackWithoutPool(t *testing.T) {
	addr := os.Getenv("SQD_RESOLVE_CH_ADDR")
	if addr == "" {
		t.Skip("set SQD_RESOLVE_CH_ADDR (host:port of a disposable ClickHouse) to run")
	}
	hot := NewHotState(256)
	hot.UserPositionsResolver.Queue(UserPositionsClockKey{User: crAddr(1), TokenID: crHash(1)})
	if err := hot.resolveAllParallel(context.Background(), nil, "irrelevant"); err == nil {
		t.Fatal("resolveAllParallel with no connections and pending work returned nil error, want an error")
	}
}
`
