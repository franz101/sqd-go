package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/holiman/uint256"
)

func main() {
	loadEnv(".env")
	ctx := context.Background()

	host := envOrDefault("CLICKHOUSE_HOST", "127.0.0.1")
	port := envOrDefault("CLICKHOUSE_NATIVE_PORT", "9004")
	user := envOrDefault("CLICKHOUSE_USER", "default")
	pass := envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := envOrDefault("CLICKHOUSE_DATABASE", "polymarket")

	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%s", host, port),
		Database: "default",
		User:     user,
		Password: pass,
	})
	if err != nil {
		log.Fatalf("connect clickhouse: %v", err)
	}
	defer conn.Close()

	var (
		blockNum  proto.ColUInt64
		maker     proto.ColStr
		taker     proto.ColStr
		feeString proto.ColStr
	)

	// Query the generated view 'exchange_order_filled'
	query := fmt.Sprintf(`
		SELECT 
			block_number, 
			maker, 
			taker, 
			fee 
		FROM %s.exchange_order_filled 
		ORDER BY block_number DESC 
		LIMIT 10
	`, quoteIdent(db))

	fmt.Println("Fetching latest 10 Exchange OrderFilled events...")

	var totalFees uint256.Int

	if err := conn.Do(ctx, ch.Query{
		Body: query,
		Result: proto.Results{
			{Name: "block_number", Data: &blockNum},
			{Name: "maker", Data: &maker},
			{Name: "taker", Data: &taker},
			{Name: "fee", Data: &feeString},
		},
		OnResult: func(ctx context.Context, block proto.Block) error {
			for i := 0; i < block.Rows; i++ {
				feeStr := feeString.Row(i)
				var fee uint256.Int
				if feeStr != "" && feeStr != "null" {
					// Parse the fee (removing quotes if present)
					if feeStr[0] == '"' && feeStr[len(feeStr)-1] == '"' {
						feeStr = feeStr[1 : len(feeStr)-1]
					}
					f, err := uint256.FromDecimal(feeStr)
					if err == nil {
						fee = *f
						totalFees.Add(&totalFees, &fee)
					}
				}

				fmt.Printf("Block: %d | Maker: %s | Taker: %s | Fee: %s\n",
					blockNum.Row(i),
					maker.Row(i),
					taker.Row(i),
					fee.Dec(),
				)
			}
			return nil
		},
	}); err != nil {
		log.Fatalf("query clickhouse: %v", err)
	}

	fmt.Printf("\nTotal fees in this batch: %s\n", totalFees.Dec())
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Try parent directory if not found (useful for examples)
		data, err = os.ReadFile("../../" + path)
		if err != nil {
			return
		}
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}
