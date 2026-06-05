package ecsbenchmark

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/unitoftime/ecs"
)

// ---------------------------------------------------------------------------
// Polymarket Component Types
// ---------------------------------------------------------------------------

type Market struct {
	MarketId     string
	Question     string
	OutcomeCount int32
	Volume       float64
	Liquidity    float64
	EndTime      int64
}

type Outcome struct {
	MarketId     string
	OutcomeIndex int32
	Price        float64 // 0..1
	Shares       float64
}

type Order struct {
	MarketId     string
	OutcomeIndex int32
	Price        float64
	Size         float64
	Side         int8   // 0=buy, 1=sell
	Timestamp    int64
	Owner        uint64
}

type Position struct {
	MarketId     string
	OutcomeIndex int32
	Shares       float64
	AvgPrice     float64
	Owner        uint64
}

type Trade struct {
	MarketId     string
	OutcomeIndex int32
	Price        float64
	Size         float64
	Timestamp    int64
}

// ---------------------------------------------------------------------------
// World setup: 1,000,000 entities
//
//   800,000  Market+Outcome entities  (100k markets x 8 outcomes each)
//   100,000  Order entities
//   100,000  Position entities
//   -------------------------------
//   1,000,000 total
// ---------------------------------------------------------------------------

const (
	numMarkets     = 100_000
	outcomesPerMkt = 8
	numOutcomes    = numMarkets * outcomesPerMkt // 800_000
	numOrders      = 100_000
	numPositions   = 100_000
	totalEntities  = numOutcomes + numOrders + numPositions // 1_000_000
)

func makeMarketId(mktIdx int) string {
	return fmt.Sprintf("mkt-%06d", mktIdx)
}

func setupWorld() *ecs.World {
	world := ecs.NewWorld()
	rng := rand.New(rand.NewSource(42))

	// ---- 800k Market+Outcome entities ----
	for i := 0; i < numMarkets; i++ {
		marketId := makeMarketId(i)
		mkt := Market{
			MarketId:     marketId,
			Question:     fmt.Sprintf("Will event %d happen?", i),
			OutcomeCount: outcomesPerMkt,
			Volume:       rng.Float64() * 1_000_000,
			Liquidity:    rng.Float64() * 500_000,
			EndTime:      1_700_000_000 + int64(i%1000),
		}
		for j := int32(0); j < outcomesPerMkt; j++ {
			id := world.NewId()
			ecs.Write(world, id,
				ecs.C(mkt),
				ecs.C(Outcome{
					MarketId:     marketId,
					OutcomeIndex: j,
					Price:        rng.Float64(),
					Shares:       rng.Float64() * 100_000,
				}),
			)
		}
	}

	// ---- 100k Order entities ----
	for i := 0; i < numOrders; i++ {
		id := world.NewId()
		marketIdx := rng.Intn(numMarkets)
		outcomeIdx := int32(rng.Intn(outcomesPerMkt))
		ecs.Write(world, id,
			ecs.C(Order{
				MarketId:     makeMarketId(marketIdx),
				OutcomeIndex: outcomeIdx,
				Price:        rng.Float64(),
				Size:         rng.Float64() * 10_000,
				Side:         int8(rng.Intn(2)),
				Timestamp:    1_700_000_000 + rng.Int63n(86_400),
				Owner:        rng.Uint64(),
			}),
		)
	}

	// ---- 100k Position entities ----
	for i := 0; i < numPositions; i++ {
		id := world.NewId()
		marketIdx := rng.Intn(numMarkets)
		outcomeIdx := int32(rng.Intn(outcomesPerMkt))
		ecs.Write(world, id,
			ecs.C(Position{
				MarketId:     makeMarketId(marketIdx),
				OutcomeIndex: outcomeIdx,
				Shares:       rng.Float64() * 50_000,
				AvgPrice:     rng.Float64(),
				Owner:        rng.Uint64(),
			}),
		)
	}

	return world
}

// ---------------------------------------------------------------------------
// System logic
// ---------------------------------------------------------------------------

// PriceUpdateSystem: for each outcome, nudge price based on market volume vs liquidity.
func priceUpdate(id ecs.Id, mkt *Market, out *Outcome) {
	// Simple adjustment: drift price toward 0.5 scaled by volume/liquidity ratio
	if mkt.Liquidity > 0 {
		drift := 0.5 - out.Price
		ratio := mkt.Volume / mkt.Liquidity
		out.Price += drift * ratio * 0.001
		if out.Price > 1.0 {
			out.Price = 1.0
		}
		if out.Price < 0.0 {
			out.Price = 0.0
		}
	}
}

// OrderMatchingSystem: for each order, accumulate "matched" volume into a running counter.
// In a real system you'd cross orders; here we just do a cheap read-modify.
func orderMatch(id ecs.Id, ord *Order) {
	// Simulate matching: round price to 4 decimals (cheap computation)
	ord.Price = float64(int(ord.Price*10000)) / 10000
	// Flip side occasionally to simulate churn (deterministic for benchmark stability)
	if ord.Size > 5000 {
		ord.Side = 1 - ord.Side
	}
}

// PositionSettlementSystem: mark-to-market each position against a fixed mark price.
func positionSettle(id ecs.Id, pos *Position) {
	const markPrice = 0.55
	pnl := (markPrice - pos.AvgPrice) * pos.Shares
	pos.Shares += pnl * 0.0001 // tiny adjustment
	if pos.Shares < 0 {
		pos.Shares = 0
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: PriceUpdateSystem  (Query2: Market + Outcome → 800k entities)
// ---------------------------------------------------------------------------

func BenchmarkPriceUpdate_MapId(b *testing.B) {
	world := setupWorld()
	query := ecs.Query2[Market, Outcome](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapId(priceUpdate)
	}
}

func BenchmarkPriceUpdate_MapSlices(b *testing.B) {
	world := setupWorld()
	query := ecs.Query2[Market, Outcome](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapSlices(func(ids []ecs.Id, mkts []Market, outs []Outcome) {
			for i := range ids {
				if ids[i] == ecs.InvalidEntity {
					continue
				}
				priceUpdate(ids[i], &mkts[i], &outs[i])
			}
		})
	}
}

func BenchmarkPriceUpdate_MapIdParallel(b *testing.B) {
	world := setupWorld()
	query := ecs.Query2[Market, Outcome](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapIdParallel(priceUpdate)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: OrderMatchingSystem  (Query1: Order → 100k entities)
// ---------------------------------------------------------------------------

func BenchmarkOrderMatching_MapId(b *testing.B) {
	world := setupWorld()
	query := ecs.Query1[Order](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapId(orderMatch)
	}
}

func BenchmarkOrderMatching_MapSlices(b *testing.B) {
	world := setupWorld()
	query := ecs.Query1[Order](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapSlices(func(ids []ecs.Id, ords []Order) {
			for i := range ids {
				if ids[i] == ecs.InvalidEntity {
					continue
				}
				orderMatch(ids[i], &ords[i])
			}
		})
	}
}

func BenchmarkOrderMatching_MapIdParallel(b *testing.B) {
	world := setupWorld()
	query := ecs.Query1[Order](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapIdParallel(orderMatch)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: PositionSettlementSystem  (Query1: Position → 100k entities)
// ---------------------------------------------------------------------------

func BenchmarkPositionSettlement_MapId(b *testing.B) {
	world := setupWorld()
	query := ecs.Query1[Position](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapId(positionSettle)
	}
}

func BenchmarkPositionSettlement_MapSlices(b *testing.B) {
	world := setupWorld()
	query := ecs.Query1[Position](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapSlices(func(ids []ecs.Id, poss []Position) {
			for i := range ids {
				if ids[i] == ecs.InvalidEntity {
					continue
				}
				positionSettle(ids[i], &poss[i])
			}
		})
	}
}

func BenchmarkPositionSettlement_MapIdParallel(b *testing.B) {
	world := setupWorld()
	query := ecs.Query1[Position](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		query.MapIdParallel(positionSettle)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: All three systems together
// ---------------------------------------------------------------------------

func BenchmarkAllSystems_MapId(b *testing.B) {
	world := setupWorld()
	qPrice := ecs.Query2[Market, Outcome](world)
	qOrder := ecs.Query1[Order](world)
	qPos := ecs.Query1[Position](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		qPrice.MapId(priceUpdate)
		qOrder.MapId(orderMatch)
		qPos.MapId(positionSettle)
	}
}

func BenchmarkAllSystems_MapSlices(b *testing.B) {
	world := setupWorld()
	qPrice := ecs.Query2[Market, Outcome](world)
	qOrder := ecs.Query1[Order](world)
	qPos := ecs.Query1[Position](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		qPrice.MapSlices(func(ids []ecs.Id, mkts []Market, outs []Outcome) {
			for i := range ids {
				if ids[i] == ecs.InvalidEntity {
					continue
				}
				priceUpdate(ids[i], &mkts[i], &outs[i])
			}
		})
		qOrder.MapSlices(func(ids []ecs.Id, ords []Order) {
			for i := range ids {
				if ids[i] == ecs.InvalidEntity {
					continue
				}
				orderMatch(ids[i], &ords[i])
			}
		})
		qPos.MapSlices(func(ids []ecs.Id, poss []Position) {
			for i := range ids {
				if ids[i] == ecs.InvalidEntity {
					continue
				}
				positionSettle(ids[i], &poss[i])
			}
		})
	}
}

func BenchmarkAllSystems_MapIdParallel(b *testing.B) {
	world := setupWorld()
	qPrice := ecs.Query2[Market, Outcome](world)
	qOrder := ecs.Query1[Order](world)
	qPos := ecs.Query1[Position](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		qPrice.MapIdParallel(priceUpdate)
		qOrder.MapIdParallel(orderMatch)
		qPos.MapIdParallel(positionSettle)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: Entity creation  (spawn 100k Markets + 800k Outcomes)
// ---------------------------------------------------------------------------

func BenchmarkSpawnMarketsAndOutcomes(b *testing.B) {
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		world := ecs.NewWorld()
		rng := rand.New(rand.NewSource(42))
		for i := 0; i < numMarkets; i++ {
			marketId := makeMarketId(i)
			mkt := Market{
				MarketId:     marketId,
				Question:     fmt.Sprintf("Will event %d happen?", i),
				OutcomeCount: outcomesPerMkt,
				Volume:       rng.Float64() * 1_000_000,
				Liquidity:    rng.Float64() * 500_000,
				EndTime:      1_700_000_000 + int64(i%1000),
			}
			for j := int32(0); j < outcomesPerMkt; j++ {
				id := world.NewId()
				ecs.Write(world, id,
					ecs.C(mkt),
					ecs.C(Outcome{
						MarketId:     marketId,
						OutcomeIndex: j,
						Price:        rng.Float64(),
						Shares:       rng.Float64() * 100_000,
					}),
				)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: Entity deletion  (delete 10% of orders)
// ---------------------------------------------------------------------------

func BenchmarkDeleteOrders(b *testing.B) {
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		world := setupWorld()
		query := ecs.Query1[Order](world)
		b.StopTimer()
		// Collect 10% of order ids ahead of time
		var toDelete []ecs.Id
		count := 0
		query.MapId(func(id ecs.Id, ord *Order) {
			if count%10 == 0 {
				toDelete = append(toDelete, id)
			}
			count++
		})
		b.StartTimer()

		for _, id := range toDelete {
			ecs.Delete(world, id)
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: Count entities (sanity check)
// ---------------------------------------------------------------------------

func BenchmarkCountEntities(b *testing.B) {
	world := setupWorld()
	qMO := ecs.Query2[Market, Outcome](world)
	qOrd := ecs.Query1[Order](world)
	qPos := ecs.Query1[Position](world)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = qMO.Count()
		_ = qOrd.Count()
		_ = qPos.Count()
	}
}
