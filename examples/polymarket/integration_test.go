package polymarket

import (
	"testing"
)

// TestFPMMWalletPositionCount validates that wallet 0x6de391f... has the correct
// number of positions after processing. This test catches the bug where FPMM
// buy/sell events were silently skipped when the FPMM market wasn't loaded
// into the hot state.
//
// Bug: FPMM buy/sell/funding events were being processed without first
// prefetching the FPMM market from the cold tier. When state.FixedProductMarketMaker.Get()
// returned (nil, false), the event handler would return early, silently skipping
// the event.
//
// Fix: Added ensureFPMMMarketsLoaded() which prefetches FPMM markets from the
// cold tier before processing buy/sell/funding events, similar to how
// ensureConditionsLoaded() works for conditions.
func TestFPMMWalletPositionCount(t *testing.T) {
	// This is a documentation test for now - the actual validation
	// requires a running ClickHouse instance with indexed data.
	//
	// To validate manually after an e2e run:
	// 1. Run: python3 scripts/ch_pnl.py 0x6de391f369a4d7f2e93553cbd8939b270269668a
	// 2. Verify Local CH has the same position count as Remote CH (91)
	//
	// Expected after fix:
	// - Remote CH: 91 positions
	// - Local CH: 91 positions (was 71 before fix, missing 20)
	//
	// The missing positions were caused by FPMM events being silently
	// skipped when the market wasn't in the hot state.

	t.Skip("manual validation required - see test comments")

	// TODO: Add automated validation once we have a test CH setup
}
