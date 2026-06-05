//go:build ignore
// +build ignore

package experiment

import (
	"bytes"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/holiman/uint256"
)

func setEventMeta(decoded *generated.DecodedLog, meta generated.EventMeta) {
	if decoded == nil || decoded.Value == nil {
		return
	}
	switch ev := decoded.Value.(type) {
	case *generated.ConditionalTokensConditionPreparation:
		ev.EventMeta = meta
	case *generated.ConditionalTokensConditionResolution:
		ev.EventMeta = meta
	case *generated.ConditionalTokensPositionSplit:
		ev.EventMeta = meta
	case *generated.ConditionalTokensPositionsMerge:
		ev.EventMeta = meta
	case *generated.ConditionalTokensPayoutRedemption:
		ev.EventMeta = meta
	case *generated.ExchangeOrderFilled:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterMarketPrepared:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterQuestionPrepared:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPositionSplit:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPositionsMerge:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPositionsConverted:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPayoutRedemption:
		ev.EventMeta = meta
	}
}

func TestDirectDecoderCorrectness(t *testing.T) {
	// Read test data
	data, err := os.ReadFile("../sqd/testdata/exchange_events.jsonl")
	if err != nil {
		data, err = os.ReadFile("/home/dev/CODING/polymarket_lowram/sqd-go/samples/exchange_events.jsonl")
		if err != nil {
			t.Fatalf("failed to read test data: %v", err)
		}
	}

	// 1. Parse using original pipeline
	rbOriginal, _ := generated.NewOrderedHistoricRingBuffer(1024)
	p := parser.NewFastJSONLParser(1024)
	err = p.Parse(data, func(block *parser.Block) error {
		var decodedLogs []generated.DecodedLog
		for _, lg := range block.Logs {
			dataBytes := common.FromHex(lg.Data)
			decoded, err := generated.UnpackLog(lg.Address, lg.Topics, dataBytes)
			if err != nil || decoded == nil {
				continue
			}
			setEventMeta(decoded, generated.EventMeta{
				BlockNumber:      block.Header.Number,
				BlockTimestamp:   time.Unix(int64(block.Header.Timestamp), 0),
				TransactionIndex: lg.TransactionIndex,
				LogIndex:         lg.LogIndex,
			})
			decodedLogs = append(decodedLogs, *decoded)
		}
		if len(decodedLogs) > 0 {
			rbOriginal.Push(block.Header.Number, block.Header.Hash, decodedLogs)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("original parser failed: %v", err)
	}

	// 2. Parse using optimized pipeline
	rbOptimized, _ := NewOrderedHistoricRingBuffer(1024, false)

	rest := data
	for len(rest) > 0 {
		lineEnd := bytes.IndexByte(rest, '\n')
		var line []byte
		if lineEnd >= 0 {
			line = rest[:lineEnd]
			rest = rest[lineEnd+1:]
		} else {
			line = rest
			rest = nil
		}
		if len(line) == 0 {
			continue
		}

		err := ParseBlockJSONLDirect(line, rbOptimized)
		if err != nil {
			t.Fatalf("optimized parser failed on line: %v", err)
		}
	}

	// 3. Compare ring buffers
	originalCount := 0
	optimizedCount := 0

	for i := uint32(0); i < 1024; i++ {
		slotOrig, okOrig := rbOriginal.GetBlockEvents(uint64(78000000 + i))
		slotOpt, okOpt := rbOptimized.GetBlockEvents(uint64(78000000 + i))

		if okOrig {
			originalCount += len(slotOrig.ExchangeOrderFilleds)
		}
		if okOpt {
			optimizedCount += len(slotOpt.ExchangeOrderFilleds)
		}

		if okOrig != okOpt {
			t.Fatalf("slot presence mismatch at block %d: original=%v, optimized=%v", 78000000+i, okOrig, okOpt)
		}

		if okOrig {
			if len(slotOrig.ExchangeOrderFilleds) != len(slotOpt.ExchangeOrderFilleds) {
				t.Fatalf("slot events count mismatch at block %d: original=%d, optimized=%d", 78000000+i, len(slotOrig.ExchangeOrderFilleds), len(slotOpt.ExchangeOrderFilleds))
			}

			for idx := range slotOrig.ExchangeOrderFilleds {
				origEv := slotOrig.ExchangeOrderFilleds[idx]
				optEv := slotOpt.ExchangeOrderFilleds[idx]

				if origEv.BlockNumber != optEv.BlockNumber ||
					origEv.TransactionIndex != optEv.TransactionIndex ||
					origEv.LogIndex != optEv.LogIndex ||
					origEv.Maker != optEv.Maker ||
					origEv.Taker != optEv.Taker ||
					!origEv.MakerAssetID.Eq(&optEv.MakerAssetID) ||
					!origEv.TakerAssetID.Eq(&optEv.TakerAssetID) ||
					!origEv.MakerAmountFilled.Eq(&optEv.MakerAmountFilled) ||
					!origEv.TakerAmountFilled.Eq(&optEv.TakerAmountFilled) ||
					!origEv.Fee.Eq(&optEv.Fee) {
					t.Fatalf("event mismatch at block %d event %d:\noriginal: %+v\noptimized: %+v", 78000000+i, idx, origEv, optEv)
				}
			}
		}
	}

	t.Logf("Tested successfully! Original decoded %d events, Optimized decoded %d events", originalCount, optimizedCount)
	if originalCount == 0 {
		t.Fatalf("test data had 0 decoded events, check that the test is actually matching events")
	}
}

func TestRingBufferMemoryLeak(t *testing.T) {
	rb, _ := NewOrderedHistoricRingBuffer(2, false)

	// Step 1: Allocate a large byte slice (e.g. 10MB) and store it in slot 0
	slot0, _ := rb.NextSlot(1, "hash1")
	largeSlice := make([]byte, 10*1024*1024) // 10 MB
	for i := range largeSlice {
		largeSlice[i] = byte(i)
	}
	slot0.NegRiskAdapterMarketPrepareds = append(slot0.NegRiskAdapterMarketPrepareds, generated.NegRiskAdapterMarketPrepared{
		Data: largeSlice,
	})

	// Step 2: Allocate large uint256 slice (e.g. 100K elements) and store it in slot 1
	slot1, _ := rb.NextSlot(2, "hash2")
	largeUints := make([]uint256.Int, 100000)
	slot1.ConditionalTokensPositionSplits = append(slot1.ConditionalTokensPositionSplits, generated.ConditionalTokensPositionSplit{
		Partition: largeUints,
	})

	// Keep references to large slices to make sure they aren't GC'd before we check
	_ = largeSlice[0]
	_ = largeUints[0]

	// Step 3: Advance the ring buffer by 2 slots so slot 0 and slot 1 are reused/overwritten
	// Overwrite slot 0
	slot0Reuse, _ := rb.NextSlot(3, "hash3")
	if slot0Reuse != slot0 {
		t.Fatalf("expected slot0 reuse")
	}
	// Overwrite slot 1
	slot1Reuse, _ := rb.NextSlot(4, "hash4")
	if slot1Reuse != slot1 {
		t.Fatalf("expected slot1 reuse")
	}

	// Step 4: Run GC and check if the large slices are still in memory
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	
	// We check if the heap allocation is large. If the memory is leaked,
	// HeapAlloc will be at least 10MB (for the byte slice) + 3.2MB (for the uint256 slice)
	// If it was cleared successfully, HeapAlloc should be small (usually < 4MB).
	t.Logf("HeapAlloc after GC: %d bytes (%f MB)", ms.HeapAlloc, float64(ms.HeapAlloc)/(1024*1024))
	if ms.HeapAlloc > 5*1024*1024 {
		t.Errorf("Potential memory leak detected: HeapAlloc = %d bytes, which is > 5MB", ms.HeapAlloc)
	}
}
