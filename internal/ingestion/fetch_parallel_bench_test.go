// Real-portal fetch throughput measurement — READ ONLY, no ClickHouse, no writes.
//
// Diagnoses why deep backfill is fetch-bound (~47 blk/s, single producer stream).
// It replays the EXACT producer fetch shape (finalized-stream endpoint,
// includeAllBlocks=true == cursorMode, real contract log filters) against the
// live SQD portal and compares:
//
//   - serial single-stream throughput (today's behaviour)
//   - parallel throughput at conc 4/8/16 (does fetch scale with connections?)
//   - bigger pages (does the portal amortize per-request latency, or cap size?)
//
// Each arm uses a DISJOINT block region so server-side caching can't make a
// later arm look artificially fast.
//
// Run:
//
//	FETCH_BENCH=1 go test ./internal/ingestion/ -run TestFetchParallelism -v -count=1 -timeout 900s
package ingestion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/klauspost/compress/zstd"
)

// streamingFetch decodes + counts the NDJSON response INCREMENTALLY: it wraps the
// HTTP body in a streaming zstd reader and scans line-by-line, so network receive,
// decompression and line-counting overlap and no 60 MB blob is ever materialized.
// This is the head-to-head against the client's io.ReadAll+DecodeAll path.
func streamingFetch(ctx context.Context, hc *http.Client, endpoint string, from, to uint64, includeAll bool, filters []client.LogFilter) (blocks int, nbytes int64, err error) {
	q := client.Query{
		Type: "evm", FromBlock: from, ToBlock: &to,
		IncludeAllBlocks: includeAll, Logs: filters, Fields: client.DefaultEVMFields(),
	}
	body, _ := json.Marshal(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "zstd")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, 0, fmt.Errorf("status %d: %s", resp.StatusCode, detail)
	}
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "zstd" {
		zr, zerr := zstd.NewReader(resp.Body)
		if zerr != nil {
			return 0, 0, zerr
		}
		defer zr.Close()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 96<<20)
	for sc.Scan() {
		blocks++
		nbytes += int64(len(sc.Bytes())) + 1
	}
	return blocks, nbytes, sc.Err()
}

// countNDJSONBlocks counts blocks in a portal response (one block per line).
func countNDJSONBlocks(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	n := 0
	for _, b := range raw {
		if b == '\n' {
			n++
		}
	}
	if raw[len(raw)-1] != '\n' {
		n++
	}
	return n
}

func TestFetchParallelism(t *testing.T) {
	if os.Getenv("FETCH_BENCH") != "1" {
		t.Skip("set FETCH_BENCH=1 to run the real-portal fetch parallelism measurement")
	}
	projPath := envOr("FETCH_BENCH_PROJECT", "../../examples/polymarket")
	project, err := config.LoadProject(projPath)
	if err != nil {
		t.Fatalf("LoadProject(%s): %v", projPath, err)
	}
	cfg := project.Config
	if len(cfg.Chains) == 0 {
		t.Fatalf("no chains in %s", projPath)
	}
	chain := cfg.Chains[0]
	_, filters, err := parser.BuildEventDecoder(chain.Contracts)
	if err != nil {
		t.Fatalf("BuildEventDecoder: %v", err)
	}
	endpoint := chainEndpoint(chain.ID, false) // finalized-stream, same as deep backfill
	includeAll := os.Getenv("FETCH_BENCH_NOALL") != "1"
	base := envUintOr("FETCH_BENCH_BASE", 84_200_000)
	t.Logf("endpoint=%s chain=%d filters=%d includeAllBlocks=%v base=%d",
		endpoint, chain.ID, len(filters), includeAll, base)

	ctx := context.Background()

	// region cursor: each arm consumes a disjoint span so no cross-arm caching.
	var region uint64
	nextRegion := func(spanBlocks uint64) uint64 {
		r := base + region
		region += spanBlocks
		return r
	}
	mkRanges := func(start, size uint64, n int) [][2]uint64 {
		out := make([][2]uint64, n)
		for i := 0; i < n; i++ {
			from := start + uint64(i)*size
			out[i] = [2]uint64{from, from + size - 1}
		}
		return out
	}

	measure := func(label string, ranges [][2]uint64, conc int) (blkPerSec float64) {
		var blocks, bytes atomic.Int64
		sem := make(chan struct{}, conc)
		var wg sync.WaitGroup
		t0 := time.Now()
		for _, rng := range ranges {
			wg.Add(1)
			sem <- struct{}{}
			go func(from, to uint64) {
				defer wg.Done()
				defer func() { <-sem }()
				c := client.New(endpoint)
				defer c.Close()
				resp, err := c.FetchWithParent(ctx, from, &to, "", includeAll, filters)
				if err != nil {
					t.Logf("  [%s] %d-%d err: %v", label, from, to, err)
					return
				}
				blocks.Add(int64(countNDJSONBlocks(resp.Raw)))
				bytes.Add(int64(len(resp.Raw)))
			}(rng[0], rng[1])
		}
		wg.Wait()
		dur := time.Since(t0)
		blk := blocks.Load()
		mb := float64(bytes.Load()) / 1e6
		blkPerSec = float64(blk) / dur.Seconds()
		perReq := 0
		if len(ranges) > 0 {
			perReq = int(blk) / len(ranges)
		}
		t.Logf("%-20s conc=%2d reqs=%3d => %6d blk (%4d/req) %7.1f MB in %7v | %6.1f blk/s  %5.1f MB/s",
			label, conc, len(ranges), blk, perReq, mb, dur.Round(time.Millisecond), blkPerSec, mb/dur.Seconds())
		return blkPerSec
	}

	N := int(envUintOr("FETCH_BENCH_N", 16))

	// 1) Serial single-stream baseline — today's producer.
	serial := measure("serial-200", mkRanges(nextRegion(uint64(N)*200), 200, N), 1)

	// 2) Parallel 200-block requests at increasing concurrency.
	for _, c := range []int{4, 8, 16} {
		bps := measure(fmt.Sprintf("parallel-200-c%d", c), mkRanges(nextRegion(uint64(N)*200), 200, N), c)
		if serial > 0 {
			t.Logf("   -> %.2fx vs serial-200", bps/serial)
		}
	}

	// 3) Bigger pages serial — does the portal amortize latency or cap response?
	measure("serial-800", mkRanges(nextRegion(8*800), 800, 8), 1)
	measure("serial-2000", mkRanges(nextRegion(8*2000), 2000, 8), 1)

	// 4) Bigger pages + parallel (the likely winning combo).
	bigPar := measure("parallel-2000-c8", mkRanges(nextRegion(8*2000), 2000, 8), 8)
	if serial > 0 {
		t.Logf("   -> %.2fx vs serial-200 (combined page+parallel)", bigPar/serial)
	}
}

// TestFetchStreamingVsBuffered isolates whether the ~34 MB/s parallel plateau is
// the CLIENT (whole-response buffering + GC) or the SERVER (per-IP rate). It pits
// the streaming decoder against the buffered client.New() path at matched
// concurrency, plus a higher-concurrency streaming run and an includeAllBlocks=false
// comparison. READ ONLY, no ClickHouse.
//
//	FETCH_BENCH=1 go test ./internal/ingestion/ -run TestFetchStreamingVsBuffered -v -count=1 -timeout 900s
func TestFetchStreamingVsBuffered(t *testing.T) {
	if os.Getenv("FETCH_BENCH") != "1" {
		t.Skip("set FETCH_BENCH=1 to run the streaming-vs-buffered measurement")
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
	base := envUintOr("FETCH_BENCH_BASE", 84_400_000)
	ctx := context.Background()

	var region uint64
	mkRanges := func(size uint64, n int) [][2]uint64 {
		start := base + region
		region += uint64(n) * size
		out := make([][2]uint64, n)
		for i := 0; i < n; i++ {
			from := start + uint64(i)*size
			out[i] = [2]uint64{from, from + size - 1}
		}
		return out
	}

	// Shared transport so streaming arms reuse connections per goroutine pool.
	hc := &http.Client{Transport: &http.Transport{
		DisableCompression:  true,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
	}}

	runArm := func(label string, ranges [][2]uint64, conc int, includeAll, streaming bool) {
		var blocks, nbytes atomic.Int64
		sem := make(chan struct{}, conc)
		var wg sync.WaitGroup
		t0 := time.Now()
		for _, rng := range ranges {
			wg.Add(1)
			sem <- struct{}{}
			go func(from, to uint64) {
				defer wg.Done()
				defer func() { <-sem }()
				if streaming {
					b, n, err := streamingFetch(ctx, hc, endpoint, from, to, includeAll, filters)
					if err != nil {
						t.Logf("  [%s] %d-%d err: %v", label, from, to, err)
						return
					}
					blocks.Add(int64(b))
					nbytes.Add(n)
				} else {
					c := client.New(endpoint)
					defer c.Close()
					resp, err := c.FetchWithParent(ctx, from, &to, "", includeAll, filters)
					if err != nil {
						t.Logf("  [%s] %d-%d err: %v", label, from, to, err)
						return
					}
					blocks.Add(int64(countNDJSONBlocks(resp.Raw)))
					nbytes.Add(int64(len(resp.Raw)))
				}
			}(rng[0], rng[1])
		}
		wg.Wait()
		dur := time.Since(t0)
		blk := blocks.Load()
		mb := float64(nbytes.Load()) / 1e6
		t.Logf("%-26s conc=%2d allBlk=%-5v stream=%-5v => %5d blk %7.1f MB in %7v | %6.1f blk/s %5.1f MB/s",
			label, conc, includeAll, streaming, blk, mb, dur.Round(time.Millisecond),
			float64(blk)/dur.Seconds(), mb/dur.Seconds())
	}

	N := int(envUintOr("FETCH_BENCH_N", 16))
	// per-stream: streaming vs buffered
	runArm("buffered-200-c1", mkRanges(200, 8), 1, true, false)
	runArm("streaming-200-c1", mkRanges(200, 8), 1, true, true)
	// ceiling: is 34 MB/s client or server?
	runArm("buffered-400-c16", mkRanges(400, N), 16, true, false)
	runArm("streaming-400-c16", mkRanges(400, N), 16, true, true)
	runArm("streaming-400-c24", mkRanges(400, N), 24, true, true)
	// does dropping fork headers (backfill stage) help?
	runArm("streaming-400-c16-noAll", mkRanges(400, N), 16, false, true)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envUintOr(k string, def uint64) uint64 {
	if v := os.Getenv(k); v != "" {
		var out uint64
		if _, err := fmt.Sscan(v, &out); err == nil {
			return out
		}
	}
	return def
}
