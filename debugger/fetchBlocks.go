//go:build ignore

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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
		"block": {
			"number": true, "timestamp": true, "hash": true,
		},
		"log": {
			"address": true, "topics": true, "data": true,
			"transactionIndex": true, "logIndex": true, "transactionHash": true,
		},
	}
}

func main() {
	configPath := flag.String("config", "examples/polymarket/config.yaml", "Path to config.yaml")
	blocksFile := flag.String("blocks", "", "File with block numbers (one per line)")
	outDir := flag.String("out", "debugger/data", "Output directory for saved jsonl.zstd files")
	endpoint := flag.String("endpoint", "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream", "Subsquid finalized-stream gateway endpoint")
	flag.Parse()

	if *blocksFile == "" {
		log.Fatal("--blocks is required (file with block numbers, one per line)")
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

	log.Printf("Building event decoders and filters from contract configurations...")
	_, decFilters, err := parser.BuildEventDecoder(chain.Contracts)
	if err != nil {
		log.Fatalf("Failed to build event filters: %v", err)
	}

	var logs []LogFilter
	for _, f := range decFilters {
		logs = append(logs, LogFilter{
			Address: f.Address,
			Topic0:  f.Topic0,
		})
	}

	log.Printf("Built %d event log filters", len(logs))

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	// Read and parse block numbers from file
	blockSet, err := readBlockFile(*blocksFile)
	if err != nil {
		log.Fatalf("Failed to read blocks file: %v", err)
	}

	if len(blockSet) == 0 {
		log.Fatal("No blocks found in file")
	}

	// Sort blocks and group into contiguous ranges
	sorted := make([]uint64, 0, len(blockSet))
	for b := range blockSet {
		sorted = append(sorted, b)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	log.Printf("Loaded %d target blocks (range %d - %d)", len(sorted), sorted[0], sorted[len(sorted)-1])

	httpClient := &http.Client{
		Timeout: 60 * time.Second,
	}

	zstdDecoder, err := zstd.NewReader(nil)
	if err != nil {
		log.Fatalf("Failed to create zstd decoder: %v", err)
	}
	defer zstdDecoder.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Printf("Termination signal received. Gracefully stopping...")
		cancel()
	}()

	var blocksCounter atomic.Uint64
	startTime := time.Now()

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

				log.Printf("[STATS] Elapsed: %.1fs | Total Blocks: %d | Avg Blocks/s: %.2f | Current Blocks/s: %.2f",
					elapsed, totalBlocks, bpsTotal, bpsInterval)

				lastCount = totalBlocks
				lastCheck = now
			}
		}
	}()

	// Group blocks into contiguous ranges (gap <= 1 = same range)
	ranges := groupIntoRanges(sorted)
	log.Printf("Grouped into %d fetch ranges", len(ranges))

	for i, r := range ranges {
		if ctx.Err() != nil {
			break
		}

		fromBlock := r[0]
		toBlock := r[1]

		log.Printf("[Range %d/%d] Fetching blocks %d to %d (%d blocks)", i+1, len(ranges), fromBlock, toBlock, toBlock-fromBlock+1)

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
			log.Fatalf("Failed to marshal query: %v", err)
		}

		var rawBytes []byte
		var lastBlock uint64
		var fetched bool

		for attempt := 0; attempt < 3; attempt++ {
			if ctx.Err() != nil {
				break
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, *endpoint, bytes.NewReader(body))
			if err != nil {
				log.Fatalf("Failed to create HTTP request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "zstd")

			resp, err := httpClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				log.Printf("HTTP request failed (attempt %d): %v. Retrying in 5 seconds...", attempt+1, err)
				time.Sleep(5 * time.Second)
				continue
			}

			if resp.StatusCode == http.StatusNoContent {
				resp.Body.Close()
				log.Printf("Received 204 No Content for range %d-%d. Skipping.", fromBlock, toBlock)
				fetched = true
				break
			}

			if resp.StatusCode != http.StatusOK {
				detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				log.Printf("Error (attempt %d): Status %d: %s. Retrying in 5 seconds...", attempt+1, resp.StatusCode, bytes.TrimSpace(detail))
				time.Sleep(5 * time.Second)
				continue
			}

			rawBytes, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				log.Printf("Failed to read response body (attempt %d): %v. Retrying...", attempt+1, err)
				continue
			}

			if len(rawBytes) == 0 {
				log.Printf("Received empty response body for range %d-%d.", fromBlock, toBlock)
				fetched = true
				break
			}

			// Decompress to find last block number
			var decompressed []byte
			if resp.Header.Get("Content-Encoding") == "zstd" {
				decompressed, err = zstdDecoder.DecodeAll(rawBytes, nil)
				if err != nil {
					log.Fatalf("Failed to decompress zstd response: %v", err)
				}
			} else {
				decompressed = rawBytes
			}

			lastBlock, err = findLastBlockNumber(decompressed)
			if err != nil {
				log.Fatalf("Failed to find last block number in response: %v", err)
			}

			// Filter to only keep blocks in our target set
			filtered := filterBlocksBySet(decompressed, blockSet)

			if len(filtered) == 0 {
				log.Printf("No target blocks found in response for range %d-%d.", fromBlock, toBlock)
				fetched = true
				break
			}

			// Save filtered data
			outFileName := fmt.Sprintf("blocks_%d_%d.jsonl.zstd", fromBlock, lastBlock)
			outFilePath := filepath.Join(*outDir, outFileName)

			var bytesToWrite []byte
			if resp.Header.Get("Content-Encoding") == "zstd" {
				// Re-compress the filtered data
				var buf bytes.Buffer
				zstdEncoder, err := zstd.NewWriter(&buf)
				if err != nil {
					log.Fatalf("Failed to create zstd encoder: %v", err)
				}
				_, err = zstdEncoder.Write(filtered)
				if err != nil {
					log.Fatalf("Failed to compress filtered bytes: %v", err)
				}
				zstdEncoder.Close()
				bytesToWrite = buf.Bytes()
			} else {
				bytesToWrite = filtered
			}

			if err := os.WriteFile(outFilePath, bytesToWrite, 0644); err != nil {
				log.Fatalf("Failed to write output file: %v", err)
			}

			numBlocks := countBlocks(filtered)
			blocksCounter.Add(numBlocks)

			log.Printf("Saved %d target blocks to %s (range %d-%d)", numBlocks, outFilePath, fromBlock, lastBlock)
			fetched = true
			break
		}

		if !fetched {
			log.Printf("FAILED to fetch range %d-%d after retries", fromBlock, toBlock)
		}
	}

	totalDuration := time.Since(startTime)
	totalBlocks := blocksCounter.Load()
	log.Printf("[DONE] Fetching complete! Total Target Blocks Saved: %d | Total Duration: %.2fs", totalBlocks, totalDuration.Seconds())
}

func readBlockFile(path string) (map[uint64]struct{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	blocks := make(map[uint64]struct{})
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		var b uint64
		if _, err := fmt.Sscanf(string(line), "%d", &b); err != nil {
			return nil, fmt.Errorf("parse block number %q: %w", string(line), err)
		}
		blocks[b] = struct{}{}
	}

	return blocks, nil
}

func groupIntoRanges(sorted []uint64) [][2]uint64 {
	if len(sorted) == 0 {
		return nil
	}

	var ranges [][2]uint64
	start := sorted[0]
	prev := sorted[0]

	for i := 1; i < len(sorted); i++ {
		if sorted[i] > prev+1 {
			ranges = append(ranges, [2]uint64{start, prev})
			start = sorted[i]
		}
		prev = sorted[i]
	}
	ranges = append(ranges, [2]uint64{start, prev})

	return ranges
}

func filterBlocksBySet(decompressed []byte, blockSet map[uint64]struct{}) []byte {
	var output bytes.Buffer
	lines := bytes.Split(decompressed, []byte("\n"))

	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var p fastjson.Parser
		val, err := p.ParseBytes(line)
		if err != nil {
			// Keep non-JSON lines (shouldn't happen but be safe)
			output.Write(line)
			output.WriteByte('\n')
			continue
		}

		num := val.Get("header").GetUint64("number")
		if _, ok := blockSet[num]; ok {
			output.Write(line)
			output.WriteByte('\n')
		}
	}

	return output.Bytes()
}

func countBlocks(data []byte) uint64 {
	var count uint64
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			count++
		}
	}
	return count
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

		num := val.Get("header").GetUint64("number")
		lastBlock = num
		found = true
		break
	}

	if !found {
		return 0, fmt.Errorf("no blocks found in response")
	}
	return lastBlock, nil
}
