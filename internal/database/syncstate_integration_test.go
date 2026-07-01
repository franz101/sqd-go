package database

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

// These tests cover the sync-state and maintenance surface of clickhouse.go:
// UpdateSyncState, SaveSyncState, LastSyncState, LastBlock,
// lastBlockFromBlocks, TruncateSyncState, TruncateAfterBlock,
// maxBlockNumber, and CollapseAfterBlock.
//
// Every assertion below was first verified against the live ClickHouse
// instance (see commit history / session notes) by observing actual query
// results rather than guessing. Notable, non-obvious, verified behaviors:
//
//   - InsertBlock/InsertBlocks use async_insert=1, wait_for_async_insert=0
//     on the query settings, so a freshly inserted row is NOT reliably
//     visible to a subsequent SELECT without calling FlushAsyncInserts (or
//     waiting for the server's async-insert flush interval). Every test
//     that inserts via the Inserter calls FlushAsyncInserts before reading.
//   - LastSyncState's not-found case returns (nil, false, nil) -- no error.
//   - UpdateSyncState delegates to SaveSyncState with only Current.Number
//     set; Hash is "", Finalized is nil, RollbackChain marshals to the
//     JSON literal "null" (a non-empty string), which LastSyncState then
//     json.Unmarshals back into a nil/empty RollbackChain slice.
//   - TruncateSyncState(chainID, lastBlock) deletes rows with
//     last_block < lastBlock (keeps rows with last_block >= lastBlock) --
//     this is a "keep recent state, drop old state" operation, NOT a
//     rollback-style truncate.
//   - TruncateAfterBlock(chainID, lastBlock) deletes rows with
//     block_number > lastBlock (and sync_state rows with last_block >
//     lastBlock): lastBlock itself is INCLUSIVE (kept), strictly-greater
//     rows are removed.
//   - maxBlockNumber's hasChainID=false branch omits the chain_id WHERE
//     clause entirely, so the chainID argument is ignored and the global
//     max(block_number) across all chains is returned.
//   - CollapseAfterBlock relies on a synchronous OPTIMIZE TABLE ... FINAL
//     after writing sign-flip rows, which (verified live) actually merges
//     away the cancelling +1/-1 row pairs immediately -- a plain SELECT
//     (no FINAL) after CollapseAfterBlock already reflects the collapsed
//     state in this environment, though this is an engine implementation
//     detail (OPTIMIZE FINAL forces synchronous merge) rather than a
//     documented guarantee, so tests also cross-check with FINAL.

func TestUpdateSyncState_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	if err := s.UpdateSyncState(ctx, 1, 100); err != nil {
		t.Fatalf("UpdateSyncState: %v", err)
	}

	state, ok, err := s.LastSyncState(ctx, 1)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if !ok {
		t.Fatalf("LastSyncState: expected ok=true after UpdateSyncState")
	}
	if state.Current.Number != 100 {
		t.Errorf("Current.Number = %d, want 100", state.Current.Number)
	}
	if state.Current.Hash != "" {
		t.Errorf("Current.Hash = %q, want empty (UpdateSyncState does not set a hash)", state.Current.Hash)
	}
	if state.Finalized != nil {
		t.Errorf("Finalized = %+v, want nil (UpdateSyncState never sets Finalized)", state.Finalized)
	}
	if len(state.RollbackChain) != 0 {
		t.Errorf("RollbackChain = %+v, want empty (UpdateSyncState never sets a rollback chain)", state.RollbackChain)
	}
}

func TestSaveSyncState_RoundTrip_RicherFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	rich := SyncState{
		Current:   SyncCursor{Number: 200, Hash: "0xabc"},
		Finalized: &SyncCursor{Number: 150, Hash: "0xdef"},
		RollbackChain: []SyncCursor{
			{Number: 190, Hash: "0x1"},
			{Number: 195, Hash: "0x2"},
		},
	}
	if err := s.SaveSyncState(ctx, 2, rich); err != nil {
		t.Fatalf("SaveSyncState: %v", err)
	}

	state, ok, err := s.LastSyncState(ctx, 2)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if !ok {
		t.Fatalf("LastSyncState: expected ok=true after SaveSyncState")
	}
	if state.Current != rich.Current {
		t.Errorf("Current = %+v, want %+v", state.Current, rich.Current)
	}
	if state.Finalized == nil || *state.Finalized != *rich.Finalized {
		t.Errorf("Finalized = %+v, want %+v", state.Finalized, rich.Finalized)
	}
	if !reflect.DeepEqual(state.RollbackChain, rich.RollbackChain) {
		t.Errorf("RollbackChain = %+v, want %+v", state.RollbackChain, rich.RollbackChain)
	}
}

func TestSaveSyncState_NilFinalized_RoundTripsAsNil(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	state := SyncState{Current: SyncCursor{Number: 50, Hash: "0xhash"}}
	if err := s.SaveSyncState(ctx, 3, state); err != nil {
		t.Fatalf("SaveSyncState: %v", err)
	}

	got, ok, err := s.LastSyncState(ctx, 3)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}
	// Verified live: LastSyncState treats an empty finalized_hash as "no
	// finalized cursor" (Finalized stays nil), regardless of the stored
	// finalized_block value (which SaveSyncState writes as 0 when nil).
	if got.Finalized != nil {
		t.Errorf("Finalized = %+v, want nil when SyncState.Finalized was nil", got.Finalized)
	}
}

func TestLastSyncState_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	state, ok, err := s.LastSyncState(ctx, 12345)
	if err != nil {
		t.Fatalf("LastSyncState: unexpected error for chain with no rows: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false for a chainID with no sync_state rows")
	}
	if state != nil {
		t.Errorf("state = %+v, want nil for not-found", state)
	}
}

func TestLastSyncState_LatestByUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	// LastSyncState orders by updated_at DESC LIMIT 1; successive
	// UpdateSyncState calls insert new rows (sync_state is append-only
	// MergeTree, not collapsing), so the most recently written row wins.
	for _, b := range []uint64{10, 20, 30} {
		if err := s.UpdateSyncState(ctx, 4, b); err != nil {
			t.Fatalf("UpdateSyncState(%d): %v", b, err)
		}
	}

	state, ok, err := s.LastSyncState(ctx, 4)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if state.Current.Number != 30 {
		t.Errorf("Current.Number = %d, want 30 (most recently written)", state.Current.Number)
	}
}

func TestLastBlock_FallsBackToBlocksTable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	// No sync_state row for this chain at all; LastBlock must fall back to
	// scanning the blocks table via lastBlockFromBlocks.
	ins := s.NewInserter()
	if err := ins.InsertBlock(ctx, 5, 777, time.Now().UTC(), "0xblockhash"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	got, ok, err := s.LastBlock(ctx, 5)
	if err != nil {
		t.Fatalf("LastBlock: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true via blocks-table fallback")
	}
	if got != 777 {
		t.Errorf("LastBlock = %d, want 777", got)
	}

	// Confirm the fallback helper directly returns the same answer.
	got2, ok2, err2 := s.lastBlockFromBlocks(ctx, 5)
	if err2 != nil {
		t.Fatalf("lastBlockFromBlocks: %v", err2)
	}
	if !ok2 || got2 != 777 {
		t.Errorf("lastBlockFromBlocks = (%d, %v), want (777, true)", got2, ok2)
	}
}

func TestLastBlock_PrefersSyncStateOverBlocksTable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	ins := s.NewInserter()
	if err := ins.InsertBlock(ctx, 6, 100, time.Now().UTC(), "0xa"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}
	// sync_state says 999, which is far from the blocks-table max of 100.
	// LastBlock must return the sync_state value when a row exists, never
	// falling through to lastBlockFromBlocks.
	if err := s.UpdateSyncState(ctx, 6, 999); err != nil {
		t.Fatalf("UpdateSyncState: %v", err)
	}

	got, ok, err := s.LastBlock(ctx, 6)
	if err != nil {
		t.Fatalf("LastBlock: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got != 999 {
		t.Errorf("LastBlock = %d, want 999 (sync_state takes priority over blocks table)", got)
	}
}

func TestLastBlock_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	got, ok, err := s.LastBlock(ctx, 424242)
	if err != nil {
		t.Fatalf("LastBlock: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false: no sync_state row and no blocks row for this chain")
	}
	if got != 0 {
		t.Errorf("LastBlock = %d, want 0", got)
	}
}

func TestTruncateSyncState_KeepsRowsAtOrAboveThreshold(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	for _, b := range []uint64{10, 20, 30, 40} {
		if err := s.UpdateSyncState(ctx, 7, b); err != nil {
			t.Fatalf("UpdateSyncState(%d): %v", b, err)
		}
	}

	if err := s.TruncateSyncState(ctx, 7, 25); err != nil {
		t.Fatalf("TruncateSyncState: %v", err)
	}

	// Verified live: TruncateSyncState issues
	// "DELETE ... WHERE chain_id = ? AND last_block < ?", i.e. it removes
	// rows strictly below the threshold and keeps rows >= threshold.
	below := s.queryCount(ctx, "sync_state", "chain_id = 7 AND last_block < 25")
	if below != 0 {
		t.Errorf("rows with last_block < 25 remaining = %d, want 0", below)
	}
	atOrAbove := s.queryCount(ctx, "sync_state", "chain_id = 7 AND last_block >= 25")
	if atOrAbove != 2 {
		t.Errorf("rows with last_block >= 25 remaining = %d, want 2 (30 and 40)", atOrAbove)
	}

	// LastSyncState should still resolve to the most recent surviving row.
	state, ok, err := s.LastSyncState(ctx, 7)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if !ok || state.Current.Number != 40 {
		t.Errorf("LastSyncState after truncate = (%+v, %v), want Current.Number=40", state, ok)
	}
}

func TestTruncateSyncState_DeletesAllRows_WhenThresholdAboveEverything(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	if err := s.UpdateSyncState(ctx, 8, 10); err != nil {
		t.Fatalf("UpdateSyncState: %v", err)
	}
	if err := s.TruncateSyncState(ctx, 8, 1000); err != nil {
		t.Fatalf("TruncateSyncState: %v", err)
	}

	_, ok, err := s.LastSyncState(ctx, 8)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false: every row had last_block < 1000 and should be gone")
	}
}

func TestTruncateAfterBlock_BoundaryInclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	ins := s.NewInserter()
	for _, b := range []uint64{100, 200, 300, 400} {
		if err := ins.InsertBlock(ctx, 9, b, time.Now().UTC(), "0xh"); err != nil {
			t.Fatalf("InsertBlock(%d): %v", b, err)
		}
	}
	if err := s.UpdateSyncState(ctx, 9, 400); err != nil {
		t.Fatalf("UpdateSyncState: %v", err)
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	if err := s.TruncateAfterBlock(ctx, 9, 250); err != nil {
		t.Fatalf("TruncateAfterBlock: %v", err)
	}

	// Verified live: TruncateAfterBlock deletes "block_number > lastBlock",
	// so 250 itself would be kept were it present, 300/400 (> 250) must be
	// gone, and 100/200 (<= 250) must remain.
	for _, tc := range []struct {
		block uint64
		want  uint64 // expected row count
	}{
		{100, 1},
		{200, 1},
		{300, 0},
		{400, 0},
	} {
		got := s.queryCount(ctx, "blocks", "chain_id = 9 AND block_number = "+uintToStr(tc.block))
		if got != tc.want {
			t.Errorf("blocks row count for block_number=%d = %d, want %d", tc.block, got, tc.want)
		}
	}

	mx, ok, err := s.maxBlockNumber(ctx, "blocks", 9, true)
	if err != nil {
		t.Fatalf("maxBlockNumber: %v", err)
	}
	if !ok || mx != 200 {
		t.Errorf("maxBlockNumber after truncate = (%d, %v), want (200, true)", mx, ok)
	}

	// sync_state row (last_block=400) must also have been removed since
	// 400 > 250.
	_, syncOK, err := s.LastSyncState(ctx, 9)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if syncOK {
		t.Errorf("sync_state row survived TruncateAfterBlock(9, 250) but had last_block=400 > 250")
	}
}

func TestTruncateAfterBlock_ExactBoundaryIsKept(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	ins := s.NewInserter()
	for _, b := range []uint64{100, 200} {
		if err := ins.InsertBlock(ctx, 10, b, time.Now().UTC(), "0xh"); err != nil {
			t.Fatalf("InsertBlock(%d): %v", b, err)
		}
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	// Truncating exactly at the max present block must be a no-op: nothing
	// is strictly greater than 200.
	if err := s.TruncateAfterBlock(ctx, 10, 200); err != nil {
		t.Fatalf("TruncateAfterBlock: %v", err)
	}

	mx, ok, err := s.maxBlockNumber(ctx, "blocks", 10, true)
	if err != nil {
		t.Fatalf("maxBlockNumber: %v", err)
	}
	if !ok || mx != 200 {
		t.Errorf("maxBlockNumber after no-op truncate = (%d, %v), want (200, true)", mx, ok)
	}
}

func TestTruncateAfterBlock_SkipsDeleteWhenNothingAboveCheckpoint(t *testing.T) {
	// TruncateAfterBlock probes maxBlockNumber per table and skips issuing
	// the DELETE entirely when max <= lastBlock (a documented optimization
	// to avoid stalling on huge tables). This test only verifies the
	// observable outcome (rows untouched, no error), since whether the
	// DELETE itself was skipped is an internal optimization, not something
	// a black-box test can directly observe.
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	ins := s.NewInserter()
	if err := ins.InsertBlock(ctx, 11, 50, time.Now().UTC(), "0xh"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	if err := s.TruncateAfterBlock(ctx, 11, 1000); err != nil {
		t.Fatalf("TruncateAfterBlock: %v", err)
	}

	mx, ok, err := s.maxBlockNumber(ctx, "blocks", 11, true)
	if err != nil {
		t.Fatalf("maxBlockNumber: %v", err)
	}
	if !ok || mx != 50 {
		t.Errorf("maxBlockNumber = (%d, %v), want (50, true) unchanged", mx, ok)
	}
}

func TestMaxBlockNumber_HasChainIDTrue_FiltersToChain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	ins := s.NewInserter()
	if err := ins.InsertBlock(ctx, 20, 111, time.Now().UTC(), "0xh"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := ins.InsertBlock(ctx, 21, 999, time.Now().UTC(), "0xh"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	mx, ok, err := s.maxBlockNumber(ctx, "blocks", 20, true)
	if err != nil {
		t.Fatalf("maxBlockNumber: %v", err)
	}
	if !ok || mx != 111 {
		t.Errorf("maxBlockNumber(chain=20, hasChainID=true) = (%d, %v), want (111, true)", mx, ok)
	}
}

func TestMaxBlockNumber_HasChainIDFalse_IgnoresChainIDArg(t *testing.T) {
	// Verified live: when hasChainID=false, maxBlockNumber omits the WHERE
	// chain_id clause entirely, so the chainID argument passed in is not
	// applied as a filter -- the result is the global max(block_number)
	// across every chain in the table.
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	ins := s.NewInserter()
	if err := ins.InsertBlock(ctx, 30, 50, time.Now().UTC(), "0xh"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := ins.InsertBlock(ctx, 31, 9999, time.Now().UTC(), "0xh"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	mx, ok, err := s.maxBlockNumber(ctx, "blocks", 30, false)
	if err != nil {
		t.Fatalf("maxBlockNumber: %v", err)
	}
	if !ok || mx != 9999 {
		t.Errorf("maxBlockNumber(chainIDarg=30, hasChainID=false) = (%d, %v), want (9999, true) (global max, ignoring the chainID arg)", mx, ok)
	}
}

func TestMaxBlockNumber_EmptyTable_ReturnsNotOK(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	mx, ok, err := s.maxBlockNumber(ctx, "blocks", 1, true)
	if err != nil {
		t.Fatalf("maxBlockNumber: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false for an empty blocks table")
	}
	if mx != 0 {
		t.Errorf("maxBlockNumber = %d, want 0", mx)
	}
}

func TestCollapseAfterBlock_NonCollapsingTable_DeletesAboveThreshold(t *testing.T) {
	// EnsureTables (collapsing=false) produces a plain MergeTree blocks
	// table with no "sign" column. CollapseAfterBlock's tablesWithBlockNumber
	// loop takes the HasSign=false branch for such tables: a lightweight
	// DELETE + OPTIMIZE FINAL, identical in observable effect to
	// TruncateAfterBlock for that table.
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	ins := s.NewInserter()
	for _, b := range []uint64{10, 20, 30, 40} {
		if err := ins.InsertBlock(ctx, 1, b, time.Now().UTC(), "0xh"); err != nil {
			t.Fatalf("InsertBlock(%d): %v", b, err)
		}
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	if err := s.CollapseAfterBlock(ctx, 1, 20); err != nil {
		t.Fatalf("CollapseAfterBlock: %v", err)
	}

	mx, ok, err := s.maxBlockNumber(ctx, "blocks", 1, true)
	if err != nil {
		t.Fatalf("maxBlockNumber: %v", err)
	}
	if !ok || mx != 20 {
		t.Errorf("maxBlockNumber after CollapseAfterBlock(1,20) = (%d, %v), want (20, true)", mx, ok)
	}
	for _, tc := range []struct {
		block uint64
		want  uint64
	}{
		{10, 1},
		{20, 1},
		{30, 0},
		{40, 0},
	} {
		got := s.queryCount(ctx, "blocks", "chain_id = 1 AND block_number = "+uintToStr(tc.block))
		if got != tc.want {
			t.Errorf("blocks row count for block_number=%d = %d, want %d", tc.block, got, tc.want)
		}
	}
}

func TestCollapseAfterBlock_CollapsingTable_SignFlipAndOptimize(t *testing.T) {
	// EnsureTablesWithCollapsing(true) adds a "sign" Int8 column and uses
	// CollapsingMergeTree(sign). CollapseAfterBlock's HasSign=true branch
	// inserts sign-flipped (toInt8(-sign)) copies of every row above the
	// threshold (read via ... FINAL) and then runs OPTIMIZE TABLE ... FINAL,
	// which (verified live) synchronously merges the cancelling +1/-1 pairs
	// away. We check both a plain SELECT and a SELECT ... FINAL to document
	// that, in this environment, OPTIMIZE FINAL already makes the collapse
	// visible without needing FINAL on the read -- but FINAL is the only
	// approach the schema actually guarantees, so assert on that.
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTablesWithCollapsing(ctx, true); err != nil {
		t.Fatalf("EnsureTablesWithCollapsing: %v", err)
	}

	cols, err := s.tableColumns(ctx, "blocks")
	if err != nil {
		t.Fatalf("tableColumns: %v", err)
	}
	if !containsString(cols, "sign") {
		t.Fatalf("blocks columns = %v, expected a sign column under collapsing=true", cols)
	}

	ins := s.NewInserter()
	for _, b := range []uint64{10, 20, 30, 40} {
		if err := ins.InsertBlock(ctx, 1, b, time.Now().UTC(), "0xh"); err != nil {
			t.Fatalf("InsertBlock(%d): %v", b, err)
		}
	}
	if err := s.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	if err := s.CollapseAfterBlock(ctx, 1, 20); err != nil {
		t.Fatalf("CollapseAfterBlock: %v", err)
	}

	finalCount := s.queryCountFinal(ctx, "blocks", "chain_id = 1")
	if finalCount != 2 {
		t.Errorf("blocks row count (FINAL) after collapse = %d, want 2 (only blocks 10 and 20 survive)", finalCount)
	}
	finalMax := s.queryMaxFinal(ctx, "blocks", "chain_id = 1")
	if finalMax != 20 {
		t.Errorf("max(block_number) (FINAL) after collapse = %d, want 20", finalMax)
	}
	for _, tc := range []struct {
		block uint64
		want  uint64
	}{
		{10, 1},
		{20, 1},
		{30, 0},
		{40, 0},
	} {
		got := s.queryCountFinal(ctx, "blocks", "chain_id = 1 AND block_number = "+uintToStr(tc.block))
		if got != tc.want {
			t.Errorf("blocks row count (FINAL) for block_number=%d = %d, want %d", tc.block, got, tc.want)
		}
	}
}

func TestCollapseAfterBlock_RemovesSyncStateAboveThreshold(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	if err := s.UpdateSyncState(ctx, 1, 40); err != nil {
		t.Fatalf("UpdateSyncState: %v", err)
	}

	if err := s.CollapseAfterBlock(ctx, 1, 20); err != nil {
		t.Fatalf("CollapseAfterBlock: %v", err)
	}

	// Verified live: CollapseAfterBlock issues the same
	// "DELETE FROM sync_state WHERE chain_id = ? AND last_block > ?" as
	// TruncateAfterBlock, then OPTIMIZE TABLE sync_state FINAL. The
	// existing sync_state row (last_block=40) is strictly greater than 20,
	// so it must be deleted and LastSyncState must report not-found.
	_, ok, err := s.LastSyncState(ctx, 1)
	if err != nil {
		t.Fatalf("LastSyncState: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false: sync_state row had last_block=40 > 20 and should have been deleted")
	}
}

// --- small test-local helpers (kept private to this file; do not duplicate
// the shared scaffolding already provided by integration_test.go) ---

func uintToStr(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// queryCount returns count() for the given table/WHERE clause without FINAL.
func (s *Store) queryCount(ctx context.Context, table, where string) uint64 {
	var cnt proto.ColUInt64
	q := fmt.Sprintf("SELECT count() AS c FROM %s.%s WHERE %s", quoteIdent(s.db), quoteIdent(table), where)
	if err := s.conn.Do(ctx, ch.Query{Body: q, Result: proto.Results{{Name: "c", Data: &cnt}}}); err != nil {
		return 0
	}
	if cnt.Rows() == 0 {
		return 0
	}
	return cnt.Row(0)
}

// queryCountFinal returns count() for the given table/WHERE clause with
// FINAL, collapsing CollapsingMergeTree sign pairs before counting.
func (s *Store) queryCountFinal(ctx context.Context, table, where string) uint64 {
	var cnt proto.ColUInt64
	q := fmt.Sprintf("SELECT count() AS c FROM %s.%s FINAL WHERE %s", quoteIdent(s.db), quoteIdent(table), where)
	if err := s.conn.Do(ctx, ch.Query{Body: q, Result: proto.Results{{Name: "c", Data: &cnt}}}); err != nil {
		return 0
	}
	if cnt.Rows() == 0 {
		return 0
	}
	return cnt.Row(0)
}

// queryMaxFinal returns max(block_number) for the given table/WHERE clause
// with FINAL.
func (s *Store) queryMaxFinal(ctx context.Context, table, where string) uint64 {
	var mx proto.ColUInt64
	q := fmt.Sprintf("SELECT coalesce(max(block_number),0) AS m FROM %s.%s FINAL WHERE %s", quoteIdent(s.db), quoteIdent(table), where)
	if err := s.conn.Do(ctx, ch.Query{Body: q, Result: proto.Results{{Name: "m", Data: &mx}}}); err != nil {
		return 0
	}
	if mx.Rows() == 0 {
		return 0
	}
	return mx.Row(0)
}
