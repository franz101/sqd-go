package clock_test

import (
	"math/rand"
	"testing"
)

// Define enums for the three event types
const (
	TypeTransfer uint8 = iota
	TypeTokenCreated
	TypeBurn
)

type Address [20]byte

// --- Event Definitions (including BlockNumber, TxIndex, LogIndex) ---

type Transfer struct {
	BlockNumber uint64
	TxIndex     uint64
	LogIndex    uint64
	From        Address
	To          Address
	Amount      uint64
	
	// For Intrusive Linked List Strategy
	NextType  uint8
	NextIndex uint32
}

type TokenCreated struct {
	BlockNumber uint64
	TxIndex     uint64
	LogIndex    uint64
	Token       Address
	Creator     Address
	
	// For Intrusive Linked List Strategy
	NextType  uint8
	NextIndex uint32
}

type Burn struct {
	BlockNumber uint64
	TxIndex     uint64
	LogIndex    uint64
	From        Address
	Amount      uint64
	
	// For Intrusive Linked List Strategy
	NextType  uint8
	NextIndex uint32
}

// --- 1. Flat Tag Array (Tuple of Type and Index) ---

type EventRef struct {
	Type  uint8
	Index uint32
}

type FlatTagBlock struct {
	Transfers     []Transfer
	TokensCreated []TokenCreated
	Burns         []Burn
	Order         []EventRef
}

// --- 2. Struct of Arrays (SoA) + Shared Index ---

type SoABlock struct {
	Transfers     []Transfer
	TokensCreated []TokenCreated
	Burns         []Burn
	
	// Parallel arrays instead of slice of structs
	EventTypes    []uint8
	EventIndices  []uint32
}

// --- 3. Intrusive Linked List (embedded next pointers) ---

type IntrusiveBlock struct {
	Transfers     []Transfer
	TokensCreated []TokenCreated
	Burns         []Burn
	
	// Head of the chain
	HeadType  uint8
	HeadIndex uint32
}

// --- 4. Sequence Ledger (1-byte uint8 + running indices) ---

type SequenceLedgerBlock struct {
	Transfers     []Transfer
	TokensCreated []TokenCreated
	Burns         []Burn
	
	// Only 1 byte per event instead of 8 bytes
	Sequence      []uint8
}

// Setup realistic simulation data (5000 blocks, 200 events each)
const NumBlocks = 5000
const EventsPerBlock = 200

func setupBenchmarkData() ([]FlatTagBlock, []SoABlock, []IntrusiveBlock, []SequenceLedgerBlock) {
	rng := rand.New(rand.NewSource(42))
	
	flatBlocks := make([]FlatTagBlock, NumBlocks)
	soaBlocks := make([]SoABlock, NumBlocks)
	intrusiveBlocks := make([]IntrusiveBlock, NumBlocks)
	ledgerBlocks := make([]SequenceLedgerBlock, NumBlocks)

	for b := 0; b < NumBlocks; b++ {
		// Generate 200 randomized events
		var transfers []Transfer
		var tokens []TokenCreated
		var burns []Burn
		
		var flatOrder []EventRef
		var soaTypes []uint8
		var soaIndices []uint32
		var sequence []uint8

		blockNum := uint64(1000 + b)

		for e := 0; e < EventsPerBlock; e++ {
			txIdx := uint64(e / 2)
			logIdx := uint64(e)

			r := rng.Float64()
			if r < 0.80 {
				// Transfer
				tf := Transfer{
					BlockNumber: blockNum,
					TxIndex:     txIdx,
					LogIndex:    logIdx,
					Amount:      uint64(rng.Int63n(100000)),
				}
				transfers = append(transfers, tf)
				idx := uint32(len(transfers) - 1)
				
				flatOrder = append(flatOrder, EventRef{Type: TypeTransfer, Index: idx})
				soaTypes = append(soaTypes, TypeTransfer)
				soaIndices = append(soaIndices, idx)
				sequence = append(sequence, TypeTransfer)
			} else if r < 0.90 {
				// TokenCreated
				tc := TokenCreated{
					BlockNumber: blockNum,
					TxIndex:     txIdx,
					LogIndex:    logIdx,
				}
				tokens = append(tokens, tc)
				idx := uint32(len(tokens) - 1)
				
				flatOrder = append(flatOrder, EventRef{Type: TypeTokenCreated, Index: idx})
				soaTypes = append(soaTypes, TypeTokenCreated)
				soaIndices = append(soaIndices, idx)
				sequence = append(sequence, TypeTokenCreated)
			} else {
				// Burn
				br := Burn{
					BlockNumber: blockNum,
					TxIndex:     txIdx,
					LogIndex:    logIdx,
					Amount:      uint64(rng.Int63n(50000)),
				}
				burns = append(burns, br)
				idx := uint32(len(burns) - 1)
				
				flatOrder = append(flatOrder, EventRef{Type: TypeBurn, Index: idx})
				soaTypes = append(soaTypes, TypeBurn)
				soaIndices = append(soaIndices, idx)
				sequence = append(sequence, TypeBurn)
			}
		}

		// Save flat, SOA, and Ledger
		flatBlocks[b] = FlatTagBlock{
			Transfers:     transfers,
			TokensCreated: tokens,
			Burns:         burns,
			Order:         flatOrder,
		}

		soaBlocks[b] = SoABlock{
			Transfers:     transfers,
			TokensCreated: tokens,
			Burns:         burns,
			EventTypes:    soaTypes,
			EventIndices:  soaIndices,
		}

		ledgerBlocks[b] = SequenceLedgerBlock{
			Transfers:     transfers,
			TokensCreated: tokens,
			Burns:         burns,
			Sequence:      sequence,
		}

		// For Intrusive Linked List, we link the generated events chronologically
		intTransfers := make([]Transfer, len(transfers))
		copy(intTransfers, transfers)
		intTokens := make([]TokenCreated, len(tokens))
		copy(intTokens, tokens)
		intBurns := make([]Burn, len(burns))
		copy(intBurns, burns)

		if len(flatOrder) > 0 {
			// Set head
			intrusiveBlocks[b].HeadType = flatOrder[0].Type
			intrusiveBlocks[b].HeadIndex = flatOrder[0].Index

			// Link sequentially
			for i := 0; i < len(flatOrder)-1; i++ {
				curr := flatOrder[i]
				next := flatOrder[i+1]
				switch curr.Type {
				case TypeTransfer:
					intTransfers[curr.Index].NextType = next.Type
					intTransfers[curr.Index].NextIndex = next.Index
				case TypeTokenCreated:
					intTokens[curr.Index].NextType = next.Type
					intTokens[curr.Index].NextIndex = next.Index
				case TypeBurn:
					intBurns[curr.Index].NextType = next.Type
					intBurns[curr.Index].NextIndex = next.Index
				}
			}
			// Last element terminates (represented as Type 255)
			last := flatOrder[len(flatOrder)-1]
			switch last.Type {
			case TypeTransfer:
				intTransfers[last.Index].NextType = 255
			case TypeTokenCreated:
				intTokens[last.Index].NextType = 255
			case TypeBurn:
				intBurns[last.Index].NextType = 255
			}
		}

		intrusiveBlocks[b].Transfers = intTransfers
		intrusiveBlocks[b].TokensCreated = intTokens
		intrusiveBlocks[b].Burns = intBurns
	}

	return flatBlocks, soaBlocks, intrusiveBlocks, ledgerBlocks
}

func BenchmarkReconstructionFlatTag(b *testing.B) {
	flatBlocks, _, _, _ := setupBenchmarkData()
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sum uint64 = 0
		for blockIdx := 0; blockIdx < NumBlocks; blockIdx++ {
			block := &flatBlocks[blockIdx]
			for _, ref := range block.Order {
				switch ref.Type {
				case TypeTransfer:
					ev := &block.Transfers[ref.Index]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
				case TypeTokenCreated:
					ev := &block.TokensCreated[ref.Index]
					sum += ev.BlockNumber + ev.TxIndex + ev.LogIndex
				case TypeBurn:
					ev := &block.Burns[ref.Index]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
				}
			}
		}
		if sum == 0 {
			b.Fatal("unexpected sum")
		}
	}
}

func BenchmarkReconstructionSoA(b *testing.B) {
	_, soaBlocks, _, _ := setupBenchmarkData()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sum uint64 = 0
		for blockIdx := 0; blockIdx < NumBlocks; blockIdx++ {
			block := &soaBlocks[blockIdx]
			types := block.EventTypes
			indices := block.EventIndices
			for j := 0; j < len(types); j++ {
				t := types[j]
				idx := indices[j]
				switch t {
				case TypeTransfer:
					ev := &block.Transfers[idx]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
				case TypeTokenCreated:
					ev := &block.TokensCreated[idx]
					sum += ev.BlockNumber + ev.TxIndex + ev.LogIndex
				case TypeBurn:
					ev := &block.Burns[idx]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
				}
			}
		}
		if sum == 0 {
			b.Fatal("unexpected sum")
		}
	}
}

func BenchmarkReconstructionIntrusive(b *testing.B) {
	_, _, intrusiveBlocks, _ := setupBenchmarkData()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sum uint64 = 0
		for blockIdx := 0; blockIdx < NumBlocks; blockIdx++ {
			block := &intrusiveBlocks[blockIdx]
			
			currType := block.HeadType
			currIndex := block.HeadIndex
			
			for currType != 255 {
				switch currType {
				case TypeTransfer:
					ev := &block.Transfers[currIndex]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
					currType = ev.NextType
					currIndex = ev.NextIndex
				case TypeTokenCreated:
					ev := &block.TokensCreated[currIndex]
					sum += ev.BlockNumber + ev.TxIndex + ev.LogIndex
					currType = ev.NextType
					currIndex = ev.NextIndex
				case TypeBurn:
					ev := &block.Burns[currIndex]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
					currType = ev.NextType
					currIndex = ev.NextIndex
				default:
					currType = 255 // safety break
				}
			}
		}
		if sum == 0 {
			b.Fatal("unexpected sum")
		}
	}
}

func BenchmarkReconstructionSequenceLedger(b *testing.B) {
	_, _, _, ledgerBlocks := setupBenchmarkData()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sum uint64 = 0
		for blockIdx := 0; blockIdx < NumBlocks; blockIdx++ {
			block := &ledgerBlocks[blockIdx]
			var tIdx, cIdx, bIdx int
			
			for _, typ := range block.Sequence {
				switch typ {
				case TypeTransfer:
					ev := &block.Transfers[tIdx]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
					tIdx++
				case TypeTokenCreated:
					ev := &block.TokensCreated[cIdx]
					sum += ev.BlockNumber + ev.TxIndex + ev.LogIndex
					cIdx++
				case TypeBurn:
					ev := &block.Burns[bIdx]
					sum += ev.Amount + ev.BlockNumber + ev.TxIndex + ev.LogIndex
					bIdx++
				}
			}
		}
		if sum == 0 {
			b.Fatal("unexpected sum")
		}
	}
}
