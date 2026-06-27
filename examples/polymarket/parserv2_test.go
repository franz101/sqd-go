package polymarket

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/klauspost/compress/zstd"
)

// Real pure-OrderFilled polymarket corpus (Polygon block 84M, Exchange +
// NegRiskExchange). Download with the address+topic0 filter to
// /private/tmp/claude-501/pmof/*.jsonl.zst (see PARSER_V2_PROOF.md). The benchmark
// Skips if the corpus is absent.
func loadOFCorpus(t *testing.T) [][]byte {
	dir := "/private/tmp/claude-501/pmof"
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("OrderFilled corpus not found at %s: %v", dir, err)
	}
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	var pages [][]byte
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".zst" {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out, err := dec.DecodeAll(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, out)
	}
	return pages
}

func rowsOF(b *generated.InsertBatches) int {
	return b.ExchangeOrderFilled.Rows() + b.NegRiskExchangeOrderFilled.Rows()
}

// TestOrderFilledV2BeatsGenerated proves the byte-scanner v2 fills the SAME
// generated batches as the generated jlexer parser (v1), faster and zero-alloc,
// on the real polymarket OrderFilled schema.
func TestOrderFilledV2BeatsGenerated(t *testing.T) {
	pages := loadOFCorpus(t)
	if len(pages) == 0 {
		t.Skip("empty corpus")
	}

	// ---- equivalence: v1 and v2 must fill identical row counts ----
	var v1Rows, v2Rows, v2Events int
	for _, pg := range pages {
		b1 := generated.NewInsertBatches()
		if _, err := generated.ParseJSONLV2(pg, b1, nil, nil); err != nil {
			t.Fatalf("v1 parse: %v", err)
		}
		v1Rows += rowsOF(b1)

		b2 := generated.NewInsertBatches()
		v2Events += int(ParseOrderFilledV2(pg, b2))
		v2Rows += rowsOF(b2)
	}
	if v1Rows != v2Rows {
		t.Fatalf("ROW MISMATCH: generated v1 filled %d OrderFilled rows, v2 filled %d (parsers disagree)", v1Rows, v2Rows)
	}
	t.Logf("corpus: %d pages, %d OrderFilled events; v1 rows == v2 rows == %d (equivalent)", len(pages), v2Events, v1Rows)

	// ---- single-thread throughput + allocs ----
	const R = 5
	// Reuse one InsertBatches with Reset per page (the real pipeline's behavior:
	// column capacity is retained across commits). This makes the column fill
	// cheap so the PARSE-layer cost dominates — which is what differs between v1
	// (jlexer) and v2 (byte-scanner). A fresh NewInsertBatches per page instead
	// measures growslice, which is identical for both and masks the parse win.
	run := func(name string, parse func(pg []byte, b *generated.InsertBatches)) {
		b := generated.NewInsertBatches()
		for _, pg := range pages { // warm column capacity
			b.Reset()
			parse(pg, b)
		}
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		var ev int
		t0 := time.Now()
		for r := 0; r < R; r++ {
			ev = 0
			for _, pg := range pages {
				b.Reset()
				parse(pg, b)
				ev += rowsOF(b)
			}
		}
		el := time.Since(t0).Seconds()
		runtime.ReadMemStats(&m1)
		t.Logf("%-16s %5.2fM ev/s   %6.0f B/event", name,
			float64(ev)*R/el/1e6, float64(m1.TotalAlloc-m0.TotalAlloc)/float64(ev*R))
	}
	run("v1 generated", func(pg []byte, b *generated.InsertBatches) { _, _ = generated.ParseJSONLV2(pg, b, nil, nil) })
	run("v2 bytescanner", func(pg []byte, b *generated.InsertBatches) { ParseOrderFilledV2(pg, b) })

	// ---- v2 parallel scaling (per-worker private batches) ----
	for _, g := range []int{1, 2, 4, 6} {
		var ev int64
		var wg sync.WaitGroup
		t0 := time.Now()
		for w := 0; w < g; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				b := generated.NewInsertBatches()
				var local int64
				for r := 0; r < R; r++ {
					for i := id; i < len(pages); i += g {
						b.Reset()
						local += int64(ParseOrderFilledV2(pages[i], b))
					}
				}
				atomic.AddInt64(&ev, local)
			}(w)
		}
		wg.Wait()
		el := time.Since(t0).Seconds()
		t.Logf("v2 g=%-2d           %5.2fM ev/s", g, float64(ev)/el/1e6)
	}
}
