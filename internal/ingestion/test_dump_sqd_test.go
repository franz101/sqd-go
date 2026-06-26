package ingestion
import (
	"context"
	"fmt"
	"github.com/franz101/sqd-go/internal/client"
    "testing"
)

func TestDumpFormat(t *testing.T) {
	cl := client.New("https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream")
	defer cl.Close()
	to := uint64(86121000)
	resp, err := cl.FetchWithParent(context.Background(), 86121000, &to, "", false, polymarket0x1Filter)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	fmt.Printf("Raw: %s\n", string(resp.Raw[:1000]))
}
