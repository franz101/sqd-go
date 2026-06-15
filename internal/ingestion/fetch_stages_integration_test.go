// Real-portal integration suite for the page-size controller across polymarket
// history stages (23M / 58M / 85M). READ ONLY — no ClickHouse, no writes.
//
// It drives nextPageSize against the live SQD polygon finalized-stream and
// asserts the properties that matter for correctness AND speed:
//
//   - DATA INTEGRITY: with includeAllBlocks=true every response starts exactly at
//     the requested cursor and the cursor advances by the served span with no gap
//     and no overlap (a fetch bug here would corrupt the indexed state).
//   - CONVERGENCE: starting "unbound" the controller settles on the server's
//     cursor cap within a few requests and keeps responses full.
//   - THROUGHPUT: the adaptive page beats the indexer's fixed 200-block page.
//   - DENSITY: reports empty-block fraction + bytes/block per stage, and an
//     includeAllBlocks true-vs-false A/B (which server path is faster).
//
// Run:
//
//	FETCH_BENCH=1 go test ./internal/ingestion/ -run TestPageSizeControllerStages -v -count=1 -timeout 1200s
package ingestion

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/parser"
)

// scanRange returns the first/last block numbers and the returned block count of
// an NDJSON portal response.
func scanRange(raw []byte) (first, last uint64, count int) {
	p := parser.NewFastJSONLParser(1024)
	firstSet := false
	_ = p.ScanHeadersWithLine(raw, func(number, _ uint64, _ string, _ []byte) error {
		if !firstSet {
			first = number
			firstSet = true
		}
		last = number
		count++
		return nil
	})
	return first, last, count
}

type stageResult struct {
	requests    int
	totalSpan   uint64
	totalReturn uint64
	totalBytes  int64
	dur         time.Duration
	finalPage   uint64
	pages       []uint64
	contiguity  string // "" == clean
}

func (r stageResult) spanPerSec() float64 { return float64(r.totalSpan) / r.dur.Seconds() }
func (r stageResult) mbPerSec() float64   { return float64(r.totalBytes) / 1e6 / r.dur.Seconds() }
func (r stageResult) emptyFrac() float64 {
	if r.totalSpan == 0 {
		return 0
	}
	return 1 - float64(r.totalReturn)/float64(r.totalSpan)
}

// driveController runs the adaptive page-size controller against the portal for
// up to nReq requests starting at `start`. startPage==0 means "start unbound"
// (begin at maxPage so the very first response reveals the cap).
func driveController(ctx context.Context, c *client.Client, filters []client.LogFilter, start uint64, nReq int, includeAll bool, startPage, minPage, maxPage uint64, checkContiguity bool) stageResult {
	page := startPage
	if page == 0 {
		page = maxPage
	}
	cursor := start
	res := stageResult{}
	t0 := time.Now()
	for i := 0; i < nReq; i++ {
		to := cursor + page - 1
		resp, err := c.FetchWithParent(ctx, cursor, &to, "", includeAll, filters)
		if err != nil {
			page = nextPageSize(page, page, 0, true, minPage, maxPage)
			continue
		}
		first, lastBlk, count := scanRange(resp.Raw)
		if count == 0 {
			break // end of data / 204
		}
		if checkContiguity && includeAll && first != cursor && res.contiguity == "" {
			res.contiguity = "first block != cursor (gap/overlap)"
		}
		span := lastBlk - cursor + 1
		res.requests++
		res.totalSpan += span
		res.totalReturn += uint64(count)
		res.totalBytes += int64(len(resp.Raw))
		res.pages = append(res.pages, page)
		cursor = lastBlk + 1
		page = nextPageSize(page, page, span, false, minPage, maxPage)
	}
	res.dur = time.Since(t0)
	res.finalPage = page
	return res
}

func TestPageSizeControllerStages(t *testing.T) {
	if os.Getenv("FETCH_BENCH") != "1" {
		t.Skip("set FETCH_BENCH=1 to run the real-portal stage integration suite")
	}
	project, err := config.LoadProject(envOr("FETCH_BENCH_PROJECT", "../../examples/polymarket"))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	chain := project.Config.Chains[0]
	_, filters, err := parser.BuildEventDecoder(chain.Contracts)
	if err != nil {
		t.Fatalf("BuildEventDecoder: %v", err)
	}
	endpoint := chainEndpoint(chain.ID, false)
	const minPage, maxPage = 200, 100000
	nReq := int(envUintOr("FETCH_BENCH_N", 12))

	stages := []struct {
		name  string
		start uint64
	}{
		{"23M_early", 23_000_000},
		{"58M_mid", 58_000_000},
		{"85M_recent", 85_000_000},
	}

	for _, st := range stages {
		st := st
		t.Run(st.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
			defer cancel()

			c := client.New(endpoint)
			defer c.Close()

			// 1) Adaptive controller, includeAllBlocks=true, "start unbound".
			adaptive := driveController(ctx, c, filters, st.start, nReq, true, 0, minPage, maxPage, true)
			if adaptive.requests == 0 {
				t.Fatalf("%s: adaptive produced no requests", st.name)
			}
			// DATA INTEGRITY: no gap/overlap in the cursor walk.
			if adaptive.contiguity != "" {
				t.Errorf("%s: CONTIGUITY VIOLATION: %s", st.name, adaptive.contiguity)
			}
			// CONVERGENCE: the last few page sizes should be stable (within 2x)
			// and responses near-full (the server cap drives the span).
			if n := len(adaptive.pages); n >= 4 {
				a, b := adaptive.pages[n-1], adaptive.pages[n-2]
				lo, hi := a, b
				if lo > hi {
					lo, hi = hi, lo
				}
				if hi > lo*2 {
					t.Errorf("%s: page size not converged: ...%d,%d", st.name, b, a)
				}
			}

			// 2) Fixed 200-block page (the live indexer's current config).
			c2 := client.New(endpoint)
			defer c2.Close()
			fixed := driveController(ctx, c2, filters, st.start, nReq, true, 200, 200, 200, true)

			// 3) Adaptive, includeAllBlocks=false (server path + empty fraction).
			c3 := client.New(endpoint)
			defer c3.Close()
			noAll := driveController(ctx, c3, filters, st.start, nReq, false, 0, minPage, maxPage, false)

			// THROUGHPUT WIN: adaptive must beat the fixed 200 page.
			if adaptive.spanPerSec() <= fixed.spanPerSec() {
				t.Errorf("%s: adaptive (%.0f blk/s) did not beat fixed-200 (%.0f blk/s)",
					st.name, adaptive.spanPerSec(), fixed.spanPerSec())
			}

			t.Logf("%-10s | adaptive: %6.0f blk/s %5.1f MB/s finalPage=%-6d empty=%4.1f%% | fixed200: %6.0f blk/s | noAll: %6.0f blk/s %5.1f MB/s empty=%4.1f%% | speedup=%.2fx",
				st.name,
				adaptive.spanPerSec(), adaptive.mbPerSec(), adaptive.finalPage, adaptive.emptyFrac()*100,
				fixed.spanPerSec(),
				noAll.spanPerSec(), noAll.mbPerSec(), noAll.emptyFrac()*100,
				adaptive.spanPerSec()/fixed.spanPerSec())
		})
	}
}
