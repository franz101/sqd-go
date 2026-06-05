//go:build chdb

package protomath

import (
	"strings"
	"testing"

	"github.com/chdb-io/chdb-go/chdb"
)

func TestChDBUInt256OperatorParity(t *testing.T) {
	a := mustUInt256FromString(t, "100000000000000000000000000000000000000000000000000000000000123456789")
	b := mustUInt256FromString(t, "1234567890123456789012345678901234567890")
	q, r := a.DivMod(b)

	cases := []struct {
		name string
		sql  string
		got  string
	}{
		{"add", "toUInt256('100000000000000000000000000000000000000000000000000000000000123456789') + toUInt256('1234567890123456789012345678901234567890')", a.Add(b).Big().String()},
		{"sub", "toUInt256('100000000000000000000000000000000000000000000000000000000000123456789') - toUInt256('1234567890123456789012345678901234567890')", a.Sub(b).Big().String()},
		{"mul", "toUInt256('12345678901234567890') * toUInt256('987654321')", FromUInt64(12345678901234567890).Mul(FromUInt64(987654321)).Big().String()},
		{"div", "intDiv(toUInt256('100000000000000000000000000000000000000000000000000000000000123456789'), toUInt256('1234567890123456789012345678901234567890'))", q.Big().String()},
		{"mod", "toUInt256('100000000000000000000000000000000000000000000000000000000000123456789') % toUInt256('1234567890123456789012345678901234567890')", r.Big().String()},
		{"and", "bitAnd(toUInt256('100000000000000000000000000000000000000000000000000000000000123456789'), toUInt256('1234567890123456789012345678901234567890'))", a.And(b).Big().String()},
		{"or", "bitOr(toUInt256('100000000000000000000000000000000000000000000000000000000000123456789'), toUInt256('1234567890123456789012345678901234567890'))", a.Or(b).Big().String()},
		{"xor", "bitXor(toUInt256('100000000000000000000000000000000000000000000000000000000000123456789'), toUInt256('1234567890123456789012345678901234567890'))", a.Xor(b).Big().String()},
		{"lsh", "bitShiftLeft(toUInt256('1234567890123456789012345678901234567890'), 17)", b.Lsh(17).Big().String()},
		{"rsh", "bitShiftRight(toUInt256('1234567890123456789012345678901234567890'), 17)", b.Rsh(17).Big().String()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCH := chdbScalar(t, "SELECT toString("+tc.sql+")")
			if gotCH != tc.got {
				t.Fatalf("chDB mismatch\ngot  %s\nwant %s", gotCH, tc.got)
			}
		})
	}
}

func TestChDBDecimal256OperatorParity(t *testing.T) {
	scale := Decimal256Scale18
	a := mustParseDecimal256(t, "1.50", scale)
	b := mustParseDecimal256(t, "-2.25", scale)
	c := mustParseDecimal256(t, "0.25", scale)
	one := mustParseDecimal256(t, "1", scale)
	three := mustParseDecimal256(t, "3", scale)
	add, _ := a.Add(b)
	sub, _ := a.Sub(b)
	mul, _ := a.Mul(b, scale)
	div, _ := a.Div(c, scale)
	fracDiv, _ := one.Div(three, scale)
	var fracDivBytes [32]byte
	fracDiv.PutLittleEndianBytes(&fracDivBytes)
	fracDivFromBytes, err := FromDecimal256LittleEndianBytes(fracDivBytes[:])
	if err != nil {
		t.Fatalf("from bytes: %v", err)
	}
	fracDivFromBig, ok := FromDecimal256ScaledBigInt(fracDiv.ScaledBig())
	if !ok {
		t.Fatal("from scaled big.Int failed")
	}
	mod, _ := b.Mod(a)

	cases := []struct {
		name string
		sql  string
		got  string
	}{
		{"add", "toDecimal256('1.50', 18) + toDecimal256('-2.25', 18)", add.String(scale)},
		{"sub", "toDecimal256('1.50', 18) - toDecimal256('-2.25', 18)", sub.String(scale)},
		{"mul", "CAST(toDecimal256('1.50', 18) * toDecimal256('-2.25', 18), 'Decimal256(18)')", mul.String(scale)},
		{"div", "CAST(toDecimal256('1.50', 18) / toDecimal256('0.25', 18), 'Decimal256(18)')", div.String(scale)},
		{"fractional_div", "CAST(toDecimal256('1', 18) / toDecimal256('3', 18), 'Decimal256(18)')", fracDiv.String(scale)},
		{"fractional_div_from_bytes", "CAST(toDecimal256('1', 18) / toDecimal256('3', 18), 'Decimal256(18)')", fracDivFromBytes.String(scale)},
		{"fractional_div_from_bigint", "CAST(toDecimal256('1', 18) / toDecimal256('3', 18), 'Decimal256(18)')", fracDivFromBig.String(scale)},
		{"mod", "toDecimal256('-2.25', 18) % toDecimal256('1.50', 18)", mod.String(scale)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok := chdbScalar(t, "SELECT toString("+tc.sql+" = toDecimal256('"+tc.got+"', 18))")
			if ok != "1" {
				gotCH := chdbScalar(t, "SELECT toString("+tc.sql+")")
				t.Fatalf("chDB mismatch\ngot  %s\nwant %s", gotCH, tc.got)
			}
		})
	}
}

func chdbScalar(t *testing.T, query string) string {
	t.Helper()
	out, err := chdb.Query(query, "CSV")
	if err != nil {
		t.Fatalf("chdb query failed: %v\n%s", err, query)
	}
	defer out.Free()
	if err := out.Error(); err != nil {
		t.Fatalf("chdb result failed: %v\n%s", err, query)
	}
	return strings.Trim(strings.TrimSpace(out.String()), `"`)
}

func mustUInt256FromString(t *testing.T, value string) UInt256 {
	t.Helper()
	parsed, err := ParseUInt256(value)
	if err != nil {
		t.Fatalf("parse uint256 %q: %v", value, err)
	}
	return parsed
}
