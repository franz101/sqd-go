package parser

import (
	"fmt"
	"strings"
	"testing"
)

// buildLogDensePage builds a synthetic SQD JSONL page of nBlocks blocks, each
// carrying nLogs logs. The header scanner must read only the header of each line;
// the logs exist to show that ScanHeadersWithLine no longer pays for walking them.
func buildLogDensePage(nBlocks, nLogs int) []byte {
	var b strings.Builder
	for blk := 0; blk < nBlocks; blk++ {
		b.WriteString(fmt.Sprintf(`{"header":{"number":%d,"hash":"0x%064x","timestamp":%d},"logs":[`, 20_000_000+blk, blk, 1_700_000_000+blk))
		for lg := 0; lg < nLogs; lg++ {
			if lg > 0 {
				b.WriteByte(',')
			}
			b.WriteString(fmt.Sprintf(`{"address":"0x8236a87084f8b84306f72007f36f2618a5634494","transactionHash":"0x%064x","transactionIndex":%d,"logIndex":%d,"data":"0x%064x","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x%064x","0x%064x"]}`, lg, lg, lg, lg*7, lg, lg))
		}
		b.WriteString("]}\n")
	}
	return []byte(b.String())
}

// BenchmarkScanHeadersVsFullParse contrasts the header-only scan against the full
// log-decoding parse on the same log-dense page. ScanHeadersWithLine should be
// far cheaper and allocation-light because the hand-rolled scanner reads three
// header fields and never enters the logs array.
func BenchmarkScanHeadersVsFullParse(b *testing.B) {
	page := buildLogDensePage(50, 40) // 50 blocks x 40 logs = 2000 logs

	b.Run("ScanHeadersWithLine", func(b *testing.B) {
		p := NewFastJSONLParser(0)
		b.ReportAllocs()
		b.SetBytes(int64(len(page)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var n uint64
			if err := p.ScanHeadersWithLine(page, func(number, _ uint64, _ string, _ []byte) error {
				n += number
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			_ = n
		}
	})

	b.Run("ParseWithLine", func(b *testing.B) {
		p := NewFastJSONLParser(64)
		b.ReportAllocs()
		b.SetBytes(int64(len(page)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var n int
			if err := p.ParseWithLine(page, func(block *Block, _ []byte) error {
				n += len(block.Logs)
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			_ = n
		}
	})
}
