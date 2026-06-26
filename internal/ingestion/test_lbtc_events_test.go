package ingestion
import (
	"context"
	"fmt"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/examples/uniswap/generated"
	"testing"
)

func TestLBTC(t *testing.T) {
	cl := client.New("https://portal.sqd.dev/datasets/ethereum-mainnet/finalized-stream")
	defer cl.Close()

	to := uint64(20601000)
	filter := []client.LogFilter{{
		Address: []string{"0x8236a87084f8B84306f72007F36F2618A5634494"},
		Topic0:  []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
	}}
	resp, err := cl.FetchWithParent(context.Background(), 20600000, &to, "", false, filter)
	if err != nil {
		t.Fatalf("Err: %v\n", err)
	}
	fmt.Printf("Raw bytes fetched: %d\n", len(resp.Raw))
	fmt.Printf("RAW:\n%s\n", string(resp.Raw))
	
	batches := generated.NewInsertBatches()
	events, err := generated.ParseJSONL(resp.Raw, batches, nil)
	fmt.Printf("Parsed Events: %d, err: %v\n", events, err)
}
