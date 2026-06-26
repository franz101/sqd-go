package ingestion
import (
	"context"
	"fmt"
	"github.com/franz101/sqd-go/internal/client"
    "testing"
)

func TestCheckFormat(t *testing.T) {
	cl := client.New("https://portal.sqd.dev/datasets/ethereum-mainnet")
	defer cl.Close()
	to := uint64(20000000)
	resp, err := cl.FetchWithParent(context.Background(), 20000000, &to, "", false, []client.LogFilter{{Address: []string{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"}}})
	if err != nil {
		fmt.Printf("Err: %v\n", err)
		return
	}
	fmt.Printf("Raw: %s\n", string(resp.Raw[:200]))
}
