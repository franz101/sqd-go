//go:build ignore
// +build ignore

package experiment

import (
	"bytes"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/parser"
)

func readTestData(b *testing.B) []byte {
	// Try the absolute path first
	data, err := os.ReadFile("/home/dev/CODING/polymarket_lowram/sqd-go/samples/exchange_events.jsonl")
	if err != nil {
		// Fallback to relative path
		data, err = os.ReadFile("../sqd/testdata/exchange_events.jsonl")
		if err != nil {
			b.Fatalf("failed to read test data: %v", err)
		}
	}
	return data
}

func BenchmarkOriginal_FastJSON_ABIDecode(b *testing.B) {
	data := readTestData(b)
	rb, _ := generated.NewOrderedHistoricRingBuffer(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := parser.NewFastJSONLParser(1024)
		_ = p.Parse(data, func(block *parser.Block) error {
			var decodedLogs []generated.DecodedLog
			for _, lg := range block.Logs {
				dataBytes := common.FromHex(lg.Data)
				decoded, err := generated.UnpackLog(lg.Address, lg.Topics, dataBytes)
				if err != nil || decoded == nil {
					continue
				}
				decodedLogs = append(decodedLogs, *decoded)
			}
			if len(decodedLogs) > 0 {
				rb.Push(block.Header.Number, block.Header.Hash, decodedLogs)
			}
			return nil
		})
	}
}

func BenchmarkOptimized_Direct_Std(b *testing.B) {
	data := readTestData(b)
	rb, _ := NewOrderedHistoricRingBuffer(1024, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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

			_ = ParseBlockJSONLDirect(line, rb)
		}
	}
}
