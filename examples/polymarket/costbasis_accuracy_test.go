package polymarket

import (
	"math/big"
	"math/rand"
	"testing"

	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
)

// TestCostBasisAccuracy compares three implementations of position PnL math
// over the same randomized fill sequence against an exact big.Rat reference:
//
//  A) avg-price recurrence with truncating Decimal256 Mul/Div (current
//     updateUserPositionWithBuyD256/SellD256 math)
//  B) the same recurrence with round-half-up division (protomath candidate)
//  C) cost-basis integers: position = (amountRaw, costRaw uint256); buys are
//     pure adds of the fill's exact USDC quote, sells remove
//     cost*adj/amount with one rounded division; avg price never feeds back.
//
// The point: A's truncation error compounds through the avg-price quotient,
// B only unbiases it, while C is exact for buys and bounded by half an ulp
// of raw USDC per sell — and exactly zero once a position fully closes.
func TestCostBasisAccuracy(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	scale := protomath.Decimal256Scale18

	// --- exact reference state (big.Rat) ---
	refAmt := new(big.Rat)
	refAvg := new(big.Rat)
	refPnL := new(big.Rat)

	// --- A: truncating Decimal256 recurrence ---
	var aAmt, aAvg, aPnL protomath.Decimal256

	// --- B: rounded-division recurrence ---
	var bAmt, bAvg, bPnL protomath.Decimal256

	// --- C: cost-basis integers at scale 18 (raw*1e12), exact buys ---
	var cAmt uint256.Int // amount raw (1e6)
	cCost := new(big.Int) // cost at 1e18 (raw USDC * 1e12)
	cPnL := new(big.Int)  // pnl at 1e18 (can go negative)

	dec18 := func(raw uint64) protomath.Decimal256 {
		var v uint256.Int
		v.SetUint64(raw)
		v.Mul(&v, uint256.NewInt(1_000_000_000_000)) // 1e6-raw -> 1e18
		d, ok := protomath.FromUInt256AsDecimal256(protomath.FromHoliman(v))
		if !ok {
			t.Fatalf("dec18 overflow for %d", raw)
		}
		return d
	}

	// roundDiv: x*scale/y with round-half-up on the magnitude.
	roundDiv := func(x, y protomath.Decimal256) (protomath.Decimal256, bool) {
		q, ok := x.Div(y, scale)
		if !ok {
			return q, false
		}
		// Reconstruct remainder via big.Int (test-only; production would use
		// uint256 MulMod). rem = x*10^18 mod y on the scaled integers.
		xb, yb := x.ScaledBig(), y.ScaledBig()
		num := new(big.Int).Mul(xb, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
		rem := new(big.Int).Mod(new(big.Int).Abs(num), new(big.Int).Abs(yb))
		twice := new(big.Int).Lsh(rem, 1)
		if twice.Cmp(new(big.Int).Abs(yb)) >= 0 {
			qb := q.ScaledBig()
			if qb.Sign() >= 0 {
				qb.Add(qb, big.NewInt(1))
			} else {
				qb.Sub(qb, big.NewInt(1))
			}
			q2, ok2 := protomath.FromDecimal256ScaledBigInt(qb)
			return q2, ok2
		}
		return q, true
	}

	const fills = 4096
	for i := 0; i < fills; i++ {
		// amounts/prices like real fills: 0.01..500 shares, price 0.001..0.999
		amtRaw := uint64(rng.Intn(500_000_000) + 10_000)  // raw 1e6 shares
		quoteRaw := uint64(rng.Intn(999) + 1)             // price in 1e3 steps
		usdcRaw := amtRaw / 1000 * quoteRaw               // exact raw USDC paid
		if usdcRaw == 0 {
			usdcRaw = 1
		}
		isBuy := rng.Intn(100) < 55 || aAmt.IsZero()

		amtD := dec18(amtRaw)
		refAmtFill := new(big.Rat).SetFrac(new(big.Int).SetUint64(amtRaw), big.NewInt(1_000_000))
		refUSDC := new(big.Rat).SetFrac(new(big.Int).SetUint64(usdcRaw), big.NewInt(1_000_000))
		// price = usdc/amount, exact rational; decimal paths see the truncated
		// scale-18 quotient like handleOrderFilledValues computes it.
		refPrice := new(big.Rat).Quo(refUSDC, refAmtFill)
		priceD, _ := dec18(usdcRaw).Div(amtD, scale)

		if isBuy {
			// reference
			num := new(big.Rat).Add(new(big.Rat).Mul(refAvg, refAmt), new(big.Rat).Mul(refPrice, refAmtFill))
			den := new(big.Rat).Add(refAmt, refAmtFill)
			refAvg.Quo(num, den)
			refAmt.Set(den)

			// A: truncating
			if denom, ok := aAmt.Add(amtD); ok && !denom.IsZero() {
				na, _ := aAvg.Mul(aAmt, scale)
				nb, _ := priceD.Mul(amtD, scale)
				if n, ok := na.Add(nb); ok {
					if avg, ok := n.Div(denom, scale); ok {
						aAvg = avg
					}
				}
			}
			aAmt, _ = aAmt.Add(amtD)

			// B: rounded
			if denom, ok := bAmt.Add(amtD); ok && !denom.IsZero() {
				na, _ := bAvg.Mul(bAmt, scale)
				nb, _ := priceD.Mul(amtD, scale)
				if n, ok := na.Add(nb); ok {
					if avg, ok := roundDiv(n, denom); ok {
						bAvg = avg
					}
				}
			}
			bAmt, _ = bAmt.Add(amtD)

			// C: pure integer adds, no division at all (cost exact at 1e18)
			cAmt.Add(&cAmt, uint256.NewInt(amtRaw))
			cCost.Add(cCost, new(big.Int).Mul(new(big.Int).SetUint64(usdcRaw), big.NewInt(1_000_000_000_000)))
		} else {
			// sell up to the held amount
			adjD := amtD
			if adjD.Gt(aAmt) {
				adjD = aAmt
			}
			refAdj := new(big.Rat).Set(refAmtFill)
			if refAdj.Cmp(refAmt) > 0 {
				refAdj.Set(refAmt)
			}
			adjRaw := new(uint256.Int).SetUint64(amtRaw)
			if adjRaw.Gt(&cAmt) {
				adjRaw.Set(&cAmt)
			}

			// reference: pnl += adj*(price-avg)
			refPnL.Add(refPnL, new(big.Rat).Mul(refAdj, new(big.Rat).Sub(refPrice, refAvg)))
			refAmt.Sub(refAmt, refAdj)

			// A
			if spread, ok := priceD.Sub(aAvg); ok {
				if pnl, ok := adjD.Mul(spread, scale); ok {
					aPnL, _ = aPnL.Add(pnl)
				}
			}
			aAmt, _ = aAmt.Sub(adjD)

			// B
			if spread, ok := priceD.Sub(bAvg); ok {
				if pnl, ok := adjD.Mul(spread, scale); ok {
					bPnL, _ = bPnL.Add(pnl)
				}
			}
			bAmt, _ = bAmt.Sub(adjD)

			// C: proceeds−costOut, costOut = cost*adj/amt rounded half-up at 1e18
			if !cAmt.IsZero() && !adjRaw.IsZero() {
				num := new(big.Int).Mul(cCost, adjRaw.ToBig())
				costOut, rem := new(big.Int).QuoRem(num, cAmt.ToBig(), new(big.Int))
				if new(big.Int).Lsh(rem, 1).Cmp(cAmt.ToBig()) >= 0 {
					costOut.Add(costOut, big.NewInt(1))
				}
				// proceeds at 1e18 attributed to adj (exact when adj == sell amount)
				proceeds := new(big.Int).Mul(new(big.Int).SetUint64(usdcRaw), big.NewInt(1_000_000_000_000))
				proceeds.Mul(proceeds, adjRaw.ToBig())
				premL := new(big.Int)
				proceeds, premL = new(big.Int).QuoRem(proceeds, new(big.Int).SetUint64(amtRaw), premL)
				if new(big.Int).Lsh(premL, 1).Cmp(new(big.Int).SetUint64(amtRaw)) >= 0 {
					proceeds.Add(proceeds, big.NewInt(1))
				}
				cPnL.Add(cPnL, new(big.Int).Sub(proceeds, costOut))
				cCost.Sub(cCost, costOut)
				cAmt.Sub(&cAmt, adjRaw)
			}
		}
	}

	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	toRat := func(d protomath.Decimal256) *big.Rat {
		return new(big.Rat).SetFrac(d.ScaledBig(), e18)
	}
	// exact error as Rat, then to float64 (small magnitudes representable)
	errF := func(r *big.Rat) float64 {
		f, _ := new(big.Rat).Sub(r, refPnL).Float64()
		return f
	}
	refF, _ := refPnL.Float64()
	errA := errF(toRat(aPnL))
	errB := errF(toRat(bPnL))
	errC := errF(new(big.Rat).SetFrac(cPnL, e18))
	t.Logf("realized PnL after %d fills: ref=%.12f", fills, refF)
	t.Logf("  A truncating recurrence: err=%+.3e", errA)
	t.Logf("  B rounded recurrence:    err=%+.3e", errB)
	t.Logf("  C cost-basis integers:   err=%+.3e", errC)

	abs := func(x float64) float64 {
		if x < 0 {
			return -x
		}
		return x
	}
	if abs(errC) > abs(errA) {
		t.Errorf("cost-basis (C) should beat truncating recurrence (A): |%g| > |%g|", errC, errA)
	}
}
