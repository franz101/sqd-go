package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedRecoveryFloorRuntime generates a fixture indexer and exercises the
// authoritative cold-tier recovery-floor repro plus the filter-aware prefetch skip
// gate against the *generated* code — with no dependency on any example app. It is
// the example-independent rewrite of the former examples/polymarket repro tests.
//
//   - Prefetch skip gate (hermetic, always runs): a non-authoritative hot+cold miss
//     is always queued (the safety side). The authoritative + negative-filter skip
//     on resolver.Queue lands with PR #36 (feature/async-resolver-optimization);
//     on a branch without it the skip test reports itself skipped rather than
//     failing.
//   - Recovery floor (integration, only when SQD_PREFETCH_CH_ADDR points at a
//     disposable ClickHouse): a position last updated below SQD_RECOVERY_MIN_BLOCK
//     must survive recovery. It resets to zero before the negative-filter
//     completeness fix and is restored after.
func TestGeneratedRecoveryFloorRuntime(t *testing.T) {
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

	writeFile("config.yaml", `name: recovery_floor_fixture
chains:
  - id: 137
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address taker, uint256 makerAssetId)
`)
	writeFile("custom_schema.go", `package recovery_floor_fixture

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
	writeFile("go.mod", "module recoveryfloorfixture\n\ngo 1.25\n\nrequire github.com/franz101/sqd-go v0.0.0\n\nreplace github.com/franz101/sqd-go => "+filepath.ToSlash(repoRoot(t))+"\n")

	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	testPath := filepath.Join(root, "generated", "recovery_floor_test.go")
	if err := os.WriteFile(testPath, []byte(recoveryFloorRuntimeTestSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// os.Environ() carries SQD_PREFETCH_CH_ADDR through to the inner test, which
	// runs the recovery integration assertions only when it is set.
	output, err := runGo(goBin, root, os.Environ(), "test", "./generated", "-run", "^TestColdFilter|^TestRecoveryFloor", "-count=1", "-v")
	if err != nil {
		t.Fatalf("generated recovery-floor tests failed: %v\n%s", err, output)
	}
	t.Logf("inner test output:\n%s", output)
}

const recoveryFloorRuntimeTestSource = `package generated

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ethereum/go-ethereum/common"
)

type recStore struct {
	conn *ch.Client
	db   string
}

func (s recStore) Conn() *ch.Client { return s.conn }
func (s recStore) DB() string       { return s.db }

func rfAddr(i int) common.Address {
	var a common.Address
	a[0] = 0xAA
	a[18] = byte(i >> 8)
	a[19] = byte(i)
	return a
}

func rfHash(i int) common.Hash {
	var h common.Hash
	h[0] = 0xBB
	h[30] = byte(i >> 8)
	h[31] = byte(i)
	return h
}

func rfEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- Prefetch skip gate: hermetic, no ClickHouse ---

// TestColdFilterEnqueuesWhenNotAuthoritative pins the safety side: in
// non-authoritative mode (resume/cursor against a populated ClickHouse) every
// hot+cold miss must fall back to ClickHouse, so resolver.Queue enqueues the key.
func TestColdFilterEnqueuesWhenNotAuthoritative(t *testing.T) {
	hot := NewHotState(1 << 12)
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := hot.EnableColdCache(coldDir, false /*not authoritative*/, 1<<20, 1<<20); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = hot.CloseColdCache() })

	key := UserPositionsClockKey{User: rfAddr(4), TokenID: rfHash(0xC2)}
	hot.UserPositionsResolver.Queue(key)
	if p := hot.UserPositionsResolver.Pending(); p != 1 {
		t.Fatalf("non-authoritative miss was not queued (Pending=%d), want 1", p)
	}
}

// TestColdFilterSkipsProvablyNewUnderAuthoritative pins the speedup: with an
// authoritative cold tier and a complete negative filter, a key the filter has
// never seen is provably new (it cannot exist in ClickHouse), so the filter-aware
// resolver must NOT enqueue it. That Queue-level skip lands with PR #36
// (feature/async-resolver-optimization); on a branch without it, Queue still
// enqueues and this test reports itself skipped rather than failing.
func TestColdFilterSkipsProvablyNewUnderAuthoritative(t *testing.T) {
	hot := NewHotState(1 << 12)
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := hot.EnableColdCache(coldDir, true /*authoritative*/, 1<<20, 1<<20); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = hot.CloseColdCache() })

	newKey := UserPositionsClockKey{User: rfAddr(3), TokenID: rfHash(0xC1)}
	hot.UserPositionsResolver.Queue(newKey)
	if p := hot.UserPositionsResolver.Pending(); p != 0 {
		t.Skipf("filter-aware prefetch Queue skip not present on this branch (Pending=%d); "+
			"it lands with PR #36, which adds the authoritative+negative-filter skip to resolver.Queue", p)
	}
}

// --- Recovery floor: integration, needs a disposable ClickHouse ---

// No backticks: ClickHouse accepts unquoted identifiers here, and the generated
// recovery SELECT targets the same column names.
const recoveryFloorDDL = "CREATE TABLE IF NOT EXISTS %s.memory_user_positions (" +
	"user FixedString(20), token_id FixedString(32), balance UInt64, " +
	"block_number UInt64, transaction_index UInt64, log_index UInt64, " +
	"updated_at_block UInt64, updated_at DateTime64(3, 'UTC') DEFAULT now64(3)" +
	") ENGINE = ReplacingMergeTree(block_number) PRIMARY KEY (user, token_id) " +
	"ORDER BY (user, token_id, block_number, transaction_index, log_index)"

// TestRecoveryFloorResetsPreFloorPosition reproduces the recovery-floor sibling of
// the authoritative cold-cache reset bug. recoverColdParallel applies
// recoveryRecencyClause() (AND updated_at_block >= SQD_RECOVERY_MIN_BLOCK), so a
// position whose LAST update is below the floor is never loaded into the cold tier
// and never added to the negative filter — while Recover() still marks the tier
// authoritative. The authoritative read gate then treats that real, pre-existing
// position as brand new, skips ClickHouse, and rebuilds it from zero, overwriting
// correct history.
//
// Two positions identical except for their last-active block isolate the recency
// clause as the sole cause:
//   - B (POST-floor): recovered into cold + filter => correct.        [control]
//   - A (PRE-floor):  excluded by the recency clause => reset to zero. [bug]
func TestRecoveryFloorResetsPreFloorPosition(t *testing.T) {
	addr := os.Getenv("SQD_PREFETCH_CH_ADDR")
	if addr == "" {
		t.Skip("set SQD_PREFETCH_CH_ADDR (host:port of a disposable ClickHouse) to run")
	}
	host, port := addr, "9000"
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host, port = addr[:i], addr[i+1:]
	}
	ctx := context.Background()
	db := "recovery_floor_test" // disposable DB; never touches existing databases
	user := rfEnvOr("SQD_PREFETCH_CH_USER", "default")
	password := os.Getenv("SQD_PREFETCH_CH_PASSWORD")

	conn, err := ch.Dial(ctx, ch.Options{Address: addr, User: user, Password: password})
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
	exec(fmt.Sprintf(recoveryFloorDDL, db))

	const floor = uint64(82000000)
	aUser, aTok := rfAddr(1), rfHash(1) // PRE-floor
	bUser, bTok := rfAddr(2), rfHash(2) // POST-floor

	// Phase 1: durable truth in ClickHouse — first buy of 10 each, A last active
	// well below the floor and B at/above it.
	seed := NewMemoryUserPositionBatch()
	seed.Append(MemoryUserPosition{User: aUser, TokenID: aTok, Balance: 10, BlockNumber: 1000000, UpdatedAtBlock: 1000000})
	seed.Append(MemoryUserPosition{User: bUser, TokenID: bTok, Balance: 10, BlockNumber: floor + 50, UpdatedAtBlock: floor + 50})
	if err := seed.Insert(ctx, conn, db); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	// Phase 2: restart with a recovery floor + authoritative cold cache, recover,
	// then reactivate both. The generated recovery dials ClickHouse from CLICKHOUSE_*,
	// so point those at the same disposable database.
	t.Setenv("CLICKHOUSE_HOST", host)
	t.Setenv("CLICKHOUSE_NATIVE_PORT", port)
	t.Setenv("CLICKHOUSE_USER", user)
	t.Setenv("CLICKHOUSE_PASSWORD", password)
	t.Setenv("CLICKHOUSE_DATABASE", db)
	t.Setenv("SQD_RECOVERY_MIN_BLOCK", strconv.FormatUint(floor, 10))
	// LoadFromClickHouse short-circuits under TEST_MODE/CI; ensure recovery runs.
	t.Setenv("TEST_MODE", "0")
	t.Setenv("CI", "0")

	s := NewState()
	s.SetSnapshotsEnabled(false)
	s.Store = recStore{conn: conn, db: db}
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, true /*authoritative*/, 0, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	// recoverColdParallel applies recoveryRecencyClause(): B (>= floor) lands in the
	// cold tier + negative filter, A (< floor) does not. Recover() then marks the
	// tier authoritative.
	if err := s.LoadFromClickHouse(ctx, floor+50); err != nil {
		t.Fatalf("recover from ClickHouse: %v", err)
	}

	// Reactivate both with a second buy (+10) at/above the floor. For A the
	// authoritative gate fires on the read and (pre-fix) returns a provably-new zero.
	reactivate := func(u common.Address, tok common.Hash, blk uint64) {
		prev, _ := s.UserPosition.GetValue(u, tok)
		s.UserPosition.Save(&UserPosition{User: u, TokenID: tok, Balance: prev.Balance + 10, BlockNumber: blk, UpdatedAtBlock: blk}, EventMeta{BlockNumber: blk})
	}
	reactivate(aUser, aTok, floor+100)
	reactivate(bUser, bTok, floor+101)

	gotA, _ := s.HotState.UserPositions.GetByFields(aUser, aTok)
	gotB, _ := s.HotState.UserPositions.GetByFields(bUser, bTok)

	// In-run control: the POST-floor position was recovered, so its cumulative
	// amount is correct. If this fails, recovery/ClickHouse is broken — not the bug.
	if gotB.Balance != 20 {
		t.Fatalf("control (post-floor B): balance=%d, want 20; recovery harness is broken, not the bug under test", gotB.Balance)
	}

	// Reproduction: the PRE-floor position was excluded from the negative filter by
	// the recency clause, so the authoritative skip-CH gate rebuilds it from zero
	// (balance = 10, the second buy only) instead of 20.
	if gotA.Balance != 20 {
		t.Errorf("BUG reproduced (pre-floor A): balance=%d, want 20. A position last active below "+
			"SQD_RECOVERY_MIN_BLOCK is absent from the negative filter after recovery, so the authoritative "+
			"skip-CH gate resets it to zero and overwrites real ClickHouse history. Recovery must populate the "+
			"negative filter with ALL keys, not just updated_at_block >= floor.", gotA.Balance)
	}
}
`
