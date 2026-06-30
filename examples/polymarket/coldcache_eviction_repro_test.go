//go:build e2e
// +build e2e

package polymarket

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestColdCacheAuthoritativeEvictionResetsPosition reproduces a memory-bound
// correctness bug in the authoritative cold-cache read path.
//
// PositionState.GetValue (generated/state.go) treats a hot+cold miss as
// "provably new" and SKIPS ClickHouse when the cold tier is authoritative:
//
//	val, ok := s.HotState.UserPositions.GetByFields(user, tokenID) // hot + cold
//	if !ok {
//	    if s.HotState.coldAuthoritative {
//	        return Position{}, false   // <-- assumes a miss means never-seen
//	    }
//	    ... // non-authoritative: resolve from ClickHouse (correct)
//	}
//
// That assumption holds only if the cold tier never evicts. The in-RAM flat
// backend (SQD_COLDCACHE_BACKEND=flat) — the one you use to stay under a memory
// bound — is capacity-bounded and DOES evict (CLOCK). So a position evicted from
// both hot and cold is wrongly treated as brand-new; the next buy/sell rebuilds
// it from zero, corrupting amount/avgPrice/totalBought/realizedPnL. This is the
// PnL discrepancy seen in the local ClickHouse under memory pressure.
//
// The test runs one scenario twice — identical except for `authoritative`:
//   - authoritative=false: the evicted position is recovered from ClickHouse
//     (the durable truth), so the cumulative amount is correct (20). [control]
//   - authoritative=true:  ClickHouse is skipped, the position resets to zero,
//     and the cumulative amount is wrong (10). [reproduces the bug]
//
// Run:
//
//	SQD_COLDCACHE_BACKEND=flat go test ./examples/polymarket/... -tags e2e \
//	    -run TestColdCacheAuthoritativeEvictionResetsPosition -v
func TestColdCacheAuthoritativeEvictionResetsPosition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cold-cache eviction integration test in short mode")
	}
	// The bug only manifests with the bounded in-RAM cold tier; the default
	// Pebble backend is disk-backed and never evicts, so a hot+cold miss really
	// is provably-new there.
	t.Setenv("SQD_COLDCACHE_BACKEND", "flat")

	projectRoot := findProjectRoot()
	ctx := context.Background()

	want := decimal.NewFromInt(20) // 10 (first buy) + 10 (second buy), same price

	// Control: the non-authoritative path resolves the evicted position from
	// ClickHouse. If this fails, the harness itself is wrong (the durable row was
	// never readable), not the bug under test.
	if got := runColdEvictionScenario(t, ctx, projectRoot, false); !got.Equal(want) {
		t.Fatalf("control (authoritative=false): cumulative amount = %s, want %s; "+
			"the evicted position should be recovered from ClickHouse", got, want)
	}

	// Reproduction: the authoritative path skips ClickHouse on the hot+cold miss
	// and rebuilds the position from zero.
	if got := runColdEvictionScenario(t, ctx, projectRoot, true); !got.Equal(want) {
		t.Errorf("BUG reproduced (authoritative=true): cumulative amount = %s, want %s. "+
			"A hot+cold miss under the bounded cold tier is NOT provably-new — "+
			"PositionState.GetValue must fall back to ClickHouse before treating the key as new.", got, want)
	}
}

// runColdEvictionScenario buys a position, commits it to ClickHouse, evicts it
// from both the hot ring and the (bounded) cold tier with unrelated positions,
// then buys it again. It returns the resulting cumulative amount.
func runColdEvictionScenario(t *testing.T, ctx context.Context, projectRoot string, authoritative bool) decimal.Decimal {
	t.Helper()

	store := setupTestClickHouse(t, ctx, projectRoot, fmt.Sprintf("polymarket_evict_auth_%v", authoritative))

	// Small hot ring so a handful of fillers spill into the cold tier; the flat
	// cold tier floors at 65536 slots, so `fillers` must exceed that to evict.
	const hotCap = 64
	s := generated.NewState()
	s.SetSnapshotsEnabled(false)
	s.HotState = generated.NewHotState(hotCap)
	s.Store = store

	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, authoritative, 1, 0); err != nil {
		t.Fatalf("enable cold cache (authoritative=%v): %v", authoritative, err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	user := common.HexToAddress("0x10f5b9bd80fc212b718c5dced42f0cff57a6c701")
	token := uint256.NewInt(0xBEEF)
	tokenHash := tokenIDHash(*token)
	price := decimal.NewFromFloat(0.5)
	ten := decimal.NewFromInt(10)
	meta := func(b uint64) generated.EventMeta {
		return generated.EventMeta{BlockNumber: b, BlockTimestamp: time.Unix(int64(b), 0).UTC()}
	}

	// 1) First buy: 10 tokens @ 0.5.
	updateUserPositionWithBuy(s, user, *token, price, ten, decimal.Zero, meta(1))

	// 2) Commit so ClickHouse (the durable truth) holds amount=10. This is the
	//    state a from-genesis backfill would have persisted before the position
	//    aged out of memory.
	if err := s.Commit(ctx, store); err != nil {
		t.Fatalf("commit position to ClickHouse: %v", err)
	}
	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("flush async inserts: %v", err)
	}

	// 3) Eviction pressure: distinct positions, written straight into the cache
	//    (they stand in for other users' positions churning through memory). Each
	//    SetByKey evicts a hot victim to cold; once the cold tier's 65536 slots
	//    overflow, our position is evicted from cold too — a true hot+cold miss.
	const fillers = 80_000
	for i := 0; i < fillers; i++ {
		ft := uint256.NewInt(uint64(1_000_000 + i))
		fh := tokenIDHash(*ft)
		s.HotState.UserPositions.SetByKey(
			generated.UserPositionsClockKey{User: user, TokenID: fh},
			generated.MemoryUserPosition{User: user, TokenID: fh},
		)
	}

	// Guard against a vacuous test: if our position were still resident, the
	// second buy would simply hit the cache and both runs would pass. The
	// authoritative run is the canary — if it returns 20 here, eviction never
	// happened and `fillers` needs to grow.

	// 4) Second buy on the original position: 10 tokens @ 0.5. Correct cumulative
	//    amount = 20.
	updateUserPositionWithBuy(s, user, *token, price, ten, decimal.Zero, meta(uint64(100+fillers+1)))

	// 5) Read back the resulting amount (the position is hot again after step 4).
	pos, ok := s.HotState.UserPositions.GetByFields(user, tokenHash)
	if !ok {
		t.Fatalf("position missing after second buy (authoritative=%v)", authoritative)
	}
	return toDecimal(pos.Amount)
}

// TestColdCacheCommitDropsDoubleEvictedDirty reproduces BUGREPORTZ §3 (BUG #1):
// Commit reads dirty keys via the raw clockcache Get (hot+cold only, no ClickHouse
// fallback), so a position that was updated (dirty) but then evicted from BOTH the
// hot ring and the bounded flat cold tier *before* the commit fires is silently
// dropped — its update never reaches ClickHouse and the dirty flag is cleared. This
// is the commit-path sibling of the read-path eviction bug; the negative filter does
// NOT protect it (the filter is only consulted by GetValue, never by Commit).
//
// Unlike the read-path repro, this needs no second buy: the loss happens at commit.
// With a large SQD_COMMIT_INTERVAL (e.g. dev-fast's 50000) the window for this is
// huge, which is why it bites a memory-bounded flat-backend run.
//
// Run: SQD_COLDCACHE_BACKEND=flat go test ./examples/polymarket/ -tags e2e \
//
//	-run TestColdCacheCommitDropsDoubleEvictedDirty -v
func TestColdCacheCommitDropsDoubleEvictedDirty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping commit-drop integration test in short mode")
	}
	t.Setenv("SQD_COLDCACHE_BACKEND", "flat")

	projectRoot := findProjectRoot()
	ctx := context.Background()
	store := setupTestClickHouse(t, ctx, projectRoot, "polymarket_commit_drop")

	const hotCap = 64
	s := generated.NewState()
	s.SetSnapshotsEnabled(false)
	s.HotState = generated.NewHotState(hotCap)
	s.Store = store
	coldDir := filepath.Join(t.TempDir(), "cold")
	// authoritative is irrelevant here — Commit never consults coldAuthoritative.
	if err := s.HotState.EnableColdCache(coldDir, false, 1, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	user := common.HexToAddress("0x10f5b9bd80fc212b718c5dced42f0cff57a6c701")
	token := uint256.NewInt(0xC0FFEE)
	tokenHash := tokenIDHash(*token)

	// 1) Buy P (10 @ 0.5). This marks P dirty and places it in the hot ring. We do
	//    NOT commit yet (simulating activity between commit cycles).
	updateUserPositionWithBuy(s, user, *token,
		decimal.NewFromFloat(0.5), decimal.NewFromInt(10), decimal.Zero,
		generated.EventMeta{BlockNumber: 1, BlockTimestamp: time.Unix(1, 0).UTC()})

	// 2) Eviction pressure: distinct positions written straight into the cache, so P
	//    is pushed out of the hot ring (cap 64) and then out of the flat cold tier
	//    (floor 65536). These fillers do NOT touch the dirty set — only P is dirty.
	const fillers = 80_000
	for i := 0; i < fillers; i++ {
		ft := uint256.NewInt(uint64(1_000_000 + i))
		fh := tokenIDHash(*ft)
		s.HotState.UserPositions.SetByKey(
			generated.UserPositionsClockKey{User: user, TokenID: fh},
			generated.MemoryUserPosition{User: user, TokenID: fh},
		)
	}

	// 3) Commit. P is still in dirtyUserPositions, but its value is gone from hot+cold.
	if err := s.Commit(ctx, store); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("flush async inserts: %v", err)
	}

	// 4) The committed ClickHouse row must carry P's update (amount=10). If Commit
	//    dropped the double-evicted dirty entry, there is no row at all.
	rows, amount := countAndAmount(t, ctx, store, user, tokenHash)
	t.Logf("after commit: ClickHouse has %d row(s) for P, amount=%s", rows, amount)
	if rows == 0 {
		t.Errorf("BUG #3 (commit-drop) reproduced: P was bought (dirty) then evicted from hot+cold " +
			"before commit; Commit's raw Get(key) missed both tiers and SILENTLY DROPPED the update — " +
			"ClickHouse has 0 rows for the position. Commit must reconcile a double-evicted dirty key " +
			"(resolve from CH or keep a dirty value overlay) instead of skipping it.")
	} else if amount != "10" {
		t.Errorf("commit persisted P but with wrong amount: got %s, want 10", amount)
	}
}

// countAndAmount returns how many rows (FINAL) exist for a position and its amount.
func countAndAmount(t *testing.T, ctx context.Context, store *database.Store, user common.Address, tokenID common.Hash) (uint64, string) {
	t.Helper()
	var cnt proto.ColUInt64
	var amt proto.ColStr
	query := fmt.Sprintf(
		"SELECT count() AS c, toString(round(sum(amount) / 1e18)) AS a "+
			"FROM %s.memory_user_positions WHERE user = unhex('%x') AND token_id = unhex('%x')",
		quoteV2Ident(store.DB()), user.Bytes(), tokenID.Bytes())
	var rows uint64
	var amount string
	err := store.Conn().Do(ctx, ch.Query{
		Body: query,
		Result: proto.Results{
			{Name: "c", Data: &cnt},
			{Name: "a", Data: &amt},
		},
		OnResult: func(_ context.Context, b proto.Block) error {
			for i := 0; i < b.Rows; i++ {
				rows = cnt[i]
				amount = amt.Row(i)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	return rows, amount
}

// TestScopedRecoveryKeepsFilterComplete proves the SQD_RECOVERY_MIN_BLOCK +
// negative-filter design (PERFORMANCE_RESULTS.md lever #1). A recency-scoped cold
// VALUE load must STILL seed the filter with EVERY key, because HotState.Recover
// force-sets coldAuthoritative=true after a load (generated hotstate.go:3831-33):
// without full filter coverage, a skipped (older) real position would be a hot+
// cold miss whose ColdMightContain=false => "provably new" => reset to zero.
//
// Seed two positions — OLD at block 100, RECENT at block 1000 — recover with
// floor=500, and assert: the recent VALUE is in cold; the old value is NOT (scoped
// out); but the old KEY is still "maybe present" in the filter (authoritative-safe).
//
// Run: go test ./examples/polymarket/ -tags e2e -run TestScopedRecoveryKeepsFilterComplete -v
func TestScopedRecoveryKeepsFilterComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scoped-recovery integration test in short mode")
	}
	t.Setenv("SQD_COLDCACHE_BACKEND", "flat")

	projectRoot := findProjectRoot()
	ctx := context.Background()
	store := setupTestClickHouse(t, ctx, projectRoot, "polymarket_scoped_recovery")

	user := common.HexToAddress("0x10f5b9bd80fc212b718c5dced42f0cff57a6c701")
	oldTok := uint256.NewInt(0x0107)
	recentTok := uint256.NewInt(0xBEEF)
	neverTok := uint256.NewInt(0xDEAD)
	oldHash := tokenIDHash(*oldTok)
	recentHash := tokenIDHash(*recentTok)
	neverHash := tokenIDHash(*neverTok)
	price := decimal.NewFromFloat(0.5)
	ten := decimal.NewFromInt(10)

	// 1) Seed ClickHouse: OLD position at block 100, RECENT at block 1000.
	seed := generated.NewState()
	seed.SetSnapshotsEnabled(false)
	seed.HotState = generated.NewHotState(1024)
	seed.Store = store
	updateUserPositionWithBuy(seed, user, *oldTok, price, ten, decimal.Zero,
		generated.EventMeta{BlockNumber: 100, BlockTimestamp: time.Unix(100, 0).UTC()})
	updateUserPositionWithBuy(seed, user, *recentTok, price, ten, decimal.Zero,
		generated.EventMeta{BlockNumber: 1000, BlockTimestamp: time.Unix(1000, 0).UTC()})
	if err := seed.Commit(ctx, store); err != nil {
		t.Fatalf("commit seed positions: %v", err)
	}
	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("flush async inserts: %v", err)
	}

	// 2) Fresh state + cold tier; recover with floor=500 (between the two blocks).
	t.Setenv("SQD_RECOVERY_MIN_BLOCK", "500")
	s := generated.NewState()
	s.SetSnapshotsEnabled(false)
	s.HotState = generated.NewHotState(64)
	s.Store = store
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, true, 1, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	if err := s.HotState.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// 3a) RECENT (block 1000 >= floor 500): its VALUE must be in the cold tier.
	if pos, ok := s.HotState.UserPositions.GetByFields(user, recentHash); !ok {
		t.Errorf("recent position (block 1000) missing from cold tier after scoped recovery")
	} else if got := toDecimal(pos.Amount); !got.Equal(ten) {
		t.Errorf("recent position amount = %s, want 10", got)
	}

	// 3b) OLD (block 100 < floor 500): its VALUE must be scoped OUT of cold...
	if _, ok := s.HotState.UserPositions.GetByFields(user, oldHash); ok {
		t.Errorf("old position (block 100) should have been scoped OUT of the cold VALUE tier")
	}
	// ...but its KEY must still be in the filter, or authoritative mode resets it.
	if !s.HotState.UserPositions.ColdMightContain(user, oldHash) {
		t.Errorf("FILTER INCOMPLETE: the scoped-out old key is absent from the negative filter — " +
			"authoritative recovery (coldAuthoritative forced true) would treat this real position " +
			"as provably-new and reset amount/avgPrice/realizedPnL to zero")
	}
	if !s.HotState.UserPositions.ColdMightContain(user, recentHash) {
		t.Errorf("recent key missing from the negative filter")
	}

	// 3c) A genuinely-new key must be absent, so authoritative mode can correctly
	//     skip ClickHouse for it (the throughput win). A false positive here would
	//     only cost an extra (correct) CH probe, so this is a soft check.
	if s.HotState.UserPositions.ColdMightContain(user, neverHash) {
		t.Logf("note: never-seen key reports maybe-present (Bloom false positive at 2 keys is "+
			"vanishingly unlikely; not a correctness failure) — old=%x recent=%x never=%x",
			oldHash[:4], recentHash[:4], neverHash[:4])
	}
}

// TestAutoRecoveryFloorBelowThresholdLoadsAll exercises the SQD_RECOVERY_MIN_BLOCK=auto
// path through the GENERATED resolver: it runs the count() probe and, because the
// table is far below recoveryAutoScopeMinRows (5M), returns floor 0 — so a small
// entity is loaded IN FULL (no scoping, no extra ClickHouse round-trips). This pins
// that the generated count() SQL is valid and the size guard works.
//
// Run: go test ./examples/polymarket/ -tags e2e -run TestAutoRecoveryFloorBelowThresholdLoadsAll -v
func TestAutoRecoveryFloorBelowThresholdLoadsAll(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping auto-floor integration test in short mode")
	}
	t.Setenv("SQD_COLDCACHE_BACKEND", "flat")

	projectRoot := findProjectRoot()
	ctx := context.Background()
	store := setupTestClickHouse(t, ctx, projectRoot, "polymarket_auto_floor")

	user := common.HexToAddress("0x10f5b9bd80fc212b718c5dced42f0cff57a6c701")
	tokA := uint256.NewInt(0x0101)
	tokB := uint256.NewInt(0x0202)
	hashA := tokenIDHash(*tokA)
	hashB := tokenIDHash(*tokB)
	price := decimal.NewFromFloat(0.5)
	ten := decimal.NewFromInt(10)

	seed := generated.NewState()
	seed.SetSnapshotsEnabled(false)
	seed.HotState = generated.NewHotState(1024)
	seed.Store = store
	// One very old, one very recent — if the auto floor wrongly scoped this 2-row
	// table, the old value would be dropped from cold.
	updateUserPositionWithBuy(seed, user, *tokA, price, ten, decimal.Zero,
		generated.EventMeta{BlockNumber: 100, BlockTimestamp: time.Unix(100, 0).UTC()})
	updateUserPositionWithBuy(seed, user, *tokB, price, ten, decimal.Zero,
		generated.EventMeta{BlockNumber: 84_000_000, BlockTimestamp: time.Unix(840, 0).UTC()})
	if err := seed.Commit(ctx, store); err != nil {
		t.Fatalf("commit seed positions: %v", err)
	}
	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("flush async inserts: %v", err)
	}

	t.Setenv("SQD_RECOVERY_MIN_BLOCK", "auto")
	s := generated.NewState()
	s.SetSnapshotsEnabled(false)
	s.HotState = generated.NewHotState(64)
	s.Store = store
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, true, 1, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	if err := s.HotState.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover (auto floor): %v", err)
	}

	// count() = 2 < 5M => floor 0 => BOTH values must be present in cold.
	if _, ok := s.HotState.UserPositions.GetByFields(user, hashA); !ok {
		t.Errorf("auto floor on a sub-threshold table must load ALL values — old (block 100) was dropped")
	}
	if _, ok := s.HotState.UserPositions.GetByFields(user, hashB); !ok {
		t.Errorf("recent (block 84,000,000) missing under auto/small-table full load")
	}
}

// TestScopedRecoveryWallet0xfNoDataLoss is the END-TO-END data-loss proof for the
// scoped-recovery design, using wallet 0xf05b67 (the known-good balance oracle from
// TestWallet0xf05b67Positions) read through the REAL processor path
// (state.Position.GetValue -> authoritative gate -> ClickHouse resolve).
//
// It seeds the four 0xf positions at an OLD block so SQD_RECOVERY_MIN_BLOCK scopes
// their VALUES out of the cold tier, recovers under an authoritative cold tier, and
// asserts each position still reads back at its exact 0xf amount/avgPrice/realizedPnL/
// totalBought — resolved from ClickHouse via the negative filter, NOT reset to zero.
// This is the "NEVER incorrect/missing data" guarantee for load-scoping: the cold
// VALUE tier holds only the recent working set, but the filter covers every key, so a
// scoped-out real position is "maybe present" -> CH resolve -> correct.
//
// Uses a real ClickHouse test database (isolated, dropped on cleanup) like the other
// 0xf/0x10f5 e2e tests — the resolve-from-CH path is exactly what's under test, so a
// mock store can't stand in for it.
//
// Run: go test ./examples/polymarket/ -tags e2e -run TestScopedRecoveryWallet0xfNoDataLoss -v
func TestScopedRecoveryWallet0xfNoDataLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scoped-recovery 0xf data-loss test in short mode")
	}
	t.Setenv("SQD_COLDCACHE_BACKEND", "flat")

	projectRoot := findProjectRoot()
	ctx := context.Background()
	store := setupTestClickHouse(t, ctx, projectRoot, "polymarket_scoped_0xf")

	user := common.HexToAddress("0xf05b670c0f91f8171984db945a28d2ad0f170cc4")
	dec := decimal.RequireFromString
	type want struct{ amount, avgPrice, realizedPnL, totalBought decimal.Decimal }
	// The exact 0xf oracle (mirrors assertWallet0xf05b67PositionDetails).
	oracle := map[common.Hash]want{
		common.HexToHash("0x0c6a838063f582923c5c7e92655f2fb937ab0bc756f5055da665ee415f8a35dd"): {dec("81.7221"), dec("0.49"), dec("26.7375"), dec("167.9721")},
		common.HexToHash("0x9fd554bb1c9ec1d7f23dd34456c11de34df46f224d6868cdebfce9e8db24e5de"): {dec("549.89"), dec("0.497623"), dec("0.532905"), dec("774.082556")},
		common.HexToHash("0xba813d48ca523eaf501ded2aa5b81f9a4f7807ff5ddaa70d891ae58bf6d83e70"): {decimal.Zero, dec("0.5"), decimal.Zero, dec("440.262556")},
		common.HexToHash("0xefb9a0f75d240bab65404da47db245ae7f7de91f2b1785402b84fe778ae58021"): {dec("0.001514"), dec("0.67"), dec("10.606"), dec("265.151514")},
	}
	metaAt := func(b uint64) generated.EventMeta {
		return generated.EventMeta{BlockNumber: b, BlockTimestamp: time.Unix(int64(b), 0).UTC()}
	}

	// 1) Seed ClickHouse: the four 0xf positions at OLD block 100 (to be scoped out),
	//    plus one RECENT filler at block 1000 so floor=500 keeps something in cold.
	seed := generated.NewState()
	seed.SetSnapshotsEnabled(false)
	seed.HotState = generated.NewHotState(1024)
	seed.Store = store
	for tok, w := range oracle {
		seed.Position.Save(&generated.Position{
			User:        user,
			TokenID:     tok,
			Amount:      fromDecimal(w.amount),
			AvgPrice:    fromDecimal(w.avgPrice),
			RealizedPnL: fromDecimal(w.realizedPnL),
			TotalBought: fromDecimal(w.totalBought),
		}, metaAt(100))
	}
	fillTok := uint256.NewInt(0xF111)
	fillHash := tokenIDHash(*fillTok)
	updateUserPositionWithBuy(seed, user, *fillTok, dec("0.5"), dec("7"), decimal.Zero, metaAt(1000))
	if err := seed.Commit(ctx, store); err != nil {
		t.Fatalf("commit 0xf seed positions: %v", err)
	}
	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("flush async inserts: %v", err)
	}

	// 2) Fresh state + authoritative cold tier; scoped recovery with floor=500.
	t.Setenv("SQD_RECOVERY_MIN_BLOCK", "500")
	s := generated.NewState()
	s.SetSnapshotsEnabled(false)
	s.HotState = generated.NewHotState(64)
	s.Store = store
	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, true, 1, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })
	if err := s.HotState.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover (floor=500): %v", err)
	}

	// 3) Recent filler (block 1000 >= 500) must be in the cold VALUE tier.
	if _, ok := s.HotState.UserPositions.GetByFields(user, fillHash); !ok {
		t.Errorf("recent filler (block 1000) missing from cold tier after scoped recovery")
	}

	// 4) Each 0xf position (block 100 < 500): VALUE scoped out of cold, yet
	//    state.Position.GetValue must resolve it from ClickHouse at the exact amount.
	const epsilon = 0.001
	for tok, w := range oracle {
		short := tok.Hex()[:10]
		if _, ok := s.HotState.UserPositions.GetByFields(user, tok); ok {
			t.Errorf("%s: 0xf position should be scoped OUT of the cold VALUE tier (block 100 < 500)", short)
		}
		pos, ok := s.Position.GetValue(user, tok)
		if !ok {
			t.Errorf("DATA LOSS: scoped-out 0xf position %s did not resolve from ClickHouse — "+
				"authoritative mode treated a real position as provably-new (reset to zero)", short)
			continue
		}
		if !withinTolerance(toDecimal(pos.Amount), w.amount, epsilon) {
			t.Errorf("%s amount: got %s, want %s (resolved from CH after scoped recovery)", short, toDecimal(pos.Amount), w.amount)
		}
		if !withinTolerance(toDecimal(pos.AvgPrice), w.avgPrice, epsilon) {
			t.Errorf("%s avgPrice: got %s, want %s", short, toDecimal(pos.AvgPrice), w.avgPrice)
		}
		if !withinTolerance(toDecimal(pos.RealizedPnL), w.realizedPnL, epsilon) {
			t.Errorf("%s realizedPnL: got %s, want %s", short, toDecimal(pos.RealizedPnL), w.realizedPnL)
		}
		if !withinTolerance(toDecimal(pos.TotalBought), w.totalBought, epsilon) {
			t.Errorf("%s totalBought: got %s, want %s", short, toDecimal(pos.TotalBought), w.totalBought)
		}
	}

	// 5) A genuinely-new token must stay provably-new (authoritative skips CH), so the
	//    throughput win is intact: not every miss pays a ClickHouse round-trip.
	newTok := common.HexToHash("0xdeadbeef00000000000000000000000000000000000000000000000000000001")
	if _, ok := s.Position.GetValue(user, newTok); ok {
		t.Errorf("never-seen token resolved to a value — authoritative mode must treat it as provably-new and skip ClickHouse")
	}
}
