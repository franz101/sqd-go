package main

import (
	"testing"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// ProtoEventBlock is a minimal version for benchmarking
type ProtoEventBlock struct {
	BlockNumber proto.ColUInt64
	Maker       proto.ColFixedStr // 20 bytes
	Taker       proto.ColFixedStr // 20 bytes
	Amount      proto.ColUInt256
}

// ExchangeOrderFilledView is the zero-copy view
type ExchangeOrderFilledView struct {
	Maker  common.Address
	Taker  common.Address
	Amount uint256.Int
}

// uint256FromProto converts proto.UInt256 to uint256.Int (zero-copy)
func uint256FromProto(p *proto.UInt256) uint256.Int {
	return uint256.Int{p.Low.Low, p.Low.High, p.High.Low, p.High.High}
}

// DirectAccess creates a view by directly accessing proto columns
func DirectAccess(b *ProtoEventBlock, i int) ExchangeOrderFilledView {
	return ExchangeOrderFilledView{
		Maker:  common.BytesToAddress(b.Maker.Row(i)),
		Taker:  common.BytesToAddress(b.Taker.Row(i)),
		Amount: uint256FromProto(&b.Amount[i]),
	}
}

// BenchmarkDirectProtoAccess measures the baseline: direct proto column access
func BenchmarkDirectProtoAccess(b *testing.B) {
	var block ProtoEventBlock
	block.Maker.SetSize(20)
	block.Taker.SetSize(20)
	// Populate with test data
	for i := 0; i < 1000; i++ {
		block.BlockNumber = append(block.BlockNumber, uint64(i))
		addr := make([]byte, 20)
		copy(addr, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i >> 8), byte(i)})
		block.Maker.Append(addr)
		block.Taker.Append(addr)
		block.Amount = append(block.Amount, proto.UInt256{})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			_ = DirectAccess(&block, j)
		}
	}
}

// BenchmarkViewCreation measures the overhead of creating view structs
func BenchmarkViewCreation(b *testing.B) {
	var block ProtoEventBlock
	block.Maker.SetSize(20)
	block.Taker.SetSize(20)
	for i := 0; i < 1000; i++ {
		block.BlockNumber = append(block.BlockNumber, uint64(i))
		addr := make([]byte, 20)
		copy(addr, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i >> 8), byte(i)})
		block.Maker.Append(addr)
		block.Taker.Append(addr)
		block.Amount = append(block.Amount, proto.UInt256{})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			view := ExchangeOrderFilledView{
				Maker:  common.BytesToAddress(block.Maker.Row(j)),
				Taker:  common.BytesToAddress(block.Taker.Row(j)),
				Amount: uint256FromProto(&block.Amount[j]),
			}
			_ = view
		}
	}
}

// BenchmarkProtoIteration measures iteration over proto columns
func BenchmarkProtoIteration(b *testing.B) {
	var block ProtoEventBlock
	block.Maker.SetSize(20)
	block.Taker.SetSize(20)
	for i := 0; i < 1000; i++ {
		addr := make([]byte, 20)
		copy(addr, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i >> 8), byte(i)})
		block.Maker.Append(addr)
		block.Taker.Append(addr)
		block.Amount = append(block.Amount, proto.UInt256{})
	}

	b.ResetTimer()
	sum := 0
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			maker := common.BytesToAddress(block.Maker.Row(j))
			taker := common.BytesToAddress(block.Taker.Row(j))
			if maker != taker {
				sum++
			}
		}
	}
	_ = sum
}

// BenchmarkViewIteration measures iteration with views
func BenchmarkViewIteration(b *testing.B) {
	var block ProtoEventBlock
	block.Maker.SetSize(20)
	block.Taker.SetSize(20)
	for i := 0; i < 1000; i++ {
		addr := make([]byte, 20)
		copy(addr, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i >> 8), byte(i)})
		block.Maker.Append(addr)
		block.Taker.Append(addr)
		block.Amount = append(block.Amount, proto.UInt256{})
	}

	b.ResetTimer()
	sum := 0
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			view := DirectAccess(&block, j)
			if view.Maker != view.Taker {
				sum++
			}
		}
	}
	_ = sum
}
