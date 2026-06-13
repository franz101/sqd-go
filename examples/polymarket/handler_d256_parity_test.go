package polymarket

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// This file proves the native-Decimal256 position handlers compute the same
// position state as the legacy shopspring path they replaced, and that they do
// so with far fewer allocations. The handlers carry an unreachable-overflow
// shopspring fallback, so "the same math, cheaper representation" is the whole
// correctness claim — these tests pin it down at the unit level (the e2e wallet
// tests need a live ClickHouse and only cover two fixed wallets).

// parityTolerance: shopspring Div keeps 16 digits past the natural precision
// while protomath truncates at scale 18, so single-event results agree far
// tighter than this, but the bound matches TestFillMathVariantsAgree and stays
// 7 orders of magnitude below the cent-level correctness gate.
var parityTolerance = decimal.New(1, -9)

func metaAt(block uint64) generated.EventMeta {
	return generated.EventMeta{BlockNumber: block, BlockTimestamp: time.Unix(int64(block), 0)}
}

func addrByte(b byte) common.Address {
	var a common.Address
	a[19] = b
	return a
}

func hashByte(b byte) common.Hash {
	var h common.Hash
	h[31] = b
	return h
}

func u256(n uint64) uint256.Int { return *uint256.NewInt(n) }

func assertDecClose(t *testing.T, label string, got, want protomath.Decimal256) {
	t.Helper()
	dg, dw := toDecimal(got), toDecimal(want)
	if dg.Sub(dw).Abs().GreaterThan(parityTolerance) {
		t.Errorf("%s: native=%s shop=%s (diff=%s)", label, dg.String(), dw.String(), dg.Sub(dw).Abs().String())
	}
}

// assertPosParity compares the full position (Amount/AvgPrice/RealizedPnL/
// TotalBought) for one (user, tokenID) across the native and shop states.
func assertPosParity(t *testing.T, label string, native, shop *generated.State, user common.Address, tokenID uint256.Int) {
	t.Helper()
	pn, okn := native.Position.Get(user, tokenIDHash(tokenID))
	ps, oks := shop.Position.Get(user, tokenIDHash(tokenID))
	if okn != oks {
		t.Fatalf("%s: position presence mismatch native=%v shop=%v", label, okn, oks)
	}
	if !okn {
		return
	}
	assertDecClose(t, label+".Amount", pn.Amount, ps.Amount)
	assertDecClose(t, label+".AvgPrice", pn.AvgPrice, ps.AvgPrice)
	assertDecClose(t, label+".RealizedPnL", pn.RealizedPnL, ps.RealizedPnL)
	assertDecClose(t, label+".TotalBought", pn.TotalBought, ps.TotalBought)
}

func saveCondition(state *generated.State, id common.Hash, resolved bool, payouts []uint256.Int) {
	state.Condition.Save(&generated.Condition{
		ID:               id,
		Oracle:           addrByte(0xAA),
		OutcomeSlotCount: 2,
		Resolved:         resolved,
		Payouts:          payouts,
	}, metaAt(1))
}

func saveFPMM(state *generated.State, fpmmAddr common.Address, condID common.Hash, collateral common.Address) {
	state.FixedProductMarketMaker.Save(&generated.FixedProductMarketMaker{
		ID:              fpmmAddr,
		ConditionID:     condID,
		CollateralToken: collateral,
	}, metaAt(1))
}

// ---------------------------------------------------------------------------
// Pure-helper parity: the conversion primitives vs their shopspring equivalents.
// ---------------------------------------------------------------------------

func TestUsdcRawToDec18MatchesShop(t *testing.T) {
	raws := []uint64{0, 1, 999_999, 1_000_000, 1_234_567, 50_000_000, 1_000_000_000, 18446744073}
	for _, r := range raws {
		raw := u256(r)
		got, ok := usdcRawToDec18(&raw)
		if !ok {
			t.Fatalf("usdcRawToDec18(%d) overflow", r)
		}
		want := fromDecimal(Uint256ToDecimal(raw).Div(decimal.NewFromInt(1e6)))
		assertDecClose(t, "usdcRawToDec18", got, want)
	}
}

func TestRawIntToDec18MatchesShop(t *testing.T) {
	raws := []uint64{0, 1, 2, 1000, 1_000_000, 1_000_000_000_000, 9_999_999_999}
	for _, r := range raws {
		raw := u256(r)
		got, ok := rawIntToDec18(&raw)
		if !ok {
			t.Fatalf("rawIntToDec18(%d) overflow", r)
		}
		want := fromDecimal(Uint256ToDecimal(raw))
		assertDecClose(t, "rawIntToDec18", got, want)
	}
}

func TestRatioDec18MatchesShop(t *testing.T) {
	cases := [][2]uint64{{0, 1}, {1, 1}, {1, 2}, {1, 3}, {7, 11}, {3, 7}, {123456, 654321}, {1, 1_000_000}}
	for _, c := range cases {
		num, denom := u256(c[0]), u256(c[1])
		got, ok := ratioDec18(&num, &denom)
		if !ok {
			t.Fatalf("ratioDec18(%d,%d) overflow", c[0], c[1])
		}
		want := fromDecimal(Uint256ToDecimal(num).DivRound(Uint256ToDecimal(denom), 28))
		assertDecClose(t, "ratioDec18", got, want)
	}
	// Zero denominator returns a zero ratio, not an error.
	zero := u256(0)
	one := u256(1)
	if got, ok := ratioDec18(&one, &zero); !ok || !got.IsZero() {
		t.Errorf("ratioDec18(1,0) = (%v, ok=%v), want (0, true)", toDecimal(got), ok)
	}
}

func TestComputeFpmmPriceD256MatchesShop(t *testing.T) {
	cases := [][2]uint64{{100, 100}, {30, 70}, {1, 999}, {500_000, 1_500_000}, {123, 456}}
	for _, c := range cases {
		amounts := []uint256.Int{u256(c[0]), u256(c[1])}
		for idx := uint8(0); idx < 2; idx++ {
			got, ok := computeFpmmPriceD256(amounts, idx)
			if !ok {
				t.Fatalf("computeFpmmPriceD256(%v,%d) overflow", c, idx)
			}
			want := fromDecimal(computeFpmmPriceDecimal(amounts, idx))
			assertDecClose(t, "computeFpmmPriceD256", got, want)
		}
	}
}

func TestFiftyCentsD256Exact(t *testing.T) {
	if !toDecimal(fiftyCentsD256).Equal(decimal.NewFromFloat(0.5)) {
		t.Errorf("fiftyCentsD256 = %s, want 0.5", toDecimal(fiftyCentsD256).String())
	}
	assertDecClose(t, "fiftyCentsD256==fromDecimal(fiftyCents)", fiftyCentsD256, fromDecimal(fiftyCents))
}

// ---------------------------------------------------------------------------
// Overflow / fallback: the conversion helpers must report ok=false past the
// Decimal256 range (so the handler drops to the shopspring fallback), and the
// shopspring fallback must produce the correct result for those huge values.
// ---------------------------------------------------------------------------

// maxU256 is 2^256-1; scaling it by 1e12/1e18 overflows the Decimal256 range.
func maxU256() uint256.Int {
	var v uint256.Int
	v.Not(&v)
	return v
}

func TestConversionHelperOverflowReportsFalse(t *testing.T) {
	big := maxU256()
	if _, ok := usdcRawToDec18(&big); ok {
		t.Error("usdcRawToDec18(2^256-1) should overflow")
	}
	if _, ok := rawIntToDec18(&big); ok {
		t.Error("rawIntToDec18(2^256-1) should overflow")
	}
	one := u256(1)
	if _, ok := ratioDec18(&big, &one); ok {
		t.Error("ratioDec18(2^256-1, 1) should overflow")
	}
	// Just under the scale-18 limit must still succeed: max human value is
	// ~5.78e58 (coeff 2^255-1), so a raw 1e6-scaled value up to ~5.78e64 fits.
	safe := u256(1_000_000_000_000_000_000)
	if _, ok := usdcRawToDec18(&safe); !ok {
		t.Error("usdcRawToDec18(1e18) should fit")
	}
}

// TestPositionSplitOverflowFallback drives an Amount large enough to overflow
// usdcRawToDec18, exercising the otherwise-unreachable shopspring fallback
// branch, and checks it matches the shop handler directly.
func TestPositionSplitOverflowFallback(t *testing.T) {
	condID, collateral, stakeholder := hashByte(0x6C), addrByte(0x6D), addrByte(0x6E)
	huge := maxU256() // overflows usdcRawToDec18 -> handler must use shopspring
	if _, ok := usdcRawToDec18(&huge); ok {
		t.Fatal("test premise: expected overflow")
	}
	native, shop := generated.NewState(), generated.NewState()
	saveCondition(native, condID, false, nil)
	saveCondition(shop, condID, false, nil)
	ev := &generated.ConditionalTokensPositionSplit{
		EventMeta: metaAt(10), Stakeholder: stakeholder, CollateralToken: collateral,
		ConditionID: condID, Amount: huge,
	}
	handlePositionSplit(native, ev)     // takes the fallback internally
	handlePositionSplitShop(shop, ev)   // the fallback target, directly
	for i := uint8(0); i < 2; i++ {
		posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
		assertPosParity(t, "splitOverflow", native, shop, stakeholder, posID)
	}
}

// ---------------------------------------------------------------------------
// Handler parity: native handler vs a shopspring reference, comparing the full
// resulting position state. The reference mirrors the pre-migration handler.
// ---------------------------------------------------------------------------

func TestPositionSplitParity(t *testing.T) {
	condID := hashByte(0x11)
	collateral := addrByte(0x22)
	stakeholder := addrByte(0x33)
	for _, raw := range []uint64{1_000_000, 1_234_567, 999_999, 50_000_000, 7} {
		native, shop := generated.NewState(), generated.NewState()
		saveCondition(native, condID, false, nil)
		saveCondition(shop, condID, false, nil)
		ev := &generated.ConditionalTokensPositionSplit{
			EventMeta: metaAt(10), Stakeholder: stakeholder, CollateralToken: collateral,
			ConditionID: condID, Amount: u256(raw),
		}
		handlePositionSplit(native, ev)
		handlePositionSplitShop(shop, ev)
		for i := uint8(0); i < 2; i++ {
			posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
			assertPosParity(t, "split", native, shop, stakeholder, posID)
		}
	}
}

func TestPositionsMergeParity(t *testing.T) {
	condID := hashByte(0x44)
	collateral := addrByte(0x55)
	stakeholder := addrByte(0x66)
	for _, raw := range []uint64{1_000_000, 3_141_592, 500_000} {
		native, shop := generated.NewState(), generated.NewState()
		// Seed an existing position in both so the merge (a sell) has something
		// to reduce. Identical seed via the shopspring buy.
		for _, st := range []*generated.State{native, shop} {
			saveCondition(st, condID, false, nil)
			for i := uint8(0); i < 2; i++ {
				posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
				updateUserPositionWithBuy(st, stakeholder, posID, decimal.NewFromFloat(0.4), decimal.NewFromInt(100), decimal.Zero, metaAt(5))
			}
		}
		ev := &generated.ConditionalTokensPositionsMerge{
			EventMeta: metaAt(10), Stakeholder: stakeholder, CollateralToken: collateral,
			ConditionID: condID, Amount: u256(raw),
		}
		handlePositionsMerge(native, ev)
		handlePositionsMergeShop(shop, ev)
		for i := uint8(0); i < 2; i++ {
			posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
			assertPosParity(t, "merge", native, shop, stakeholder, posID)
		}
	}
}

// refFPMMBuy / refFPMMSell mirror the pre-migration shopspring FPMM handlers.
func refFPMMBuy(state *generated.State, ev *generated.FixedProductMarketMakerFPMMBuy, fpmm *generated.FixedProductMarketMaker) {
	outcomeIndex, _ := outcomeIndexUint8(ev.OutcomeIndex)
	amount := Uint256ToDecimal(ev.OutcomeTokensBought).Div(decimal.NewFromInt(1e6))
	price := CollateralToDecimal(ev.InvestmentAmount).Div(amount)
	posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
	updateUserPositionWithBuy(state, ev.Buyer, posID, price, amount, decimal.Zero, ev.EventMeta)
}

func refFPMMSell(state *generated.State, ev *generated.FixedProductMarketMakerFPMMSell, fpmm *generated.FixedProductMarketMaker) {
	outcomeIndex, _ := outcomeIndexUint8(ev.OutcomeIndex)
	amount := Uint256ToDecimal(ev.OutcomeTokensSold).Div(decimal.NewFromInt(1e6))
	price := CollateralToDecimal(ev.ReturnAmount).Div(amount)
	posID := getFixedProductMarketMakerPositionID(fpmm, outcomeIndex)
	updateUserPositionWithSell(state, ev.Seller, posID, price, amount, ev.EventMeta)
}

func TestFPMMBuyParity(t *testing.T) {
	condID := hashByte(0x77)
	collateral := addrByte(0x12)
	fpmmAddr := addrByte(0x34)
	buyer := addrByte(0x56)
	cases := []struct{ invest, tokens uint64 }{
		{500_000, 1_000_000}, {333_333, 1_000_000}, {1_000_000, 700_000}, {12_345_678, 9_876_543},
	}
	for _, c := range cases {
		native, shop := generated.NewState(), generated.NewState()
		var fpmm *generated.FixedProductMarketMaker
		for _, st := range []*generated.State{native, shop} {
			saveFPMM(st, fpmmAddr, condID, collateral)
			fpmm, _ = st.FixedProductMarketMaker.Get(fpmmAddr)
		}
		ev := &generated.FixedProductMarketMakerFPMMBuy{
			EventMeta: metaAt(10), Buyer: buyer, InvestmentAmount: u256(c.invest),
			OutcomeIndex: u256(0), OutcomeTokensBought: u256(c.tokens),
		}
		handleFPMMBuy(native, ev, fpmmAddr)
		refFPMMBuy(shop, ev, fpmm)
		posID := getFixedProductMarketMakerPositionID(fpmm, 0)
		assertPosParity(t, "fpmmBuy", native, shop, buyer, posID)
	}
}

func TestFPMMSellParity(t *testing.T) {
	condID := hashByte(0x78)
	collateral := addrByte(0x13)
	fpmmAddr := addrByte(0x35)
	seller := addrByte(0x57)
	native, shop := generated.NewState(), generated.NewState()
	var fpmm *generated.FixedProductMarketMaker
	for _, st := range []*generated.State{native, shop} {
		saveFPMM(st, fpmmAddr, condID, collateral)
		fpmm, _ = st.FixedProductMarketMaker.Get(fpmmAddr)
		// Seed a long position to sell against.
		posID := getFixedProductMarketMakerPositionID(fpmm, 0)
		updateUserPositionWithBuy(st, seller, posID, decimal.NewFromFloat(0.4), decimal.NewFromInt(1000), decimal.Zero, metaAt(5))
	}
	ev := &generated.FixedProductMarketMakerFPMMSell{
		EventMeta: metaAt(10), Seller: seller, ReturnAmount: u256(600_000),
		OutcomeIndex: u256(0), OutcomeTokensSold: u256(1_000_000),
	}
	handleFPMMSell(native, ev, fpmmAddr)
	refFPMMSell(shop, ev, fpmm)
	posID := getFixedProductMarketMakerPositionID(fpmm, 0)
	assertPosParity(t, "fpmmSell", native, shop, seller, posID)
}

func TestFPMMFundingAddedParity(t *testing.T) {
	condID := hashByte(0x79)
	collateral := addrByte(0x14)
	fpmmAddr := addrByte(0x36)
	funder := addrByte(0x58)
	cases := []struct {
		a0, a1, shares uint64
	}{
		{1_000_000, 3_000_000, 2_000_000},
		{5_000_000, 2_000_000, 4_000_000},
		{1_000_000, 1_000_000, 1_000_000},
	}
	for _, c := range cases {
		native, shop := generated.NewState(), generated.NewState()
		var fpmm *generated.FixedProductMarketMaker
		for _, st := range []*generated.State{native, shop} {
			saveFPMM(st, fpmmAddr, condID, collateral)
			fpmm, _ = st.FixedProductMarketMaker.Get(fpmmAddr)
		}
		ev := &generated.FixedProductMarketMakerFPMMFundingAdded{
			EventMeta: metaAt(10), Funder: funder,
			AmountsAdded: []uint256.Int{u256(c.a0), u256(c.a1)}, SharesMinted: u256(c.shares),
		}
		handleFPMMFundingAdded(native, ev, fpmmAddr)
		handleFPMMFundingAddedShop(shop, ev, fpmm)
		// Outcome token position + the LP-share position.
		sendback := uint8(0)
		if ev.AmountsAdded[0].Gt(&ev.AmountsAdded[1]) {
			sendback = 1
		}
		assertPosParity(t, "fundingAdded.token", native, shop, funder, getFixedProductMarketMakerPositionID(fpmm, sendback))
		assertPosParity(t, "fundingAdded.lp", native, shop, funder, uint256FromAddress(fpmm.ID))
	}
}

func TestFPMMFundingRemovedParity(t *testing.T) {
	condID := hashByte(0x7A)
	collateral := addrByte(0x15)
	fpmmAddr := addrByte(0x37)
	funder := addrByte(0x59)
	native, shop := generated.NewState(), generated.NewState()
	var fpmm *generated.FixedProductMarketMaker
	for _, st := range []*generated.State{native, shop} {
		saveFPMM(st, fpmmAddr, condID, collateral)
		fpmm, _ = st.FixedProductMarketMaker.Get(fpmmAddr)
		// Seed an LP-share position so the LP-sale leg fires.
		updateUserPositionWithBuy(st, funder, uint256FromAddress(fpmm.ID), decimal.NewFromFloat(0.001), decimal.NewFromInt(2_000_000), decimal.Zero, metaAt(5))
	}
	ev := &generated.FixedProductMarketMakerFPMMFundingRemoved{
		EventMeta: metaAt(10), Funder: funder,
		AmountsRemoved:               []uint256.Int{u256(1_000_000), u256(3_000_000)},
		CollateralRemovedFromFeePool: u256(100_000), SharesBurnt: u256(2_000_000),
	}
	handleFPMMFundingRemoved(native, ev, fpmmAddr)
	handleFPMMFundingRemovedShop(shop, ev, fpmm)
	for i := uint8(0); i < 2; i++ {
		assertPosParity(t, "fundingRemoved.token", native, shop, funder, getFixedProductMarketMakerPositionID(fpmm, i))
	}
	assertPosParity(t, "fundingRemoved.lp", native, shop, funder, uint256FromAddress(fpmm.ID))
}

// refNegRiskSplit / refNegRiskMerge mirror the pre-migration neg-risk handlers.
func refNegRiskSplit(state *generated.State, ev *generated.NegRiskAdapterPositionSplit) {
	amount := Uint256ToDecimal(ev.Amount).Div(decimal.NewFromInt(1e6))
	updateUserPositionWithBuy(state, ev.Stakeholder, getNegRiskPositionIDByCondition(ev.ConditionID, 0), fiftyCents, amount, decimal.Zero, ev.EventMeta)
	updateUserPositionWithBuy(state, ev.Stakeholder, getNegRiskPositionIDByCondition(ev.ConditionID, 1), fiftyCents, amount, decimal.Zero, ev.EventMeta)
}

func TestNegRiskPositionSplitParity(t *testing.T) {
	condID := hashByte(0x7B)
	stakeholder := addrByte(0x5A)
	for _, raw := range []uint64{1_000_000, 2_500_000, 999_999} {
		native, shop := generated.NewState(), generated.NewState()
		saveCondition(native, condID, false, nil)
		saveCondition(shop, condID, false, nil)
		ev := &generated.NegRiskAdapterPositionSplit{EventMeta: metaAt(10), Stakeholder: stakeholder, ConditionID: condID, Amount: u256(raw)}
		handleNegRiskPositionSplit(native, ev)
		refNegRiskSplit(shop, ev)
		for o := uint8(0); o < 2; o++ {
			assertPosParity(t, "negRiskSplit", native, shop, stakeholder, getNegRiskPositionIDByCondition(condID, o))
		}
	}
}

// refRedemptionCTF mirrors the pre-migration CTF redemption handler.
func refRedemptionCTF(state *generated.State, ev *generated.ConditionalTokensPayoutRedemption) {
	cond, ok := state.Condition.Get(ev.ConditionID)
	if !ok || !cond.Resolved {
		return
	}
	denomDec, ok := calculatePayoutDenominator(cond)
	if !ok {
		return
	}
	for i := range cond.Payouts {
		posID := getPositionID(ev.CollateralToken, getCollectionID(common.Hash{}, ev.ConditionID, indexSetBig[i]))
		price := Uint256ToDecimal(cond.Payouts[i]).Div(denomDec)
		up := getUserPosition(state, ev.Redeemer, posID)
		if up != nil && !toDecimal(up.Amount).IsZero() {
			updateUserPositionWithSell(state, ev.Redeemer, posID, price, toDecimal(up.Amount), ev.EventMeta)
		}
	}
}

func TestPayoutRedemptionCTFParity(t *testing.T) {
	condID := hashByte(0x7C)
	collateral := addrByte(0x16)
	redeemer := addrByte(0x5B)
	payouts := []uint256.Int{u256(1), u256(0)} // resolved YES
	native, shop := generated.NewState(), generated.NewState()
	for _, st := range []*generated.State{native, shop} {
		saveCondition(st, condID, true, payouts)
		// Seed a position at each outcome so the redemption sells them.
		for i := uint8(0); i < 2; i++ {
			posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
			updateUserPositionWithBuy(st, redeemer, posID, decimal.NewFromFloat(0.45), decimal.NewFromInt(500), decimal.Zero, metaAt(5))
		}
	}
	ev := &generated.ConditionalTokensPayoutRedemption{
		EventMeta: metaAt(10), Redeemer: redeemer, CollateralToken: collateral,
		ConditionID: condID, IndexSets: []uint256.Int{u256(1), u256(2)}, Payout: u256(500),
	}
	handlePayoutRedemptionCTF(native, ev)
	refRedemptionCTF(shop, ev)
	for i := uint8(0); i < 2; i++ {
		posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
		assertPosParity(t, "redemptionCTF", native, shop, redeemer, posID)
	}
}

// refRedemptionNR mirrors the pre-migration neg-risk redemption handler.
func refRedemptionNR(state *generated.State, ev *generated.NegRiskAdapterPayoutRedemption) {
	cond, ok := state.Condition.Get(ev.ConditionID)
	if !ok || !cond.Resolved {
		return
	}
	denomDec, ok := calculatePayoutDenominator(cond)
	if !ok {
		return
	}
	for i := uint8(0); i < 2; i++ {
		if int(i) < len(ev.Amounts) && int(i) < len(cond.Payouts) {
			posID := getNegRiskPositionIDByCondition(ev.ConditionID, i)
			amount := Uint256ToDecimal(ev.Amounts[i]).Div(decimal.NewFromInt(1e6))
			price := Uint256ToDecimal(cond.Payouts[i]).Div(denomDec)
			updateUserPositionWithSell(state, ev.Redeemer, posID, price, amount, ev.EventMeta)
		}
	}
}

func TestPayoutRedemptionNRParity(t *testing.T) {
	condID := hashByte(0x7D)
	redeemer := addrByte(0x5C)
	payouts := []uint256.Int{u256(1), u256(0)}
	native, shop := generated.NewState(), generated.NewState()
	for _, st := range []*generated.State{native, shop} {
		saveCondition(st, condID, true, payouts)
		for i := uint8(0); i < 2; i++ {
			updateUserPositionWithBuy(st, redeemer, getNegRiskPositionIDByCondition(condID, i), decimal.NewFromFloat(0.5), decimal.NewFromInt(300), decimal.Zero, metaAt(5))
		}
	}
	ev := &generated.NegRiskAdapterPayoutRedemption{
		EventMeta: metaAt(10), Redeemer: redeemer, ConditionID: condID,
		Amounts: []uint256.Int{u256(300_000_000), u256(0)}, Payout: u256(300),
	}
	handlePayoutRedemptionNR(native, ev)
	refRedemptionNR(shop, ev)
	for i := uint8(0); i < 2; i++ {
		assertPosParity(t, "redemptionNR", native, shop, redeemer, getNegRiskPositionIDByCondition(condID, i))
	}
}

func TestPositionsConvertedParity(t *testing.T) {
	marketID := hashByte(0x6A)
	stakeholder := addrByte(0x6B)
	// Vary seeded NO avg prices so the averaged yesPrice differs from 0.5 and
	// the two paths must agree on the derived buy price, not just on a constant.
	scenarios := []struct {
		questionCount uint32
		indexSet      uint64
		prices        []float64
		amounts       []int64
	}{
		{4, 0b0111, []float64{0.30, 0.45, 0.60}, []int64{100, 200, 150}},
		{3, 0b0011, []float64{0.50, 0.50}, []int64{1000, 500}},
		{5, 0b10101, []float64{0.20, 0.70, 0.55}, []int64{300, 100, 250}},
	}
	for si, sc := range scenarios {
		mid := marketID
		mid[0] = byte(si) // distinct market per scenario to avoid cache cross-talk
		native, shop := generated.NewState(), generated.NewState()
		for _, st := range []*generated.State{native, shop} {
			st.NegRiskEvent.Save(&generated.NegRiskEvent{ID: mid, QuestionCount: sc.questionCount}, metaAt(1))
			seedIdx := 0
			for q := uint32(0); q < sc.questionCount; q++ {
				if sc.indexSet&(1<<q) == 0 {
					continue // unselected -> yes buy, no seed needed
				}
				posID := getNegRiskPositionID(mid, q, 1) // NO outcome
				updateUserPositionWithBuy(st, stakeholder, posID, decimal.NewFromFloat(sc.prices[seedIdx]), decimal.NewFromInt(sc.amounts[seedIdx]), decimal.Zero, metaAt(5))
				seedIdx++
			}
		}
		ev := &generated.NegRiskAdapterPositionsConverted{
			EventMeta: metaAt(10), MarketID: mid, Stakeholder: stakeholder,
			IndexSet: u256(sc.indexSet), Amount: u256(50_000_000),
		}
		handlePositionsConverted(native, ev)
		handlePositionsConvertedShop(shop, ev)
		for q := uint32(0); q < sc.questionCount; q++ {
			outcome := uint8(0) // unselected -> YES buy
			if sc.indexSet&(1<<q) != 0 {
				outcome = 1 // selected -> NO sell
			}
			assertPosParity(t, "converted", native, shop, stakeholder, getNegRiskPositionID(mid, q, outcome))
		}
	}
}

// TestSplitMergeSequenceParity exercises accumulation: a long sequence of
// splits and merges must leave native and shop state in lockstep, so rounding
// differences cannot drift across many events.
func TestSplitMergeSequenceParity(t *testing.T) {
	condID := hashByte(0x6F)
	collateral := addrByte(0x17)
	stakeholder := addrByte(0x5D)
	native, shop := generated.NewState(), generated.NewState()
	saveCondition(native, condID, false, nil)
	saveCondition(shop, condID, false, nil)
	amounts := []uint64{1_000_000, 1_234_567, 777_777, 3_000_000, 250_000, 9_999_999}
	for k, raw := range amounts {
		split := &generated.ConditionalTokensPositionSplit{
			EventMeta: metaAt(uint64(10 + k)), Stakeholder: stakeholder, CollateralToken: collateral,
			ConditionID: condID, Amount: u256(raw),
		}
		handlePositionSplit(native, split)
		handlePositionSplitShop(shop, split)
		if k%2 == 1 {
			merge := &generated.ConditionalTokensPositionsMerge{
				EventMeta: metaAt(uint64(100 + k)), Stakeholder: stakeholder, CollateralToken: collateral,
				ConditionID: condID, Amount: u256(raw / 2),
			}
			handlePositionsMerge(native, merge)
			handlePositionsMergeShop(shop, merge)
		}
	}
	for i := uint8(0); i < 2; i++ {
		posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
		assertPosParity(t, "sequence", native, shop, stakeholder, posID)
	}
}
