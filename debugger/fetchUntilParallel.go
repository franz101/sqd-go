//go:build ignore

// fetchUntilParallel is a parallel-range variant of fetchUntil.go. The single
// cursor loop in fetchUntil is gated by serial HTTP round-trip latency to the
// SQD portal (~0.6s/page, ~1.5k blocks/page in dense ranges => ~2.5k blocks/s),
// which makes a full backfill round-trip bound rather than data bound. This
// version splits [start, end] into N contiguous segments and runs one cursor
// loop per segment concurrently, so throughput scales ~N x until the portal or
// the local NIC/CPU saturates.
//
// Usage:
//
//	go run debugger/fetchUntilParallel.go \
//	  -config examples/uniswap/config.yaml \
//	  -start 20500000 -end 22200000 -workers 8 \
//	  -endpoint https://portal.sqd.dev/datasets/ethereum-mainnet/finalized-stream \
//	  -out /tmp/lbtc
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/fastjson"
)

type LogFilter struct {
	Address []string `json:"address,omitempty"`
	Topic0  []string `json:"topic0,omitempty"`
}

type Fields map[string]map[string]bool

type Query struct {
	Type             string      `json:"type"`
	FromBlock        uint64      `json:"fromBlock"`
	ToBlock          *uint64     `json:"toBlock,omitempty"`
	IncludeAllBlocks bool        `json:"includeAllBlocks"`
	Logs             []LogFilter `json:"logs,omitempty"`
	Fields           Fields      `json:"fields,omitempty"`
}

func DefaultEVMFields() Fields {
	return Fields{
		"block": {"number": true, "timestamp": true, "hash": true},
		"log": {
			"address": true, "topics": true, "data": true,
			"transactionIndex": true, "logIndex": true, "transactionHash": true,
		},
	}
}

func main() {
	configPath := flag.String("config", "examples/polymarket/config.yaml", "Path to config.yaml")
	startBlockFlag := flag.Uint64("start", 0, "Start block (inclusive)")
	endBlockFlag := flag.Uint64("end", 0, "End block (inclusive, required)")
	outDir := flag.String("out", "debugger/data", "Output directory for saved jsonl.zstd files")
	endpoint := flag.String("endpoint", "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream", "Subsquid finalized-stream gateway endpoint")
	workers := flag.Int("workers", 4, "Number of concurrent range workers")
	flag.Parse()

	if *endBlockFlag == 0 {
		log.Fatalf("-end is required (parallel mode splits a bounded [start,end] range)")
	}
	if *workers < 1 {
		*workers = 1
	}

	log.Printf("Loading config from %s...", *configPath)
	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if len(cfg.Chains) == 0 {
		log.Fatalf("No chains configured in %s", *configPath)
	}
	chain := cfg.Chains[0]

	start := chain.StartBlock
	if *startBlockFlag > 0 {
		start = *startBlockFlag
	}
	end := *endBlockFlag
	if end < start {
		log.Fatalf("end block %d < start block %d", end, start)
	}

	log.Printf("Building event decoders and filters from contract configurations...")
	_, decFilters, err := parser.BuildEventDecoder(chain.Contracts)
	if err != nil {
		log.Fatalf("Failed to build event filters: %v", err)
	}
	var logs []LogFilter
	for _, f := range decFilters {
		logs = append(logs, LogFilter{Address: f.Address, Topic0: f.Topic0})
	}
	log.Printf("Built %d event log filters", len(logs))

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Printf("Termination signal received. Gracefully stopping...")
		cancel()
	}()

	// Split [start, end] into N contiguous, disjoint segments.
	total := end - start + 1
	seg := total / uint64(*workers)
	if seg == 0 {
		seg = 1
	}
	type rng struct{ from, to uint64 }
	var segments []rng
	for w := 0; w < *workers; w++ {
		s := start + uint64(w)*seg
		if s > end {
			break
		}
		e := s + seg - 1
		if w == *workers-1 || e > end {
			e = end
		}
		segments = append(segments, rng{s, e})
	}
	log.Printf("Range [%d, %d] (%d blocks) split into %d segments across %d workers",
		start, end, total, len(segments), *workers)

	var blocksCounter atomic.Uint64
	startTime := time.Now()

	// Stats goroutine: avg and current blocks/s every 10s.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		var lastCount uint64
		lastCheck := startTime
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				totalBlocks := blocksCounter.Load()
				elapsed := now.Sub(startTime).Seconds()
				intervalBlocks := totalBlocks - lastCount
				intervalElapsed := now.Sub(lastCheck).Seconds()
				var bpsTotal, bpsInterval float64
				if elapsed > 0 {
					bpsTotal = float64(totalBlocks) / elapsed
				}
				if intervalElapsed > 0 {
					bpsInterval = float64(intervalBlocks) / intervalElapsed
				}
				log.Printf("[STATS] Elapsed: %.1fs | Total Blocks: %d/%d (%.1f%%) | Avg Blocks/s: %.2f | Current Blocks/s: %.2f",
					elapsed, totalBlocks, total, float64(totalBlocks)/float64(total)*100, bpsTotal, bpsInterval)
				lastCount = totalBlocks
				lastCheck = now
			}
		}
	}()

	var wg sync.WaitGroup
	for i, s := range segments {
		wg.Add(1)
		go func(id int, segFrom, segTo uint64) {
			defer wg.Done()
			runSegment(ctx, id, segFrom, segTo, *endpoint, *outDir, logs, &blocksCounter)
		}(i, s.from, s.to)
	}
	wg.Wait()

	totalDuration := time.Since(startTime)
	totalBlocks := blocksCounter.Load()
	var bps float64
	if totalDuration.Seconds() > 0 {
		bps = float64(totalBlocks) / totalDuration.Seconds()
	}
	log.Printf("[DONE] Parallel fetch complete! Workers: %d | Blocks: %d/%d | Duration: %.2fs | Throughput: %.2f blocks/s",
		len(segments), totalBlocks, total, totalDuration.Seconds(), bps)
}

// runSegment fetches [segFrom, segTo] with its own cursor loop. ToBlock is
// pinned to segTo so a worker never crosses into another worker's range.
func runSegment(ctx context.Context, id int, segFrom, segTo uint64, endpoint, outDir string, logs []LogFilter, counter *atomic.Uint64) {
	httpClient := &http.Client{Timeout: 60 * time.Second}
	zstdDecoder, err := zstd.NewReader(nil)
	if err != nil {
		log.Fatalf("[w%d] zstd decoder: %v", id, err)
	}
	defer zstdDecoder.Close()

	// Stagger worker starts so N workers don't fire simultaneously and drain
	// the portal's burst bucket in lockstep.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(id) * 150 * time.Millisecond):
	}

	// backoff sleeps with jitter so workers that hit 429 together desynchronize
	// instead of retrying in phase. attempt resets on every success.
	backoff := func(attempt int) {
		d := time.Duration(150*(1<<min(attempt, 4))) * time.Millisecond // 150ms..2.4s
		d += time.Duration(rand.Intn(250)) * time.Millisecond
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}

	fromBlock := segFrom
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if fromBlock > segTo {
			return
		}
		toBlock := segTo
		q := Query{
			Type:             "evm",
			FromBlock:        fromBlock,
			ToBlock:          &toBlock,
			IncludeAllBlocks: false,
			Logs:             logs,
			Fields:           DefaultEVMFields(),
		}
		body, err := json.Marshal(q)
		if err != nil {
			log.Fatalf("[w%d] marshal: %v", id, err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			log.Fatalf("[w%d] new request: %v", id, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "zstd")

		resp, err := httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attempt++
			backoff(attempt)
			continue
		}
		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			return // reached end of available data for this segment
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			attempt++
			backoff(attempt) // throttled: jittered backoff, no log spam
			continue
		}
		if resp.StatusCode != http.StatusOK {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			log.Printf("[w%d] Status %d: %s. Retrying...", id, resp.StatusCode, bytes.TrimSpace(detail))
			attempt++
			backoff(attempt)
			continue
		}
		attempt = 0 // success: reset backoff
		rawBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[w%d] read body: %v. Retrying...", id, err)
			continue
		}
		if len(rawBytes) == 0 {
			return
		}

		var decompressed []byte
		if resp.Header.Get("Content-Encoding") == "zstd" {
			decompressed, err = zstdDecoder.DecodeAll(rawBytes, nil)
			if err != nil {
				log.Fatalf("[w%d] decompress: %v", id, err)
			}
		} else {
			decompressed = rawBytes
		}
		lastBlock, err := findLastBlockNumber(decompressed)
		if err != nil {
			log.Fatalf("[w%d] find last block: %v", id, err)
		}

		outFileName := fmt.Sprintf("blocks_%d_%d.jsonl.zstd", fromBlock, lastBlock)
		outFilePath := filepath.Join(outDir, outFileName)
		var bytesToWrite []byte
		if resp.Header.Get("Content-Encoding") == "zstd" {
			bytesToWrite = rawBytes
		} else {
			var buf bytes.Buffer
			enc, err := zstd.NewWriter(&buf)
			if err != nil {
				log.Fatalf("[w%d] zstd writer: %v", id, err)
			}
			if _, err := enc.Write(rawBytes); err != nil {
				log.Fatalf("[w%d] compress: %v", id, err)
			}
			enc.Close()
			bytesToWrite = buf.Bytes()
		}
		if err := os.WriteFile(outFilePath, bytesToWrite, 0644); err != nil {
			log.Fatalf("[w%d] write file: %v", id, err)
		}

		counter.Add(lastBlock - fromBlock + 1)
		fromBlock = lastBlock + 1
	}
}

func findLastBlockNumber(decompressed []byte) (uint64, error) {
	var lastBlock uint64
	var found bool
	lines := bytes.Split(decompressed, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var p fastjson.Parser
		val, err := p.ParseBytes(line)
		if err != nil {
			return 0, fmt.Errorf("parse JSON line: %w", err)
		}
		lastBlock = val.Get("header").GetUint64("number")
		found = true
		break
	}
	if !found {
		return 0, fmt.Errorf("no blocks found in response")
	}
	return lastBlock, nil
}
