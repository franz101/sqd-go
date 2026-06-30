package polymarket

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
)

func BenchmarkCollectionIDCacheHit(b *testing.B) {
	previous := collectionCache
	collectionCache = newClockCache[collectionKey, common.Hash](maxCryptoCacheLen, 64, hashCollectionKey)
	b.Cleanup(func() { collectionCache = previous })

	condition := common.HexToHash("0x8571597013046477495fe6fb5c9d9bfb97791ce9310d8dae5f79cdb0676faf33")
	want := getCollectionIDForOutcome(common.Hash{}, condition, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := getCollectionIDForOutcome(common.Hash{}, condition, 0); got != want {
			b.Fatalf("collection ID changed: got %s, want %s", got, want)
		}
	}
}

func BenchmarkCollectionIDUniqueMiss(b *testing.B) {
	previous := collectionCache
	collectionCache = newClockCache[collectionKey, common.Hash](maxCryptoCacheLen, 64, hashCollectionKey)
	b.Cleanup(func() { collectionCache = previous })

	var condition common.Hash
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		binary.BigEndian.PutUint64(condition[:8], uint64(i+1))
		binary.BigEndian.PutUint64(condition[24:], uint64(i+1))
		_ = getCollectionIDForOutcome(common.Hash{}, condition, uint8(i&1))
	}
}

func BenchmarkCollectionIDMissImplementation(b *testing.B) {
	b.Run("legacy", func(b *testing.B) {
		var condition common.Hash
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			binary.BigEndian.PutUint64(condition[24:], uint64(i+1))
			_ = computeCollectionIDLegacy(common.Hash{}, condition, collectionIndexWords[i&1])
		}
	})

	b.Run("fixed", func(b *testing.B) {
		var condition common.Hash
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			binary.BigEndian.PutUint64(condition[24:], uint64(i+1))
			_ = computeCollectionIDFixed(common.Hash{}, condition, collectionIndexWords[i&1])
		}
	})
}

func BenchmarkPolymarketCollectionCacheState(b *testing.B) {
	data := loadPolymarketBenchData(b)
	blocks := parsePolymarketBenchBlocks(b, data)
	ctx := context.Background()
	state := generated.NewState()

	previousCollection := collectionCache
	previousPosition := positionCache
	previousNegRisk := negRiskPosCache
	previousCondition := conditionIDCache
	b.Cleanup(func() {
		collectionCache = previousCollection
		positionCache = previousPosition
		negRiskPosCache = previousNegRisk
		conditionIDCache = previousCondition
	})

	resetDependentCaches := func() {
		positionCache = newClockCache[positionKey, uint256.Int](maxCryptoCacheLen, 64, hashPositionKey)
		negRiskPosCache = newClockCache[negRiskKey, uint256.Int](maxCryptoCacheLen, 64, hashNegRiskKey)
		conditionIDCache = newClockCache[conditionIDKey, common.Hash](maxCryptoCacheLen, 64, hashConditionIDKey)
	}
	process := func(b *testing.B) {
		for _, block := range blocks {
			if err := generated.CustomProcessingProto(ctx, nil, state, block); err != nil {
				b.Fatalf("process proto: %v", err)
			}
		}
	}

	collectionCache = newClockCache[collectionKey, common.Hash](maxCryptoCacheLen, 64, hashCollectionKey)
	resetDependentCaches()
	process(b)

	b.Run("warm", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			resetDependentCaches()
			b.StartTimer()
			process(b)
		}
	})

	b.Run("cold", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			collectionCache = newClockCache[collectionKey, common.Hash](maxCryptoCacheLen, 64, hashCollectionKey)
			resetDependentCaches()
			b.StartTimer()
			process(b)
		}
	})
}
