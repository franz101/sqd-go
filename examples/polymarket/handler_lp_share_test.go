package polymarket

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// These tests lock in the FPMM LP-share accounting that a DB-comparison agent
// mistook for a "CTF token scaling bug" on wallet 0x0b9a5211. Two facts were
// inverted in that report and are pinned here:
//
//  1. A token_id with a long 0xffffffff… prefix is an *LP share*, produced by
//     uint256FromAddress (graph-ts BigInt.fromByteArray, little-endian signed),
//     NOT a CTF conditional token. It only looks like 0xffff… when the FPMM
//     address's last byte has its high bit set, which triggers sign extension.
//  2. The stored LP position (amount/avg_price/realized_pnl/total_bought)
//     reconciles exactly with the raw on-chain funding events under the
//     chain-accurate human-USDC convention. A separate reference indexer that
//     keeps LP price/PnL in raw collateral units (price ×1e12, pnl ×1e6) is the
//     outlier, not ground truth.

// TestFPMMLPShareSignExtendedTokenID pins the sign-extension that makes an
// LP-share id look like 0xffffffff… — the exact appearance that was misread as
// a CTF conditional token.
func TestFPMMLPShareSignExtendedTokenID(t *testing.T) {
	// Real FPMM from 0x0b9a5211's funding history; last byte 0x8a has the high
	// bit set, so the LP token id sign-extends.
	fpmmAddr := common.HexToAddress("0x170c18ea4fd9b142a392e7fea1f3dd1da224fe8a")
	h := tokenIDHash(uint256FromAddress(fpmmAddr))
	gotHex := common.Bytes2Hex(h[:])

	const wantPrefix = "ffffffffffffffffffffffff" // high 12 bytes all 0xff
	const wantLow = "8afe24a21dddf3a1fee792a342b1d94fea180c17"
	if gotHex != wantPrefix+wantLow {
		t.Fatalf("LP token_id: got %s, want %s%s", gotHex, wantPrefix, wantLow)
	}

	// An address whose last byte has the high bit clear must NOT sign-extend.
	plain := tokenIDHash(uint256FromAddress(common.HexToAddress("0x000000000000000000000000000000000000007f")))
	if ph := common.Bytes2Hex(plain[:]); ph[:2] == "ff" {
		t.Fatalf("low-bit address must not sign-extend, got %s", ph)
	}
}

// TestFPMMLPShareSequenceMatchesChain replays the exact funding sequence for
// FPMM …180c17 (funder 0x0b9a5211, blocks 44404123–44404433) and asserts the
// resulting LP position. Each add/remove is balanced (amounts=[a,a],
// shares=a), so every lpSharePrice is (a/1e6)/a = 1e-6 and every sale is at
// −1e-6. The final values match the production MASTERDEV DB and the hand-traced
// chain math; both the native-Decimal256 and shopspring paths must agree.
func TestFPMMLPShareSequenceMatchesChain(t *testing.T) {
	fpmmAddr := common.HexToAddress("0x170c18ea4fd9b142a392e7fea1f3dd1da224fe8a")
	condID, collateral, funder := hashByte(0x01), addrByte(0x02), addrByte(0x0b)

	added := func(blk, amt, shares uint64) *generated.FixedProductMarketMakerFPMMFundingAdded {
		return &generated.FixedProductMarketMakerFPMMFundingAdded{
			EventMeta: metaAt(blk), Funder: funder,
			AmountsAdded: []uint256.Int{u256(amt), u256(amt)}, SharesMinted: u256(shares),
		}
	}
	removed := func(blk, amt, shares uint64) *generated.FixedProductMarketMakerFPMMFundingRemoved {
		return &generated.FixedProductMarketMakerFPMMFundingRemoved{
			EventMeta: metaAt(blk), Funder: funder,
			AmountsRemoved: []uint256.Int{u256(amt), u256(amt)},
			CollateralRemovedFromFeePool: u256(0), SharesBurnt: u256(shares),
		}
	}

	// Replay the same 5 events through either the native or the shop handlers.
	run := func(native bool) *generated.Position {
		st := generated.NewState()
		saveFPMM(st, fpmmAddr, condID, collateral)
		fpmm, _ := st.FixedProductMarketMaker.Get(fpmmAddr)
		add := func(ev *generated.FixedProductMarketMakerFPMMFundingAdded) {
			if native {
				handleFPMMFundingAdded(st, ev, fpmmAddr)
			} else {
				handleFPMMFundingAddedShop(st, ev, fpmm)
			}
		}
		rem := func(ev *generated.FixedProductMarketMakerFPMMFundingRemoved) {
			if native {
				handleFPMMFundingRemoved(st, ev, fpmmAddr)
			} else {
				handleFPMMFundingRemovedShop(st, ev, fpmm)
			}
		}
		add(added(44404123, 100_000_000, 100_000_000))
		add(added(44404181, 400_000_000, 400_000_000))
		rem(removed(44404289, 499_995_000, 499_995_000))
		add(added(44404364, 500_000_000, 500_000_000))
		rem(removed(44404433, 500_000_000, 500_000_000))

		pos, ok := st.Position.Get(funder, tokenIDHash(uint256FromAddress(fpmmAddr)))
		if !ok {
			t.Fatalf("LP position missing (native=%v)", native)
		}
		return pos
	}

	// Chain-derived ground truth (matches MASTERDEV polymarket DB exactly).
	want := []struct {
		field string
		got   func(*generated.Position) decimal.Decimal
		val   decimal.Decimal
	}{
		{"Amount", func(p *generated.Position) decimal.Decimal { return toDecimal(p.Amount) }, decimal.NewFromInt(5_000)},
		{"TotalBought", func(p *generated.Position) decimal.Decimal { return toDecimal(p.TotalBought) }, decimal.NewFromInt(1_000_000_000)},
		{"AvgPrice", func(p *generated.Position) decimal.Decimal { return toDecimal(p.AvgPrice) }, decimal.RequireFromString("0.000001")},
		{"RealizedPnL", func(p *generated.Position) decimal.Decimal { return toDecimal(p.RealizedPnL) }, decimal.RequireFromString("-1999.99")},
	}

	for _, native := range []bool{true, false} {
		label := "shop"
		if native {
			label = "native"
		}
		pos := run(native)
		for _, w := range want {
			if got := w.got(pos); got.Sub(w.val).Abs().GreaterThan(parityTolerance) {
				t.Errorf("%s.%s = %s, want %s", label, w.field, got, w.val)
			}
		}
	}
}
