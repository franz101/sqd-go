//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	polymarketgamma "github.com/ivanzzeth/polymarket-go-gamma-client"
)

func main() {
	ctx := context.Background()
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  "127.0.0.1:9003",
		Database: "polymarket",
		User:     "default",
		Password: "sqd-clickhouse",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// Fetch 2 pages, 1 market each (different IDs)
	httpClient := &http.Client{Timeout: 30 * time.Second}

	type kr struct {
		Markets    []polymarketgamma.Market `json:"markets"`
		NextCursor string                   `json:"next_cursor"`
	}

	// Page 1
	resp, _ := httpClient.Get("https://gamma-api.polymarket.com/markets/keyset?limit=1&order=id&ascending=true")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var r1 kr
	json.Unmarshal(body, &r1)
	log.Printf("Market 1: id=%s", r1.Markets[0].ID)

	// Page 2
	cursor := r1.NextCursor
	resp2, _ := httpClient.Get(fmt.Sprintf("https://gamma-api.polymarket.com/markets/keyset?limit=1&order=id&ascending=true&next_cursor=%s", cursor))
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	var r2 kr
	json.Unmarshal(body2, &r2)
	log.Printf("Market 2: id=%s", r2.Markets[0].ID)

	if r1.Markets[0].ID == r2.Markets[0].ID {
		log.Fatalf("Same ID! Pages didn't advance")
	}

	// Now insert THESE TWO markets using the FETCHMARKETS pattern (reused columns)
	var (
		colID     proto.ColStr
		colQ      proto.ColStr
		colCID    proto.ColFixedStr
		colSlug   proto.ColStr
		colQID    proto.ColFixedStr
	)
	colCID.SetSize(common.HashLength)
	colQID.SetSize(common.HashLength)

	columns := []proto.InputColumn{
		{Name: "id", Data: &colID},
		{Name: "question", Data: &colQ},
		{Name: "condition_id", Data: &colCID},
		{Name: "slug", Data: &colSlug},
		{Name: "question_id", Data: &colQID},
	}

	insertBody := "INSERT INTO polymarket.markets VALUES"

	z32 := make([]byte, 32)

	// Append market 1
	colID.Append(r1.Markets[0].ID)
	colQ.Append(r1.Markets[0].Question)
	colCID.Append(z32)
	colSlug.Append(r1.Markets[0].Slug)
	colQID.Append(z32)

	log.Printf("Before flush 1: colID rows=%d", colID.Rows())

	err = conn.Do(ctx, ch.Query{Body: insertBody, Input: columns})
	if err != nil {
		log.Fatalf("flush 1: %v", err)
	}
	log.Printf("Flush 1 OK")

	// Reset
	colID.Reset(); colQ.Reset(); colCID.Reset(); colSlug.Reset(); colQID.Reset()
	log.Printf("After reset: colID rows=%d", colID.Rows())

	// Append market 2
	colID.Append(r2.Markets[0].ID)
	colQ.Append(r2.Markets[0].Question)
	colCID.Append(z32)
	colSlug.Append(r2.Markets[0].Slug)
	colQID.Append(z32)

	log.Printf("Before flush 2: colID rows=%d", colID.Rows())

	err = conn.Do(ctx, ch.Query{Body: insertBody, Input: columns})
	if err != nil {
		log.Fatalf("flush 2: %v", err)
	}
	log.Printf("Flush 2 OK")

	// Verify
	time.Sleep(time.Second)
	var found proto.ColStr
	conn.Do(ctx, ch.Query{
		Body:   fmt.Sprintf("SELECT id FROM polymarket.markets WHERE id IN ('%s','%s') ORDER BY id", r1.Markets[0].ID, r2.Markets[0].ID),
		Result: proto.Results{{Name: "id", Data: &found}},
	})
	log.Printf("Found %d rows", found.Rows())
	for i := 0; i < found.Rows(); i++ {
		log.Printf("  row %d: %s", i, found.Row(i))
	}
}
