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
	startBlockFlag := flag.Uint64("start", 0, "Start block override (0 to use config start block)")
	endBlockFlag := flag.Uint64("end", 0, "End block (optional)")
	outDir := flag.String("out", "debugger/data", "Output directory for saved jsonl.zstd files")
	endpoint := flag.String("endpoint", "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream", "Subsquid finalized-stream gateway endpoint")
	flag.Parse()

	log.Printf("Loading config from %s...", *configPath)
	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if len(cfg.Chains) == 0 {
		log.Fatalf("No chains configured in %s", *configPath)
	}
	chain := cfg.Chains[0]

	fromBlock := chain.StartBlock
	if *startBlockFlag > 0 {
		fromBlock = *startBlockFlag
	}

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

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
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

	for {
		if ctx.Err() != nil {
			break
		}
		if *endBlockFlag > 0 && fromBlock > *endBlockFlag {
			log.Printf("Reached end block %d. Stopping.", *endBlockFlag)
			break
		}

		var toBlockPtr *uint64
		if *endBlockFlag > 0 {
			toBlockPtr = endBlockFlag
		}

		q := Query{
			Type:             "evm",
			FromBlock:        fromBlock,
			ToBlock:          toBlockPtr,
			IncludeAllBlocks: false,
			Logs:             logs,
			Fields:           DefaultEVMFields(),
		}

		body, err := json.Marshal(q)
		if err != nil {
			log.Fatalf("Failed to marshal query: %v", err)
		}

		log.Printf("Fetching chunk starting from block %d...", fromBlock)
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
			log.Printf("HTTP request failed: %v. Retrying in 5 seconds...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			log.Printf("Received 204 No Content. Done or waiting.")
			break
		}

		if resp.StatusCode != http.StatusOK {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			log.Printf("Error: Status %d: %s. Retrying in 5 seconds...", resp.StatusCode, bytes.TrimSpace(detail))
			time.Sleep(5 * time.Second)
			continue
		}

		rawBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("Failed to read response body: %v. Retrying...", err)
			continue
		}

		if len(rawBytes) == 0 {
			log.Printf("Received empty response body. Stopping.")
			break
		}

		var decompressed []byte
		if resp.Header.Get("Content-Encoding") == "zstd" {
			decompressed, err = zstdDecoder.DecodeAll(rawBytes, nil)
			if err != nil {
				log.Fatalf("Failed to decompress zstd response: %v", err)
			}
		} else {
			decompressed = rawBytes
		}

		lastBlock, err := findLastBlockNumber(decompressed)
		if err != nil {
			log.Fatalf("Failed to find last block number in response: %v", err)
		}

		outFileName := fmt.Sprintf("blocks_%d_%d.jsonl.zstd", fromBlock, lastBlock)
		outFilePath := filepath.Join(*outDir, outFileName)

		var bytesToWrite []byte
		if resp.Header.Get("Content-Encoding") == "zstd" {
			bytesToWrite = rawBytes
		} else {
			var buf bytes.Buffer
			zstdEncoder, err := zstd.NewWriter(&buf)
			if err != nil {
				log.Fatalf("Failed to create zstd encoder: %v", err)
			}
			_, err = zstdEncoder.Write(rawBytes)
			if err != nil {
				log.Fatalf("Failed to compress raw bytes: %v", err)
			}
			zstdEncoder.Close()
			bytesToWrite = buf.Bytes()
		}

		if err := os.WriteFile(outFilePath, bytesToWrite, 0644); err != nil {
			log.Fatalf("Failed to write output file: %v", err)
		}

		log.Printf("Saved chunk to %s (blocks %d to %d)", outFilePath, fromBlock, lastBlock)

		numBlocks := lastBlock - fromBlock + 1
		blocksCounter.Add(numBlocks)
		fromBlock = lastBlock + 1
	}

	totalDuration := time.Since(startTime)
	totalBlocks := blocksCounter.Load()
	log.Printf("[DONE] Fetching complete! Total Blocks Saved: %d | Total Duration: %.2fs", totalBlocks, totalDuration.Seconds())
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
