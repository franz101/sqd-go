package ecsbenchmark

import (
	"fmt"
	"math/rand"
	"runtime"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/shopspring/decimal"

	"github.com/franz101/sqd-go/drafts/protomath"
)

// ---------------------------------------------------------------------------
// Struct Definitions & Layouts
// ---------------------------------------------------------------------------

// Mutation represents a pointer-free rollback delta inside the MutationRing.
type Mutation struct {
	EntityID       uint64
	RowIdx         int
	OldAmount      protomath.Decimal256
	OldTotalBought protomath.Decimal256
	OldAvgPrice    protomath.Decimal256
	NewAmount      protomath.Decimal256
	NewTotalBought protomath.Decimal256
	NewAvgPrice    protomath.Decimal256
}

// MutationRing is a circular, pre-allocated, pointer-free rollback log.
type MutationRing struct {
	data []Mutation
	head int
	mask int // size - 1 (must be power of 2)
}

func NewMutationRing(size int) *MutationRing {
	// Round to next power of 2
	p2 := 1
	for p2 < size {
		p2 <<= 1
	}
	return &MutationRing{
		data: make([]Mutation, p2),
		head: 0,
		mask: p2 - 1,
	}
}

func (r *MutationRing) Record(m Mutation) {
	r.data[r.head] = m
	r.head = (r.head + 1) & r.mask
}

// Rollback restores state from the ring buffer for the last N steps.
func (r *MutationRing) Rollback(steps int, s *protoPositionStore) {
	for i := 0; i < steps; i++ {
		r.head = (r.head - 1) & r.mask
		m := r.data[r.head]
		s.amount[m.RowIdx] = m.OldAmount
		s.totalBought[m.RowIdx] = m.OldTotalBought
		s.avgPrice[m.RowIdx] = m.OldAvgPrice
	}
}

// RollbackTo restores state until the head of the ring buffer matches the targetHead index.
func (r *MutationRing) RollbackTo(targetHead int, s *protoPositionStore) {
	for r.head != targetHead {
		r.head = (r.head - 1) & r.mask
		m := r.data[r.head]
		s.amount[m.RowIdx] = m.OldAmount
		s.totalBought[m.RowIdx] = m.OldTotalBought
		s.avgPrice[m.RowIdx] = m.OldAvgPrice
	}
}

// protoPositionStore represents the pointer-free ECS/SoA store.
type protoPositionStore struct {
	index       map[uint64]int
	entityID    []uint64
	amount      []protomath.Decimal256
	totalBought []protomath.Decimal256
	avgPrice    []protomath.Decimal256

	// Backing columns for zero-copy streaming
	colEntityID    proto.ColUInt64
	colAmount      proto.ColDecimal256
	colTotalBought proto.ColDecimal256
	colAvgPrice    proto.ColDecimal256
}

func newProtoPositionStore(rows int) *protoPositionStore {
	scale := protomath.Decimal256Scale18
	store := &protoPositionStore{
		index:          make(map[uint64]int, rows),
		entityID:       make([]uint64, rows),
		amount:         make([]protomath.Decimal256, rows),
		totalBought:    make([]protomath.Decimal256, rows),
		avgPrice:       make([]protomath.Decimal256, rows),
		colEntityID:    make(proto.ColUInt64, 0, rows),
		colAmount:      make(proto.ColDecimal256, 0, rows),
		colTotalBought: make(proto.ColDecimal256, 0, rows),
		colAvgPrice:    make(proto.ColDecimal256, 0, rows),
	}

	for i := 0; i < rows; i++ {
		id := uint64(i + 1)
		store.index[id] = i
		store.entityID[i] = id
		amt, _ := protomath.FromInt64(int64(100+i%100), scale)
		price := protomath.FromScaledInt64(500_000_000_000_000_000 + int64(i%500)*1_000_000_000_000_000)
		total, _ := amt.Mul(price, scale)

		store.amount[i] = amt
		store.avgPrice[i] = price
		store.totalBought[i] = total
	}
	return store
}

// applyEvent processes a single event and records the mutation.
func (s *protoPositionStore) applyEvent(entityID uint64, delta, price protomath.Decimal256, scale protomath.Decimal256Scale, ring *MutationRing) {
	idx, exists := s.index[entityID]
	if !exists {
		// Out of scope for benchmark, we only update existing entities
		return
	}

	oldAmt := s.amount[idx]
	oldTotal := s.totalBought[idx]
	oldAvg := s.avgPrice[idx]

	newAmt, _ := oldAmt.Add(delta)
	bought, _ := delta.Mul(price, scale)
	newTotal, _ := oldTotal.Add(bought)
	newAvg, _ := newTotal.Div(newAmt, scale)

	s.amount[idx] = newAmt
	s.totalBought[idx] = newTotal
	s.avgPrice[idx] = newAvg

	if ring != nil {
		ring.Record(Mutation{
			EntityID:       entityID,
			RowIdx:         idx,
			OldAmount:      oldAmt,
			OldTotalBought: oldTotal,
			OldAvgPrice:    oldAvg,
			NewAmount:      newAmt,
			NewTotalBought: newTotal,
			NewAvgPrice:    newAvg,
		})
	}
}

// buildInsertInput packs the ECS slices into proto.Input columns.
func (s *protoPositionStore) buildInsertInput() proto.Input {
	s.colEntityID.Reset()
	s.colAmount.Reset()
	s.colTotalBought.Reset()
	s.colAvgPrice.Reset()

	for i, entityID := range s.entityID {
		s.colEntityID.Append(entityID)
		s.colAmount.Append(s.amount[i].Proto())
		s.colTotalBought.Append(s.totalBought[i].Proto())
		s.colAvgPrice.Append(s.avgPrice[i].Proto())
	}

	return proto.Input{
		{Name: "entity_id", Data: &s.colEntityID},
		{Name: "amount", Data: &s.colAmount},
		{Name: "total_bought", Data: &s.colTotalBought},
		{Name: "avg_price", Data: &s.colAvgPrice},
	}
}

// ---------------------------------------------------------------------------
// Shopspring decimal structure (pointer-heavy reference)
// ---------------------------------------------------------------------------

type shopPosition struct {
	entityID    uint64
	amount      decimal.Decimal
	totalBought decimal.Decimal
	avgPrice    decimal.Decimal
}

type shopPositionStore struct {
	index map[uint64]*shopPosition
	list  []*shopPosition

	colEntityID    proto.ColUInt64
	colAmount      proto.ColDecimal256
	colTotalBought proto.ColDecimal256
	colAvgPrice    proto.ColDecimal256
}

func newShopPositionStore(rows int) *shopPositionStore {
	store := &shopPositionStore{
		index:          make(map[uint64]*shopPosition, rows),
		list:           make([]*shopPosition, rows),
		colEntityID:    make(proto.ColUInt64, 0, rows),
		colAmount:      make(proto.ColDecimal256, 0, rows),
		colTotalBought: make(proto.ColDecimal256, 0, rows),
		colAvgPrice:    make(proto.ColDecimal256, 0, rows),
	}

	for i := 0; i < rows; i++ {
		id := uint64(i + 1)
		pos := &shopPosition{
			entityID:    id,
			amount:      decimal.NewFromInt(int64(100 + i%100)),
			avgPrice:    decimal.New(500_000_000_000_000_000+int64(i%500)*1_000_000_000_000_000, -18),
			totalBought: decimal.NewFromInt(int64(100 + i%100)).Mul(decimal.New(500_000_000_000_000_000+int64(i%500)*1_000_000_000_000_000, -18)),
		}
		store.index[id] = pos
		store.list[i] = pos
	}
	return store
}

func (s *shopPositionStore) applyEvent(entityID uint64, delta, price decimal.Decimal) {
	pos, exists := s.index[entityID]
	if !exists {
		return
	}

	pos.amount = pos.amount.Add(delta)
	bought := delta.Mul(price)
	pos.totalBought = pos.totalBought.Add(bought)
	pos.avgPrice = pos.totalBought.Div(pos.amount)
}

func (s *shopPositionStore) buildInsertInput() proto.Input {
	s.colEntityID.Reset()
	s.colAmount.Reset()
	s.colTotalBought.Reset()
	s.colAvgPrice.Reset()

	multiplier := decimal.New(1, 18)

	for _, pos := range s.list {
		s.colEntityID.Append(pos.entityID)

		// Requires conversion from big.Int coefficients to little-endian bytes
		coefAmount := pos.amount.Mul(multiplier).BigInt()
		rawAmount, _ := protomath.FromDecimal256ScaledBigInt(coefAmount)
		s.colAmount.Append(rawAmount.Proto())

		coefTotal := pos.totalBought.Mul(multiplier).BigInt()
		rawTotal, _ := protomath.FromDecimal256ScaledBigInt(coefTotal)
		s.colTotalBought.Append(rawTotal.Proto())

		coefAvg := pos.avgPrice.Mul(multiplier).BigInt()
		rawAvg, _ := protomath.FromDecimal256ScaledBigInt(coefAvg)
		s.colAvgPrice.Append(rawAvg.Proto())
	}

	return proto.Input{
		{Name: "entity_id", Data: &s.colEntityID},
		{Name: "amount", Data: &s.colAmount},
		{Name: "total_bought", Data: &s.colTotalBought},
		{Name: "avg_price", Data: &s.colAvgPrice},
	}
}

// ---------------------------------------------------------------------------
// E2E Simulation Test & Estimations
// ---------------------------------------------------------------------------

func TestRunSimulation(t *testing.T) {
	const (
		numPositions = 100_000
		numEvents    = 1_000_000 // 1 Million events inside simulation
	)

	fmt.Println("=========================================================================")
	fmt.Printf("Polymarket 50GB Scale Simulation Test (%d Positions, %d Events)\n", numPositions, numEvents)
	fmt.Println("=========================================================================")

	// 1. Generate events
	rng := rand.New(rand.NewSource(42))
	eventUserIDs := make([]uint64, numEvents)
	for i := 0; i < numEvents; i++ {
		eventUserIDs[i] = uint64(rng.Intn(numPositions) + 1)
	}

	// For ProtoMath model
	protoDeltas := make([]protomath.Decimal256, numEvents)
	protoPrices := make([]protomath.Decimal256, numEvents)
	for i := 0; i < numEvents; i++ {
		protoDeltas[i] = protomath.FromScaledInt64(10_000_000_000_000_000 + int64(rng.Intn(10))*1_000_000_000_000_000)
		protoPrices[i] = protomath.FromScaledInt64(450_000_000_000_000_000 + int64(rng.Intn(200))*1_000_000_000_000_000)
	}

	// For Shopspring model
	shopDeltas := make([]decimal.Decimal, numEvents)
	shopPrices := make([]decimal.Decimal, numEvents)
	for i := 0; i < numEvents; i++ {
		shopDeltas[i] = decimal.New(10_000_000_000_000_000+int64(rng.Intn(10))*1_000_000_000_000_000, -18)
		shopPrices[i] = decimal.New(450_000_000_000_000_000+int64(rng.Intn(200))*1_000_000_000_000_000, -18)
	}

	// Force garbage collection before running Shopspring
	runtime.GC()
	var msStart runtime.MemStats
	runtime.ReadMemStats(&msStart)

	// ----- Shopspring run -----
	shopStore := newShopPositionStore(numPositions)
	startShop := time.Now()
	for i := 0; i < numEvents; i++ {
		shopStore.applyEvent(eventUserIDs[i], shopDeltas[i], shopPrices[i])
	}
	shopStore.buildInsertInput()
	durShop := time.Since(startShop)

	var msEndShop runtime.MemStats
	runtime.ReadMemStats(&msEndShop)
	allocsShop := msEndShop.TotalAlloc - msStart.TotalAlloc
	sysShop := msEndShop.Sys

	// Force GC before running ProtoMath
	runtime.GC()
	runtime.ReadMemStats(&msStart)

	// ----- ProtoMath ECS run -----
	protoStore := newProtoPositionStore(numPositions)
	ring := NewMutationRing(100_000) // Preallocated rollback log
	scale := protomath.Decimal256Scale18

	startProto := time.Now()
	for i := 0; i < numEvents; i++ {
		protoStore.applyEvent(eventUserIDs[i], protoDeltas[i], protoPrices[i], scale, ring)
	}
	protoStore.buildInsertInput()
	durProto := time.Since(startProto)

	var msEndProto runtime.MemStats
	runtime.ReadMemStats(&msEndProto)
	allocsProto := msEndProto.TotalAlloc - msStart.TotalAlloc
	sysProto := msEndProto.Sys

	// Print results
	tpsShop := float64(numEvents) / durShop.Seconds()
	tpsProto := float64(numEvents) / durProto.Seconds()

	fmt.Printf("%-24s %-12s %-15s %-15s\n", "Metric", "Shopspring", "ProtoMath ECS", "Improvement")
	fmt.Println("-------------------------------------------------------------------------")
	fmt.Printf("%-24s %-12s %-15s %-15.1fx\n", "Duration", durShop.String(), durProto.String(), durShop.Seconds()/durProto.Seconds())
	fmt.Printf("%-24s %-12.0f %-15.0f %-15.1fx\n", "Throughput (Events/sec)", tpsShop, tpsProto, tpsProto/tpsShop)
	fmt.Printf("%-24s %-12.2f MB %-15.2f MB %-15.1fx (less)\n", "Mem Allocated", float64(allocsShop)/1024/1024, float64(allocsProto)/1024/1024, float64(allocsShop)/float64(allocsProto))
	fmt.Printf("%-24s %-12.2f MB %-15.2f MB %-15.2f MB (saved)\n", "Sys Memory Ceiling", float64(sysShop)/1024/1024, float64(sysProto)/1024/1024, float64(sysShop-sysProto)/1024/1024)

	// Extrapolate to 50 GB workload (250 Million events)
	const scale50GB = 250.0 // 250M events is 250x 1M events
	estTimeShop := durShop.Seconds() * scale50GB
	estTimeProto := durProto.Seconds() * scale50GB
	estAllocShop := float64(allocsShop) * scale50GB / 1024 / 1024 / 1024
	estAllocProto := float64(allocsProto) * scale50GB / 1024 / 1024 / 1024

	fmt.Println("\n=========================================================================")
	fmt.Println("Extrapolated 50GB Workload Metrics (250 Million events)")
	fmt.Println("=========================================================================")
	fmt.Printf("Est. Duration (Shopspring):      %.1fs (%.2f hours)\n", estTimeShop, estTimeShop/3600.0)
	fmt.Printf("Est. Duration (ProtoMath ECS):   %.1fs (%.2f minutes)\n", estTimeProto, estTimeProto/60.0)
	fmt.Printf("Est. Allocations (Shopspring):   %.2f GB\n", estAllocShop)
	fmt.Printf("Est. Allocations (ProtoMath ECS):%.2f GB\n", estAllocProto)
	fmt.Printf("Est. Garbage Saved:              %.2f GB of heap churn avoided\n", estAllocShop-estAllocProto)
	fmt.Println("=========================================================================")
}

// ---------------------------------------------------------------------------
// Standard Go Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkSimulation_ProtoMathECS(b *testing.B) {
	const numPositions = 100_000
	const numEvents = 200_000

	rng := rand.New(rand.NewSource(42))
	eventUserIDs := make([]uint64, numEvents)
	for i := 0; i < numEvents; i++ {
		eventUserIDs[i] = uint64(rng.Intn(numPositions) + 1)
	}

	protoDeltas := make([]protomath.Decimal256, numEvents)
	protoPrices := make([]protomath.Decimal256, numEvents)
	for i := 0; i < numEvents; i++ {
		protoDeltas[i] = protomath.FromScaledInt64(10_000_000_000_000_000 + int64(rng.Intn(10))*1_000_000_000_000_000)
		protoPrices[i] = protomath.FromScaledInt64(450_000_000_000_000_000 + int64(rng.Intn(200))*1_000_000_000_000_000)
	}

	protoStore := newProtoPositionStore(numPositions)
	ring := NewMutationRing(100_000)
	scale := protomath.Decimal256Scale18

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := 0; i < numEvents; i++ {
			protoStore.applyEvent(eventUserIDs[i], protoDeltas[i], protoPrices[i], scale, ring)
		}
		_ = protoStore.buildInsertInput()
	}
}

func BenchmarkSimulation_ShopspringDecimal(b *testing.B) {
	const numPositions = 100_000
	const numEvents = 200_000

	rng := rand.New(rand.NewSource(42))
	eventUserIDs := make([]uint64, numEvents)
	for i := 0; i < numEvents; i++ {
		eventUserIDs[i] = uint64(rng.Intn(numPositions) + 1)
	}

	shopDeltas := make([]decimal.Decimal, numEvents)
	shopPrices := make([]decimal.Decimal, numEvents)
	for i := 0; i < numEvents; i++ {
		shopDeltas[i] = decimal.New(10_000_000_000_000_000+int64(rng.Intn(10))*1_000_000_000_000_000, -18)
		shopPrices[i] = decimal.New(450_000_000_000_000_000+int64(rng.Intn(200))*1_000_000_000_000_000, -18)
	}

	shopStore := newShopPositionStore(numPositions)

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := 0; i < numEvents; i++ {
			shopStore.applyEvent(eventUserIDs[i], shopDeltas[i], shopPrices[i])
		}
		_ = shopStore.buildInsertInput()
	}
}

func TestBlockRollback(t *testing.T) {
	const (
		numPositions = 1000
		numBlocks    = 5000
		txsPerBlock  = 4
	)

	fmt.Println("=========================================================================")
	fmt.Printf("Simulating 5,000 Blocks Rollback (Total Transactions: %d)\n", numBlocks*txsPerBlock)
	fmt.Println("=========================================================================")

	store := newProtoPositionStore(numPositions)
	// preallocate mutation ring to hold all mutations for 5000 blocks
	// 5000 blocks * 4 tx/block = 20,000 mutations
	ring := NewMutationRing(numBlocks * txsPerBlock)
	scale := protomath.Decimal256Scale18

	// Track the head index at the start of each block
	var blockHeads [numBlocks]int

	rng := rand.New(rand.NewSource(1337))

	// Pre-generate transactions
	eventUserIDs := make([]uint64, numBlocks*txsPerBlock)
	eventDeltas := make([]protomath.Decimal256, numBlocks*txsPerBlock)
	eventPrices := make([]protomath.Decimal256, numBlocks*txsPerBlock)
	for i := 0; i < numBlocks*txsPerBlock; i++ {
		eventUserIDs[i] = uint64(rng.Intn(numPositions) + 1)
		eventDeltas[i] = protomath.FromScaledInt64(5_000_000_000_000_000 + int64(rng.Intn(5))*1_000_000_000_000_000)
		eventPrices[i] = protomath.FromScaledInt64(450_000_000_000_000_000 + int64(rng.Intn(100))*1_000_000_000_000_000)
	}

	// 1. Process 5000 blocks
	var targetAmount [numPositions]protomath.Decimal256
	var targetTotal [numPositions]protomath.Decimal256
	var targetAvg [numPositions]protomath.Decimal256

	for bIdx := 0; bIdx < numBlocks; bIdx++ {
		// Record start of block
		blockHeads[bIdx] = ring.head

		// At block 4500 (meaning we want to roll back the last 500 blocks),
		// we capture the exact position state of all entities.
		if bIdx == 4500 {
			for i := 0; i < numPositions; i++ {
				targetAmount[i] = store.amount[i]
				targetTotal[i] = store.totalBought[i]
				targetAvg[i] = store.avgPrice[i]
			}
		}

		// Apply transactions in this block
		for tx := 0; tx < txsPerBlock; tx++ {
			idx := bIdx*txsPerBlock + tx
			store.applyEvent(eventUserIDs[idx], eventDeltas[idx], eventPrices[idx], scale, ring)
		}
	}

	// Verify that state at block 5000 is different from target state at block 4500
	modifiedCount := 0
	for i := 0; i < numPositions; i++ {
		if store.amount[i].Proto() != targetAmount[i].Proto() {
			modifiedCount++
		}
	}
	t.Logf("Number of positions modified during the last 500 blocks: %d\n", modifiedCount)
	if modifiedCount == 0 {
		t.Fatal("Expected some positions to be modified during the last 500 blocks")
	}

	// 2. Perform the rollback to block 4500 (500 blocks)
	startRollback := time.Now()
	runtime.GC()
	var memStart runtime.MemStats
	runtime.ReadMemStats(&memStart)

	targetHead := blockHeads[4500]
	ring.RollbackTo(targetHead, store)

	var memEnd runtime.MemStats
	runtime.ReadMemStats(&memEnd)
	durRollback := time.Since(startRollback)

	// 3. Assert target state is exactly restored
	for i := 0; i < numPositions; i++ {
		if store.amount[i].Proto() != targetAmount[i].Proto() {
			t.Errorf("Position %d amount mismatch: got %v, expected %v", i, store.amount[i], targetAmount[i])
		}
		if store.totalBought[i].Proto() != targetTotal[i].Proto() {
			t.Errorf("Position %d totalBought mismatch: got %v, expected %v", i, store.totalBought[i], targetTotal[i])
		}
		if store.avgPrice[i].Proto() != targetAvg[i].Proto() {
			t.Errorf("Position %d avgPrice mismatch: got %v, expected %v", i, store.avgPrice[i], targetAvg[i])
		}
	}

	allocBytes := memEnd.TotalAlloc - memStart.TotalAlloc
	fmt.Printf("Rollback of 500 blocks (2,000 transactions) completed successfully!\n")
	fmt.Printf("Duration:         %s\n", durRollback)
	fmt.Printf("Memory Allocated: %d bytes (completely allocation-free!)\n", allocBytes)
	fmt.Println("=========================================================================")
}

