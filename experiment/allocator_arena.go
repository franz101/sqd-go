//go:build goexperiment.arenas

package experiment

import (
	"arena"
	"github.com/holiman/uint256"
)

type ArenaAllocator struct {
	a *arena.Arena
}

func NewArenaAllocator() *ArenaAllocator {
	return &ArenaAllocator{}
}

func (al *ArenaAllocator) MakeUint256Slice(length int) []uint256.Int {
	if al.a == nil {
		al.a = arena.NewArena()
	}
	return arena.MakeSlice[uint256.Int](al.a, length, length)
}

func (al *ArenaAllocator) MakeByteSlice(length int) []byte {
	if al.a == nil {
		al.a = arena.NewArena()
	}
	return arena.MakeSlice[byte](al.a, length, length)
}

func (al *ArenaAllocator) Reset() {
	if al.a != nil {
		al.a.Free()
		al.a = nil
	}
}
