//go:build ignore

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

func main() {
	ctx := context.Background()
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  "127.0.0.1:9003",
		Database: "default",
		User:     "default",
		Password: "sqd-clickhouse",
	})
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	// Create DB
	if err := conn.Do(ctx, ch.Query{Body: "CREATE DATABASE IF NOT EXISTS test_v2"}); err != nil {
		panic(err)
	}
	fmt.Println("DB created")

	// Create table
	ddl := `CREATE TABLE IF NOT EXISTS test_v2.blocks (
		chain_id UInt64, block_number UInt64,
		block_timestamp DateTime64(3, 'UTC'), block_hash String
	) ENGINE = MergeTree() ORDER BY (chain_id, block_number)`
	if err := conn.Do(ctx, ch.Query{Body: ddl}); err != nil {
		panic(fmt.Errorf("create table: %w", err))
	}
	fmt.Println("Table created")

	// Insert
	var colChain, colNum proto.ColUInt64
	var colTime proto.ColDateTime64
	var colHash proto.ColStr
	colTime.WithPrecision(proto.Precision(3))
	colTime.WithLocation(time.UTC)

	colChain.Append(137)
	colNum.Append(1)
	colTime.Append(time.Now().UTC())
	colHash.Append("0xabc")

	err = conn.Do(ctx, ch.Query{
		Body: "INSERT INTO test_v2.blocks (chain_id, block_number, block_timestamp, block_hash) VALUES",
		Input: []proto.InputColumn{
			{Name: "chain_id", Data: &colChain},
			{Name: "block_number", Data: &colNum},
			{Name: "block_timestamp", Data: &colTime},
			{Name: "block_hash", Data: &colHash},
		},
	})
	if err != nil {
		panic(fmt.Errorf("insert: %w", err))
	}
	fmt.Println("Insert OK")

	// Read back
	var result proto.ColUInt64
	if err := conn.Do(ctx, ch.Query{
		Body:   "SELECT count() FROM test_v2.blocks",
		Result: proto.Results{{Name: "c", Data: &result}},
	}); err != nil {
		panic(fmt.Errorf("select: %w", err))
	}
	fmt.Printf("Row count: %d\n", result.Row(0))

	// Cleanup
	conn.Do(ctx, ch.Query{Body: "DROP DATABASE IF EXISTS test_v2"})
	fmt.Println("Done")
}
