package codegen

import (
	"testing"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/holiman/uint256"
)

// Simple benchmark to verify zero-copy behavior

func BenchmarkUInt256View_At(b *testing.B) {
	data := make([]proto.UInt256, 100)
	for i := range data {
		data[i] = proto.UInt256{
			Low:  proto.UInt128{Low: uint64(i), High: 0},
			High: proto.UInt128{Low: 0, High: 0},
		}
	}
	view := NewUInt256ArrayView(data)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for j := 0; j < len(data); j++ {
			_ = view.At(j)
		}
	}
}

func BenchmarkUInt256Slice_Conversion(b *testing.B) {
	data := make([]proto.UInt256, 100)
	for i := range data {
		data[i] = proto.UInt256{
			Low:  proto.UInt128{Low: uint64(i), High: 0},
			High: proto.UInt128{Low: 0, High: 0},
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out := make([]uint256.Int, len(data))
		for j, v := range data {
			out[j] = uint256.Int{
				v.Low.Low,
				v.Low.High,
				v.High.Low,
				v.High.High,
			}
		}
		_ = out
	}
}

func BenchmarkUInt256View_ForEach(b *testing.B) {
	data := make([]proto.UInt256, 100)
	for i := range data {
		data[i] = proto.UInt256{
			Low:  proto.UInt128{Low: uint64(i), High: 0},
			High: proto.UInt128{Low: 0, High: 0},
		}
	}
	view := NewUInt256ArrayView(data)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var sum uint256.Int
		view.ForEach(func(_ int, value uint256.Int) bool {
			sum.Add(&sum, &value)
			return true
		})
		_ = sum
	}
}

func BenchmarkUInt256Slice_Iteration(b *testing.B) {
	data := make([]proto.UInt256, 100)
	for i := range data {
		data[i] = proto.UInt256{
			Low:  proto.UInt128{Low: uint64(i + 1), High: 0},
			High: proto.UInt128{Low: 0, High: 0},
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		slice := make([]uint256.Int, len(data))
		for j, v := range data {
			slice[j] = uint256.Int{
				v.Low.Low,
				v.Low.High,
				v.High.Low,
				v.High.High,
			}
		}
		var sum uint256.Int
		for _, v := range slice {
			sum.Add(&sum, &v)
		}
		_ = sum
	}
}
