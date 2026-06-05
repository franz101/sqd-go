package protomath

import (
	"math/big"
	"strings"
	"testing"

	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

func TestUInt256Div1e18MatchesBigHolimanDecimal(t *testing.T) {
	maxUInt256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	cases := []string{
		"0",
		"1",
		"999999999999999999",
		"1000000000000000000",
		"1000000000000000001",
		"10000000000000000000000000000000000000000000000000000000000000000000000000000",  // 1e76
		"100000000000000000000000000000000000000000000000000000000000000000000000000000", // 1e77
		maxUInt256.String(),
	}

	divisorBig := new(big.Int).SetUint64(DecimalScale18)
	divisorHoliman := uint256.NewInt(DecimalScale18)

	for _, input := range cases {
		valueBig, ok := new(big.Int).SetString(input, 10)
		if !ok {
			t.Fatalf("invalid decimal fixture %q", input)
		}
		valueHoliman, err := uint256.FromDecimal(input)
		if err != nil {
			t.Fatalf("holiman parse %q: %v", input, err)
		}

		value, ok := FromBig(valueBig)
		if !ok {
			t.Fatalf("FromBig rejected %q", input)
		}

		gotQ, gotR := value.DivUint64(DecimalScale18)
		gotFastQ, gotFastR := value.Div1e18()
		wantQBig, wantRBig := new(big.Int), new(big.Int)
		wantQBig.QuoRem(valueBig, divisorBig, wantRBig)

		if gotQ.Big().Cmp(wantQBig) != 0 {
			t.Fatalf("quotient mismatch for %q\ngot  %s\nwant %s", input, gotQ.Big(), wantQBig)
		}
		if gotR != wantRBig.Uint64() {
			t.Fatalf("remainder mismatch for %q: got %d want %s", input, gotR, wantRBig)
		}
		if gotFastQ.Big().Cmp(wantQBig) != 0 || gotFastR != wantRBig.Uint64() {
			t.Fatalf("fast quotient/remainder mismatch for %q", input)
		}

		var holimanQ, holimanR uint256.Int
		holimanQ.DivMod(valueHoliman, divisorHoliman, &holimanR)
		gotHolimanQ := gotQ.Holiman()
		if gotHolimanQ.Cmp(&holimanQ) != 0 {
			t.Fatalf("holiman quotient mismatch for %q", input)
		}
		if holimanR.Uint64() != gotR {
			t.Fatalf("holiman remainder mismatch for %q: got %d want %d", input, gotR, holimanR.Uint64())
		}

		gotDecimal := gotQ.Decimal(0).Add(decimal.NewFromBigInt(new(big.Int).SetUint64(gotR), -18))
		wantDecimal := decimal.NewFromBigInt(valueBig, -18)
		if !gotDecimal.Equal(wantDecimal) {
			t.Fatalf("decimal mismatch for %q\ngot  %s\nwant %s", input, gotDecimal, wantDecimal)
		}
	}
}

func TestUInt256RangeLimit(t *testing.T) {
	if _, err := uint256.FromDecimal("1" + strings.Repeat("0", 78)); err == nil {
		t.Fatal("10^78 should not fit in uint256")
	}

	maxUInt256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	if _, ok := FromBig(maxUInt256); !ok {
		t.Fatal("max uint256 should fit")
	}
}

func TestColUInt256WrapsProtoColumn(t *testing.T) {
	var col ColUInt256
	col.Append(UInt256{})
	col.Append(FromProto(col.Proto().Row(0)))

	if col.Rows() != 2 {
		t.Fatalf("rows: got %d want 2", col.Rows())
	}
	if col.Proto().Rows() != 2 {
		t.Fatalf("proto rows: got %d want 2", col.Proto().Rows())
	}

	q := make([]UInt256, col.Rows())
	r := make([]uint64, col.Rows())
	col.Div1e18Into(q, r)
	if q[0].Big().Sign() != 0 || r[0] != 0 {
		t.Fatal("zero division result changed")
	}
}
