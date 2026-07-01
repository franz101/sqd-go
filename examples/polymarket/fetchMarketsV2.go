//go:build ignore

// fetchMarketsV2 loads the complete Polymarket CLOB market catalogue directly
// into ClickHouse. Unlike Gamma, CLOB serves 1,000 markets per cursor and accepts
// offset cursors, so independent pages can be fetched concurrently.
//
// Core metadata shared with (or directly useful from) the Gamma markets table
// is indexed in typed columns. By default, raw_json also preserves the complete
// CLOB object, including nested and future fields; pass -include-raw=false for
// compact storage. Progress is checkpointed after every successfully inserted
// wave. Replaying a wave is safe because the table is a ReplacingMergeTree
// keyed by the stable condition ID.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

const (
	clobMarketsURL = "https://clob.polymarket.com/markets"
	pageSize       = uint64(1000)
	endCursor      = "LTE=" // base64("-1")
	maxRetries     = 8
)

type config struct {
	chHost     string
	chPort     int
	chUser     string
	chPass     string
	chDB       string
	chTable    string
	stateFile  string
	workers    int
	reset      bool
	includeRaw bool
}

type clobToken struct {
	TokenID string  `json:"token_id"`
	Outcome string  `json:"outcome"`
	Price   float64 `json:"price"`
}

type clobMarket struct {
	EnableOrderBook bool            `json:"enable_order_book"`
	Active          bool            `json:"active"`
	Closed          bool            `json:"closed"`
	Archived        bool            `json:"archived"`
	AcceptingOrders bool            `json:"accepting_orders"`
	ConditionID     string          `json:"condition_id"`
	QuestionID      string          `json:"question_id"`
	Question        string          `json:"question"`
	MarketSlug      string          `json:"market_slug"`
	EndDateISO      string          `json:"end_date_iso"`
	NegRisk         bool            `json:"neg_risk"`
	Tokens          []clobToken     `json:"tokens"`
	RawJSON         json.RawMessage `json:"-"`
}

type pageResponse struct {
	Data       []clobMarket `json:"data"`
	NextCursor string       `json:"next_cursor"`
	Limit      int          `json:"limit"`
	Count      int          `json:"count"`
}

type pageResult struct {
	offset uint64
	page   pageResponse
	err    error
}

type checkpoint struct {
	Offset     uint64    `json:"offset"`
	Fetched    uint64    `json:"fetched"`
	Completed  bool      `json:"completed"`
	IncludeRaw bool      `json:"include_raw"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (market *clobMarket) UnmarshalJSON(data []byte) error {
	type plainMarket clobMarket
	var decoded plainMarket
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*market = clobMarket(decoded)
	market.RawJSON = append(market.RawJSON[:0], data...)
	return nil
}

func main() {
	cfg := parseFlags()
	if err := run(context.Background(), cfg); err != nil {
		log.Fatal(err)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.chHost, "ch-host", env("CLICKHOUSE_HOST", "127.0.0.1"), "")
	flag.IntVar(&cfg.chPort, "ch-port", envInt("CLICKHOUSE_NATIVE_PORT", 9003), "")
	flag.StringVar(&cfg.chUser, "ch-user", env("CLICKHOUSE_USER", "default"), "")
	flag.StringVar(&cfg.chPass, "ch-password", env("CLICKHOUSE_PASSWORD", "sqd-clickhouse"), "")
	flag.StringVar(&cfg.chDB, "ch-db", env("CLICKHOUSE_DATABASE", "polymarket"), "")
	flag.StringVar(&cfg.chTable, "ch-table", env("CLICKHOUSE_TABLE", "markets"), "")
	flag.StringVar(&cfg.stateFile, "state-file", "data/markets_clob_state.json", "")
	flag.IntVar(&cfg.workers, "workers", 20, "")
	flag.BoolVar(&cfg.reset, "reset", false, "truncate the destination and restart at offset zero")
	flag.BoolVar(&cfg.includeRaw, "include-raw", true, "preserve the complete CLOB object in raw_json")
	flag.Parse()
	if cfg.workers < 1 {
		cfg.workers = 1
	}
	return cfg
}

func run(ctx context.Context, cfg config) error {
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", cfg.chHost, cfg.chPort),
		Database: "default",
		User:     cfg.chUser,
		Password: cfg.chPass,
	})
	if err != nil {
		return fmt.Errorf("dial ClickHouse: %w", err)
	}
	defer conn.Close()

	if err := conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", qi(cfg.chDB)),
	}); err != nil {
		return err
	}
	if err := ensureTable(ctx, conn, cfg.chDB, cfg.chTable); err != nil {
		return err
	}

	state, err := loadCheckpoint(cfg.stateFile)
	if err != nil {
		return err
	}
	if cfg.reset {
		if err := conn.Do(ctx, ch.Query{
			Body: fmt.Sprintf("TRUNCATE TABLE %s.%s", qi(cfg.chDB), qi(cfg.chTable)),
		}); err != nil {
			return fmt.Errorf("truncate destination: %w", err)
		}
		state = checkpoint{}
		if err := os.Remove(cfg.stateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if state.Offset > 0 && state.IncludeRaw != cfg.includeRaw {
		return fmt.Errorf("checkpoint include_raw=%t does not match -include-raw=%t; use -reset or another state file",
			state.IncludeRaw, cfg.includeRaw)
	}
	if state.Completed {
		log.Printf("%s.%s is already complete at offset %d (%d rows checkpointed); use -reset to reload",
			cfg.chDB, cfg.chTable, state.Offset, state.Fetched)
		return nil
	}

	client := newHTTPClient(cfg.workers)
	started := time.Now()
	offset := state.Offset
	fetched := state.Fetched
	log.Printf("Loading CLOB markets into %s.%s with %d concurrent requests (resume offset %d, include raw %t)",
		cfg.chDB, cfg.chTable, cfg.workers, offset, cfg.includeRaw)

	for {
		results := fetchWave(ctx, client, offset, cfg.workers)
		var wave []clobMarket
		var scannedPages uint64
		done := false

		for i, result := range results {
			if result.err != nil {
				return fmt.Errorf("fetch offset %d: %w", result.offset, result.err)
			}
			if len(result.page.Data) == 0 {
				done = true
				break
			}
			wave = append(wave, result.page.Data...)
			scannedPages++
			if len(result.page.Data) < int(pageSize) || result.page.NextCursor == endCursor {
				done = true
				break
			}
			if i > 0 && result.offset != results[i-1].offset+pageSize {
				return fmt.Errorf("non-contiguous wave at offset %d", result.offset)
			}
		}

		inserted, err := insertMarkets(ctx, conn, cfg.chDB, cfg.chTable, wave, cfg.includeRaw)
		if err != nil {
			return err
		}
		fetched += inserted
		offset += scannedPages * pageSize

		state = checkpoint{
			Offset:     offset,
			Fetched:    fetched,
			Completed:  done,
			IncludeRaw: cfg.includeRaw,
			UpdatedAt:  time.Now().UTC(),
		}
		if err := saveCheckpoint(cfg.stateFile, state); err != nil {
			return err
		}
		log.Printf("fetched %d markets (scanned through offset %d, %.1fs)",
			fetched, offset, time.Since(started).Seconds())

		if done {
			log.Printf("Done. %d markets inserted into %s.%s in %s",
				fetched, cfg.chDB, cfg.chTable, time.Since(started).Round(time.Millisecond))
			return nil
		}
	}
}

func ensureTable(ctx context.Context, conn *ch.Client, db, table string) error {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.%s (
			id String,
			condition_id FixedString(32),
			question_id FixedString(32),
			question String,
			slug String,
			end_date DateTime64(3, 'UTC'),
			clob_token_ids String,
			outcomes String,
			outcome_prices String,
			active UInt8,
			closed UInt8,
			archived UInt8,
			accepting_orders UInt8,
			enable_order_book UInt8,
			neg_risk UInt8,
			raw_json String,
			category String ALIAS arrayFirst(x -> x != 'All', JSONExtract(raw_json, 'tags', 'Array(String)')),
			volume_num Float64 ALIAS 0.,
			inserted_at DateTime64(3, 'UTC')
		) ENGINE = ReplacingMergeTree(inserted_at)
		ORDER BY id
	`, qi(db), qi(table))}); err != nil {
		return err
	}
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf(
		"ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS raw_json String AFTER neg_risk",
		qi(db), qi(table),
	)}); err != nil {
		return err
	}
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf(
		"ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS category String ALIAS arrayFirst(x -> x != 'All', JSONExtract(raw_json, 'tags', 'Array(String)')) AFTER raw_json",
		qi(db), qi(table),
	)}); err != nil {
		return err
	}
	return conn.Do(ctx, ch.Query{Body: fmt.Sprintf(
		"ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS volume_num Float64 ALIAS 0. AFTER category",
		qi(db), qi(table),
	)})
}

func newHTTPClient(workers int) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = workers * 2
	transport.MaxIdleConnsPerHost = workers
	transport.MaxConnsPerHost = workers
	return &http.Client{Transport: transport, Timeout: 45 * time.Second}
}

func fetchWave(ctx context.Context, client *http.Client, offset uint64, workers int) []pageResult {
	results := make([]pageResult, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		pageOffset := offset + uint64(i)*pageSize
		go func() {
			defer wg.Done()
			page, err := fetchPage(ctx, client, pageOffset)
			results[i] = pageResult{offset: pageOffset, page: page, err: err}
		}()
	}
	wg.Wait()
	return results
}

func fetchPage(ctx context.Context, client *http.Client, offset uint64) (pageResponse, error) {
	endpoint, err := url.Parse(clobMarketsURL)
	if err != nil {
		return pageResponse{}, err
	}
	query := endpoint.Query()
	query.Set("next_cursor", cursor(offset))
	endpoint.RawQuery = query.Encode()

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<min(attempt, 4)) * time.Second
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return pageResponse{}, ctx.Err()
			case <-timer.C:
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return pageResponse{}, err
		}
		req.Header.Set("User-Agent", "sqd-go-fetch-markets-v2/1.0")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			var page pageResponse
			err = json.NewDecoder(resp.Body).Decode(&page)
			resp.Body.Close()
			if err == nil {
				return page, nil
			}
			lastErr = fmt.Errorf("decode response: %w", err)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return pageResponse{}, lastErr
		}
	}
	return pageResponse{}, fmt.Errorf("failed after %d attempts: %w", maxRetries, lastErr)
}

func insertMarkets(
	ctx context.Context,
	conn *ch.Client,
	db, table string,
	markets []clobMarket,
	includeRaw bool,
) (uint64, error) {
	if len(markets) == 0 {
		return 0, nil
	}

	ids := new(proto.ColStr)
	conditionIDs := fixedStr(32)
	questionIDs := fixedStr(32)
	questions := new(proto.ColStr)
	slugs := new(proto.ColStr)
	endDates := dateTime64()
	tokenIDs := new(proto.ColStr)
	outcomes := new(proto.ColStr)
	prices := new(proto.ColStr)
	active := new(proto.ColUInt8)
	closed := new(proto.ColUInt8)
	archived := new(proto.ColUInt8)
	accepting := new(proto.ColUInt8)
	orderBook := new(proto.ColUInt8)
	negRisk := new(proto.ColUInt8)
	rawJSON := new(proto.ColStr)
	insertedAt := dateTime64()
	now := time.Now().UTC()

	var inserted uint64
	seen := make(map[string]struct{}, len(markets))
	for _, market := range markets {
		if market.ConditionID == "" {
			continue
		}
		if _, ok := seen[market.ConditionID]; ok {
			continue
		}
		seen[market.ConditionID] = struct{}{}

		var tokenValues []string
		var outcomeValues []string
		var priceValues []float64
		for _, token := range market.Tokens {
			if token.TokenID != "" {
				tokenValues = append(tokenValues, token.TokenID)
			}
			outcomeValues = append(outcomeValues, token.Outcome)
			priceValues = append(priceValues, token.Price)
		}

		ids.Append(market.ConditionID)
		conditionIDs.Append(hexFixed(market.ConditionID, 32))
		questionIDs.Append(hexFixed(market.QuestionID, 32))
		questions.Append(market.Question)
		slugs.Append(market.MarketSlug)
		endDates.Append(parseTime(market.EndDateISO))
		tokenIDs.Append(jsonString(tokenValues))
		outcomes.Append(jsonString(outcomeValues))
		prices.Append(jsonString(priceValues))
		active.Append(bool8(market.Active))
		closed.Append(bool8(market.Closed))
		archived.Append(bool8(market.Archived))
		accepting.Append(bool8(market.AcceptingOrders))
		orderBook.Append(bool8(market.EnableOrderBook))
		negRisk.Append(bool8(market.NegRisk))
		if includeRaw {
			rawJSON.Append(string(market.RawJSON))
		} else {
			rawJSON.Append("")
		}
		insertedAt.Append(now)
		inserted++
	}
	if inserted == 0 {
		return 0, nil
	}

	query := ch.Query{
		Body: fmt.Sprintf("INSERT INTO %s.%s VALUES", qi(db), qi(table)),
		Input: []proto.InputColumn{
			input("id", ids),
			input("condition_id", conditionIDs),
			input("question_id", questionIDs),
			input("question", questions),
			input("slug", slugs),
			input("end_date", endDates),
			input("clob_token_ids", tokenIDs),
			input("outcomes", outcomes),
			input("outcome_prices", prices),
			input("active", active),
			input("closed", closed),
			input("archived", archived),
			input("accepting_orders", accepting),
			input("enable_order_book", orderBook),
			input("neg_risk", negRisk),
			input("raw_json", rawJSON),
			input("inserted_at", insertedAt),
		},
	}
	if err := conn.Do(ctx, query); err != nil {
		return 0, fmt.Errorf("insert %d markets: %w", inserted, err)
	}
	return inserted, nil
}

func loadCheckpoint(path string) (checkpoint, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return checkpoint{}, nil
	}
	if err != nil {
		return checkpoint{}, fmt.Errorf("read checkpoint: %w", err)
	}
	var state checkpoint
	if err := json.Unmarshal(body, &state); err != nil {
		return checkpoint{}, fmt.Errorf("decode checkpoint: %w", err)
	}
	return state, nil
}

func saveCheckpoint(path string, state checkpoint) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

func cursor(offset uint64) string {
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(offset, 10)))
}

func input(name string, data proto.ColInput) proto.InputColumn {
	return proto.InputColumn{Name: name, Data: data}
}

func fixedStr(size int) *proto.ColFixedStr {
	column := new(proto.ColFixedStr)
	column.SetSize(size)
	return column
}

func dateTime64() *proto.ColDateTime64 {
	column := new(proto.ColDateTime64)
	column.WithPrecision(proto.Precision(3))
	column.WithLocation(time.UTC)
	return column
}

func hexFixed(value string, size int) []byte {
	value = strings.TrimPrefix(value, "0x")
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return make([]byte, size)
	}
	if len(decoded) >= size {
		return decoded[len(decoded)-size:]
	}
	out := make([]byte, size)
	copy(out[size-len(decoded):], decoded)
	return out
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Unix(0, 0).UTC()
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return parsed.UTC()
}

func jsonString(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(body)
}

func bool8(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}

func qi(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
