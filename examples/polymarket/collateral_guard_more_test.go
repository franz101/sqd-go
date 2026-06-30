package polymarket

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
)

// ---------------------------------------------------------------------------
// --restart re-backfill coverage. A `start --restart` drops the polymarket DB
// and re-indexes every split through the guarded handlePositionSplit, so the
// regenerated memory_user_positions depends entirely on the guard:
//   * EVERY supported collateral must still be ACCEPTED + correctly scaled, or
//     the re-backfill silently loses legitimate positions (false-negative).
//   * EVERY unsupported collateral seen on-chain must be DROPPED, or the
//     re-backfill re-pollutes (the $2T/$10B WMATIC inflation comes back).
// Both lists are grounded in the real collateral distribution sampled from
// polymarket.conditional_tokens_position_split_events (2026-06-30).
// ---------------------------------------------------------------------------

var supportedCollateralAddrs = []common.Address{
	common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"), // bridged USDC.e (6-dec, 536M splits)
	common.HexToAddress("0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359"), // native USDC (6-dec)
	common.HexToAddress("0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB"), // pUSD (6-dec)
	common.HexToAddress("0x3A3BD7bb9528E159577F7C2e685CC81A765002E2"), // WCOL / neg-risk wrapped (6-dec, 129M splits)
}

// Real unsupported collaterals observed in the split-events table. After
// --restart the guard must reject every one. USDT is flagged: it is a valid
// 6-dec stablecoin, so it is scaled correctly but still dropped — a deliberate
// (small) completeness tradeoff of the type-based whitelist, not a misscale.
var onChainUnsupportedCollateralAddrs = []common.Address{
	common.HexToAddress("0x0d500b1d8e8ef31e21c99d1db9a6444d3adf1270"), // WMATIC 18-dec — the $2T/$10B splits
	common.HexToAddress("0x67b2d4aab17ccd057432337adb1447fe72e2004e"),
	common.HexToAddress("0xd3afc3af2fe2c98c5f7cc704a6eb2abecfc0fc0b"),
	common.HexToAddress("0x6baa8bdf469e3ad2b847b8d88f0675f7479ac425"),
	common.HexToAddress("0xc2132d05d31c914a87c6611c10748aeb04b58e8f"), // USDT 6-dec — correctly scaled but still dropped
	common.HexToAddress("0x82f3cad175cffc55ef77bdfb9ffeccec438771c8"),
	common.HexToAddress("0xcbf1bebce22abd253b67858986735eaef5791be9"),
}

func TestSupportedCollaterals_AllAcceptAndScale(t *testing.T) {
	wantAmt := mustDec(t, "5")
	for _, coll := range supportedCollateralAddrs {
		coll := coll
		t.Run(coll.Hex(), func(t *testing.T) {
			if !isSupportedCollateral(coll) {
				t.Fatalf("%s must be supported (re-backfill would drop legit positions)", coll.Hex())
			}
			state := generated.NewState()
			state.SetSnapshotsEnabled(false)
			condID := binaryCondition(t, state, 1)
			user := common.HexToAddress("0x3000000000000000000000000000000000000001")

			handlePositionSplit(state, &generated.ConditionalTokensPositionSplit{
				EventMeta:       generated.EventMeta{BlockNumber: 2},
				Stakeholder:     user,
				CollateralToken: coll,
				ConditionID:     condID,
				Amount:          *uint256.NewInt(5_000_000), // 5 units, 6-dec
			})
			for outcome := uint8(0); outcome < 2; outcome++ {
				collID := getCollectionIDForOutcome(common.Hash{}, condID, outcome)
				posID := getPositionID(coll, collID)
				up, ok := getUserPositionValue(state, user, posID)
				if !ok {
					t.Fatalf("supported collateral %s dropped outcome %d", coll.Hex(), outcome)
				}
				decEq(t, up.Amount, wantAmt, "amount")
				decEq(t, up.AvgPrice, fiftyCentsD256, "avg price")
			}
		})
	}
}

func TestUnsupportedCollaterals_OnChainSet_AllRejected(t *testing.T) {
	for _, coll := range onChainUnsupportedCollateralAddrs {
		coll := coll
		t.Run(coll.Hex(), func(t *testing.T) {
			if isSupportedCollateral(coll) {
				t.Fatalf("%s is on-chain unsupported collateral but the guard accepts it", coll.Hex())
			}
			state := generated.NewState()
			state.SetSnapshotsEnabled(false)
			condID := binaryCondition(t, state, 1)
			user := common.HexToAddress("0x3000000000000000000000000000000000000002")

			handlePositionSplit(state, &generated.ConditionalTokensPositionSplit{
				EventMeta:       generated.EventMeta{BlockNumber: 2},
				Stakeholder:     user,
				CollateralToken: coll,
				ConditionID:     condID,
				Amount:          *uint256.NewInt(5_000_000_000_000_000), // 5e15, the WMATIC-class raw
			})
			for outcome := uint8(0); outcome < 2; outcome++ {
				collID := getCollectionIDForOutcome(common.Hash{}, condID, outcome)
				posID := getPositionID(coll, collID)
				if _, ok := getUserPositionValue(state, user, posID); ok {
					t.Fatalf("unsupported collateral %s wrote a position (outcome %d) — re-backfill would re-pollute", coll.Hex(), outcome)
				}
			}
		})
	}
}

// Coverage the BUGR.md audit flagged as missing: the FPMM-creation guard (present
// but untested) and the neg-risk handlers (split / merge / redemption — none has
// an isSupportedCollateral guard because the NegRiskAdapter events carry NO
// collateralToken field; their collateral is always the hard-wired 6-decimal
// WrappedCollateral, custom_processor.go:108). These tests lock that
// safe-by-construction scaling so a regression in usdcRawToDec18's ÷1e6 path on
// the neg-risk handlers is caught, and pin the FPMM creation guard both ways.
//
// Reuses helpers from collateral_guard_test.go / redemption_loss_booking_test.go
// (same package): decEq, mustDec, binaryCondition, wmaticAddr, usdcNativeAddr,
// getUserPositionValue, getNegRiskPositionIDByCondition, fiftyCentsD256.

// ---------------------------------------------------------------------------
// On-chain-grounded regression: the two REAL WMATIC splits that exist in
// polymarket.conditional_tokens_position_split_events. Verified by sampling the
// production DB (2026-06-30):
//
//   block 75853761  stakeholder 0x8f3ff3c5…  amount 1e18 (1.0 WMATIC)
//       -> ÷1e6 books 1e12 per leg == $1,000,000,000,000  ($1T; ~$2T both legs)
//   block 86845299  stakeholder 0x2f083494…  amount 5e15 (0.005 WMATIC)
//       -> ÷1e6 books 5e9  per leg == $5,000,000,000      (BUGR.md's ~$10B outlier)
//
// Both inflated positions are STILL present in memory_user_positions (amount
// 1e12 and 5e9 at avg_price 0.5) — the 1e12 pair is the single largest position
// in the entire 310M-row table. BUGR.md documented only the 5e9 case; the 1e18
// split is ~200x larger and was missed. The guard must reject the REAL WMATIC
// address (the old fixture was a malformed typo that normalized to a different,
// harmless address and so never actually exercised the real collateral).
// ---------------------------------------------------------------------------

func TestRealWMATIC_OnChainMisscaleGuarded(t *testing.T) {
	realWMATIC := common.HexToAddress("0x0d500b1d8e8ef31e21c99d1db9a6444d3adf1270")
	if isSupportedCollateral(realWMATIC) {
		t.Fatal("the real on-chain WMATIC address must be unsupported collateral")
	}

	// Lock the exact $-per-leg the buggy ÷1e6 rescale produces for the two real
	// raw amounts. These are the figures sitting in memory_user_positions today.
	oneTrillion, _ := protomath.FromInt64(1_000_000_000_000, protomath.Decimal256Scale18)
	fiveBillion, _ := protomath.FromInt64(5_000_000_000, protomath.Decimal256Scale18)

	raw1e18 := uint256.NewInt(1_000_000_000_000_000_000) // 1.0 WMATIC, block 75853761
	got1e18, ok1 := usdcRawToDec18(raw1e18)
	if !ok1 {
		t.Fatal("1e18 rescale unexpectedly overflowed")
	}
	decEq(t, got1e18, oneTrillion, "1e18 WMATIC misscale (block 75853761, $1T/leg)")

	raw5e15 := uint256.NewInt(5_000_000_000_000_000) // 0.005 WMATIC, block 86845299
	got5e15, ok2 := usdcRawToDec18(raw5e15)
	if !ok2 {
		t.Fatal("5e15 rescale unexpectedly overflowed")
	}
	decEq(t, got5e15, fiveBillion, "5e15 WMATIC misscale (block 86845299, $5B/leg)")

	// End-to-end: the guard must block the real worst-case (1e18) split so neither
	// leg's $1T position is written.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := binaryCondition(t, state, 1)
	user := common.HexToAddress("0x8f3ff3c5750c20479f68db28407912bd8df67afa")

	handlePositionSplit(state, &generated.ConditionalTokensPositionSplit{
		EventMeta:       generated.EventMeta{BlockNumber: 75853761},
		Stakeholder:     user,
		CollateralToken: realWMATIC,
		ConditionID:     condID,
		Amount:          *raw1e18,
	})
	for outcome := uint8(0); outcome < 2; outcome++ {
		collID := getCollectionIDForOutcome(common.Hash{}, condID, outcome)
		posID := getPositionID(realWMATIC, collID)
		if _, ok := getUserPositionValue(state, user, posID); ok {
			t.Fatalf("guard let the real 1e18 WMATIC split through (outcome %d): would book $1T/leg", outcome)
		}
	}
}

// ---------------------------------------------------------------------------
// FPMM creation guard (custom_processor.go:829) — both directions.
// ---------------------------------------------------------------------------

func TestFPMMCreation_RejectsUnsupportedCollateral(t *testing.T) {
	// A FixedProductMarketMaker created against non-USDC collateral must NOT be
	// registered. Because creation is the sole writer of FPMM state and Buy/Sell/
	// Funding all bail on a Get miss, rejecting it here keeps non-6-dec collateral
	// out of every downstream usdcRawToDec18 call site.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := binaryCondition(t, state, 1)
	fpmmAddr := common.HexToAddress("0xfff0000000000000000000000000000000000001")

	handleFixedProductMarketMakerCreation(state, &generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation{
		EventMeta:               generated.EventMeta{BlockNumber: 2},
		FixedProductMarketMaker: fpmmAddr,
		CollateralToken:         wmaticAddr,
		ConditionIds:            []common.Hash{condID},
	})

	if _, ok := state.FixedProductMarketMaker.Get(fpmmAddr); ok {
		t.Fatal("unsupported-collateral FPMM was registered; guard at custom_processor.go:829 did not fire")
	}
}

func TestFPMMCreation_AcceptsUSDC(t *testing.T) {
	// The USDC happy path: a supported-collateral FPMM is registered with its
	// collateral and condition recorded for the downstream Buy/Sell/Funding lookups.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := binaryCondition(t, state, 1)
	fpmmAddr := common.HexToAddress("0xfff0000000000000000000000000000000000002")

	handleFixedProductMarketMakerCreation(state, &generated.FixedProductMarketMakerFactoryFixedProductMarketMakerCreation{
		EventMeta:               generated.EventMeta{BlockNumber: 2},
		FixedProductMarketMaker: fpmmAddr,
		CollateralToken:         usdcNativeAddr,
		ConditionIds:            []common.Hash{condID},
	})

	fpmm, ok := state.FixedProductMarketMaker.Get(fpmmAddr)
	if !ok {
		t.Fatal("supported-collateral FPMM was not registered")
	}
	if fpmm.CollateralToken != usdcNativeAddr {
		t.Fatalf("FPMM collateral = %s, want %s", fpmm.CollateralToken.Hex(), usdcNativeAddr.Hex())
	}
	if fpmm.ConditionID != condID {
		t.Fatalf("FPMM condition = %s, want %s", fpmm.ConditionID.Hex(), condID.Hex())
	}
}

// ---------------------------------------------------------------------------
// Neg-risk handlers — no collateralToken field; collateral is always 6-dec WCOL.
// These lock that usdcRawToDec18's ÷1e6 rescale is applied (5e6 → 5, not 5e9).
// ---------------------------------------------------------------------------

// negRiskCondition is a distinct id from binaryCondition's, used by the neg-risk
// handlers which derive position ids via getNegRiskPositionIDByCondition.
func negRiskCondition() common.Hash {
	return common.HexToHash("0xbeef000000000000000000000000000000000000000000000000000000000002")
}

func TestNegRiskPositionSplit_AcceptsAndScales(t *testing.T) {
	// A neg-risk split of 5 WCOL (5e6 raw) mints both YES and NO at amount=5,
	// avg price 0.5 — proving the handler writes positions (no spurious guard) and
	// applies the 6-decimal rescale (5e6 → 5). The handler auto-creates the
	// 2-outcome condition.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := negRiskCondition()
	user := common.HexToAddress("0x2000000000000000000000000000000000000001")

	handleNegRiskPositionSplit(state, &generated.NegRiskAdapterPositionSplit{
		EventMeta:   generated.EventMeta{BlockNumber: 2},
		Stakeholder: user,
		ConditionID: condID,
		Amount:      *uint256.NewInt(5_000_000),
	})

	wantAmt := mustDec(t, "5")
	for outcome := uint8(0); outcome < 2; outcome++ {
		posID := getNegRiskPositionIDByCondition(condID, outcome)
		up, ok := getUserPositionValue(state, user, posID)
		if !ok {
			t.Fatalf("neg-risk split missing outcome %d position", outcome)
		}
		decEq(t, up.Amount, wantAmt, "outcome amount")
		decEq(t, up.AvgPrice, fiftyCentsD256, "outcome avg price")
		if !up.RealizedPnL.IsZero() {
			t.Fatalf("neg-risk split realized PnL = %s, want 0",
				up.RealizedPnL.String(protomath.Decimal256Scale18))
		}
	}
}

func TestNegRiskPositionsMerge_PartialScales(t *testing.T) {
	// Split 10 (1e7 raw) then merge 3 (3e6 raw): a PARTIAL merge so the residual
	// proves the scale. A mis-scaled merge (3e6 → 3e9) would clamp to the held 10
	// and zero the position; correct ÷1e6 scaling leaves exactly 7.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := negRiskCondition()
	user := common.HexToAddress("0x2000000000000000000000000000000000000001")

	handleNegRiskPositionSplit(state, &generated.NegRiskAdapterPositionSplit{
		EventMeta:   generated.EventMeta{BlockNumber: 2},
		Stakeholder: user,
		ConditionID: condID,
		Amount:      *uint256.NewInt(10_000_000),
	})
	handleNegRiskPositionsMerge(state, &generated.NegRiskAdapterPositionsMerge{
		EventMeta:   generated.EventMeta{BlockNumber: 3},
		Stakeholder: user,
		ConditionID: condID,
		Amount:      *uint256.NewInt(3_000_000),
	})

	wantAmt := mustDec(t, "7")
	for outcome := uint8(0); outcome < 2; outcome++ {
		posID := getNegRiskPositionIDByCondition(condID, outcome)
		up, ok := getUserPositionValue(state, user, posID)
		if !ok {
			t.Fatalf("neg-risk merge dropped outcome %d position", outcome)
		}
		decEq(t, up.Amount, wantAmt, "residual amount after partial merge")
		// merge sells at 0.5 == avg 0.5, so no PnL is realized.
		if !up.RealizedPnL.IsZero() {
			t.Fatalf("neg-risk merge realized PnL = %s, want 0",
				up.RealizedPnL.String(protomath.Decimal256Scale18))
		}
	}
}

func TestPayoutRedemptionNR_ScalesAndSettles(t *testing.T) {
	// The critical unguarded handler (custom_processor.go:1327): it has NO
	// isSupportedCollateral guard and rescales ev.Amounts[i] via usdcRawToDec18 at
	// :1345 — safe only because the NegRiskAdapter PayoutRedemption event carries no
	// collateralToken field (generated/events.go:946) and the positions are pinned
	// to the 6-dec WrappedCollateral. This locks that scaling: a 1e7 redemption
	// books realized ±5, NOT ±5e9.
	//
	// Setup: split 10 (YES=10@0.5, NO=10@0.5), resolve YES (payouts [1,0]), redeem
	// both legs. denom = 1+0 = 1, so YES marks at 1.0 and NO at 0.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := negRiskCondition()
	user := common.HexToAddress("0x2000000000000000000000000000000000000001")

	handleNegRiskPositionSplit(state, &generated.NegRiskAdapterPositionSplit{
		EventMeta:   generated.EventMeta{BlockNumber: 2},
		Stakeholder: user,
		ConditionID: condID,
		Amount:      *uint256.NewInt(10_000_000),
	})
	handleConditionResolution(state, &generated.ConditionalTokensConditionResolution{
		EventMeta:        generated.EventMeta{BlockNumber: 3},
		ConditionID:      condID,
		PayoutNumerators: []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(0)},
	})
	handlePayoutRedemptionNR(state, &generated.NegRiskAdapterPayoutRedemption{
		EventMeta:   generated.EventMeta{BlockNumber: 4},
		Redeemer:    user,
		ConditionID: condID,
		Amounts:     []uint256.Int{*uint256.NewInt(10_000_000), *uint256.NewInt(10_000_000)},
	})

	// YES: sell 10 @1.0, avg 0.5 → realized +5; NO: sell 10 @0, avg 0.5 → realized -5.
	yesID := getNegRiskPositionIDByCondition(condID, 0)
	noID := getNegRiskPositionIDByCondition(condID, 1)

	yes, ok := getUserPositionValue(state, user, yesID)
	if !ok {
		t.Fatal("YES position vanished")
	}
	if !yes.Amount.IsZero() {
		t.Fatalf("YES amount after redemption = %s, want 0",
			yes.Amount.String(protomath.Decimal256Scale18))
	}
	decEq(t, yes.RealizedPnL, mustDec(t, "5"), "YES realized PnL")

	no, ok := getUserPositionValue(state, user, noID)
	if !ok {
		t.Fatal("NO position vanished")
	}
	if !no.Amount.IsZero() {
		t.Fatalf("NO amount after redemption = %s, want 0",
			no.Amount.String(protomath.Decimal256Scale18))
	}
	decEq(t, no.RealizedPnL, mustDec(t, "-5"), "NO realized PnL")
}
