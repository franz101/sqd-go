package protomath

import (
	"math/big"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

var (
	decimal256AllocSink Decimal256
	uint256AllocSink    UInt256
	uint64AllocSink     uint64
)

func TestUInt256RangeAndScale18Division(t *testing.T) {
	maxBig := uint256MaxBig()
	max, err := ParseUInt256(maxBig.String())
	if err != nil {
		t.Fatalf("parse UInt256 max: %v", err)
	}
	assertUInt256Big(t, "uint256 max", max, maxBig)

	divisor := new(big.Int).SetUint64(DecimalScale18)
	wantQ, wantR := new(big.Int), new(big.Int)
	wantQ.QuoRem(maxBig, divisor, wantR)

	q, r := max.Div1e18()
	assertUInt256Big(t, "max / 1e18 quotient", q, wantQ)
	if r != wantR.Uint64() {
		t.Fatalf("max / 1e18 remainder = %d, want %s", r, wantR)
	}

	qBits, rBits := max.DivUint64(DecimalScale18)
	if !qBits.Eq(q) || rBits != r {
		t.Fatalf("DivUint64 mismatch: q=%s r=%d want q=%s r=%d", qBits.Big(), rBits, q.Big(), r)
	}

	qHoliman, rHoliman := max.DivBy(DecimalScale18Divisor())
	if !qHoliman.Eq(q) || rHoliman != r {
		t.Fatalf("DivBy mismatch: q=%s r=%d want q=%s r=%d", qHoliman.Big(), rHoliman, q.Big(), r)
	}

	tenPow77 := "1" + strings.Repeat("0", 77)
	fits, err := ParseUInt256(tenPow77)
	if err != nil {
		t.Fatalf("10^77 should fit UInt256: %v", err)
	}
	fitsQ, fitsR := fits.Div1e18()
	if fitsR != 0 {
		t.Fatalf("10^77 / 1e18 remainder = %d, want 0", fitsR)
	}
	assertUInt256Big(t, "10^77 / 1e18", fitsQ, mustBigIntString(t, "1"+strings.Repeat("0", 59)))

	if _, err := ParseUInt256(new(big.Int).Add(maxBig, big.NewInt(1)).String()); err == nil {
		t.Fatal("ParseUInt256 accepted 2^256")
	}
	if _, err := ParseUInt256("1" + strings.Repeat("0", 78)); err == nil {
		t.Fatal("ParseUInt256 accepted 10^78, which is above UInt256 max")
	}
}

func TestDecimal256SignedRangeAndOverflow(t *testing.T) {
	scale := Decimal256Scale18
	maxCoeff := int256MaxBig()
	minCoeff := new(big.Int).Neg(int256MinMagnitudeBig())

	max, ok := FromDecimal256ScaledBigInt(maxCoeff)
	if !ok {
		t.Fatal("int256 max coefficient should fit Decimal256")
	}
	if got := max.ScaledBig(); got.Cmp(maxCoeff) != 0 {
		t.Fatalf("max scaled coefficient = %s, want %s", got, maxCoeff)
	}
	parsedMax, err := ParseDecimal256(max.String(scale), scale)
	if err != nil {
		t.Fatalf("parse max Decimal256 string: %v", err)
	}
	if !parsedMax.Eq(max) {
		t.Fatalf("max string roundtrip changed value: got %s want %s", parsedMax.String(scale), max.String(scale))
	}

	min, ok := FromDecimal256ScaledBigInt(minCoeff)
	if !ok {
		t.Fatal("int256 min coefficient should fit Decimal256")
	}
	if got := min.ScaledBig(); got.Cmp(minCoeff) != 0 {
		t.Fatalf("min scaled coefficient = %s, want %s", got, minCoeff)
	}
	parsedMin, err := ParseDecimal256(min.String(scale), scale)
	if err != nil {
		t.Fatalf("parse min Decimal256 string: %v", err)
	}
	if !parsedMin.Eq(min) {
		t.Fatalf("min string roundtrip changed value: got %s want %s", parsedMin.String(scale), min.String(scale))
	}

	if _, ok := FromDecimal256ScaledBigInt(new(big.Int).Add(maxCoeff, big.NewInt(1))); ok {
		t.Fatal("Decimal256 accepted coefficient above int256 max")
	}
	if _, ok := FromDecimal256ScaledBigInt(new(big.Int).Sub(minCoeff, big.NewInt(1))); ok {
		t.Fatal("Decimal256 accepted coefficient below int256 min")
	}

	oneRawUnit := FromScaledInt64(1)
	if _, ok := max.Add(oneRawUnit); ok {
		t.Fatal("Decimal256 max + 1 raw unit did not report overflow")
	}
	if _, ok := min.Sub(oneRawUnit); ok {
		t.Fatal("Decimal256 min - 1 raw unit did not report overflow")
	}
	if _, ok := min.Neg(); ok {
		t.Fatal("negating Decimal256 min did not report overflow")
	}

	two, ok := FromInt64(2, scale)
	if !ok {
		t.Fatal("construct 2.0")
	}
	if _, ok := max.Mul(two, scale); ok {
		t.Fatal("Decimal256 max * 2 did not report overflow")
	}
	half := mustParseDecimal256(t, "0.5", scale)
	if _, ok := max.Div(half, scale); ok {
		t.Fatal("Decimal256 max / 0.5 did not report overflow")
	}

	if _, err := ParseDecimal256("1"+strings.Repeat("0", 58), scale); err != nil {
		t.Fatalf("10^58 should fit Decimal256(18): %v", err)
	}
	if _, err := ParseDecimal256("1"+strings.Repeat("0", 59), scale); err == nil {
		t.Fatal("10^59 should not fit Decimal256(18)")
	}
}

func TestDecimal256LargeNegativeArithmeticMatchesShopDecimal(t *testing.T) {
	scale := Decimal256Scale18

	a := mustParseDecimal256(t, "100000000000000000000000000000.000000000000000001", scale)
	b := mustParseDecimal256(t, "-99999999999999999999999999999.999999999999999999", scale)
	shopA := mustShopDecimal(t, "100000000000000000000000000000.000000000000000001")
	shopB := mustShopDecimal(t, "-99999999999999999999999999999.999999999999999999")

	add, ok := a.Add(b)
	if !ok {
		t.Fatal("large add overflowed")
	}
	assertDecimal256Scaled(t, "large add", add, shopA.Add(shopB), scale)

	sub, ok := a.Sub(b)
	if !ok {
		t.Fatal("large sub overflowed")
	}
	assertDecimal256Scaled(t, "large sub", sub, shopA.Sub(shopB), scale)

	amount := mustParseDecimal256(t, "1000000000000000000000000.000000000000000000", scale)
	price := mustParseDecimal256(t, "0.300000000000000000", scale)
	avgPrice := mustParseDecimal256(t, "0.700000000000000000", scale)
	shopAmount := mustShopDecimal(t, "1000000000000000000000000.000000000000000000")
	shopPrice := mustShopDecimal(t, "0.300000000000000000")
	shopAvgPrice := mustShopDecimal(t, "0.700000000000000000")

	spread, ok := price.Sub(avgPrice)
	if !ok {
		t.Fatal("price spread overflowed")
	}
	if !spread.IsNegative() {
		t.Fatalf("spread should be negative, got %s", spread.String(scale))
	}
	pnl, ok := amount.Mul(spread, scale)
	if !ok {
		t.Fatal("PnL multiplication overflowed")
	}
	assertDecimal256Scaled(t, "negative PnL", pnl, shopAmount.Mul(shopPrice.Sub(shopAvgPrice)), scale)

	one := mustParseDecimal256(t, "1", scale)
	three := mustParseDecimal256(t, "3", scale)
	div, ok := one.Div(three, scale)
	if !ok {
		t.Fatal("1 / 3 overflowed")
	}
	shopDiv, _ := mustShopDecimal(t, "1").QuoRem(mustShopDecimal(t, "3"), scale.Scale())
	assertDecimal256Scaled(t, "1 / 3", div, shopDiv, scale)
}

func TestProtoMathHotOpsAllocateZero(t *testing.T) {
	scale := Decimal256Scale18
	amount := mustParseDecimal256(t, "1000000000000000000000000.000000000000000000", scale)
	price := mustParseDecimal256(t, "0.300000000000000000", scale)
	avgPrice := mustParseDecimal256(t, "0.700000000000000000", scale)
	three := mustParseDecimal256(t, "3", scale)
	max, err := ParseUInt256(uint256MaxBig().String())
	if err != nil {
		t.Fatalf("parse UInt256 max: %v", err)
	}

	decimalAllocs := testing.AllocsPerRun(1000, func() {
		spread, ok := price.Sub(avgPrice)
		if !ok {
			panic("sub overflow")
		}
		pnl, ok := amount.Mul(spread, scale)
		if !ok {
			panic("mul overflow")
		}
		decimal256AllocSink, ok = pnl.Div(three, scale)
		if !ok {
			panic("div overflow")
		}
	})
	if decimalAllocs != 0 {
		t.Fatalf("Decimal256 hot ops allocated %.2f times per run, want 0", decimalAllocs)
	}

	uintAllocs := testing.AllocsPerRun(1000, func() {
		uint256AllocSink, uint64AllocSink = max.Div1e18()
	})
	if uintAllocs != 0 {
		t.Fatalf("UInt256 Div1e18 allocated %.2f times per run, want 0", uintAllocs)
	}
}

func assertDecimal256Scaled(t *testing.T, name string, got Decimal256, want decimal.Decimal, scale Decimal256Scale) {
	t.Helper()
	wantCoeff := want.Shift(scale.Scale()).BigInt()
	if gotCoeff := got.ScaledBig(); gotCoeff.Cmp(wantCoeff) != 0 {
		t.Fatalf("%s mismatch\ngot  %s (%s)\nwant %s (%s)", name, got.String(scale), gotCoeff, want.String(), wantCoeff)
	}
}

func mustShopDecimal(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	out, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", value, err)
	}
	return out
}

func mustBigIntString(t *testing.T, value string) *big.Int {
	t.Helper()
	out, ok := new(big.Int).SetString(value, 10)
	if !ok {
		t.Fatalf("parse big.Int %q", value)
	}
	return out
}

func uint256Max() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
}

func uint256MaxBig() *big.Int {
	return new(big.Int).Set(uint256Max())
}

func int256MaxBig() *big.Int {
	return new(big.Int).Sub(int256MinMagnitudeBig(), big.NewInt(1))
}

func int256MinMagnitudeBig() *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), 255)
}
