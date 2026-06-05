package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"runtime"
	"time"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/ingestion"
)

type blockNode struct {
	Ref    client.BlockRef
	Parent *blockNode
}

type Simulation struct {
	head          *blockNode
	finalized     *blockNode
	blocksByHash  map[string]*blockNode
	finalityLag   uint64
	totalBlocks   uint64
	maxForks      int
	forksOccurred int
	rng           *rand.Rand
}

func newSimulation(seed int64, finalityLag uint64, maxForks int) *Simulation {
	gen := rand.New(rand.NewSource(seed))
	genesis := &blockNode{
		Ref: client.BlockRef{Number: 0, Hash: "genesis"},
	}
	return &Simulation{
		head:         genesis,
		finalized:    genesis,
		blocksByHash: map[string]*blockNode{"genesis": genesis},
		finalityLag:  finalityLag,
		maxForks:     maxForks,
		rng:          gen,
	}
}

func (s *Simulation) hashFor(num uint64, parentHash string) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%d-%s-%d", num, parentHash, s.rng.Int63())))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Simulation) advance() *blockNode {
	nextNum := s.head.Ref.Number + 1
	nextHash := s.hashFor(nextNum, s.head.Ref.Hash)
	next := &blockNode{
		Ref:    client.BlockRef{Number: nextNum, Hash: nextHash},
		Parent: s.head,
	}
	s.head = next
	s.blocksByHash[nextHash] = next
	s.totalBlocks++

	// Advance finalized
	if s.head.Ref.Number >= s.finalityLag {
		targetFinal := s.head.Ref.Number - s.finalityLag
		curr := s.head
		for curr != nil && curr.Ref.Number > targetFinal {
			curr = curr.Parent
		}
		if curr != nil && curr.Ref.Number > s.finalized.Ref.Number {
			s.finalized = curr
		}
	}
	return next
}

func (s *Simulation) triggerFork(depth uint64) []client.BlockRef {
	if s.forksOccurred >= s.maxForks {
		return nil
	}
	if s.head.Ref.Number < depth {
		depth = s.head.Ref.Number
	}
	if depth == 0 {
		return nil
	}

	// Go back 'depth' blocks, but not before finalized
	forkBase := s.head
	for i := uint64(0); i < depth; i++ {
		if forkBase.Parent == nil || forkBase.Parent.Ref.Number <= s.finalized.Ref.Number {
			break
		}
		forkBase = forkBase.Parent
	}

	// Generate fork blocks
	s.head = forkBase
	for i := uint64(0); i < depth+1; i++ { // +1 to make it longer/different
		s.advance()
	}
	s.forksOccurred++

	// Return the "previousBlocks" that SQD would return on 409 Fork
	// SQD returns a few blocks from the actual chain.
	// Since we are simulating the perspective of the *client* receiving a fork error:
	// The client tried to fetch from `currentBlock` with parent `parentHash`.
	// SQD saw `parentHash` isn't on its canonical chain anymore.
	// So SQD returns the canonical chain's blocks around that height.
	// For our simulation, we just need to provide some valid canonical blocks to `HandleFork`.

	canonicalChain := make([]client.BlockRef, 0)
	curr := s.head
	for curr != nil && curr.Ref.Number >= forkBase.Ref.Number {
		canonicalChain = append([]client.BlockRef{curr.Ref}, canonicalChain...)
		curr = curr.Parent
	}
	return canonicalChain
}

func runBenchmark(name string, mode config.ForkMode, totalBlocks int, forkProbability float64, maxForkDepth uint64) {
	fmt.Printf("\n--- Running Benchmark: %s (%s) ---\n", name, mode)

	tracker := ingestion.NewForkTracker(mode)
	sim := newSimulation(42, 256, int(float64(totalBlocks)*forkProbability))

	// Init
	tracker.Init(&sim.head.Ref, &sim.finalized.Ref, nil)

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	start := time.Now()

	forksHandled := 0
	droppedForks := 0

	for i := 0; i < totalBlocks; i++ {
		if sim.rng.Float64() < forkProbability {
			depth := uint64(sim.rng.Int63n(int64(maxForkDepth))) + 1
			previousBlocks := sim.triggerFork(depth)
			if len(previousBlocks) > 0 {
				_, ok := tracker.HandleFork(previousBlocks)
				if !ok {
					droppedForks++
				} else {
					forksHandled++
				}
			}
		}

		sim.advance()

		// Simulate Batch apply
		// We apply 1 block at a time for max stress
		blocks := []client.BlockRef{sim.head.Ref}
		tracker.ApplyBatch(&sim.finalized.Ref, blocks)
	}

	duration := time.Since(start)
	runtime.ReadMemStats(&memAfter)

	allocBytes := memAfter.TotalAlloc - memBefore.TotalAlloc
	allocMegabytes := float64(allocBytes) / 1024 / 1024

	fmt.Printf("Total Blocks: %d\n", sim.totalBlocks)
	fmt.Printf("Forks Handled: %d (Dropped/Failed: %d)\n", forksHandled, droppedForks)
	fmt.Printf("Time Taken: %v\n", duration)
	fmt.Printf("Blocks/sec: %.2f\n", float64(sim.totalBlocks)/duration.Seconds())
	fmt.Printf("Memory Allocated: %.2f MB\n", allocMegabytes)
	fmt.Printf("Mallocs: %d\n", memAfter.Mallocs-memBefore.Mallocs)
}

func main() {
	blocks := 1_000_000
	prob := 0.005
	depth := uint64(30)

	runBenchmark("Default (Ring Buffer)", config.ForkModeDefault, blocks, prob, depth)
}
