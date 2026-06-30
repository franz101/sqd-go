package polymarket

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
)

// TestRedemptionBooksResolutionLoss is a regression guard for the
// "unbooked resolution losses → overstated PnL" bug.
//
// Background (see memory: polymarket-ctf-stream-stuck-unbooked-losses): a held-
// to-zero losing position only realizes its loss at market RESOLUTION, via the
// redemption handlers. Both handlePayoutRedemptionNR (custom_processor.go:1293)
// and handlePayoutRedemptionCTF (:1260) early-return unless cond.Resolved, which
// is set ONLY by handleConditionResolution (:1255) on the ConditionalTokens
// ConditionResolution event. In the local DB the ConditionalTokens stream had
// stalled at the start block, so memory_conditions was empty, every redemption
// no-op'd, and realized losses were silently dropped — inflating wallet PnL
// (one wallet: +$238k local vs +$4.6k true).
//
// This test locks both halves of that contract:
//   - resolved   → the losing-outcome redemption books the full realized loss.
//   - unresolved → the SAME redemption books nothing (the production failure
//     mode). If someone "fixes" this by booking without a resolution, or breaks
//     the resolved path, this test fails.
func TestRedemptionBooksResolutionLoss(t *testing.T) {
	conditionID := common.HexToHash("0x37e6c1227c1ee562cbd906099dadbd658ddca8cdb9730736857b4d82701c6393")
	user := common.HexToAddress("0x28fd0b8379233bd4d424220ae892b840b4586e18")
	const loseOutcome uint8 = 1 // outcome 0 wins (payout 1), outcome 1 loses (payout 0)

	// avgPrice 0.30, holding 100 outcome tokens → cost basis 30; resolves to 0.
	price30 := mustDec(t, "0.3")
	amt100, _ := protomath.FromInt64(100, protomath.Decimal256Scale18)
	wantLoss, _ := protomath.FromInt64(-30, protomath.Decimal256Scale18)
	meta := generated.EventMeta{BlockNumber: 84162482}

	// buildLosingPosition returns a state where `user` holds 100 of the losing
	// outcome at avg 0.30 (no PnL realized yet).
	buildLosingPosition := func() (*generated.State, uint256.Int) {
		state := generated.NewState()
		state.HotState = generated.NewHotState(1 << 12)
		tokenID := getNegRiskPositionIDByCondition(conditionID, loseOutcome)
		updateUserPositionWithBuyD256(state, user, tokenID, price30, amt100, protomath.Decimal256{}, meta)
		return state, tokenID
	}

	resolution := &generated.ConditionalTokensConditionResolution{
		EventMeta:        meta,
		ConditionID:      conditionID,
		PayoutNumerators: []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(0)},
	}
	// Redeemer turns in their 100 losing tokens (USDC raw = 6 decimals).
	redemption := &generated.NegRiskAdapterPayoutRedemption{
		EventMeta:   meta,
		Redeemer:    user,
		ConditionID: conditionID,
		Amounts:     []uint256.Int{*uint256.NewInt(0), *uint256.NewInt(100_000000)},
	}

	t.Run("resolved_books_full_loss", func(t *testing.T) {
		state, tokenID := buildLosingPosition()
		handleConditionResolution(state, resolution)
		handlePayoutRedemptionNR(state, redemption)

		up, ok := getUserPositionValue(state, user, tokenID)
		if !ok {
			t.Fatal("position vanished after redemption")
		}
		if !up.RealizedPnL.Eq(wantLoss) {
			t.Fatalf("realized PnL = %s, want -30 (full cost-basis loss)",
				up.RealizedPnL.String(protomath.Decimal256Scale18))
		}
		if !up.Amount.IsZero() {
			t.Fatalf("position not closed: amount = %s, want 0",
				up.Amount.String(protomath.Decimal256Scale18))
		}
	})

	t.Run("unresolved_books_nothing", func(t *testing.T) {
		// The production failure mode: ConditionResolution never ingested, so the
		// condition is absent/unresolved and the redemption silently no-ops. This
		// is exactly how the +$234k of losses went missing — asserting it makes
		// the dependency explicit rather than a silent data-coverage assumption.
		state, tokenID := buildLosingPosition()
		handlePayoutRedemptionNR(state, redemption) // no resolution first

		up, ok := getUserPositionValue(state, user, tokenID)
		if !ok {
			t.Fatal("position vanished")
		}
		if !up.RealizedPnL.IsZero() {
			t.Fatalf("loss booked without resolution: realized PnL = %s, want 0",
				up.RealizedPnL.String(protomath.Decimal256Scale18))
		}
		if !up.Amount.Eq(amt100) {
			t.Fatalf("position altered without resolution: amount = %s, want 100",
				up.Amount.String(protomath.Decimal256Scale18))
		}
	})
}

func mustDec(t *testing.T, s string) protomath.Decimal256 {
	t.Helper()
	d, err := protomath.ParseDecimal256(s, protomath.Decimal256Scale18)
	if err != nil {
		t.Fatalf("ParseDecimal256(%q): %v", s, err)
	}
	return d
}
