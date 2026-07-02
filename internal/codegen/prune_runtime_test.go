package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedPruneRuntime generates a fixture indexer and exercises the
// generated CompactionPruneState / State.StartPrune-PollPrune-WaitPrune
// against a real, disposable ClickHouse (set SQD_PRUNE_CH_ADDR to run; skips
// otherwise). This is the test-db coverage for the prune/hot-Conn decoupling:
// before this, CompactionPruneState had zero test coverage anywhere in the
// repo despite running a DELETE mutation against production hot-state tables
// every CLICKHOUSE_PRUNE_INTERVAL blocks.
func TestGeneratedPruneRuntime(t *testing.T) {
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

	writeFile("config.yaml", `name: prune_fixture
chains:
  - id: 137
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address taker, uint256 makerAssetId)
`)
	writeFile("custom_schema.go", `package prune_fixture

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
`)
	writeFile("go.mod", "module prunefixture\n\ngo 1.25\n\nrequire github.com/franz101/sqd-go v0.0.0\n\nreplace github.com/franz101/sqd-go => "+filepath.ToSlash(repoRoot(t))+"\n")

	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	testPath := filepath.Join(root, "generated", "prune_test.go")
	if err := os.WriteFile(testPath, []byte(pruneRuntimeTestSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// os.Environ() carries SQD_PRUNE_CH_ADDR through to the inner test, which
	// runs the live-ClickHouse assertions only when it is set.
	output, err := runGo(goBin, root, os.Environ(), "test", "./generated", "-run", "^TestPrune", "-count=1", "-v")
	if err != nil {
		t.Fatalf("generated prune tests failed: %v\n%s", err, output)
	}
	t.Logf("inner test output:\n%s", output)
}

const pruneRuntimeTestSource = `package generated

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
)

// pruneStore implements the generated Store interface plus the optional
// PruneConn seam, with conn and pruneConn deliberately backed by two distinct
// *ch.Client connections (matching how database.Store wires a dedicated
// pruneConn) so tests can prove CompactionPruneState actually picks PruneConn
// over the hot Conn, not just that the connections happen to be equal.
type pruneStore struct {
	conn      *ch.Client
	pruneConn *ch.Client
	db        string
}

func (s pruneStore) Conn() *ch.Client      { return s.conn }
func (s pruneStore) PruneConn() *ch.Client { return s.pruneConn }
func (s pruneStore) DB() string            { return s.db }

// The generated inserter writes to the "_log" physical table (see the
// _log/_live custom-table split), so the DDL and count queries below must use
// that suffixed name too.
const pruneDDL = "CREATE TABLE IF NOT EXISTS %s.memory_user_positions_log (" +
	"user FixedString(20), token_id FixedString(32), balance UInt64, " +
	"block_number UInt64, transaction_index UInt64, log_index UInt64, " +
	"updated_at_block UInt64, updated_at DateTime64(3, 'UTC') DEFAULT now64(3)" +
	") ENGINE = MergeTree() PRIMARY KEY (user, token_id) " +
	"ORDER BY (user, token_id, block_number, transaction_index, log_index)"

func pruneAddr(i int) common.Address {
	var a common.Address
	a[0] = 0xEF
	a[18] = byte(i >> 8)
	a[19] = byte(i)
	return a
}

func prHash(i int) common.Hash {
	var h common.Hash
	h[0] = 0x12
	h[30] = byte(i >> 8)
	h[31] = byte(i)
	return h
}

// pruneTestDial connects two independent *ch.Client to addr (mirroring the
// two real sockets database.Store dials for Conn/PruneConn) and creates a
// fresh disposable database. Callers must close both clients and drop db.
func pruneTestDial(t *testing.T) (conn, prune *ch.Client, db string) {
	t.Helper()
	addr := os.Getenv("SQD_PRUNE_CH_ADDR")
	if addr == "" {
		t.Skip("set SQD_PRUNE_CH_ADDR (host:port of a disposable ClickHouse) to run")
	}
	ctx := context.Background()
	user := "default"
	if v := os.Getenv("SQD_PRUNE_CH_USER"); v != "" {
		user = v
	}
	password := os.Getenv("SQD_PRUNE_CH_PASSWORD")

	dial := func() *ch.Client {
		c, err := ch.Dial(ctx, ch.Options{Address: addr, User: user, Password: password})
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		return c
	}
	conn = dial()
	prune = dial()

	db = fmt.Sprintf("prune_rt_test_%d", time.Now().UnixNano())
	if err := conn.Do(ctx, ch.Query{Body: "CREATE DATABASE " + db}); err != nil {
		t.Fatalf("create database %s: %v", db, err)
	}
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf(pruneDDL, db)}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Do(dropCtx, ch.Query{Body: "DROP DATABASE IF EXISTS " + db})
		conn.Close()
		prune.Close()
	})
	return conn, prune, db
}

func pruneSeedRow(t *testing.T, conn *ch.Client, db string, addr common.Address, tokenID common.Hash, block uint64) {
	t.Helper()
	batch := NewMemoryUserPositionBatch()
	batch.Append(MemoryUserPosition{
		User: addr, TokenID: tokenID, Balance: block,
		BlockNumber: block, UpdatedAtBlock: block,
	})
	if err := batch.Insert(context.Background(), conn, db); err != nil {
		t.Fatalf("seed row (block=%d): %v", block, err)
	}
}

// pruneCountAtBlock reports how many rows survive at exactly block_number =
// block. Tests in this file pick globally-unique block numbers per seeded
// key, so a plain block_number filter unambiguously identifies one row
// without needing to round-trip FixedString user/token_id values through SQL.
func pruneCountAtBlock(t *testing.T, conn *ch.Client, db string, block uint64) uint64 {
	t.Helper()
	var cnt proto.ColUInt64
	q := fmt.Sprintf("SELECT count() AS c FROM %s.memory_user_positions_log WHERE block_number = %d", db, block)
	if err := conn.Do(context.Background(), ch.Query{
		Body:   q,
		Result: proto.Results{{Name: "c", Data: &cnt}},
	}); err != nil {
		t.Fatalf("query count at block %d: %v", block, err)
	}
	if cnt.Rows() == 0 {
		return 0
	}
	return cnt.Row(0)
}

// TestPruneCollapsesOldVersionsKeepsLatest is the correctness test for the
// DELETE half of CompactionPruneState: a key with several superseded
// versions below the prune threshold should be collapsed to its single
// latest version, and a key whose only rows are all old must still keep its
// latest row (never fully deleted) — the property the LIMIT-1-BY keep-set
// exists to guarantee.
func TestPruneCollapsesOldVersionsKeepsLatest(t *testing.T) {
	conn, prune, db := pruneTestDial(t)
	store := pruneStore{conn: conn, pruneConn: prune, db: db}

	multi := pruneAddr(1)
	multiTok := prHash(1)
	for _, b := range []uint64{100, 500, 900, 1500} {
		pruneSeedRow(t, conn, db, multi, multiTok, b)
	}

	staleOnly := pruneAddr(2)
	staleOnlyTok := prHash(2)
	for _, b := range []uint64{200, 600} {
		pruneSeedRow(t, conn, db, staleOnly, staleOnlyTok, b)
	}

	singleOld := pruneAddr(3)
	singleOldTok := prHash(3)
	pruneSeedRow(t, conn, db, singleOld, singleOldTok, 50)

	// retainInterval far larger than any seeded block => every row lands in
	// bucket intDiv(block, N)=0, so bucketed retention degenerates to the
	// classic collapse-to-one-latest-per-key this test asserts.
	if err := CompactionPruneState(context.Background(), store, 0, 2000, 100000); err != nil {
		t.Fatalf("CompactionPruneState: %v", err)
	}

	for _, deleted := range []uint64{100, 500, 900, 200} {
		if got := pruneCountAtBlock(t, conn, db, deleted); got != 0 {
			t.Errorf("count at block %d = %d, want 0 (superseded version must be deleted)", deleted, got)
		}
	}
	for _, kept := range []uint64{1500, 600, 50} {
		if got := pruneCountAtBlock(t, conn, db, kept); got != 1 {
			t.Errorf("count at block %d = %d, want 1 (latest version of its key must survive)", kept, got)
		}
	}
}

// TestPruneBelowMinBlockIsNoop verifies the blockNumber<=1000 early return:
// CompactionPruneState must not touch the table at all this early (it has no
// way to compute a safe pruneThreshold yet).
func TestPruneBelowMinBlockIsNoop(t *testing.T) {
	conn, prune, db := pruneTestDial(t)
	store := pruneStore{conn: conn, pruneConn: prune, db: db}

	addr, tok := pruneAddr(9), prHash(9)
	pruneSeedRow(t, conn, db, addr, tok, 10)

	if err := CompactionPruneState(context.Background(), store, 0, 999, 100000); err != nil {
		t.Fatalf("CompactionPruneState: %v", err)
	}
	if got := pruneCountAtBlock(t, conn, db, 10); got != 1 {
		t.Errorf("count at block 10 = %d, want 1 unchanged (blockNumber=999 must no-op)", got)
	}
}

// TestPruneBucketedRetainsPerBucket is the correctness test for block-bucketed
// retention (the "one snapshot every N blocks" behaviour): with a small
// retainInterval, each intDiv(block_number, N) bucket keeps its own latest row
// rather than the table collapsing to a single latest per key. This is what
// makes the "_log" table a usable time series for points accrual while still
// bounding growth.
func TestPruneBucketedRetainsPerBucket(t *testing.T) {
	conn, prune, db := pruneTestDial(t)
	store := pruneStore{conn: conn, pruneConn: prune, db: db}

	addr, tok := pruneAddr(11), prHash(11)
	// retainInterval = 200 => buckets [0,200),[200,400),[400,600),[800,1000).
	for _, b := range []uint64{100, 150, 250, 350, 450, 550, 900} {
		pruneSeedRow(t, conn, db, addr, tok, b)
	}

	if err := CompactionPruneState(context.Background(), store, 0, 2000, 200); err != nil {
		t.Fatalf("CompactionPruneState: %v", err)
	}

	// Non-latest rows within a bucket are dropped.
	for _, deleted := range []uint64{100, 250, 450} {
		if got := pruneCountAtBlock(t, conn, db, deleted); got != 0 {
			t.Errorf("count at block %d = %d, want 0 (superseded within its bucket)", deleted, got)
		}
	}
	// One surviving snapshot per bucket (the bucket's latest).
	for _, kept := range []uint64{150, 350, 550, 900} {
		if got := pruneCountAtBlock(t, conn, db, kept); got != 1 {
			t.Errorf("count at block %d = %d, want 1 (per-bucket latest must survive)", kept, got)
		}
	}
}

// TestPruneUsesPruneConnNotHotConn is the decoupling proof: it hands
// CompactionPruneState a store whose Conn() is a *closed* connection and
// whose PruneConn() is the only live one. If the optional-interface seam in
// CompactionPruneState (internal/template/templates/code/compaction.go.tmpl)
// ever regressed to preferring Conn(), this query would fail against the
// closed socket; a pass here is a positive, deterministic proof — not just an
// inference from reading the source — that the prune runs on PruneConn.
func TestPruneUsesPruneConnNotHotConn(t *testing.T) {
	conn, prune, db := pruneTestDial(t)
	addr, tok := pruneAddr(7), prHash(7)
	pruneSeedRow(t, conn, db, addr, tok, 1500)
	pruneSeedRow(t, conn, db, addr, tok, 100)

	// conn did the setup above; close it now so the store's Conn() is dead and
	// only PruneConn() (prune) can possibly serve CompactionPruneState's
	// queries. Drop the throwaway database via prune (still live) before
	// pruneTestDial's own cleanup runs and closes it too — t.Cleanup is LIFO,
	// so registering this after pruneTestDial guarantees it runs first.
	conn.Close()
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = prune.Do(dropCtx, ch.Query{Body: "DROP DATABASE IF EXISTS " + db})
	})
	store := pruneStore{conn: conn, pruneConn: prune, db: db}

	if err := CompactionPruneState(context.Background(), store, 0, 2000, 100000); err != nil {
		t.Fatalf("CompactionPruneState with a closed Conn: %v (it should have run entirely on PruneConn)", err)
	}

	if got := pruneCountAtBlock(t, prune, db, 100); got != 0 {
		t.Errorf("count at block 100 = %d, want 0 (PruneConn must have actually executed the DELETE)", got)
	}
	if got := pruneCountAtBlock(t, prune, db, 1500); got != 1 {
		t.Errorf("count at block 1500 = %d, want 1", got)
	}
}

// TestPruneStartPollWaitLifecycle is the async dispatch test: StartPrune (a)
// returns to the caller without waiting for the DELETE round trip — this is
// the actual "decouple from the consumer's hot path" property, since the
// consumer/fold goroutine calls StartPrune and must keep processing blocks
// rather than blocking for however long the mutation takes — (b) refuses to
// start a second prune while one is in flight, and (c) completes
// asynchronously, observable via PollPrune/WaitPrune, advancing
// LastPruneBlock only once the result is actually observed.
func TestPruneStartPollWaitLifecycle(t *testing.T) {
	conn, prune, db := pruneTestDial(t)
	store := pruneStore{conn: conn, pruneConn: prune, db: db}

	addr, tok := pruneAddr(5), prHash(5)
	for _, b := range []uint64{100, 1500} {
		pruneSeedRow(t, conn, db, addr, tok, b)
	}

	state := NewState()
	dispatchStart := time.Now()
	if ok := state.StartPrune(context.Background(), store, 0, 2000, 100000); !ok {
		t.Fatal("StartPrune returned false, want true (no prune should be in flight yet)")
	}
	dispatchElapsed := time.Since(dispatchStart)
	if dispatchElapsed > 200*time.Millisecond {
		t.Errorf("StartPrune took %s to return, want it to dispatch and return near-instantly (it must not block on the DELETE round trip)", dispatchElapsed)
	}

	if ok := state.StartPrune(context.Background(), store, 0, 2000, 100000); ok {
		t.Error("StartPrune returned true while a prune was already in flight, want false (only one prune at a time)")
	}

	if err := state.WaitPrune(context.Background()); err != nil {
		t.Fatalf("WaitPrune: %v", err)
	}
	if state.PruneInFlight() {
		t.Error("PruneInFlight() is true after WaitPrune returned, want false")
	}
	if state.LastPruneBlock != 2000 {
		t.Errorf("LastPruneBlock = %d, want 2000 after a successful prune", state.LastPruneBlock)
	}
	if got := pruneCountAtBlock(t, conn, db, 100); got != 0 {
		t.Errorf("count at block 100 = %d, want 0 (the background prune must have actually executed)", got)
	}
	if got := pruneCountAtBlock(t, conn, db, 1500); got != 1 {
		t.Errorf("count at block 1500 = %d, want 1", got)
	}

	// PollPrune on an already-drained channel must be a no-op, not a re-read.
	if err := state.PollPrune(); err != nil {
		t.Errorf("PollPrune after WaitPrune drained the channel: %v, want nil", err)
	}
}
`
