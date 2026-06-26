package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/franz101/sqd-go/internal/client"
)

// polymarket0x1Filter matches every polymarket contract the indexer cares about
// over the wallet 0x10f5b9bd…6701 window (2026-04-28, CLOB V2). With the full
// filter the range is dense (every page has matches), so a data-driven sequential
// walk is provably complete — the ground truth the parallel path must reproduce.
var polymarket0x1Filter = []client.LogFilter{{Address: []string{
	"0x4D97DCd97eC945f40cF65F87097ACe5EA0476045", // ConditionalTokens
	"0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E", // Exchange (V1)
	"0xC5d563A36AE78145C45a50134d48A1215220f80a", // NegRiskExchange (V1)
	"0xE111180000d2663C0091e4f400237545B87B996B", // ExchangeV2
	"0xe2222d279d744050d28e00520010520000310F59", // NegRiskExchangeV2
	"0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296", // NegRiskAdapter
	"0x8B9805A2f595B6705e74F7310829f2d299D21522", // FPMM factory
}}}

// logCountsOf returns matching-log count per block number from a raw JSONL chunk.
// Header-only blocks (coverage markers with no matching logs) count 0, so they do
// not inflate the comparison.
func logCountsOf(t *testing.T, raw []byte) map[uint64]int {
	t.Helper()
	out := map[uint64]int{}
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var b struct {
			Header struct {
				Number uint64 `json:"number"`
			} `json:"header"`
			Logs []json.RawMessage `json:"logs"`
		}
		if err := json.Unmarshal(line, &b); err != nil {
			t.Fatalf("parse block line: %v", err)
		}
		out[b.Header.Number] += len(b.Logs)
	}
	return out
}

// TestParallelVsSequential_0x1Window verifies the parallel prefetcher returns the
// exact same matching logs, per block, as a data-driven sequential walk over the
// 0x10f5b9bd window with the full polymarket filter. This is the data the 0x1 PnL
// test consumes — if parallel drops, dups, or reorders a log-bearing block, the
// indexed PnL would diverge.
//
// Run: SQD_LIVE_PORTAL=1 go test ./internal/ingestion/ -run TestParallelVsSequential_0x1Window -v
func TestParallelVsSequential_0x1Window(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live 0x1-window completeness check")
	}

	const endpoint = "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream"
	const start, end uint64 = 86_121_000, 86_133_000 // 0x10f5 activity window (~12k blocks)
	const pageSize uint64 = 1000

	// Sequential ground truth: request [cur,end], advance by the actual last block
	// the portal returned (handles its size/scan cap), until it stops returning new
	// blocks. Dense filter ⇒ no premature empties ⇒ complete.
	seq := map[uint64]int{}
	cl := client.New(endpoint)
	defer cl.Close()
	for cur := start; cur <= end; {
		to := end
		resp, err := cl.FetchWithParent(context.Background(), cur, &to, "", false, polymarket0x1Filter)
		if err != nil {
			t.Fatalf("sequential fetch from %d: %v", cur, err)
		}
		counts := logCountsOf(t, resp.Raw)
		if len(counts) == 0 {
			break // no more matches in [cur,end]
		}
		var last uint64
		for bn, c := range counts {
			seq[bn] += c
			if bn > last {
				last = bn
			}
		}
		if last < cur {
			break
		}
		cur = last + 1
	}

	// Parallel prefetcher under test (same range, page size, filter).
	par := map[uint64]int{}
	p := newParallelPrefetcher(endpoint, polymarket0x1Filter, false /*includeAllBlocks*/, start, end, pageSize, 6,
		newRateLimiter(defaultParallelRPS, defaultParallelBurst))
	p.launch(context.Background())
	for {
		chunk, ok := p.Next(context.Background())
		if !ok {
			break
		}
		if chunk.err != nil {
			t.Fatalf("parallel fetch error: %v", chunk.err)
		}
		for bn, c := range logCountsOf(t, chunk.raw) {
			par[bn] += c
		}
	}

	// Compare per-block matching-log counts.
	var droppedLogs, extraLogs, mismatched []uint64
	var seqTotal, parTotal int
	for bn, c := range seq {
		seqTotal += c
		if par[bn] != c {
			if par[bn] == 0 {
				droppedLogs = append(droppedLogs, bn)
			} else {
				mismatched = append(mismatched, bn)
			}
		}
	}
	for bn, c := range par {
		parTotal += c
		if seq[bn] == 0 && c > 0 {
			extraLogs = append(extraLogs, bn)
		}
	}
	sort.Slice(droppedLogs, func(i, j int) bool { return droppedLogs[i] < droppedLogs[j] })
	sort.Slice(extraLogs, func(i, j int) bool { return extraLogs[i] < extraLogs[j] })
	sort.Slice(mismatched, func(i, j int) bool { return mismatched[i] < mismatched[j] })

	t.Logf("sequential: %d log-bearing blocks, %d logs; parallel: %d log-bearing blocks, %d logs",
		len(seq), seqTotal, len(par), parTotal)
	if len(droppedLogs) > 0 {
		t.Errorf("PARALLEL DROPPED logs in %d blocks (first 20): %v", len(droppedLogs), firstNBlocks(droppedLogs, 20))
	}
	if len(extraLogs) > 0 {
		t.Errorf("PARALLEL has logs in %d blocks sequential did not (first 20): %v", len(extraLogs), firstNBlocks(extraLogs, 20))
	}
	if len(mismatched) > 0 {
		t.Errorf("PARALLEL per-block log count mismatch in %d blocks (first 20): %v", len(mismatched), firstNBlocks(mismatched, 20))
	}
	if len(droppedLogs) == 0 && len(extraLogs) == 0 && len(mismatched) == 0 {
		t.Logf("OK: parallel fetch matches sequential log-for-log over the 0x1 window")
	}
}

func firstNBlocks(s []uint64, n int) []uint64 {
	if len(s) > n {
		return s[:n]
	}
	return s
}
