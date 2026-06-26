//go:build e2e
// +build e2e

package polymarket

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestColdCacheAuthoritativeRecoveryFloorResetsPreFloorPosition reproduces the
// recovery-floor sibling of the authoritative cold-cache reset bug (see
// TestColdCacheAuthoritativeEvictionResetsPosition for the eviction variant).
//
// The authoritative read-path gate is:
//
//	val, ok := s.HotState.UserPositions.GetByFields(user, tokenID) // hot + cold
//	if !ok {
//	    if s.HotState.coldAuthoritative && !ColdMightContain(user, tokenID) {
//	        return Position{}, false   // <-- "provably new" => SKIP ClickHouse
//	    }
//	    ... // fall back to ClickHouse
//	}
//
// ColdMightContain is the negative Bloom filter. A key is only in the filter if it
// was Put to the cold tier — during processing (on hot eviction) or during
// recovery. But recovery's cold load (recoverColdParallel) carries
// recoveryRecencyClause():
//
//	... AND `updated_at_block` >= SQD_RECOVERY_MIN_BLOCK
//
// so a position whose LAST update was below the floor is never loaded and never
// added to the filter — while Recover() still sets coldAuthoritative=true. The
// gate then treats that real, pre-existing position as brand-new, skips
// ClickHouse, and rebuilds it from zero, overwriting correct history.
//
// Two positions, identical except for the block they were last active at before
// the restart, isolate the recency clause as the sole cause:
//   - B (POST-floor): loaded by recovery into cold + filter => correct. [control]
//   - A (PRE-floor):  excluded by the recency clause => reset to zero. [bug]
//
// This is the DEFAULT (Pebble) backend — unlike the eviction variant it needs no
// flat backend, because the keys are never written to the cold tier at all.
//
// Run:
//
//	go test ./examples/polymarket/ -tags e2e \
//	    -run TestColdCacheAuthoritativeRecoveryFloorResetsPreFloorPosition -v
func TestColdCacheAuthoritativeRecoveryFloorResetsPreFloorPosition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping recovery-floor integration test in short mode")
	}

	const floor = uint64(82_000_000)

	projectRoot := findProjectRoot()
	ctx := context.Background()
	store := setupTestClickHouse(t, ctx, projectRoot, "polymarket_recovery_floor")

	price := decimal.NewFromFloat(0.5)
	ten := decimal.NewFromInt(10)
	want := decimal.NewFromInt(20) // first buy (10) + second buy (10)
	meta := func(b uint64) generated.EventMeta {
		return generated.EventMeta{BlockNumber: b, BlockTimestamp: time.Unix(int64(b), 0).UTC()}
	}

	aUser := common.HexToAddress("0xaa00000000000000000000000000000000000001")
	bUser := common.HexToAddress("0xbb00000000000000000000000000000000000002")
	aTok := uint256.NewInt(0xA1)
	bTok := uint256.NewInt(0xB2)

	// --- Phase 1: establish both positions in ClickHouse (the durable truth). ---
	// A is last active well below the floor; B is at/above it.
	{
		s := generated.NewState()
		s.SetSnapshotsEnabled(false)
		s.HotState = generated.NewHotState(1 << 12)
		s.Store = store

		updateUserPositionWithBuy(s, aUser, *aTok, price, ten, decimal.Zero, meta(1_000_000)) // PRE-floor
		updateUserPositionWithBuy(s, bUser, *bTok, price, ten, decimal.Zero, meta(floor+50))  // POST-floor

		if err := s.Commit(ctx, store); err != nil {
			t.Fatalf("commit positions to ClickHouse: %v", err)
		}
		if err := store.FlushAsyncInserts(ctx); err != nil {
			t.Fatalf("flush async inserts: %v", err)
		}
	}

	// --- Phase 2: restart with a recovery floor, recover, then reactivate both. ---
	t.Setenv("SQD_RECOVERY_MIN_BLOCK", strconv.FormatUint(floor, 10))
	// LoadFromClickHouse short-circuits under TEST_MODE/CI; ensure recovery runs.
	t.Setenv("TEST_MODE", "0")
	t.Setenv("CI", "0")

	s := generated.NewState()
	s.SetSnapshotsEnabled(false)
	s.HotState = generated.NewHotState(1 << 12)
	s.Store = store

	coldDir := filepath.Join(t.TempDir(), "cold")
	if err := s.HotState.EnableColdCache(coldDir, true /*authoritative*/, 0, 0); err != nil {
		t.Fatalf("enable cold cache: %v", err)
	}
	t.Cleanup(func() { _ = s.HotState.CloseColdCache() })

	// Recover hot/cold state at the checkpoint. recoverColdParallel applies
	// recoveryRecencyClause(): B (>= floor) lands in the cold tier + negative
	// filter, A (< floor) does not. Recover() then sets coldAuthoritative=true.
	if err := s.LoadFromClickHouse(ctx, floor+50); err != nil {
		t.Fatalf("recover from ClickHouse: %v", err)
	}

	// Reactivate both with a second identical buy at/above the floor.
	updateUserPositionWithBuy(s, aUser, *aTok, price, ten, decimal.Zero, meta(floor+100))
	updateUserPositionWithBuy(s, bUser, *bTok, price, ten, decimal.Zero, meta(floor+101))

	gotA := readPositionAmount(t, s, aUser, *aTok)
	gotB := readPositionAmount(t, s, bUser, *bTok)

	// In-run control: the POST-floor position was recovered, so its cumulative
	// amount is correct. If this fails, recovery/ClickHouse is broken — not the
	// bug under test.
	if !gotB.Equal(want) {
		t.Fatalf("control (post-floor position B): amount = %s, want %s; "+
			"recovery harness is broken, not the bug under test", gotB, want)
	}

	// Reproduction: the PRE-floor position was excluded from the negative filter by
	// the recency clause, so the authoritative gate skips ClickHouse and rebuilds
	// it from zero (amount = 10, the second buy only) instead of 20.
	if !gotA.Equal(want) {
		t.Errorf("BUG reproduced (pre-floor position A): amount = %s, want %s. "+
			"A position last active below SQD_RECOVERY_MIN_BLOCK is absent from the "+
			"negative filter after recovery, so the authoritative skip-CH gate resets "+
			"it to zero and overwrites real ClickHouse history. Recovery must populate "+
			"the negative filter with ALL keys, not just updated_at_block >= floor.", gotA, want)
	}
}

func readPositionAmount(t *testing.T, s *generated.State, user common.Address, tok uint256.Int) decimal.Decimal {
	t.Helper()
	pos, ok := s.HotState.UserPositions.GetByFields(user, tokenIDHash(tok))
	if !ok {
		t.Fatalf("position missing after reactivation for %s", user.Hex())
	}
	return toDecimal(pos.Amount)
}
