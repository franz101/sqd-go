package experiment

import (
	"github.com/holiman/uint256"
)

type StdAllocator struct{}

func NewStdAllocator() *StdAllocator {
	return &StdAllocator{}
}

func (al *StdAllocator) MakeUint256Slice(length int) []uint256.Int {
	return make([]uint256.Int, length)
}

func (al *StdAllocator) MakeByteSlice(length int) []byte {
	return make([]byte, length)
}

func (al *StdAllocator) Reset() {}
