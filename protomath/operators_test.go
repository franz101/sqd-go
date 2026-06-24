package protomath

import (
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
)

func TestUInt256OperatorsMatchBigInt(t *testing.T) {
	mod := new(big.Int).Lsh(big.NewInt(1), 256)
	aBig := new(big.Int)
	bBig := new(big.Int)
	if _, ok := aBig.SetString("100000000000000000000000000000000000000000000000000000000000123456789", 10); !ok {
		t.Fatal("bad a fixture")
	}
	if _, ok := bBig.SetString("1234567890123456789012345678901234567890", 10); !ok {
		t.Fatal("bad b fixture")
	}

	a, ok := FromBig(aBig)
	if !ok {
		t.Fatal("a did not fit UInt256")
	}
	b, ok := FromBig(bBig)
	if !ok {
		t.Fatal("b did not fit UInt256")
	}

	assertUInt256Big(t, "add", a.Add(b), new(big.Int).Mod(new(big.Int).Add(aBig, bBig), mod))
	assertUInt256Big(t, "sub", a.Sub(b), new(big.Int).Mod(new(big.Int).Sub(aBig, bBig), mod))
	assertUInt256Big(t, "mul", a.Mul(b), new(big.Int).Mod(new(big.Int).Mul(aBig, bBig), mod))
	assertUInt256Big(t, "div", a.Div(b), new(big.Int).Div(aBig, bBig))
	assertUInt256Big(t, "mod", a.Mod(b), new(big.Int).Mod(aBig, bBig))
	assertUInt256Big(t, "and", a.And(b), new(big.Int).And(aBig, bBig))
	assertUInt256Big(t, "or", a.Or(b), new(big.Int).Or(aBig, bBig))
	assertUInt256Big(t, "xor", a.Xor(b), new(big.Int).Xor(aBig, bBig))
	assertUInt256Big(t, "not", a.Not(), new(big.Int).Mod(new(big.Int).Not(aBig), mod))
	assertUInt256Big(t, "lsh", a.Lsh(17), new(big.Int).Mod(new(big.Int).Lsh(aBig, 17), mod))
	assertUInt256Big(t, "rsh", a.Rsh(17), new(big.Int).Rsh(aBig, 17))

	if !a.Gt(b) || a.Lt(b) || !a.Eq(a) {
		t.Fatal("comparison operators returned inconsistent results")
	}
}

func TestUInt256Constructors(t *testing.T) {
	if FromUInt8(7).Uint64() != 7 ||
		FromUInt16(700).Uint64() != 700 ||
		FromUInt32(70_000).Uint64() != 70_000 ||
		FromUInt(7_000_000).Uint64() != 7_000_000 ||
		FromUInt64(7_000_000_000).Uint64() != 7_000_000_000 {
		t.Fatal("small unsigned constructors changed value")
	}
	if FromProtoUInt128(FromUInt64(9).Proto().Low).Uint64() != 9 {
		t.Fatal("proto UInt128 constructor changed value")
	}
}

func TestDecimal256Operators(t *testing.T) {
	scale := Decimal256Scale18
	a := mustParseDecimal256(t, "1.50", scale)
	b := mustParseDecimal256(t, "-2.25", scale)
	c := mustParseDecimal256(t, "0.25", scale)

	add, ok := a.Add(b)
	if !ok || add.String(scale) != "-0.750000000000000000" {
		t.Fatalf("add: got %q ok=%v", add.String(scale), ok)
	}

	sub, ok := a.Sub(b)
	if !ok || sub.String(scale) != "3.750000000000000000" {
		t.Fatalf("sub: got %q ok=%v", sub.String(scale), ok)
	}

	mul, ok := a.Mul(b, scale)
	if !ok || mul.String(scale) != "-3.375000000000000000" {
		t.Fatalf("mul: got %q ok=%v", mul.String(scale), ok)
	}

	div, ok := a.Div(c, scale)
	if !ok || div.String(scale) != "6.000000000000000000" {
		t.Fatalf("div: got %q ok=%v", div.String(scale), ok)
	}

	mod, ok := b.Mod(a)
	if !ok || mod.String(scale) != "-0.750000000000000000" {
		t.Fatalf("mod: got %q ok=%v", mod.String(scale), ok)
	}

	neg, ok := b.Neg()
	if !ok || neg.String(scale) != "2.250000000000000000" {
		t.Fatalf("neg: got %q ok=%v", neg.String(scale), ok)
	}

	withCommas := mustParseDecimal256(t, "1,234.567", scale)
	if withCommas.String(scale) != "1234.567000000000000000" {
		t.Fatalf("comma parse: got %q", withCommas.String(scale))
	}

	euroComma := mustParseDecimal256(t, "1.234,567", scale)
	if euroComma.String(scale) != "1234.567000000000000000" {
		t.Fatalf("euro comma parse: got %q", euroComma.String(scale))
	}
}

func TestDecimal256ConstructorsAndFractionalDiv(t *testing.T) {
	scale := Decimal256Scale18
	one := mustParseDecimal256(t, "1", scale)
	three := mustParseDecimal256(t, "3", scale)
	got, ok := one.Div(three, scale)
	if !ok || got.String(scale) != "0.333333333333333333" {
		t.Fatalf("1/3: got %q ok=%v", got.String(scale), ok)
	}

	var le [32]byte
	got.PutLittleEndianBytes(&le)
	fromLE, err := FromDecimal256LittleEndianBytes(le[:])
	if err != nil {
		t.Fatalf("from little endian: %v", err)
	}
	if !fromLE.Eq(got) {
		t.Fatal("little-endian bytes roundtrip changed Decimal256")
	}

	var be [32]byte
	got.PutBigEndianBytes(&be)
	fromBE, err := FromDecimal256BigEndianBytes(be[:])
	if err != nil {
		t.Fatalf("from big endian: %v", err)
	}
	if !fromBE.Eq(got) {
		t.Fatal("big-endian bytes roundtrip changed Decimal256")
	}

	fromScaledBig, ok := FromDecimal256ScaledBigInt(got.ScaledBig())
	if !ok || !fromScaledBig.Eq(got) {
		t.Fatal("scaled big.Int constructor changed Decimal256")
	}

	fromWholeBig, ok := FromDecimal256BigInt(big.NewInt(-42), scale)
	if !ok || fromWholeBig.String(scale) != "-42.000000000000000000" {
		t.Fatalf("whole big.Int constructor: got %q ok=%v", fromWholeBig.String(scale), ok)
	}
}

func TestUInt256ToDecimal256(t *testing.T) {
	scale := Decimal256Scale18
	raw := FromUInt64(42).Mul(scale.Multiplier())
	dec, ok := FromUInt256AsDecimal256(raw)
	if !ok {
		t.Fatal("UInt256 scaled coefficient did not fit Decimal256")
	}
	if dec.String(scale) != "42.000000000000000000" {
		t.Fatalf("got %s", dec.String(scale))
	}
	if dec.Raw().Cmp(raw) != 0 {
		t.Fatal("raw Decimal256 coefficient changed")
	}
}

func TestPositionsE2EProtoMatchesShopDecimal(t *testing.T) {
	scale := Decimal256Scale18
	protoStore := newProtoPositionStore(64)
	shopStore := newShopPositionStore(64)
	protoEvents := newProtoOrderEvents(256, 64)
	shopEvents := newShopOrderEvents(256, 64)

	protoStore.applyEventsMapSlices(protoEvents, scale)
	shopStore.applyEventsMapSlices(shopEvents)

	for i := range protoStore.entityID {
		assertDecimal256String(t, "amount", i, protoStore.amount[i], shopStore.amount[i], scale)
		assertDecimal256String(t, "totalBought", i, protoStore.totalBought[i], shopStore.totalBought[i], scale)
		assertDecimal256String(t, "avgPrice", i, protoStore.avgPrice[i], shopStore.avgPrice[i], scale)
	}
}

func assertUInt256Big(t *testing.T, name string, got UInt256, want *big.Int) {
	t.Helper()
	if gotBig := got.Big(); gotBig.Cmp(want) != 0 {
		t.Fatalf("%s mismatch\ngot  %s\nwant %s", name, gotBig, want)
	}
}

func assertDecimal256String(t *testing.T, name string, row int, got Decimal256, want decimal.Decimal, scale Decimal256Scale) {
	t.Helper()
	wantProto := shopDecimalToProtoDecimal256(want, decimal.New(1, scale.Scale()))
	if !got.Eq(wantProto) {
		t.Fatalf("%s row %d mismatch\ngot  %s\nwant %s", name, row, got.String(scale), wantProto.String(scale))
	}
}

func mustParseDecimal256(t *testing.T, value string, scale Decimal256Scale) Decimal256 {
	t.Helper()
	out, err := ParseDecimal256(value, scale)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return out
}
