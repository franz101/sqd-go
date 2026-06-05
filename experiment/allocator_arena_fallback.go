//go:build !goexperiment.arenas

package experiment

type ArenaAllocator struct {
	StdAllocator
}

func NewArenaAllocator() *ArenaAllocator {
	return &ArenaAllocator{}
}

func (al *ArenaAllocator) Reset() {}
