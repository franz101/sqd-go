package parser

import (
	"os"
	"strings"
	"testing"
)

func TestFastJSONLParserParsesRepeatedPages(t *testing.T) {
	data := []byte(`{"header":{"number":1,"hash":"0x1","timestamp":11},"logs":[{"address":"0x0000000000000000000000000000000000000001","transactionHash":"0x01","data":"0x","transactionIndex":0,"logIndex":0,"topics":["0xaa"]}]}
{"header":{"number":2,"hash":"0x2","timestamp":12},"logs":[{"address":"0x0000000000000000000000000000000000000002","transactionHash":"0x02","data":"0x","transactionIndex":1,"logIndex":2,"topics":["0xbb","0xcc"]}]}
`)
	parser := NewFastJSONLParser(1)
	for run := 0; run < 2; run++ {
		var numbers []uint64
		var logCounts []int
		if err := parser.Parse(data, func(block *Block) error {
			numbers = append(numbers, block.Header.Number)
			logCounts = append(logCounts, len(block.Logs))
			if len(block.Logs) == 1 && block.Logs[0].Address == "" {
				t.Fatal("log address was not parsed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(numbers) != 2 || numbers[0] != 1 || numbers[1] != 2 {
			t.Fatalf("run %d numbers = %#v, want [1 2]", run, numbers)
		}
		if len(logCounts) != 2 || logCounts[0] != 1 || logCounts[1] != 1 {
			t.Fatalf("run %d logCounts = %#v, want [1 1]", run, logCounts)
		}
	}
}

// ScanHeadersWithLine must agree with the full ParseWithLine on header fields
// and per-line raw bytes for real portal-format fixtures (single-parse mode
// depends on this parity for replay-buffer identity and fork tracking).
func TestScanHeadersWithLineMatchesParseWithLine(t *testing.T) {
	data, err := os.ReadFile("../../tests/wallet_0xa79af3b_compact.jsonl")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}

	type hdr struct {
		number, timestamp uint64
		hash              string
		lineLen           int
	}
	var full []hdr
	if err := NewFastJSONLParser(0).ParseWithLine(data, func(b *Block, line []byte) error {
		full = append(full, hdr{b.Header.Number, b.Header.Timestamp, b.Header.Hash, len(line)})
		return nil
	}); err != nil {
		t.Fatalf("ParseWithLine: %v", err)
	}

	var scanned []hdr
	if err := NewFastJSONLParser(0).ScanHeadersWithLine(data, func(number, timestamp uint64, hash string, line []byte) error {
		scanned = append(scanned, hdr{number, timestamp, strings.Clone(hash), len(line)})
		return nil
	}); err != nil {
		t.Fatalf("ScanHeadersWithLine: %v", err)
	}

	if len(full) == 0 || len(full) != len(scanned) {
		t.Fatalf("block count mismatch: full=%d scanned=%d", len(full), len(scanned))
	}
	for i := range full {
		if full[i] != scanned[i] {
			t.Fatalf("block %d mismatch: full=%+v scanned=%+v", i, full[i], scanned[i])
		}
	}
}
