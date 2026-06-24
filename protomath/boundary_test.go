package protomath

import (
	"math/big"
	"testing"
)

// Decimal256 is a signed scaled integer in [-2^255, 2^255-1]. These tests pin
// the arithmetic against a math/big oracle at the magnitude boundary — the
// regime where the order-fill/position handlers' overflow fallback is supposed
// to trigger, and where two's-complement sign handling is easiest to get wrong.
// The oracle computes the exact mathematical result and asserts both the value
// (when representable) and that the ok flag is true iff the result is in range.

func d256Max() *big.Int { return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1)) } // 2^255-1
func d256Min() *big.Int { return new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 255)) }                // -2^255
func e18Big() *big.Int  { return new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) }

// quoTrunc divides truncating toward zero (matching sign-magnitude truncation
// in Decimal256.Mul/Div); big.Int.Quo already truncates toward zero.
func quoTrunc(a, b *big.Int) *big.Int { return new(big.Int).Quo(a, b) }

func boundaryCoeffs() []*big.Int {
	max, min, e18 := d256Max(), d256Min(), e18Big()
	half := new(big.Int).Quo(max, big.NewInt(2))
	// A value near max divided by 1e18 lands here; useful as a Div operand.
	nearMax := new(big.Int).Sub(max, big.NewInt(12345))
	return []*big.Int{
		big.NewInt(0),
		e18, new(big.Int).Neg(e18), // ±1.0
		max, new(big.Int).Sub(max, big.NewInt(1)),
		min, new(big.Int).Add(min, big.NewInt(1)),
		half, new(big.Int).Neg(half),
		nearMax, new(big.Int).Neg(nearMax),
		new(big.Int).Mul(e18, big.NewInt(1_000_000_000)), // 1e9 scaled
		new(big.Int).Neg(new(big.Int).Mul(e18, big.NewInt(7))),
	}
}

func TestDecimal256BoundaryArithmeticMatchesBigInt(t *testing.T) {
	scale := Decimal256Scale18
	max, min, e18 := d256Max(), d256Min(), e18Big()
	inRange := func(x *big.Int) bool { return x.Cmp(min) >= 0 && x.Cmp(max) <= 0 }

	mk := func(coeff *big.Int) Decimal256 {
		d, ok := FromDecimal256ScaledBigInt(coeff)
		if !ok {
			t.Fatalf("fixture coeff %s does not fit Decimal256", coeff)
		}
		return d
	}
	check := func(name string, got Decimal256, ok bool, want *big.Int) {
		t.Helper()
		wantOK := inRange(want)
		if ok != wantOK {
			t.Errorf("%s: ok=%v, want %v (true result %s)", name, ok, wantOK, want)
			return
		}
		if ok && got.ScaledBig().Cmp(want) != 0 {
			t.Errorf("%s: coeff %s, want %s", name, got.ScaledBig(), want)
		}
	}

	coeffs := boundaryCoeffs()
	for _, ca := range coeffs {
		for _, cb := range coeffs {
			a, b := mk(ca), mk(cb)

			sum, ok := a.Add(b)
			check("add", sum, ok, new(big.Int).Add(ca, cb))

			diff, ok := a.Sub(b)
			check("sub", diff, ok, new(big.Int).Sub(ca, cb))

			prod, ok := a.Mul(b, scale)
			check("mul", prod, ok, quoTrunc(new(big.Int).Mul(ca, cb), e18))

			if cb.Sign() != 0 {
				quo, ok := a.Div(b, scale)
				check("div", quo, ok, quoTrunc(new(big.Int).Mul(ca, e18), cb))
			} else if _, ok := a.Div(b, scale); ok {
				t.Errorf("div by zero returned ok=true")
			}
		}
	}
}

// TestDecimal256NearMaxDivByE18 is the case called out explicitly: a magnitude
// near 2^255 divided by 1e18, both signs. The 512-bit intermediate in
// MulDivOverflow means this does NOT overflow even though coeff*1e18 far
// exceeds 2^256 — the result fits, so the native path handles it (no fallback).
func TestDecimal256NearMaxDivByE18(t *testing.T) {
	scale := Decimal256Scale18
	e18 := e18Big()
	one := mustDecFromCoeff(t, e18)             // value 1.0 (coeff 1e18)
	divisor := mustDecFromCoeff(t, new(big.Int).Mul(e18, e18)) // value 1e18 (coeff 1e36)

	for _, sign := range []int{1, -1} {
		coeff := new(big.Int).Sub(d256Max(), big.NewInt(99))
		if sign < 0 {
			coeff.Neg(coeff)
		}
		v := mustDecFromCoeff(t, coeff)

		// v / 1.0 == v (identity through the wide intermediate).
		q, ok := v.Div(one, scale)
		if !ok || q.ScaledBig().Cmp(coeff) != 0 {
			t.Fatalf("near-max / 1.0: ok=%v got=%s want=%s", ok, q.ScaledBig(), coeff)
		}

		// v / 1e18: result coeff = trunc(coeff * 1e18 / 1e36) = trunc(coeff/1e18).
		q, ok = v.Div(divisor, scale)
		want := quoTrunc(new(big.Int).Mul(coeff, e18), new(big.Int).Mul(e18, e18))
		if !ok {
			t.Fatalf("near-max / 1e18 overflowed unexpectedly")
		}
		if q.ScaledBig().Cmp(want) != 0 {
			t.Errorf("near-max / 1e18: got %s want %s", q.ScaledBig(), want)
		}
	}
}

// TestDecimal256BoundaryZeroAlloc proves the boundary arithmetic stays on the
// stack — the whole reason to use Decimal256 over shopspring is that ±max-range
// math allocates nothing.
func TestDecimal256BoundaryZeroAlloc(t *testing.T) {
	scale := Decimal256Scale18
	a := mustDecFromCoeff(t, new(big.Int).Sub(d256Max(), big.NewInt(7)))
	b := mustDecFromCoeff(t, new(big.Int).Neg(e18Big()))
	var sink Decimal256
	ops := map[string]func(){
		"Add": func() { sink, _ = a.Add(b) },
		"Sub": func() { sink, _ = a.Sub(b) },
		"Mul": func() { sink, _ = a.Mul(b, scale) },
		"Div": func() { sink, _ = a.Div(b, scale) },
	}
	for name, op := range ops {
		if n := testing.AllocsPerRun(1000, op); n != 0 {
			t.Errorf("Decimal256.%s allocates %.1f objects/op at the boundary, want 0", name, n)
		}
	}
	_ = sink
}

func mustDecFromCoeff(t *testing.T, coeff *big.Int) Decimal256 {
	t.Helper()
	d, ok := FromDecimal256ScaledBigInt(coeff)
	if !ok {
		t.Fatalf("coeff %s does not fit Decimal256", coeff)
	}
	return d
}
