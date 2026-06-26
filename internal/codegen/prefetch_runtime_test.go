package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedPrefetchRuntime generates a fixture indexer and exercises the
// two-pass batch prefetch (--prefetch) against the *generated* code:
//
//   - Mechanism (hermetic, always runs): the EnablePrefetch/SetRecordMode toggles,
//     that a record-mode dry run queues the read-set WITHOUT any ClickHouse
//     round-trip, that ResolveAllPending with a nil conn is a no-op, and that Save
//     is suppressed while recording.
//   - Round-trip reduction (integration, runs only when SQD_PREFETCH_CH_ADDR points
//     at a disposable ClickHouse): seeds K positions, then proves the lazy path
//     issues one round-trip per distinct missing key (K) while prefetch issues a
//     single batched round-trip (1), serving byte-identical values.
func TestGeneratedPrefetchRuntime(t *testing.T) {
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

	writeFile("config.yaml", `name: prefetch_fixture
chains:
  - id: 137
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address taker, uint256 makerAssetId)
`)
	writeFile("custom_schema.go", `package prefetch_fixture

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
	writeFile("go.mod", "module prefetchfixture\n\ngo 1.25\n\nrequire github.com/franz101/sqd-go v0.0.0\n\nreplace github.com/franz101/sqd-go => "+filepath.ToSlash(repoRoot(t))+"\n")

	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	testPath := filepath.Join(root, "generated", "prefetch_test.go")
	if err := os.WriteFile(testPath, []byte(prefetchRuntimeTestSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// os.Environ() carries SQD_PREFETCH_CH_ADDR through to the inner test, which
	// runs the round-trip integration assertions only when it is set.
	output, err := runGo(goBin, root, os.Environ(), "test", "./generated", "-run", "^TestPrefetch", "-count=1", "-v")
	if err != nil {
		t.Fatalf("generated prefetch tests failed: %v\n%s", err, output)
	}
	t.Logf("inner test output:\n%s", output)
}

const prefetchRuntimeTestSource = `package generated

import (
	"context"
	"fmt"
	"os"
	"testing"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ethereum/go-ethereum/common"
)

type prefetchStore struct {
	conn *ch.Client
	db   string
}

func (s prefetchStore) Conn() *ch.Client { return s.conn }
func (s prefetchStore) DB() string       { return s.db }

func pfAddr(i int) common.Address {
	var a common.Address
	a[0] = 0xAB
	a[18] = byte(i >> 8)
	a[19] = byte(i)
	return a
}

func pfHash(i int) common.Hash {
	var h common.Hash
	h[0] = 0xCD
	h[30] = byte(i >> 8)
	h[31] = byte(i)
	return h
}

func pfBalance(i int) uint64 { return uint64(1000 + i) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- Mechanism: hermetic, no ClickHouse ---

func TestPrefetchToggles(t *testing.T) {
	hot := NewHotState(64)
	if hot.PrefetchEnabled() {
		t.Fatal("prefetch should default off")
	}
	hot.EnablePrefetch(true)
	if !hot.PrefetchEnabled() {
		t.Fatal("EnablePrefetch(true) not reflected")
	}
	if hot.RecordMode() {
		t.Fatal("recordMode should default off")
	}
	hot.SetRecordMode(true)
	if !hot.RecordMode() {
		t.Fatal("SetRecordMode(true) not reflected")
	}
	hot.SetRecordMode(false)
	if err := hot.ResolveAllPending(context.Background(), nil, "x"); err != nil {
		t.Fatalf("ResolveAllPending(nil conn) = %v, want nil no-op", err)
	}
}

// The dry-run pass must collect the read-set purely from local Queue calls — no
// per-key ClickHouse round-trip. A non-nil, never-dialed client proves the record
// path never touches the connection.
func TestPrefetchRecordModeQueuesWithoutRoundTrip(t *testing.T) {
	state := NewState()
	state.Store = prefetchStore{conn: &ch.Client{}, db: "x"}
	hot := state.HotState
	hot.EnablePrefetch(true)
	hot.UserPositionsResolver.EnableMetrics(true)
	hot.SetRecordMode(true)

	if _, ok := state.UserPosition.GetValue(pfAddr(0), pfHash(0)); ok {
		t.Fatal("record-mode GetValue should report a miss")
	}
	if _, ok := state.UserPosition.GetValue(pfAddr(1), pfHash(1)); ok {
		t.Fatal("record-mode GetValue should report a miss")
	}
	if got := hot.UserPositionsResolver.Pending(); got < 2 {
		t.Fatalf("Pending()=%d, want >=2 queued keys after dry run", got)
	}
	if rt := hot.UserPositionsResolver.Metrics().RoundTrips; rt != 0 {
		t.Fatalf("dry run issued %d ClickHouse round-trips, want 0", rt)
	}
}

// TestPrefetchDryRunSkipsUnreachedDependentRead proves the claim "dry run misses A
// -> never queues B": when B's read is guarded by A being found, and A misses in the
// dry run, the dependent read of B is never reached, so B is never queued (the apply
// pass rescues it lazily — covered by TestPrefetchEquivalenceDependentReads).
func TestPrefetchDryRunSkipsUnreachedDependentRead(t *testing.T) {
	state := NewState()
	state.Store = prefetchStore{conn: &ch.Client{}, db: "x"}
	hot := state.HotState
	hot.EnablePrefetch(true)
	hot.UserPositionsResolver.EnableMetrics(true)

	addrA, hashA := pfAddr(1), pfHash(1)
	hashB := pfHash(2)

	hot.SetRecordMode(true)
	if a, ok := state.UserPosition.GetValue(addrA, hashA); ok {
		// Dependent read: only reached when A is found. A misses in the dry run, so
		// this never runs and B is never queued.
		state.UserPosition.GetValue(addrA, hashB)
		_ = a
	}
	hot.SetRecordMode(false)

	if q := hot.UserPositionsResolver.Metrics().QueuedMisses; q != 1 {
		t.Fatalf("queued misses=%d, want 1 (only A; B's dependent read was never reached)", q)
	}
	if p := hot.UserPositionsResolver.Pending(); p != 1 {
		t.Fatalf("pending=%d, want 1 (B must not be queued)", p)
	}
}

// TestPrefetchDryRunSuppressesNewKeyCreate proves the "fetched-then-not-found-because-
// not-saved" claim: a brand-new key created during the dry run is not written to the
// ring (Save suppressed), so a read-after-write in the same dry-run pass still misses.
// The apply pass creates it for real (covered by TestPrefetchEquivalenceDependentReads).
func TestPrefetchDryRunSuppressesNewKeyCreate(t *testing.T) {
	state := NewState()
	state.Store = prefetchStore{conn: &ch.Client{}, db: "x"}
	hot := state.HotState
	hot.EnablePrefetch(true)

	addrC, hashC := pfAddr(3), pfHash(3)
	meta := EventMeta{BlockNumber: 1}

	hot.SetRecordMode(true)
	cv, _ := state.UserPosition.GetValue(addrC, hashC) // miss (new key)
	state.UserPosition.Save(&UserPosition{User: addrC, TokenID: hashC, Balance: cv.Balance + 7}, meta)
	cv2, ok := state.UserPosition.GetValue(addrC, hashC) // read-after-write: still a miss in the dry run
	hot.SetRecordMode(false)

	if ok {
		t.Fatalf("read-after-write saw the suppressed write (balance=%d); dry-run Save must be a no-op", cv2.Balance)
	}
	if _, present := hot.UserPositions.GetByFields(addrC, hashC); present {
		t.Fatal("new key C present in ring after dry run; Save must be suppressed")
	}
	if hot.UserPositionsResolver.Pending() < 1 {
		t.Fatal("C should have been queued during the dry run")
	}
}

func TestPrefetchSaveSuppressed(t *testing.T) {
	state := NewState()
	hot := state.HotState
	a, h := pfAddr(7), pfHash(7)

	hot.SetRecordMode(true)
	state.UserPosition.Save(&UserPosition{User: a, TokenID: h, Balance: 42}, EventMeta{BlockNumber: 1})
	if _, ok := hot.UserPositions.GetByFields(a, h); ok {
		t.Fatal("record-mode Save must be suppressed (no write to the ring)")
	}

	hot.SetRecordMode(false)
	state.UserPosition.Save(&UserPosition{User: a, TokenID: h, Balance: 42}, EventMeta{BlockNumber: 1})
	got, ok := hot.UserPositions.GetByFields(a, h)
	if !ok || got.Balance != 42 {
		t.Fatalf("apply-mode Save failed: ok=%v balance=%d want 42", ok, got.Balance)
	}
}

// --- Round-trip reduction: integration, needs a disposable ClickHouse ---

// No backticks: ClickHouse accepts unquoted identifiers here, and the generated
// resolver's backtick-quoted SELECT targets the same column names.
const prefetchDDL = "CREATE TABLE IF NOT EXISTS %s.memory_user_positions (" +
	"user FixedString(20), token_id FixedString(32), balance UInt64, " +
	"block_number UInt64, transaction_index UInt64, log_index UInt64, " +
	"updated_at_block UInt64, updated_at DateTime64(3, 'UTC') DEFAULT now64(3)" +
	") ENGINE = ReplacingMergeTree(block_number) PRIMARY KEY (user, token_id) " +
	"ORDER BY (user, token_id, block_number, transaction_index, log_index)"

func TestPrefetchRoundTripsCH(t *testing.T) {
	addr := os.Getenv("SQD_PREFETCH_CH_ADDR")
	if addr == "" {
		t.Skip("set SQD_PREFETCH_CH_ADDR (host:port of a disposable ClickHouse) to run")
	}
	ctx := context.Background()
	db := "prefetch_rt_test" // disposable DB; never touches existing databases

	conn, err := ch.Dial(ctx, ch.Options{
		Address:  addr,
		User:     envOr("SQD_PREFETCH_CH_USER", "default"),
		Password: os.Getenv("SQD_PREFETCH_CH_PASSWORD"),
	})
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	exec := func(q string) {
		t.Helper()
		if err := conn.Do(ctx, ch.Query{Body: q}); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec("DROP DATABASE IF EXISTS " + db)
	exec("CREATE DATABASE " + db)
	defer exec("DROP DATABASE IF EXISTS " + db)
	exec(fmt.Sprintf(prefetchDDL, db))

	const K = 200
	store := prefetchStore{conn: conn, db: db}

	// Seed K positions via the generated columnar inserter (the real write path).
	batch := NewMemoryUserPositionBatch()
	for i := 0; i < K; i++ {
		batch.Append(MemoryUserPosition{
			User: pfAddr(i), TokenID: pfHash(i), Balance: pfBalance(i),
			BlockNumber: 1, UpdatedAtBlock: 1,
		})
	}
	if err := batch.Insert(ctx, conn, db); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	// LAZY: one ClickHouse round-trip per distinct missing key.
	lazy := NewState()
	lazy.Store = store
	lazy.HotState.UserPositionsResolver.EnableMetrics(true)
	for i := 0; i < K; i++ {
		v, ok := lazy.UserPosition.GetValue(pfAddr(i), pfHash(i))
		if !ok || v.Balance != pfBalance(i) {
			t.Fatalf("lazy GetValue[%d]: ok=%v balance=%d want %d", i, ok, v.Balance, pfBalance(i))
		}
	}
	lazyRT := lazy.HotState.UserPositionsResolver.Metrics().RoundTrips

	// PREFETCH: dry-run collects the read-set; one batched round-trip resolves it.
	pf := NewState()
	pf.Store = store
	pf.HotState.EnablePrefetch(true)
	pf.HotState.UserPositionsResolver.EnableMetrics(true)
	pf.HotState.SetRecordMode(true)
	for i := 0; i < K; i++ {
		pf.UserPosition.GetValue(pfAddr(i), pfHash(i)) // queues, no round-trip
	}
	pf.HotState.SetRecordMode(false)
	if err := pf.HotState.ResolveAllPending(ctx, conn, db); err != nil {
		t.Fatalf("ResolveAllPending: %v", err)
	}
	pfRT := pf.HotState.UserPositionsResolver.Metrics().RoundTrips

	// Warm reads after prefetch must hit the cache (no further round-trips) and
	// return values identical to the lazy path.
	for i := 0; i < K; i++ {
		v, ok := pf.UserPosition.GetValue(pfAddr(i), pfHash(i))
		if !ok || v.Balance != pfBalance(i) {
			t.Fatalf("prefetch warm GetValue[%d]: ok=%v balance=%d want %d", i, ok, v.Balance, pfBalance(i))
		}
	}
	pfRTAfterWarm := pf.HotState.UserPositionsResolver.Metrics().RoundTrips

	if lazyRT != K {
		t.Fatalf("lazy round-trips=%d, want %d (one per distinct key)", lazyRT, K)
	}
	if pfRT != 1 {
		t.Fatalf("prefetch round-trips=%d, want 1 (single batched resolve)", pfRT)
	}
	if pfRTAfterWarm != 1 {
		t.Fatalf("prefetch warm reads added round-trips: got %d, want still 1", pfRTAfterWarm)
	}
	t.Logf("ClickHouse round-trips for %d keys: lazy=%d  prefetch=%d  (%dx fewer)", K, lazyRT, pfRT, lazyRT/pfRT)
}

// TestPrefetchEquivalenceDependentReads is the correctness test for the case the
// dry run gets WRONG: reads whose existence/keys depend on values the dry run
// can't see yet. It drives the real generated two-pass CustomProcessing with a
// handler that has:
//
//   - a dependent read: B is read only when A is found. The dry run misses A (not
//     resolved yet), so it never takes the branch and never even queues B. Prefetch
//     therefore does NOT prefetch B — the apply pass must rescue it via the lazy
//     fallback.
//   - a read-after-write on a brand-new key C (absent in ClickHouse): created then
//     re-read in the same block. The dry run's Save is suppressed, so the re-read
//     also misses there; only the apply pass produces the right value.
//
// Despite the dry run mispredicting both, the final state must be byte-identical to
// the lazy path (and equal to the hand-computed values) — because the apply pass
// re-runs everything for real and keeps the lazy fallback. In-memory hot state is
// exactly what Commit serializes, so this is the "fixed on the second run / commit"
// guarantee.
func TestPrefetchEquivalenceDependentReads(t *testing.T) {
	addr := os.Getenv("SQD_PREFETCH_CH_ADDR")
	if addr == "" {
		t.Skip("set SQD_PREFETCH_CH_ADDR (host:port of a disposable ClickHouse) to run")
	}
	ctx := context.Background()
	db := "prefetch_dep_test"

	conn, err := ch.Dial(ctx, ch.Options{
		Address:  addr,
		User:     envOr("SQD_PREFETCH_CH_USER", "default"),
		Password: os.Getenv("SQD_PREFETCH_CH_PASSWORD"),
	})
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	exec := func(q string) {
		t.Helper()
		if err := conn.Do(ctx, ch.Query{Body: q}); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec("DROP DATABASE IF EXISTS " + db)
	exec("CREATE DATABASE " + db)
	defer exec("DROP DATABASE IF EXISTS " + db)
	exec(fmt.Sprintf(prefetchDDL, db))

	addrA, hashA := pfAddr(1), pfHash(1)
	hashB := pfHash(2) // dependent token under the same user
	addrC, hashC := pfAddr(3), pfHash(3)

	// Seed A (balance 1000) and B (balance 50); C is left absent on purpose.
	seed := NewMemoryUserPositionBatch()
	seed.Append(MemoryUserPosition{User: addrA, TokenID: hashA, Balance: 1000, BlockNumber: 1, UpdatedAtBlock: 1})
	seed.Append(MemoryUserPosition{User: addrA, TokenID: hashB, Balance: 50, BlockNumber: 1, UpdatedAtBlock: 1})
	if err := seed.Insert(ctx, conn, db); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	prev := CustomProcessFn
	defer func() { CustomProcessFn = prev }()
	CustomProcessFn = func(s *State, _ *ParsedBlock) error {
		meta := EventMeta{BlockNumber: 1}
		if a, ok := s.UserPosition.GetValue(addrA, hashA); ok {
			// Dependent read: only reached when A is found, so the dry run (which
			// misses A) never queues B.
			bv, _ := s.UserPosition.GetValue(addrA, hashB)
			s.UserPosition.Save(&UserPosition{User: addrA, TokenID: hashB, Balance: bv.Balance + a.Balance}, meta)
		}
		// Read-after-write on a brand-new key: create, then re-read in the same block.
		cv, _ := s.UserPosition.GetValue(addrC, hashC)
		s.UserPosition.Save(&UserPosition{User: addrC, TokenID: hashC, Balance: cv.Balance + 7}, meta)
		cv2, _ := s.UserPosition.GetValue(addrC, hashC)
		s.UserPosition.Save(&UserPosition{User: addrC, TokenID: hashC, Balance: cv2.Balance + 100}, meta)
		return nil
	}

	// A single block never trips the commit cadence, so neither run writes back to
	// ClickHouse — both read the same seeded A=1000/B=50 and we compare the
	// resulting in-memory state (what Commit would serialize).
	run := func(prefetch bool) (a, b, c, roundTrips uint64) {
		st := NewState()
		st.Store = prefetchStore{conn: conn, db: db}
		st.HotState.EnablePrefetch(prefetch)
		st.HotState.UserPositionsResolver.EnableMetrics(true)
		if err := CustomProcessing(ctx, st.Store, st, &ParsedBlock{BlockNumber: 1}); err != nil {
			t.Fatalf("CustomProcessing(prefetch=%v): %v", prefetch, err)
		}
		av, _ := st.HotState.UserPositions.GetByFields(addrA, hashA)
		bv, _ := st.HotState.UserPositions.GetByFields(addrA, hashB)
		cvv, _ := st.HotState.UserPositions.GetByFields(addrC, hashC)
		return av.Balance, bv.Balance, cvv.Balance, st.HotState.UserPositionsResolver.Metrics().RoundTrips
	}

	la, lb, lc, lrt := run(false)
	pa, pb, pc, prt := run(true)

	if la != pa || lb != pb || lc != pc {
		t.Fatalf("prefetch diverged from lazy: A %d/%d  B %d/%d  C %d/%d", la, pa, lb, pb, lc, pc)
	}
	// Hand-computed expectation proves the handler did real work (not both broken
	// identically): B = 50 + 1000, C = (0+7) then read-after-write +100.
	if la != 1000 || lb != 1050 || lc != 107 {
		t.Fatalf("unexpected balances: A=%d (want 1000) B=%d (want 1050) C=%d (want 107)", la, lb, lc)
	}
	t.Logf("dependent-read equivalence OK: A=%d B=%d C=%d; round-trips lazy=%d prefetch=%d", la, lb, lc, lrt, prt)
}
`
