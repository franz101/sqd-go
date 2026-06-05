package protomath

import (
	"math/big"
	"os"
	"strconv"
	"testing"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

func BenchmarkDiv1e18_ProtoSubtype(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	col := makeHugeProtoCol(rows)
	q := make([]UInt256, rows)
	r := make([]uint64, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ColUInt256(col).Div1e18Into(q, r)
	}
}

func BenchmarkDiv1e18_ProtoBits(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	col := makeHugeProtoCol(rows)
	q := make([]UInt256, rows)
	r := make([]uint64, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ColUInt256(col).DivUint64Into(DecimalScale18, q, r)
	}
}

func BenchmarkDiv1e18_Holiman(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	values := makeHugeHolimanValues(rows)
	q := make([]uint256.Int, rows)
	r := make([]uint256.Int, rows)
	divisor := uint256.NewInt(DecimalScale18)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range values {
			q[i].DivMod(&values[i], divisor, &r[i])
		}
	}
}

func BenchmarkDiv1e18_BigIntReuse(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	values := makeHugeBigValues(rows)
	q := make([]big.Int, rows)
	r := make([]big.Int, rows)
	divisor := new(big.Int).SetUint64(DecimalScale18)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range values {
			q[i].QuoRem(&values[i], divisor, &r[i])
		}
	}
}

func BenchmarkDiv1e18_ShopDecimal(b *testing.B) {
	rows := benchRows("PROTO_MATH_DECIMAL_ROWS", 10_000)
	values := makeHugeDecimalValues(rows)
	q := make([]decimal.Decimal, rows)
	divisor := decimal.New(1, 18)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range values {
			q[i] = values[i].Div(divisor)
		}
	}
}

func BenchmarkStreamBlocks_ProtoSubtype(b *testing.B) {
	blocks := benchRows("PROTO_MATH_BLOCKS", 5000)
	rows := benchRows("PROTO_MATH_BLOCK_ROWS", 2000)
	col := makeHugeProtoCol(rows)
	q := make([]UInt256, rows)
	r := make([]uint64, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(blocks), "blocks/op")
	b.ReportMetric(float64(blocks*rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for block := 0; block < blocks; block++ {
			ColUInt256(col).Div1e18Into(q, r)
		}
	}
}

func BenchmarkStreamBlocks_Holiman(b *testing.B) {
	blocks := benchRows("PROTO_MATH_BLOCKS", 5000)
	rows := benchRows("PROTO_MATH_BLOCK_ROWS", 2000)
	values := makeHugeHolimanValues(rows)
	q := make([]uint256.Int, rows)
	r := make([]uint256.Int, rows)
	divisor := uint256.NewInt(DecimalScale18)

	b.ReportAllocs()
	b.ReportMetric(float64(blocks), "blocks/op")
	b.ReportMetric(float64(blocks*rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for block := 0; block < blocks; block++ {
			for i := range values {
				q[i].DivMod(&values[i], divisor, &r[i])
			}
		}
	}
}

func BenchmarkStreamBlocks_ShopDecimal(b *testing.B) {
	blocks := benchRows("PROTO_MATH_DECIMAL_BLOCKS", 500)
	rows := benchRows("PROTO_MATH_BLOCK_ROWS", 2000)
	values := makeHugeDecimalValues(rows)
	q := make([]decimal.Decimal, rows)
	divisor := decimal.New(1, 18)

	b.ReportAllocs()
	b.ReportMetric(float64(blocks), "blocks/op")
	b.ReportMetric(float64(blocks*rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for block := 0; block < blocks; block++ {
			for i := range values {
				q[i] = values[i].Div(divisor)
			}
		}
	}
}

func BenchmarkDecimal256_Add(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	left, right := makeDecimal256BenchValues(rows)
	out := make([]Decimal256, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range left {
			out[i], _ = left[i].Add(right[i])
		}
	}
}

func BenchmarkShopDecimal_Add(b *testing.B) {
	rows := benchRows("PROTO_MATH_DECIMAL_ROWS", 10_000)
	left, right := makeShopDecimalBenchValues(rows)
	out := make([]decimal.Decimal, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range left {
			out[i] = left[i].Add(right[i])
		}
	}
}

func BenchmarkDecimal256_Mul(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	left, right := makeDecimal256BenchValues(rows)
	out := make([]Decimal256, rows)
	scale := Decimal256Scale18

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range left {
			out[i], _ = left[i].Mul(right[i], scale)
		}
	}
}

func BenchmarkShopDecimal_Mul(b *testing.B) {
	rows := benchRows("PROTO_MATH_DECIMAL_ROWS", 10_000)
	left, right := makeShopDecimalBenchValues(rows)
	out := make([]decimal.Decimal, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range left {
			out[i] = left[i].Mul(right[i])
		}
	}
}

func BenchmarkDecimal256_Div(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	left, right := makeDecimal256BenchValues(rows)
	out := make([]Decimal256, rows)
	scale := Decimal256Scale18

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range left {
			out[i], _ = left[i].Div(right[i], scale)
		}
	}
}

func BenchmarkShopDecimal_Div(b *testing.B) {
	rows := benchRows("PROTO_MATH_DECIMAL_ROWS", 10_000)
	left, right := makeShopDecimalBenchValues(rows)
	out := make([]decimal.Decimal, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range left {
			out[i] = left[i].Div(right[i])
		}
	}
}

func makeHugeProtoCol(rows int) proto.ColUInt256 {
	out := make(proto.ColUInt256, rows)
	for i := range out {
		x := uint64(i + 1)
		out[i] = proto.UInt256{
			Low: proto.UInt128{
				Low:  x*0x9e3779b97f4a7c15 + 0x632be59bd9b4e019,
				High: x*0xbf58476d1ce4e5b9 + 0x94d049bb133111eb,
			},
			High: proto.UInt128{
				Low:  x*0x94d049bb133111eb + 0xdeadbeefcafebabe,
				High: ^(x * 0xd2b74407b1ce6e93),
			},
		}
	}
	return out
}

func makeHugeHolimanValues(rows int) []uint256.Int {
	col := makeHugeProtoCol(rows)
	out := make([]uint256.Int, rows)
	for i, value := range col {
		out[i] = FromProto(value).Holiman()
	}
	return out
}

func makeHugeBigValues(rows int) []big.Int {
	col := makeHugeProtoCol(rows)
	out := make([]big.Int, rows)
	for i, value := range col {
		FromProto(value).IntoBig(&out[i])
	}
	return out
}

func makeHugeDecimalValues(rows int) []decimal.Decimal {
	values := makeHugeBigValues(rows)
	out := make([]decimal.Decimal, rows)
	for i := range values {
		out[i] = decimal.NewFromBigInt(&values[i], 0)
	}
	return out
}

func makeDecimal256BenchValues(rows int) ([]Decimal256, []Decimal256) {
	left := make([]Decimal256, rows)
	right := make([]Decimal256, rows)
	scale := Decimal256Scale18
	for i := range left {
		left[i] = mustDecimal256Bench("123.456789012345678901", scale)
		if i%2 == 0 {
			right[i] = mustDecimal256Bench("-2.500000000000000000", scale)
		} else {
			right[i] = mustDecimal256Bench("2.000000000000000000", scale)
		}
	}
	return left, right
}

func makeShopDecimalBenchValues(rows int) ([]decimal.Decimal, []decimal.Decimal) {
	left := make([]decimal.Decimal, rows)
	right := make([]decimal.Decimal, rows)
	for i := range left {
		l, err := decimal.NewFromString("123.456789012345678901")
		if err != nil {
			panic(err)
		}
		left[i] = l
		if i%2 == 0 {
			r, err := decimal.NewFromString("-2.500000000000000000")
			if err != nil {
				panic(err)
			}
			right[i] = r
		} else {
			r, err := decimal.NewFromString("2.000000000000000000")
			if err != nil {
				panic(err)
			}
			right[i] = r
		}
	}
	return left, right
}

func mustDecimal256Bench(value string, scale Decimal256Scale) Decimal256 {
	out, err := ParseDecimal256(value, scale)
	if err != nil {
		panic(err)
	}
	return out
}

func benchRows(env string, fallback int) int {
	if raw := os.Getenv(env); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return fallback
}
