package uniswap_test

import (
	"context"
	"testing"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/examples/uniswap/generated"
)

func TestUniswapFastIntegration(t *testing.T) {
	cl := client.New("https://portal.sqd.dev/datasets/ethereum-mainnet/finalized-stream")
	defer cl.Close()

	// LBTC was deployed around block 20,550,000. 
	// The range [20,600,000, 20,601,000] contains 86 LBTC Transfer events.
	to := uint64(20601000)
	filter := []client.LogFilter{{
		Address: []string{"0x8236a87084f8B84306f72007F36F2618A5634494"},
		Topic0:  []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
	}}
	resp, err := cl.FetchWithParent(context.Background(), 20600000, &to, "", false, filter)
	if err != nil {
		t.Fatalf("Failed to fetch from Subsquid: %v", err)
	}
	
	if len(resp.Raw) == 0 {
		t.Fatalf("Expected non-empty response from Subsquid")
	}

	batches := generated.NewInsertBatches()
	events, err := generated.ParseJSONL(resp.Raw, batches, nil)
	if err != nil {
		t.Fatalf("Failed to parse JSONL: %v", err)
	}
	
	if events == 0 {
		t.Fatalf("Expected to parse events for LBTC in this block range, but got 0. This proves the parsing works on non-empty blocks that actually contain matching events.")
	}
	
	t.Logf("Successfully parsed %d events from the fetched block range.", events)
}
