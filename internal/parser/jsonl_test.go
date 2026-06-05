package parser

import "testing"

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
