package polymarket

// Proof benchmarks for the hypothesis that per-fill position math pays a
// shopspring<->protomath round-trip tax: positions store protomath.Decimal256,
// but every update converts to shopspring decimal, does the arithmetic there
// (allocating big.Ints), and converts back.
//
//   BenchmarkFillMath_ShopRoundtrip   — the exact math handleOrderFilledValues +
//                                       updateUserPositionWithBuy/Sell do today
//   BenchmarkFillMath_ProtoNative     — the same math done natively on Decimal256
//   BenchmarkOrderFilled_RealHandler  — the real handler with real hot state,
//                                       for -memprofile/-cpuprofile attribution
//
// Both math variants compute identical position fields (asserted in
// TestFillMathVariantsAgree) so the comparison is apples-to-apples.

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// position mirrors the Decimal256 fields of generated.Position that the fill
// math touches, without the hot-state machinery.
type benchPosition struct {
	Amount      protomath.Decimal256
	AvgPrice    protomath.Decimal256
	RealizedPnL protomath.Decimal256
	TotalBought protomath.Decimal256
}

type benchFill struct {
	baseRaw  uint256.Int // outcome tokens, 1e6 scale
	quoteRaw uint256.Int // USDC, 1e6 scale
	isBuy    bool
}

// makeBenchFills generates a deterministic buy/sell stream. Buys outnumber
// sells and sells are smaller than the accumulated position so the sell branch
// (PnL math) actually executes.
func makeBenchFills(n int) []benchFill {
	fills := make([]benchFill, n)
	rng := uint64(0x9E3779B97F4A7C15)
	for i := range fills {
		rng ^= rng << 13
		rng ^= rng >> 7
		rng ^= rng << 17
		base := 1_000_000 + rng%50_000_000 // 1..51 stake units
		price := 200_000 + rng%600_000     // 0.2..0.8 USDC
		quote := base * price / 1_000_000
		isBuy := i%3 != 2 // 2 buys : 1 sell
		if !isBuy {
			base /= 4
			quote /= 4
		}
		fills[i] = benchFill{
			baseRaw:  *uint256.NewInt(base),
			quoteRaw: *uint256.NewInt(quote),
			isBuy:    isBuy,
		}
	}
	return fills
}

// --- Variant A: today's code path (shopspring round-trip) -------------------

var oneE6 = decimal.NewFromInt(1e6)

func shopRoundtripFill(pos *benchPosition, f *benchFill) {
	// handleOrderFilledValues: raw -> shopspring, with the per-event
	// decimal.NewFromInt(1e6) exactly as written today.
	base := Uint256ToDecimal(f.baseRaw).Div(decimal.NewFromInt(1e6))
	quote := Uint256ToDecimal(f.quoteRaw).Div(decimal.NewFromInt(1e6))
	price := decimal.Zero
	if !base.IsZero() {
		price = quote.Div(base)
	}

	if f.isBuy {
		// updateUserPositionWithBuy math
		pos.AvgPrice = fromDecimal(updateAvgPriceDecimal(toDecimal(pos.AvgPrice), toDecimal(pos.Amount), price, base))
		pos.Amount = fromDecimal(toDecimal(pos.Amount).Add(base))
		pos.TotalBought = fromDecimal(toDecimal(pos.TotalBought).Add(base))
		return
	}
	// updateUserPositionWithSell math
	adjAmt := base
	if adjAmt.GreaterThan(toDecimal(pos.Amount)) {
		adjAmt = toDecimal(pos.Amount)
	}
	if adjAmt.IsZero() {
		return
	}
	pnl := adjAmt.Mul(price.Sub(toDecimal(pos.AvgPrice)))
	pos.RealizedPnL = fromDecimal(toDecimal(pos.RealizedPnL).Add(pnl))
	pos.Amount = fromDecimal(toDecimal(pos.Amount).Sub(adjAmt))
}

// --- Variant B: native protomath ---------------------------------------------

var e12 = uint256.NewInt(1_000_000_000_000)

// rawToDec18 converts a 1e6-scaled raw amount to a scale-18 Decimal256:
// coefficient = raw * 1e12.
func rawToDec18(v *uint256.Int) protomath.Decimal256 {
	var scaled uint256.Int
	scaled.Mul(v, e12)
	d, _ := protomath.FromUInt256AsDecimal256(protomath.FromHoliman(scaled))
	return d
}

func protoNativeFill(pos *benchPosition, f *benchFill) {
	scale := protomath.Decimal256Scale18
	base := rawToDec18(&f.baseRaw)
	quote := rawToDec18(&f.quoteRaw)
	var price protomath.Decimal256
	if !base.IsZero() {
		price, _ = quote.Div(base, scale)
	}

	if f.isBuy {
		// avg' = (avg*amt + price*base) / (amt + base)
		denom, _ := pos.Amount.Add(base)
		if !denom.IsZero() {
			numerA, _ := pos.AvgPrice.Mul(pos.Amount, scale)
			numerB, _ := price.Mul(base, scale)
			numer, _ := numerA.Add(numerB)
			pos.AvgPrice, _ = numer.Div(denom, scale)
		}
		pos.Amount, _ = pos.Amount.Add(base)
		pos.TotalBought, _ = pos.TotalBought.Add(base)
		return
	}
	adjAmt := base
	if adjAmt.Gt(pos.Amount) {
		adjAmt = pos.Amount
	}
	if adjAmt.IsZero() {
		return
	}
	spread, _ := price.Sub(pos.AvgPrice)
	pnl, _ := adjAmt.Mul(spread, scale)
	pos.RealizedPnL, _ = pos.RealizedPnL.Add(pnl)
	pos.Amount, _ = pos.Amount.Sub(adjAmt)
}

// --- Benchmarks ---------------------------------------------------------------

const benchFillCount = 4096

func BenchmarkFillMath_ShopRoundtrip(b *testing.B) {
	fills := makeBenchFills(benchFillCount)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var pos benchPosition
		for i := range fills {
			shopRoundtripFill(&pos, &fills[i])
		}
	}
}

func BenchmarkFillMath_ProtoNative(b *testing.B) {
	fills := makeBenchFills(benchFillCount)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var pos benchPosition
		for i := range fills {
			protoNativeFill(&pos, &fills[i])
		}
	}
}

// Real handler with real hot state — run with -memprofile/-cpuprofile to see
// where the allocations and CPU actually go (shopspring, big.Int, user.Hex).
func BenchmarkOrderFilled_RealHandler(b *testing.B) {
	state := generated.NewState()
	fills := makeBenchFills(benchFillCount)
	users := make([]common.Address, 64)
	for i := range users {
		users[i][0] = byte(i + 1)
		users[i][19] = byte(i * 7)
	}
	tokens := make([]uint256.Int, 256)
	for i := range tokens {
		tokens[i] = *uint256.NewInt(uint64(i + 1))
	}
	meta := generated.EventMeta{BlockNumber: 1}

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range fills {
			f := &fills[i]
			user := users[i%len(users)]
			token := tokens[i%len(tokens)]
			var makerAsset, takerAsset, makerAmt, takerAmt uint256.Int
			if f.isBuy {
				takerAsset = token
				makerAmt = f.quoteRaw
				takerAmt = f.baseRaw
			} else {
				makerAsset = token
				makerAmt = f.baseRaw
				takerAmt = f.quoteRaw
			}
			handleOrderFilledValues(state, user, makerAsset, takerAsset, makerAmt, takerAmt, meta)
		}
	}
}

// TestFillMathVariantsAgree proves the two variants compute identical results,
// so the benchmark deltas measure representation cost, not different math.
func TestFillMathVariantsAgree(t *testing.T) {
	fills := makeBenchFills(benchFillCount)
	var shopPos, protoPos benchPosition
	for i := range fills {
		shopRoundtripFill(&shopPos, &fills[i])
		protoNativeFill(&protoPos, &fills[i])
	}
	check := func(name string, a, b protomath.Decimal256) {
		da, db := toDecimal(a), toDecimal(b)
		// shopspring Div keeps 16 digits past the natural precision; protomath
		// truncates at scale 18. Over 4096 fills the truncation accumulates to
		// ~1e-11 on the PnL sum — allow 1e-9, still 7 orders of magnitude below
		// the cent-level A79 correctness gate.
		if da.Sub(db).Abs().GreaterThan(decimal.New(1, -9)) {
			t.Errorf("%s mismatch: shop=%s proto=%s", name, da, db)
		}
	}
	check("Amount", shopPos.Amount, protoPos.Amount)
	check("AvgPrice", shopPos.AvgPrice, protoPos.AvgPrice)
	check("RealizedPnL", shopPos.RealizedPnL, protoPos.RealizedPnL)
	check("TotalBought", shopPos.TotalBought, protoPos.TotalBought)
}
