//go:build goexperiment.arenas

package experiment

import (
	"bytes"
	"testing"
)

func BenchmarkOptimized_Direct_Arena(b *testing.B) {
	data := readTestData(b)
	rb, _ := NewOrderedHistoricRingBuffer(1024, true)
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
