package polymarket

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// These tests lock in the allocation win from the native-Decimal256 migration:
// each handler must allocate strictly fewer objects than its shopspring
// counterpart, and the native path must be near-zero once the ID caches are
// warm (AllocsPerRun does an untracked warmup call first, priming them).

// allocsReduced runs native and shop with the same warm inputs and asserts the
// native path allocates at least minRatio× fewer objects. A relative bound is
// robust to clock-cache/Save bookkeeping allocs (which both paths share and
// which dwarf neither): the claim is "the migration removed the per-position
// big.Int churn", i.e. a large multiplicative cut, not an absolute count.
func allocsReduced(t *testing.T, name string, minRatio float64, native, shop func()) {
	t.Helper()
	nativeAllocs := testing.AllocsPerRun(100, native)
	shopAllocs := testing.AllocsPerRun(100, shop)
	ratio := shopAllocs / nativeAllocs
	t.Logf("%s allocs/call: native=%.1f shop=%.1f (%.1fx fewer)", name, nativeAllocs, shopAllocs, ratio)
	if nativeAllocs >= shopAllocs {
		t.Errorf("%s: native allocs %.1f not fewer than shop %.1f", name, nativeAllocs, shopAllocs)
	}
	if ratio < minRatio {
		t.Errorf("%s: native cut allocs only %.1fx (want >= %.1fx): native=%.1f shop=%.1f", name, ratio, minRatio, nativeAllocs, shopAllocs)
	}
}

func TestPositionSplitAllocsReduced(t *testing.T) {
	condID, collateral, stakeholder := hashByte(0x91), addrByte(0x92), addrByte(0x93)
	ns := generated.NewState()
	saveCondition(ns, condID, false, nil)
	ss := generated.NewState()
	saveCondition(ss, condID, false, nil)
	mkEv := func() *generated.ConditionalTokensPositionSplit {
		return &generated.ConditionalTokensPositionSplit{
			EventMeta: metaAt(10), Stakeholder: stakeholder, CollateralToken: collateral,
			ConditionID: condID, Amount: u256(1_000_000),
		}
	}
	evN, evS := mkEv(), mkEv()
	allocsReduced(t, "positionSplit", 5,
		func() { handlePositionSplit(ns, evN) },
		func() { handlePositionSplitShop(ss, evS) })
}

func TestPositionsMergeAllocsReduced(t *testing.T) {
	condID, collateral, stakeholder := hashByte(0x94), addrByte(0x95), addrByte(0x96)
	mk := func() *generated.State {
		st := generated.NewState()
		saveCondition(st, condID, false, nil)
		for i := uint8(0); i < 2; i++ {
			posID := getPositionID(collateral, getCollectionID(common.Hash{}, condID, indexSetBig[i]))
			updateUserPositionWithBuy(st, stakeholder, posID, decimal.NewFromFloat(0.4), decimal.NewFromInt(1e12), decimal.Zero, metaAt(5))
		}
		return st
	}
	ns, ss := mk(), mk()
	mkEv := func() *generated.ConditionalTokensPositionsMerge {
		return &generated.ConditionalTokensPositionsMerge{
			EventMeta: metaAt(10), Stakeholder: stakeholder, CollateralToken: collateral,
			ConditionID: condID, Amount: u256(1_000),
		}
	}
	evN, evS := mkEv(), mkEv()
	allocsReduced(t, "positionsMerge", 5,
		func() { handlePositionsMerge(ns, evN) },
		func() { handlePositionsMergeShop(ss, evS) })
}

func TestFPMMBuyAllocsReduced(t *testing.T) {
	condID, collateral, fpmmAddr, buyer := hashByte(0x97), addrByte(0x98), addrByte(0x99), addrByte(0x9A)
	ns := generated.NewState()
	saveFPMM(ns, fpmmAddr, condID, collateral)
	ss := generated.NewState()
	saveFPMM(ss, fpmmAddr, condID, collateral)
	fpmmS, _ := ss.FixedProductMarketMaker.Get(fpmmAddr)
	mkEv := func() *generated.FixedProductMarketMakerFPMMBuy {
		return &generated.FixedProductMarketMakerFPMMBuy{
			EventMeta: metaAt(10), Buyer: buyer, InvestmentAmount: u256(500_000),
			OutcomeIndex: u256(0), OutcomeTokensBought: u256(1_000_000),
		}
	}
	evN, evS := mkEv(), mkEv()
	allocsReduced(t, "fpmmBuy", 5,
		func() { handleFPMMBuy(ns, evN, fpmmAddr) },
		func() { refFPMMBuy(ss, evS, fpmmS) })
}

func TestPositionsConvertedAllocsReduced(t *testing.T) {
	marketID, stakeholder := hashByte(0x90), addrByte(0x8F)
	mk := func() *generated.State {
		st := generated.NewState()
		st.NegRiskEvent.Save(&generated.NegRiskEvent{ID: marketID, QuestionCount: 4}, metaAt(1))
		for q := uint32(0); q < 3; q++ {
			posID := getNegRiskPositionID(marketID, q, 1)
			updateUserPositionWithBuy(st, stakeholder, posID, decimal.NewFromFloat(0.4), decimal.NewFromInt(100), decimal.Zero, metaAt(5))
		}
		return st
	}
	ns, ss := mk(), mk()
	mkEv := func() *generated.NegRiskAdapterPositionsConverted {
		return &generated.NegRiskAdapterPositionsConverted{
			EventMeta: metaAt(10), MarketID: marketID, Stakeholder: stakeholder,
			IndexSet: u256(0b0111), Amount: u256(50_000_000),
		}
	}
	evN, evS := mkEv(), mkEv()
	// noSells/yesBuys slice growth is shared by both paths, so the ratio is
	// smaller than the single-position handlers — but the decimal math is gone.
	allocsReduced(t, "positionsConverted", 2,
		func() { handlePositionsConverted(ns, evN) },
		func() { handlePositionsConvertedShop(ss, evS) })
}

func TestFPMMFundingAddedAllocsReduced(t *testing.T) {
	condID, collateral, fpmmAddr, funder := hashByte(0x9B), addrByte(0x9C), addrByte(0x9D), addrByte(0x9E)
	ns := generated.NewState()
	saveFPMM(ns, fpmmAddr, condID, collateral)
	ss := generated.NewState()
	saveFPMM(ss, fpmmAddr, condID, collateral)
	fpmmS, _ := ss.FixedProductMarketMaker.Get(fpmmAddr)
	mkEv := func() *generated.FixedProductMarketMakerFPMMFundingAdded {
		return &generated.FixedProductMarketMakerFPMMFundingAdded{
			EventMeta: metaAt(10), Funder: funder,
			AmountsAdded: []uint256.Int{u256(1_000_000), u256(3_000_000)}, SharesMinted: u256(2_000_000),
		}
	}
	evN, evS := mkEv(), mkEv()
	allocsReduced(t, "fpmmFundingAdded", 5,
		func() { handleFPMMFundingAdded(ns, evN, fpmmAddr) },
		func() { handleFPMMFundingAddedShop(ss, evS, fpmmS) })
}
