package ingestion

import (
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/database"
)

type recoveryBase struct {
	Number        uint64
	Hash          string
	FromFinalized bool
	NeedsRollback bool

	TrackerCurrent       *client.BlockRef
	TrackerFinalized     *client.BlockRef
	TrackerRollbackChain []client.BlockRef
}

func selectRecoveryBase(saved *database.SyncState, hasSaved bool) (recoveryBase, bool) {
	if !hasSaved || saved == nil {
		return recoveryBase{}, false
	}

	if finalized := syncCursorPtrToBlockRef(saved.Finalized); finalized != nil {
		unfinalized := filterUnfinalizedRollbackChain(syncCursorsToBlockRefs(saved.RollbackChain), finalized)
		return recoveryBase{
			Number:               finalized.Number,
			Hash:                 finalized.Hash,
			FromFinalized:        true,
			NeedsRollback:        saved.Current.Number > finalized.Number || len(unfinalized) > 0,
			TrackerCurrent:       finalized,
			TrackerFinalized:     finalized,
			TrackerRollbackChain: nil,
		}, true
	}

	current := syncCursorPtrToBlockRef(&saved.Current)
	rollbackChain := syncCursorsToBlockRefs(saved.RollbackChain)
	return recoveryBase{
		Number:               saved.Current.Number,
		Hash:                 saved.Current.Hash,
		NeedsRollback:        true,
		TrackerCurrent:       current,
		TrackerFinalized:     nil,
		TrackerRollbackChain: rollbackChain,
	}, true
}

func filterUnfinalizedRollbackChain(refs []client.BlockRef, finalized *client.BlockRef) []client.BlockRef {
	if len(refs) == 0 {
		return nil
	}
	if finalized == nil {
		return append([]client.BlockRef(nil), refs...)
	}
	out := make([]client.BlockRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Number <= finalized.Number {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func recoverySyncState(base recoveryBase) database.SyncState {
	return database.SyncState{
		Current: database.SyncCursor{
			Number: base.Number,
			Hash:   base.Hash,
		},
		Finalized:     blockRefPtrToSyncCursor(base.TrackerFinalized),
		RollbackChain: blockRefsToSyncCursors(filterUnfinalizedRollbackChain(base.TrackerRollbackChain, base.TrackerFinalized)),
	}
}
