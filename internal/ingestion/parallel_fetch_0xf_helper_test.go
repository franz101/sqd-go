package ingestion

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/franz101/sqd-go/internal/client"
)

func clientNew(endpoint string) *client.Client { return client.New(endpoint) }

// polymarket0x1Filter matches every polymarket contract the indexer cares about
// over the wallet 0x10f5b9bd…6701 window (2026-04-28, CLOB V2).
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

func firstNBlocks(s []uint64, n int) []uint64 {
	if len(s) > n {
		return s[:n]
	}
	return s
}
