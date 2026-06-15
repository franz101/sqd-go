package ingestion

import (
	"testing"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/database"
)

func TestSelectRecoveryBasePrefersFinalizedCheckpoint(t *testing.T) {
	saved := &database.SyncState{
		Current: database.SyncCursor{Number: 12, Hash: "0x12"},
		Finalized: &database.SyncCursor{
			Number: 10,
			Hash:   "0x10",
		},
		RollbackChain: []database.SyncCursor{
			{Number: 11, Hash: "0x11"},
			{Number: 12, Hash: "0x12"},
		},
	}

	recovery, ok := selectRecoveryBase(saved, true)

	if !ok {
		t.Fatal("expected recovery base")
	}
	if !recovery.FromFinalized {
		t.Fatal("expected finalized recovery base")
	}
	if !recovery.NeedsRollback {
		t.Fatal("expected rollback when current checkpoint is ahead of finalized")
	}
	if recovery.Number != 10 || recovery.Hash != "0x10" {
		t.Fatalf("recovery cursor = %d/%s, want 10/0x10", recovery.Number, recovery.Hash)
	}
	if recovery.TrackerCurrent == nil || *recovery.TrackerCurrent != ref(10, "0x10") {
		t.Fatalf("tracker current = %#v, want finalized cursor", recovery.TrackerCurrent)
	}
	if recovery.TrackerFinalized == nil || *recovery.TrackerFinalized != ref(10, "0x10") {
		t.Fatalf("tracker finalized = %#v, want finalized cursor", recovery.TrackerFinalized)
	}
	if len(recovery.TrackerRollbackChain) != 0 {
		t.Fatalf("rollback chain = %#v, want empty when recovering from finalized", recovery.TrackerRollbackChain)
	}
}

func TestSelectRecoveryBaseSkipsRollbackForCleanFinalizedCheckpoint(t *testing.T) {
	saved := &database.SyncState{
		Current:   database.SyncCursor{Number: 10, Hash: "0x10"},
		Finalized: &database.SyncCursor{Number: 10, Hash: "0x10"},
	}

	recovery, ok := selectRecoveryBase(saved, true)

	if !ok {
		t.Fatal("expected recovery base")
	}
	if !recovery.FromFinalized {
		t.Fatal("expected finalized recovery base")
	}
	if recovery.NeedsRollback {
		t.Fatal("did not expect rollback for current==finalized with empty rollback chain")
	}
}

func TestSelectRecoveryBaseFallsBackToCurrentCheckpoint(t *testing.T) {
	saved := &database.SyncState{
		Current: database.SyncCursor{Number: 12, Hash: "0x12"},
		RollbackChain: []database.SyncCursor{
			{Number: 11, Hash: "0x11"},
			{Number: 12, Hash: "0x12"},
		},
	}

	recovery, ok := selectRecoveryBase(saved, true)

	if !ok {
		t.Fatal("expected recovery base")
	}
	if recovery.FromFinalized {
		t.Fatal("did not expect finalized recovery base")
	}
	if !recovery.NeedsRollback {
		t.Fatal("expected rollback for current recovery")
	}
	if recovery.Number != 12 || recovery.Hash != "0x12" {
		t.Fatalf("recovery cursor = %d/%s, want 12/0x12", recovery.Number, recovery.Hash)
	}
	if recovery.TrackerCurrent == nil || *recovery.TrackerCurrent != ref(12, "0x12") {
		t.Fatalf("tracker current = %#v, want current cursor", recovery.TrackerCurrent)
	}
	wantRollback := []database.SyncCursor{
		{Number: 11, Hash: "0x11"},
		{Number: 12, Hash: "0x12"},
	}
	if got := blockRefsToSyncCursors(recovery.TrackerRollbackChain); !sameSyncCursors(got, wantRollback) {
		t.Fatalf("rollback chain = %#v, want %#v", got, wantRollback)
	}
}

func TestSelectRecoveryBaseAllowsNumberOnlyCurrentCheckpoint(t *testing.T) {
	saved := &database.SyncState{
		Current: database.SyncCursor{Number: 42},
	}

	recovery, ok := selectRecoveryBase(saved, true)

	if !ok {
		t.Fatal("expected recovery base")
	}
	if recovery.Number != 42 || recovery.Hash != "" {
		t.Fatalf("recovery cursor = %d/%s, want 42/<empty>", recovery.Number, recovery.Hash)
	}
	if recovery.TrackerCurrent != nil {
		t.Fatalf("tracker current = %#v, want nil without a hash", recovery.TrackerCurrent)
	}
}

func TestSelectRecoveryBaseReturnsFalseWithoutSavedState(t *testing.T) {
	if recovery, ok := selectRecoveryBase(nil, false); ok {
		t.Fatalf("recovery = %#v, want no recovery base", recovery)
	}
}

func TestFilterUnfinalizedRollbackChainRemovesFinalizedAndOlderBlocks(t *testing.T) {
	refs := []database.SyncCursor{
		{Number: 9, Hash: "0x9"},
		{Number: 10, Hash: "0x10"},
		{Number: 11, Hash: "0x11"},
	}

	filtered := filterUnfinalizedRollbackChain(syncCursorsToBlockRefs(refs), refPtr(10, "0x10"))

	want := []database.SyncCursor{{Number: 11, Hash: "0x11"}}
	if got := blockRefsToSyncCursors(filtered); !sameSyncCursors(got, want) {
		t.Fatalf("filtered rollback chain = %#v, want %#v", got, want)
	}
}

func TestRecoverySyncStateSavesSelectedBase(t *testing.T) {
	base := recoveryBase{
		Number:           10,
		Hash:             "0x10",
		TrackerFinalized: refPtr(10, "0x10"),
		TrackerRollbackChain: []client.BlockRef{
			ref(10, "0x10"),
			ref(11, "0x11"),
		},
	}

	got := recoverySyncState(base)

	if got.Current != (database.SyncCursor{Number: 10, Hash: "0x10"}) {
		t.Fatalf("current = %#v, want selected base", got.Current)
	}
	if got.Finalized == nil || *got.Finalized != (database.SyncCursor{Number: 10, Hash: "0x10"}) {
		t.Fatalf("finalized = %#v, want selected finalized", got.Finalized)
	}
	wantRollback := []database.SyncCursor{{Number: 11, Hash: "0x11"}}
	if !sameSyncCursors(got.RollbackChain, wantRollback) {
		t.Fatalf("rollback chain = %#v, want %#v", got.RollbackChain, wantRollback)
	}
}

func sameSyncCursors(a, b []database.SyncCursor) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
