//go:build ignore

package polymarket

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
)

func init() {
	// Set TEST_MODE to prevent LoadStateFromClickHouseFn from connecting to production DB
	os.Setenv("TEST_MODE", "1")
}

// Test Helpers

func makeTestEventMeta(blockNum uint64) generated.EventMeta {
	return generated.EventMeta{
		BlockNumber:      blockNum,
		BlockTimestamp:   time.Now(),
		BlockHash:        common.HexToHash("0xab"),
		ContractAddress:  common.HexToAddress("0x123"),
		TransactionHash:  common.HexToHash("0xcd"),
		TransactionIndex: 0,
		LogIndex:         0,
	}
}

func populateProtoBlockWithEvents(block *generated.ProtoEventBlock, eventCount int) []common.Hash {
	var conditionIDs []common.Hash

	for i := 0; i < eventCount; i++ {
		blockNum := uint64(1000 + i)
		meta := makeTestEventMeta(blockNum)

		// Add PositionSplit event (has array field)
		condID := common.HexToHash("0x" + string(rune('a'+i%26)) + "001")
		conditionIDs = append(conditionIDs, condID)

		splitEv := &generated.ConditionalTokensPositionSplit{
			EventMeta:        meta,
			Stakeholder:      common.HexToAddress("0x456"),
			CollateralToken:  common.HexToAddress("0x789"),
			ParentCollectionID: common.HexToHash("0xdef"),
			ConditionID:      condID,
			Partition:        []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
			Amount:           *uint256.NewInt(1000),
		}
		block.AppendConditionalTokensPositionSplit(meta, splitEv)

		// Add ConditionResolution event (has array field)
		resEv := &generated.ConditionalTokensConditionResolution{
			EventMeta:         meta,
			ConditionID:       condID,
			Oracle:            common.HexToAddress("0xabc"),
			QuestionID:        common.HexToHash("0xqst"),
			PayoutDenominator: *uint256.NewInt(100),
			PayoutNumerators:  []uint256.Int{*uint256.NewInt(50), *uint256.NewInt(50)},
		}
		block.AppendConditionalTokensConditionResolution(meta, resEv)

		// Add PayoutRedemption event (has array field)
		payoutEv := &generated.ConditionalTokensPayoutRedemption{
			EventMeta:          meta,
			Redeemer:           common.HexToAddress("0xredeemer"),
			CollateralToken:    common.HexToAddress("0xcol"),
			ParentCollectionID: common.HexToHash("0xparent"),
			ConditionID:        condID,
			IndexSets:          []uint256.Int{*uint256.NewInt(1)},
			Payout:             *uint256.NewInt(500),
		}
		block.AppendConditionalTokensPayoutRedemption(meta, payoutEv)

		// Add FPMMFundingAdded event (has array field)
		fundingEv := &generated.FixedProductMarketMakerFPMMFundingAdded{
			EventMeta:     meta,
			Funder:        common.HexToAddress("0xfunder"),
			AmountsAdded:  []uint256.Int{*uint256.NewInt(100), *uint256.NewInt(200)},
			SharesMinted: *uint256.NewInt(300),
		}
		block.AppendFixedProductMarketMakerFPMMFundingAdded(meta, fundingEv)
	}

	return conditionIDs
}

// Benchmarks: Array Field Access Allocations

func BenchmarkArrayAccess_Partition(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.QueryConditionalTokensPositionSplit().Map(func(ev generated.ConditionalTokensPositionSplitProtoView) {
			_ = ev.Partition() // This allocates a new []uint256.Int slice
		})
	}
}

func BenchmarkArrayAccess_PayoutNumerators(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.QueryConditionalTokensConditionResolution().Map(func(ev generated.ConditionalTokensConditionResolutionProtoView) {
			_ = ev.PayoutNumerators() // This allocates a new []uint256.Int slice
		})
	}
}

func BenchmarkArrayAccess_IndexSets(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.QueryConditionalTokensPayoutRedemption().Map(func(ev generated.ConditionalTokensPayoutRedemptionProtoView) {
			_ = ev.IndexSets() // This allocates a new []uint256.Int slice
		})
	}
}

func BenchmarkArrayAccess_AmountsAdded(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.QueryFixedProductMarketMakerFPMMFundingAdded().Map(func(ev generated.FixedProductMarketMakerFPMMFundingAddedProtoView) {
			_ = ev.AmountsAdded() // This allocates a new []uint256.Int slice
		})
	}
}

// Benchmarks: ProcessProto Full Path

func BenchmarkProcessProto_FullBlock(b *testing.B) {
	block := generated.NewProtoEventBlock()
	state := generated.NewState()
	conditionIDs := populateProtoBlockWithEvents(block, 50)

	// Pre-populate conditions
	for _, condID := range conditionIDs {
		cond := &generated.Condition{
			ID:               condID,
			Oracle:           common.HexToAddress("0xoracle"),
			QuestionID:       common.HexToHash("0xquestion"),
			OutcomeSlotCount: 2,
			Resolved:         false,
			Payouts:          []uint256.Int{*uint256.NewInt(50), *uint256.NewInt(50)},
		}
		state.Condition.Save(cond, generated.EventMeta{})
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// ProcessProto implementation (simplified)
		var condIDs []common.Hash
		block.QueryConditionalTokensPositionSplit().Map(func(ev generated.ConditionalTokensPositionSplitProtoView) {
			condIDs = append(condIDs, ev.ConditionID())
		})
		_ = condIDs

		// Iterate through all events
		var splitIdx int
		for _, typ := range block.Sequence {
			switch generated.EventType(typ) {
			case generated.EventTypeConditionalTokensPositionSplit:
				ev := block.ConditionalTokensPositionSplitProtoAt(splitIdx)
				splitIdx++
				_ = ev.Partition() // Allocation here
				_ = ev.Amount()
			}
		}
	}
}

// Benchmarks: Sequence Iteration vs EventsIter

func BenchmarkSequenceIteration(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var count int
		for _, typ := range block.Sequence {
			_ = typ
			count++
		}
		_ = count
	}
}

// Benchmarks: Map Function Overhead

func BenchmarkQueryMap_Overhead(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.QueryConditionalTokensPositionSplit().Map(func(ev generated.ConditionalTokensPositionSplitProtoView) {
			_ = ev.ConditionID()
		})
	}
}

// Benchmarks: Block Preallocation Benefits

func BenchmarkBlockAppend_NoPrealloc(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block := generated.NewProtoEventBlock()
		populateProtoBlockWithEvents(block, 100)
	}
}

func BenchmarkBlockAppend_WithReuse(b *testing.B) {
	block := generated.NewProtoEventBlock()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.Reset()
		populateProtoBlockWithEvents(block, 100)
	}
}

func BenchmarkBlockAppend_WithReserve(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block := generated.NewProtoEventBlock()
		block.Reserve(100)
		populateProtoBlockWithEvents(block, 100)
	}
}

func BenchmarkBlockAppend_WithReserveAndReuse(b *testing.B) {
	block := generated.NewProtoEventBlock()
	block.Reserve(100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.Reset()
		if cap(block.BlockNumber) < 100 {
			block.Reserve(100)
		}
		populateProtoBlockWithEvents(block, 100)
	}
}

// Benchmarks: Array Length Check (common operation)

func BenchmarkArrayLen(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.QueryConditionalTokensPositionSplit().Map(func(ev generated.ConditionalTokensPositionSplitProtoView) {
			partition := ev.Partition()
			_ = len(partition)
		})
	}
}

// Benchmarks: HotStateUint256Slice Conversion (core allocation point)

func BenchmarkHotStateUint256Slice_Small(b *testing.B) {
	// Simulate small array (2 elements)
	input := []protoUInt256{{0x1, 0x0}, {0x2, 0x0}}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out := make([]uint256.Int, 0, len(input))
		for _, v := range input {
			out = append(out, uint256.Int{v[0], v[1]})
		}
		_ = out
	}
}

func BenchmarkHotStateUint256Slice_Medium(b *testing.B) {
	// Simulate medium array (10 elements)
	input := make([]protoUInt256, 10)
	for i := range input {
		input[i] = protoUInt256{uint64(i), 0}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out := make([]uint256.Int, 0, len(input))
		for _, v := range input {
			out = append(out, uint256.Int{v[0], v[1]})
		}
		_ = out
	}
}

type protoUInt256 [2]uint64

// Benchmark: Compare ParseBlock vs Proto

func BenchmarkParseBlockVsProto(b *testing.B) {
	// Create test JSONL data
	events := []map[string]interface{}{}
	for i := 0; i < 100; i++ {
		events = append(events, map[string]interface{}{
			"block_number":       1000 + i,
			"block_timestamp":    time.Now().Unix(),
			"block_hash":         "0xab" + string(rune('0'+i%10)),
			"contract_address":   "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045",
			"transaction_hash":   "0xcd" + string(rune('0'+i%10)),
			"transaction_index": 0,
			"log_index":          uint64(i),
			"event_name":        "PositionSplit",
			"stakeholder":       "0x456",
			"collateral_token":  "0x789",
			"parent_collection_id": "0xdef",
			"condition_id":      "0xcond001",
			"partition":         []interface{}{"1", "2"},
			"amount":            "1000",
		})
	}

	// Convert to JSONL format (each object on new line)
	lines := []map[string]interface{}{events[0]} // Simplified for benchmark

	b.Run("JSONParsing", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var ev generated.ConditionalTokensPositionSplit
			data, _ := json.Marshal(lines[0])
			json.Unmarshal(data, &ev)
		}
	})
}

// Benchmark: ProtoAt access patterns

func BenchmarkProtoAt_Access(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		block.QueryConditionalTokensPositionSplit().Map(func(ev generated.ConditionalTokensPositionSplitProtoView) {
			_ = ev.ConditionID()
			_ = ev.Amount()
		})
	}
}

// Benchmark: Combined access pattern (realistic usage)

func BenchmarkRealisticProcessProto(b *testing.B) {
	block := generated.NewProtoEventBlock()
	populateProtoBlockWithEvents(block, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Simulate ProcessProto logic
		var splitIdx, resIdx, payoutIdx, fundingIdx int

		for _, typ := range block.Sequence {
			switch generated.EventType(typ) {
			case generated.EventTypeConditionalTokensPositionSplit:
				ev := block.ConditionalTokensPositionSplitProtoAt(splitIdx)
				splitIdx++
				_ = ev.ConditionID()
				_ = ev.Stakeholder()
				_ = ev.Partition() // Allocation
				_ = ev.Amount()

			case generated.EventTypeConditionalTokensConditionResolution:
				ev := block.ConditionalTokensConditionResolutionProtoAt(resIdx)
				resIdx++
				_ = ev.ConditionID()
				_ = ev.PayoutNumerators() // Allocation
				_ = ev.PayoutDenominator()

			case generated.EventTypeConditionalTokensPayoutRedemption:
				ev := block.ConditionalTokensPayoutRedemptionProtoAt(payoutIdx)
				payoutIdx++
				_ = ev.ConditionID()
				_ = ev.IndexSets() // Allocation
				_ = ev.Payout()

			case generated.EventTypeFixedProductMarketMakerFPMMFundingAdded:
				ev := block.FixedProductMarketMakerFPMMFundingAddedProtoAt(fundingIdx)
				fundingIdx++
				_ = ev.Funder()
				_ = ev.AmountsAdded() // Allocation
				_ = ev.SharesMinted()
			}
		}
	}
}
