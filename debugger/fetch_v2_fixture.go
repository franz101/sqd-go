//go:build ignore

// Command fetch_v2_fixture regenerates the Polymarket V2 OrderFilled parity
// fixture consumed by examples/polymarket/v2_realworld_e2e_test.go.
//
// It fetches a small block range from the SQD portal (server-side filtered to
// the two V2 exchange contracts and the OrderFilled topic), then keeps only the
// logs whose indexed maker is one of the target accounts, and writes the result
// as a compressed JSONL fixture. The output is deterministic: same range + same
// makers => byte-identical decoded events, so the parity test reproduces exact
// positions without committing the data to git.
//
//	go run debugger/fetch_v2_fixture.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed
// taker, ...) — maker is the third topic (index 2).
const orderFilledV2Topic0 = "0xd543adfd945773f1a62f74f0ee55a5e3b9b1a28262980ba90b1a89f2ea84d8ee"

// V2 exchange contracts that emit OrderFilledV2 (see examples/polymarket/config.yaml).
var v2ExchangeAddresses = []string{
	"0xE111180000d2663C0091e4f400237545B87B996B", // ExchangeV2
	"0xe2222d279d744050d28e00520010520000310F59", // NegRiskExchangeV2
}

// Target accounts whose fills make up the fixture.
var defaultMakers = []string{
	"0xf1f0e9fb4823c0cff89c9cb3e82760c73370d2e6",
	"0xf3338c0f5c52e48fbe883d731226b7820e70ba41",
}

type logFilter struct {
	Address []string `json:"address,omitempty"`
	Topic0  []string `json:"topic0,omitempty"`
}

type query struct {
	Type             string                       `json:"type"`
	FromBlock        uint64                       `json:"fromBlock"`
	ToBlock          *uint64                      `json:"toBlock,omitempty"`
	IncludeAllBlocks bool                         `json:"includeAllBlocks"`
	Logs             []logFilter                  `json:"logs,omitempty"`
	Fields           map[string]map[string]bool   `json:"fields,omitempty"`
}

func main() {
	out := flag.String("out", "debugger/data/polymarket_v2_orderfilled/blocks_87200028_87200177.jsonl.zstd", "Output fixture path (.jsonl.zstd)")
	start := flag.Uint64("start", 87200028, "Start block (inclusive)")
	end := flag.Uint64("end", 87200177, "End block (inclusive)")
	endpoint := flag.String("endpoint", "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream", "SQD portal finalized-stream endpoint")
	makerList := flag.String("makers", strings.Join(defaultMakers, ","), "Comma-separated maker addresses to keep")
	flag.Parse()

	makers := map[string]bool{}
	for _, m := range strings.Split(*makerList, ",") {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" {
			continue
		}
		// Logs store indexed addresses left-padded to 32 bytes.
		makers["0x"+strings.Repeat("0", 24)+strings.TrimPrefix(m, "0x")] = true
	}
	if len(makers) == 0 {
		log.Fatal("no makers provided")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	toBlock := *end
	q := query{
		Type:      "evm",
		FromBlock: *start,
		ToBlock:   &toBlock,
		Logs:      []logFilter{{Address: v2ExchangeAddresses, Topic0: []string{orderFilledV2Topic0}}},
		Fields: map[string]map[string]bool{
			"block": {"number": true, "timestamp": true, "hash": true},
			"log":   {"address": true, "topics": true, "data": true, "transactionIndex": true, "logIndex": true, "transactionHash": true},
		},
	}

	body, err := json.Marshal(q)
	if err != nil {
		log.Fatalf("marshal query: %v", err)
	}

	log.Printf("fetching blocks %d..%d (V2 OrderFilled) from %s", *start, *end, *endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *endpoint, bytes.NewReader(body))
	if err != nil {
		log.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		log.Fatalf("http: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.Fatalf("status %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read body: %v", err)
	}

	kept, logs := filterMakerLogs(raw, makers)
	if logs == 0 {
		log.Fatal("no matching maker logs found — fixture would be empty")
	}
	log.Printf("kept %d blocks / %d OrderFilled logs", kept, logs)

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		log.Fatalf("zstd writer: %v", err)
	}
	jsonl := renderJSONL(raw, makers)
	if _, err := enc.Write(jsonl); err != nil {
		log.Fatalf("compress: %v", err)
	}
	if err := enc.Close(); err != nil {
		log.Fatalf("close zstd: %v", err)
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("wrote %s (%d bytes compressed)", *out, buf.Len())
}

// filterMakerLogs counts the blocks and logs that survive the maker filter.
func filterMakerLogs(raw []byte, makers map[string]bool) (blocks, logs int) {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if n := keptLogs(line, makers); n > 0 {
			blocks++
			logs += n
		}
	}
	return blocks, logs
}

// renderJSONL re-emits each block keeping only the maker-matching logs, dropping
// blocks that end up empty. All log fields are preserved verbatim.
func renderJSONL(raw []byte, makers map[string]bool) []byte {
	var buf bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var block map[string]json.RawMessage
		if err := json.Unmarshal(line, &block); err != nil {
			log.Fatalf("parse block: %v", err)
		}
		var rawLogs []json.RawMessage
		if err := json.Unmarshal(block["logs"], &rawLogs); err != nil {
			continue
		}
		kept := rawLogs[:0]
		for _, lg := range rawLogs {
			if matchesMaker(lg, makers) {
				kept = append(kept, lg)
			}
		}
		if len(kept) == 0 {
			continue
		}
		logsJSON, _ := json.Marshal(kept)
		block["logs"] = logsJSON
		out, _ := json.Marshal(block)
		buf.Write(out)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func keptLogs(line []byte, makers map[string]bool) int {
	var block struct {
		Logs []json.RawMessage `json:"logs"`
	}
	if err := json.Unmarshal(line, &block); err != nil {
		return 0
	}
	n := 0
	for _, lg := range block.Logs {
		if matchesMaker(lg, makers) {
			n++
		}
	}
	return n
}

func matchesMaker(lg json.RawMessage, makers map[string]bool) bool {
	var l struct {
		Topics []string `json:"topics"`
	}
	if err := json.Unmarshal(lg, &l); err != nil {
		return false
	}
	if len(l.Topics) < 3 {
		return false
	}
	if !strings.EqualFold(l.Topics[0], orderFilledV2Topic0) {
		return false
	}
	return makers[strings.ToLower(l.Topics[2])]
}
