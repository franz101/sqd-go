package protomath

import (
	"testing"

	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

var (
	pnlDecimal256Sink  Decimal256
	pnlShopDecimalSink decimal.Decimal
	pnlHolimanMagSink  uint256.Int
	pnlHolimanNegSink  bool
)

func BenchmarkPnLDelta_ProtoDecimal256(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	amounts, prices, avgPrices := makePnLDecimal256BenchValues(rows)
	out := make([]Decimal256, rows)
	scale := Decimal256Scale18

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range amounts {
			spread, ok := prices[i].Sub(avgPrices[i])
			if !ok {
				b.Fatal("price spread overflow")
			}
			out[i], ok = amounts[i].Mul(spread, scale)
			if !ok {
				b.Fatal("PnL multiplication overflow")
			}
		}
	}
	pnlDecimal256Sink = out[rows-1]
}

func BenchmarkPnLDelta_HolimanSignMagnitude(b *testing.B) {
	rows := benchRows("PROTO_MATH_ROWS", 100_000)
	amounts, prices, avgPrices := makePnLHolimanScaledBenchValues(rows)
	outMag := make([]uint256.Int, rows)
	outNeg := make([]bool, rows)
	scale := uint256.NewInt(DecimalScale18)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range amounts {
			var spread uint256.Int
			outNeg[i] = subMagnitude(&spread, &prices[i], &avgPrices[i])
			if _, overflow := outMag[i].MulDivOverflow(&amounts[i], &spread, scale); overflow {
				b.Fatal("PnL multiplication overflow")
			}
		}
	}
	pnlHolimanMagSink = outMag[rows-1]
	pnlHolimanNegSink = outNeg[rows-1]
}

func BenchmarkPnLDelta_ShopDecimal(b *testing.B) {
	rows := benchRows("PROTO_MATH_DECIMAL_ROWS", 10_000)
	amounts, prices, avgPrices := makePnLShopDecimalBenchValues(rows)
	out := make([]decimal.Decimal, rows)

	b.ReportAllocs()
	b.ReportMetric(float64(rows), "rows/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range amounts {
			out[i] = amounts[i].Mul(prices[i].Sub(avgPrices[i]))
		}
	}
	pnlShopDecimalSink = out[rows-1]
}

func makePnLDecimal256BenchValues(rows int) ([]Decimal256, []Decimal256, []Decimal256) {
	amounts := make([]Decimal256, rows)
	prices := make([]Decimal256, rows)
	avgPrices := make([]Decimal256, rows)
	scale := Decimal256Scale18
	hugeAmount := mustDecimal256Bench("1000000000000000000000000.000000000000000000", scale)
	for i := range amounts {
		if i%4 == 0 {
			amounts[i] = hugeAmount
		} else {
			amount, ok := FromInt64(int64(10+i%90), scale)
			if !ok {
				panic("amount overflow")
			}
			amounts[i] = amount
		}
		prices[i] = FromScaledInt64(300_000_000_000_000_000 + int64(i%500)*1_000_000_000_000_000)
		avgPrices[i] = FromScaledInt64(500_000_000_000_000_000 + int64((i*7)%300)*1_000_000_000_000_000)
	}
	return amounts, prices, avgPrices
}

func makePnLHolimanScaledBenchValues(rows int) ([]uint256.Int, []uint256.Int, []uint256.Int) {
	amountDecimals, priceDecimals, avgPriceDecimals := makePnLDecimal256BenchValues(rows)
	amounts := make([]uint256.Int, rows)
	prices := make([]uint256.Int, rows)
	avgPrices := make([]uint256.Int, rows)
	for i := range amounts {
		_, amounts[i] = amountDecimals[i].signMagnitude()
		_, prices[i] = priceDecimals[i].signMagnitude()
		_, avgPrices[i] = avgPriceDecimals[i].signMagnitude()
	}
	return amounts, prices, avgPrices
}

func makePnLShopDecimalBenchValues(rows int) ([]decimal.Decimal, []decimal.Decimal, []decimal.Decimal) {
	amounts := make([]decimal.Decimal, rows)
	prices := make([]decimal.Decimal, rows)
	avgPrices := make([]decimal.Decimal, rows)
	hugeAmount := mustShopDecimalBench("1000000000000000000000000.000000000000000000")
	for i := range amounts {
		if i%4 == 0 {
			amounts[i] = hugeAmount
		} else {
			amounts[i] = decimal.NewFromInt(int64(10 + i%90))
		}
		prices[i] = decimal.New(300_000_000_000_000_000+int64(i%500)*1_000_000_000_000_000, -18)
		avgPrices[i] = decimal.New(500_000_000_000_000_000+int64((i*7)%300)*1_000_000_000_000_000, -18)
	}
	return amounts, prices, avgPrices
}

func subMagnitude(out, x, y *uint256.Int) bool {
	if x.Cmp(y) >= 0 {
		out.Sub(x, y)
		return false
	}
	out.Sub(y, x)
	return true
}

func mustShopDecimalBench(value string) decimal.Decimal {
	out, err := decimal.NewFromString(value)
	if err != nil {
		panic(err)
	}
	return out
}
