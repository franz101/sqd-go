package parser

import (
	"os"
	"runtime"
	"sync"
	"testing"
)

// ParallelJSONLParser parses JSONL bytes in parallel.
type ParallelJSONLParser struct {
	numWorkers int
}

func NewParallelJSONLParser(numWorkers int) *ParallelJSONLParser {
	if numWorkers <= 0 {
		numWorkers = runtime.NumCPU()
	}
	return &ParallelJSONLParser{numWorkers: numWorkers}
}

func (p *ParallelJSONLParser) Parse(data []byte, onBlock func(*Block) error) error {
	// 1. Find all line bounds (fast sequential pass)
	type lineBound struct {
		start int
		end   int
	}
	var lines []lineBound
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			if i > start {
				lines = append(lines, lineBound{start: start, end: i})
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, lineBound{start: start, end: len(data)})
	}

	if len(lines) == 0 {
		return nil
	}

	numLines := len(lines)
	numWorkers := p.numWorkers
	if numWorkers > numLines {
		numWorkers = numLines
	}

	// 2. Chunk lines among workers
	chunkSize := (numLines + numWorkers - 1) / numWorkers
	type workerResult struct {
		blocks []Block
		err    error
	}
	results := make([]workerResult, numWorkers)

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for w := 0; w < numWorkers; w++ {
		go func(workerID int) {
			defer wg.Done()

			startLine := workerID * chunkSize
			endLine := startLine + chunkSize
			if endLine > numLines {
				endLine = numLines
			}
			if startLine >= numLines {
				return
			}

			// Preallocate parsed blocks slice for this chunk
			count := endLine - startLine
			blocks := make([]Block, 0, count)
			var localParser FastJSONLParser

			for i := startLine; i < endLine; i++ {
				bound := lines[i]
				lineData := data[bound.start:bound.end]
				if len(lineData) == 0 {
					continue
				}
				v, err := localParser.parser.ParseBytes(lineData)
				if err != nil {
					results[workerID].err = err
					return
				}
				var block Block
				header := v.Get("header")
				block.Header.Number = header.GetUint64("number")
				block.Header.Hash = bytesToString(header.GetStringBytes("hash"))
				block.Header.Timestamp = header.GetUint64("timestamp")

				logsArr := v.GetArray("logs")
				block.Logs = make([]Log, len(logsArr))
				for j, lv := range logsArr {
					lg := &block.Logs[j]
					lg.Address = bytesToString(lv.GetStringBytes("address"))
					lg.TransactionHash = bytesToString(lv.GetStringBytes("transactionHash"))
					lg.Data = bytesToString(lv.GetStringBytes("data"))
					lg.TransactionIndex = lv.GetUint64("transactionIndex")
					lg.LogIndex = lv.GetUint64("logIndex")
					topics := lv.GetArray("topics")
					lg.Topics = make([]string, len(topics))
					for k, t := range topics {
						lg.Topics[k] = bytesToString(t.GetStringBytes())
					}
				}
				blocks = append(blocks, block)
			}
			results[workerID].blocks = blocks
		}(w)
	}
	wg.Wait()

	// 3. Process the results in order
	for w := 0; w < numWorkers; w++ {
		if results[w].err != nil {
			return results[w].err
		}
		for i := range results[w].blocks {
			if err := onBlock(&results[w].blocks[i]); err != nil {
				return err
			}
		}
	}

	return nil
}

func BenchmarkParserComparison(b *testing.B) {
	// Load actual exchange_orderfilled.jsonl file
	filePath := "/home/dev/CODING/polymarket_lowram/sqd-go/samples/exchange_orderfilled.jsonl"
	data, err := os.ReadFile(filePath)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}

	b.Run("Sequential", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p := NewFastJSONLParser(1024)
			var blockCount int
			err := p.Parse(data, func(block *Block) error {
				blockCount++
				return nil
			})
			if err != nil {
				b.Fatalf("parse failed: %v", err)
			}
		}
	})

	b.Run("Parallel-4Workers", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p := NewParallelJSONLParser(4)
			var blockCount int
			err := p.Parse(data, func(block *Block) error {
				blockCount++
				return nil
			})
			if err != nil {
				b.Fatalf("parse failed: %v", err)
			}
		}
	})

	b.Run("Parallel-8Workers", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p := NewParallelJSONLParser(8)
			var blockCount int
			err := p.Parse(data, func(block *Block) error {
				blockCount++
				return nil
			})
			if err != nil {
				b.Fatalf("parse failed: %v", err)
			}
		}
	})

	b.Run("Parallel-12Workers", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p := NewParallelJSONLParser(12)
			var blockCount int
			err := p.Parse(data, func(block *Block) error {
				blockCount++
				return nil
			})
			if err != nil {
				b.Fatalf("parse failed: %v", err)
			}
		}
	})
}
