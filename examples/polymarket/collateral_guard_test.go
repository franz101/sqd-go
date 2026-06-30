package polymarket

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// These tests lock the supported-collateral guard and the 6-decimal USDC
// rescale that the $10B outlier (see BUGR.md) violated.
//
// handlePositionSplit / handlePositionsMerge / handlePayoutRedemptionCTF are the
// functions the proto dispatch (ProcessProto, custom_processor.go:592) and the
// Map dispatch (:480) both call, so handler-level coverage here exercises the
// production proto path's arithmetic and guard. A packed ProtoEventBlock is only
// ever built from JSONL in this repo, so we drive the handlers directly rather
// than synthesize one.

var (
	wmaticAddr     = common.HexToAddress("0x0d500b1d8e8ef31e21c99d1db9a6444d3adf1270") // 18-dec WMATIC (real on-chain Polygon addr)
	usdcNativeAddr = common.HexToAddress("0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359") // 6-dec USDC
)

func decEq(t *testing.T, got, want protomath.Decimal256, name string) {
	t.Helper()
	if !got.Eq(want) {
		t.Fatalf("%s = %s, want %s", name,
			got.String(protomath.Decimal256Scale18),
			want.String(protomath.Decimal256Scale18))
	}
}

// ---------------------------------------------------------------------------
// Pure-math rescale correctness (zero-alloc hot paths).
// ---------------------------------------------------------------------------

func TestUSDCRawToDec18(t *testing.T) {
	// usdcRawToDec18 = raw/1e6 read at scale-18: the rescale is correct ONLY for
	// 6-decimal USDC. The 5e15 row is the exact $10B-outlier figure from BUGR.md —
	// had the guard not fired, a 5e15-raw WMATIC split would book $5B per leg.
	fiveBillion, _ := protomath.FromInt64(5_000_000_000, protomath.Decimal256Scale18)
	cases := []struct {
		name string
		raw  uint256.Int
		want protomath.Decimal256
		ok   bool
	}{
		{"zero", *uint256.NewInt(0), protomath.Decimal256{}, true},
		{"one_micro_usdc", *uint256.NewInt(1), mustDec(t, "0.000001"), true},
		{"five_usdc", *uint256.NewInt(5_000_000), mustDec(t, "5"), true},
		{"wmatic_5e15_records_5e9", *uint256.NewInt(5_000_000_000_000_000), fiveBillion, true},
		{"overflow", *maxU256(), protomath.Decimal256{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := usdcRawToDec18(&c.raw)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if c.ok {
				decEq(t, got, c.want, "value")
			}
		})
	}
}

func TestRawIntToDec18(t *testing.T) {
	// rawIntToDec18 is an identity at scale-18 (raw integer count × 1e18 read at
	// scale-18): used for LP shares that are not 1e6-rescaled.
	seven, _ := protomath.FromInt64(7, protomath.Decimal256Scale18)
	cases := []struct {
		name string
		raw  uint256.Int
		want protomath.Decimal256
		ok   bool
	}{
		{"zero", *uint256.NewInt(0), protomath.Decimal256{}, true},
		{"identity", *uint256.NewInt(7), seven, true},
		{"overflow", *maxU256(), protomath.Decimal256{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := rawIntToDec18(&c.raw)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if c.ok {
				decEq(t, got, c.want, "value")
			}
		})
	}
}

func TestRatioDec18(t *testing.T) {
	half := mustDec(t, "0.5")
	oneHalf := mustDec(t, "1.5")
	third := mustDec(t, "0.333333333333333333") // 18-digit floor of 1/3
	cases := []struct {
		name              string
		num, denom        uint256.Int
		want              protomath.Decimal256
		ok                bool
		skipExactFraction bool
	}{
		{"half", *uint256.NewInt(1), *uint256.NewInt(2), half, true, false},
		{"reduces", *uint256.NewInt(2), *uint256.NewInt(4), half, true, false},
		{"three_halves", *uint256.NewInt(3), *uint256.NewInt(2), oneHalf, true, false},
		{"zero_denom", *uint256.NewInt(1), *uint256.NewInt(0), protomath.Decimal256{}, false, false},
		{"zero_num", *uint256.NewInt(0), *uint256.NewInt(1), protomath.Decimal256{}, true, false},
		{"third_floor18", *uint256.NewInt(1), *uint256.NewInt(3), third, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ratioDec18(&c.num, &c.denom)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if c.ok {
				decEq(t, got, c.want, "ratio")
			}
		})
	}
}

func TestCollateralToDecimal(t *testing.T) {
	// Legacy shopspring /1e6 path (unreachable fallback). Correct for USDC; the
	// 5e15 row is the would-be $5B mis-scale for 18-dec collateral.
	cases := []struct {
		name string
		raw  uint256.Int
		want string
	}{
		{"five_usdc", *uint256.NewInt(5_000_000), "5"},
		{"one_micro", *uint256.NewInt(1), "0.000001"},
		{"wmatic_5e15_mis_scales_to_5e9", *uint256.NewInt(5_000_000_000_000_000), "5000000000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CollateralToDecimal(c.raw)
			want := decimal.RequireFromString(c.want)
			if !got.Equal(want) {
				t.Fatalf("CollateralToDecimal = %s, want %s", got.String(), want.String())
			}
		})
	}
}

func maxU256() *uint256.Int {
	m := uint256.NewInt(0)
	m.Sub(m, uint256.NewInt(1)) // 2^256 - 1
	return m
}

// The raw rescale paths run once per order fill / split / merge / redemption —
// they MUST stay zero-allocation. These guards pin 0 B/op so a future change
// cannot silently regress the hot path (measured: 0 allocs/op).
func TestRescalePaths_ZeroAlloc(t *testing.T) {
	check := func(name string, f func()) {
		t.Helper()
		if n := testing.AllocsPerRun(8, f); n != 0 {
			t.Fatalf("%s allocates %v/op, want 0", name, n)
		}
	}
	raw := uint256.NewInt(5_000_000_000_000_000)
	check("usdcRawToDec18", func() { _, _ = usdcRawToDec18(raw) })
	check("rawIntToDec18", func() { _, _ = rawIntToDec18(raw) })
	n, d := uint256.NewInt(1), uint256.NewInt(3)
	check("ratioDec18", func() { _, _ = ratioDec18(n, d) })
}

// ---------------------------------------------------------------------------
// Guard predicate.
// ---------------------------------------------------------------------------

func TestIsSupportedCollateral(t *testing.T) {
	supported := []common.Address{
		common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"), // bridged USDC
		usdcNativeAddr,                                                  // native USDC
		common.HexToAddress("0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB"), // pUSD
		negRiskWrappedCollateral,                                        // 0x3A3B…
	}
	for _, a := range supported {
		if !isSupportedCollateral(a) {
			t.Fatalf("expected supported: %s", a.Hex())
		}
	}
	unsupported := []common.Address{
		wmaticAddr,                                                  // 18-dec — the outlier collateral
		common.HexToAddress("0x0000000000000000000000000000000000000001"), // arbitrary
		common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"), // WETH (18-dec)
	}
	for _, a := range unsupported {
		if isSupportedCollateral(a) {
			t.Fatalf("expected unsupported: %s", a.Hex())
		}
	}
}

// ---------------------------------------------------------------------------
// Handler regression: the $10B outlier scenario and the USDC happy path.
// ---------------------------------------------------------------------------

// binaryCondition seeds a 2-outcome condition and returns its ID.
func binaryCondition(t *testing.T, state *generated.State, block uint64) common.Hash {
	t.Helper()
	condID := common.HexToHash("0xabcd000000000000000000000000000000000000000000000000000000000001")
	meta := generated.EventMeta{BlockNumber: block}
	state.Condition.Save(&generated.Condition{
		ID:               condID,
		OutcomeSlotCount: 2,
	}, meta)
	return condID
}

func TestPositionSplit_RejectsUnsupportedCollateral(t *testing.T) {
	// The $10B outlier: 0.005 WMATIC (5e15 raw, 18 decimals) split. Before the
	// guard this booked $5B per leg (≈$10B across YES+NO). The supported-
	// collateral guard must short-circuit before usdcRawToDec18 runs.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := binaryCondition(t, state, 1)
	user := common.HexToAddress("0x1000000000000000000000000000000000000001")

	handlePositionSplit(state, &generated.ConditionalTokensPositionSplit{
		EventMeta:      generated.EventMeta{BlockNumber: 2},
		Stakeholder:    user,
		CollateralToken: wmaticAddr,
		ConditionID:    condID,
		Amount:         *uint256.NewInt(5_000_000_000_000_000),
	})

	for outcome := uint8(0); outcome < 2; outcome++ {
		collID := getCollectionIDForOutcome(common.Hash{}, condID, outcome)
		posID := getPositionID(wmaticAddr, collID)
		if up, ok := getUserPositionValue(state, user, posID); ok {
			t.Fatalf("unsupported-collateral split wrote a position (outcome %d): amount=%s",
				outcome, up.Amount.String(protomath.Decimal256Scale18))
		}
	}
}

func TestPositionSplit_AcceptsUSDC(t *testing.T) {
	// Happy path: a 5 USDC split (5e6 raw) over a binary condition must mint BOTH
	// outcome legs at amount=5 and avg price 0.5 — full numerical correctness on
	// the collateral the 6-dec rescale is valid for.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := binaryCondition(t, state, 1)
	user := common.HexToAddress("0x1000000000000000000000000000000000000001")

	handlePositionSplit(state, &generated.ConditionalTokensPositionSplit{
		EventMeta:      generated.EventMeta{BlockNumber: 2},
		Stakeholder:    user,
		CollateralToken: usdcNativeAddr,
		ConditionID:    condID,
		Amount:         *uint256.NewInt(5_000_000),
	})

	wantAmt := mustDec(t, "5")
	wantPrice := fiftyCentsD256 // 0.5
	for outcome := uint8(0); outcome < 2; outcome++ {
		collID := getCollectionIDForOutcome(common.Hash{}, condID, outcome)
		posID := getPositionID(usdcNativeAddr, collID)
		up, ok := getUserPositionValue(state, user, posID)
		if !ok {
			t.Fatalf("USDC split missing outcome %d position", outcome)
		}
		decEq(t, up.Amount, wantAmt, "outcome amount")
		decEq(t, up.AvgPrice, wantPrice, "outcome avg price")
		if !up.RealizedPnL.IsZero() {
			t.Fatalf("split realized PnL = %s, want 0",
				up.RealizedPnL.String(protomath.Decimal256Scale18))
		}
	}
}

func TestPositionsMerge_RejectsUnsupportedCollateral(t *testing.T) {
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := binaryCondition(t, state, 1)
	user := common.HexToAddress("0x1000000000000000000000000000000000000001")

	handlePositionsMerge(state, &generated.ConditionalTokensPositionsMerge{
		EventMeta:      generated.EventMeta{BlockNumber: 2},
		Stakeholder:    user,
		CollateralToken: wmaticAddr,
		ConditionID:    condID,
		Amount:         *uint256.NewInt(5_000_000_000_000_000),
	})

	for outcome := uint8(0); outcome < 2; outcome++ {
		collID := getCollectionIDForOutcome(common.Hash{}, condID, outcome)
		posID := getPositionID(wmaticAddr, collID)
		if _, ok := getUserPositionValue(state, user, posID); ok {
			t.Fatalf("unsupported-collateral merge wrote a position (outcome %d)", outcome)
		}
	}
}

func TestPayoutRedemptionCTF_RejectsUnsupportedCollateral(t *testing.T) {
	// Seed a WMATIC-collateral position directly (bypassing the ingestion guard),
	// resolve the condition, then redeem. The guard must block the redemption so
	// the seeded position is left untouched.
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	condID := binaryCondition(t, state, 1)
	user := common.HexToAddress("0x1000000000000000000000000000000000000001")

	collID := getCollectionIDForOutcome(common.Hash{}, condID, 0)
	posID := getPositionID(wmaticAddr, collID)
	amt100, _ := protomath.FromInt64(100, protomath.Decimal256Scale18)
	updateUserPositionWithBuyD256(state, user, posID, fiftyCentsD256, amt100,
		protomath.Decimal256{}, generated.EventMeta{BlockNumber: 2})

	handleConditionResolution(state, &generated.ConditionalTokensConditionResolution{
		EventMeta:        generated.EventMeta{BlockNumber: 3},
		ConditionID:      condID,
		PayoutNumerators: []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(0)},
	})

	handlePayoutRedemptionCTF(state, &generated.ConditionalTokensPayoutRedemption{
		EventMeta:      generated.EventMeta{BlockNumber: 4},
		Redeemer:       user,
		CollateralToken: wmaticAddr,
		ConditionID:    condID,
	})

	up, ok := getUserPositionValue(state, user, posID)
	if !ok {
		t.Fatal("seeded WMATIC position vanished")
	}
	decEq(t, up.Amount, amt100, "redeemed amount (must be unchanged)")
	if !up.RealizedPnL.IsZero() {
		t.Fatalf("unsupported-collateral redemption booked PnL = %s, want 0",
			up.RealizedPnL.String(protomath.Decimal256Scale18))
	}
}
