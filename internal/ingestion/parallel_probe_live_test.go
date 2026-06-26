package ingestion

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

// TestProbePinnedEmptyScan_LBTC answers the design question that decides the
// parallel-fetch policy: in an EMPTY region, does the SQD portal honor a wide
// pinned toBlock (scanning the whole pinned range in one response), or does it
// cap its own scan regardless of the pin?
//
//   - If a wide pin is honored → pinned pages can match the open-ended walk's
//     per-request reach, so the prefetcher never needs an open-ended (no-toBlock)
//     fallback and adaptiveGapMax should be raised to the honored ceiling.
//   - If the portal caps its scan below the pin → open-ended fetch reaches
//     further per request than any pin, so a genuine no-toBlock fallback is
//     warranted for deep-empty stretches.
//
// It issues one request per strategy from the same empty start block and reports
// the marker (last block in the response) each reached. Gated behind
// SQD_LIVE_PORTAL=1.
func TestProbePinnedEmptyScan_LBTC(t *testing.T) {
	if os.Getenv("SQD_LIVE_PORTAL") == "" {
		t.Skip("set SQD_LIVE_PORTAL=1 to run the live pinned-vs-open empty-scan probe")
	}

	const start uint64 = 1_000_000 // deep in the empty pre-LBTC region

	cl := client.New(lbtcEndpoint)
	defer cl.Close()

	reach := func(label string, toBlock *uint64) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		t0 := time.Now()
		resp, err := cl.FetchWithParent(ctx, start, toBlock, "", false /*includeAllBlocks*/, lbtcFilter)
		dur := time.Since(t0)
		if err != nil {
			t.Fatalf("%s: fetch err: %v", label, err)
		}
		if len(resp.Raw) == 0 {
			t.Logf("%-14s -> empty body (no marker), %v", label, dur.Round(time.Millisecond))
			return
		}
		last, err := lastBlockNumber(resp.Raw)
		if err != nil {
			t.Fatalf("%s: marker parse: %v", label, err)
		}
		t.Logf("%-14s -> scanned to block %d (advance %d blocks) in %v",
			label, last, last-start, dur.Round(time.Millisecond))
	}

	pin := func(span uint64) *uint64 { v := start + span - 1; return &v }

	reach("open(no-pin)", nil)
	reach("pin=50k", pin(50_000))
	reach("pin=100k", pin(100_000))
	reach("pin=200k", pin(200_000))
	reach("pin=500k", pin(500_000))
	reach("pin=1M", pin(1_000_000))
}
