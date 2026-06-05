package generated_test

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	// Generated type-safe ring buffer & events
	"github.com/franz101/sqd-go/examples/uniswap/generated"
)

const NumBlocks = 1000
const EventsPerBlock = 100

// Setup benchmark inputs
func setupData() []generated.DecodedLog {
	// Pre-generate data
	fromAddr := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	toAddr := common.HexToAddress("0x0000000000000000000000000000000000000000")
	val := uint256.NewInt(1000)

	typeSafeLogs := make([]generated.DecodedLog, EventsPerBlock)

	for e := 0; e < EventsPerBlock; e++ {
		// Type-Safe generated Log setup (completely flat concrete struct)
		transfer := generated.LBTCTransfer{
			EventMeta: generated.EventMeta{
				BlockNumber:      100,
				BlockTimestamp:   time.Now(),
				BlockHash:        common.HexToHash("0xabc"),
				ContractAddress:  common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494"),
				TransactionHash:  common.HexToHash("0xdef"),
				TransactionIndex: uint64(e),
				LogIndex:         uint64(e),
			},
			From:  fromAddr,
			To:    toAddr,
			Value: *val,
		}

		typeSafeLogs[e] = generated.DecodedLog{
			EventName: "Transfer",
			Topic0:    "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
			Value:     &transfer,
		}
	}

	return typeSafeLogs
}

// --- Benchmark 1: PUSH OPERATIONS ---

func BenchmarkPush_GeneratedTypeSafe(b *testing.B) {
	typeSafeLogs := setupData()
	buf, _ := generated.NewOrderedHistoricRingBuffer(2048)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for block := uint64(0); block < NumBlocks; block++ {
			// Simulating pushes
			buf.Push(block, "0xabc", typeSafeLogs)
		}
	}
}

// --- Benchmark 2: RECONSTRUCT OPERATIONS ---

func BenchmarkReconstruct_GeneratedTypeSafe(b *testing.B) {
	typeSafeLogs := setupData()
	buf, _ := generated.NewOrderedHistoricRingBuffer(2048)

	// Populate buffer
	for block := uint64(0); block < NumBlocks; block++ {
		buf.Push(block, "0xabc", typeSafeLogs)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sum uint64 = 0
		for block := uint64(0); block < NumBlocks; block++ {
			slot, found := buf.GetParsedBlock(block)
			if !found {
				b.Fatal("not found")
			}
			for ev := range slot.EventsIter() {
				if transfer, ok := ev.(*generated.LBTCTransfer); ok {
					sum += transfer.Value[0]
				}
			}
		}
		if sum == 0 {
			b.Fatal("sum is zero")
		}
	}
}
