//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	polymarketgamma "github.com/ivanzzeth/polymarket-go-gamma-client"
)

func main() {
	chHost := flag.String("ch-host", env("CLICKHOUSE_HOST", "127.0.0.1"), "")
	chPort := flag.Int("ch-port", 9003, "")
	chUser := flag.String("ch-user", env("CLICKHOUSE_USER", "default"), "")
	chPass := flag.String("ch-password", env("CLICKHOUSE_PASSWORD", "sqd-clickhouse"), "")
	chDB := flag.String("ch-db", env("CLICKHOUSE_DATABASE", "polymarket"), "")
	chTable := flag.String("ch-table", env("CLICKHOUSE_TABLE", "market_gamma"), "")
	fromValue := flag.String("from", "2018-01-01", "minimum end_date (YYYY-MM-DD or RFC3339)")
	toValue := flag.String("to", "2031-01-01", "maximum end_date (YYYY-MM-DD or RFC3339)")
	fetchActive := flag.Bool("active", true, "fetch active markets")
	fetchClosed := flag.Bool("closed", true, "fetch closed markets")
	useKeyset := flag.Bool("keyset", true, "use complete sequential keyset pagination")
	ascending := flag.Bool("ascending", true, "keyset ID sort direction")
	stopID := flag.Uint64("stop-id", 0, "stop keyset after this numeric market ID (0 disables)")
	flag.Parse()
	from, err := parseBoundary(*fromValue)
	if err != nil {
		log.Fatal(err)
	}
	to, err := parseBoundary(*toValue)
	if err != nil {
		log.Fatal(err)
	}
	if !from.Before(to) {
		log.Fatal("-from must be before -to")
	}
	if !*fetchActive && !*fetchClosed {
		log.Fatal("at least one of -active or -closed must be true")
	}

	ctx := context.Background()
	conn, err := ch.Dial(ctx, ch.Options{
		Address: fmt.Sprintf("%s:%d", *chHost, *chPort), Database: "default",
		User: *chUser, Password: *chPass,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", qi(*chDB))}); err != nil {
		log.Fatal(err)
	}
	if err := ensureTable(ctx, conn, *chDB, *chTable); err != nil {
		log.Fatal(err)
	}
	log.Printf("Table %s.%s ready", *chDB, *chTable)

	total, err := ingestAll(
		ctx, conn, *chDB, *chTable, from, to,
		*fetchActive, *fetchClosed, *useKeyset, *ascending, *stopID,
	)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Done. %d markets ingested", total)
}

func ensureTable(ctx context.Context, conn *ch.Client, db, table string) error {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.%s (
			id String, question String,
			condition_id FixedString(32), slug String, question_id FixedString(32),
			description String, image String, icon String, resolution_source String,
			end_date DateTime64(3,'UTC'), start_date DateTime64(3,'UTC'),
			uma_end_date DateTime64(3,'UTC'), closed_time DateTime64(3,'UTC'),
			category String, amm_type String,
			liquidity_num Float64, volume_num Float64,
			fee String, denomination_token String,
			outcomes String, outcome_prices String, short_outcomes String,
			active UInt8, closed UInt8, archived UInt8, restricted UInt8, ready UInt8, funded UInt8,
			market_type String, format_type String,
			market_maker_address FixedString(20),
			created_at DateTime64(3,'UTC'), updated_at DateTime64(3,'UTC'),
			resolved_by String, uma_resolution_status String, uma_bond String, uma_reward String,
			enable_order_book UInt8, order_price_min_tick_size Float64, order_min_size Float64,
			maker_base_fee Int32, taker_base_fee Int32, accepting_orders UInt8,
			score Float64, curation_order Int32,
			volume_24hr Float64, volume_1wk Float64, volume_1mo Float64, volume_1yr Float64,
			volume_24hr_amm Float64, volume_1wk_amm Float64, volume_1mo_amm Float64, volume_1yr_amm Float64,
			volume_24hr_clob Float64, volume_1wk_clob Float64, volume_1mo_clob Float64, volume_1yr_clob Float64,
			volume_amm Float64, volume_clob Float64,
			liquidity_amm Float64, liquidity_clob Float64,
			spread Float64, last_trade_price Float64, best_bid Float64, best_ask Float64,
			one_day_price_change Float64, one_hour_price_change Float64,
			one_week_price_change Float64, one_month_price_change Float64, one_year_price_change Float64,
			clob_token_ids String, game_id String, sports_market_type String,
			line Float64,
			fpmm_live UInt8, neg_risk_other UInt8, automatically_resolved UInt8, rfq_enabled UInt8,
			creator String, competitive Float64,
			raw_json String,
			inserted_at DateTime64(3,'UTC') DEFAULT now64(3)
		) ENGINE = ReplacingMergeTree(inserted_at) ORDER BY id
	`, qi(db), qi(table))}); err != nil {
		return err
	}
	return conn.Do(ctx, ch.Query{Body: fmt.Sprintf(
		"ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS raw_json String AFTER competitive",
		qi(db), qi(table),
	)})
}

func ingestAll(
	ctx context.Context,
	conn *ch.Client,
	db, table string,
	from, to time.Time,
	fetchActive, fetchClosed bool,
	useKeyset bool,
	ascending bool,
	stopID uint64,
) (uint64, error) {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	insertSQL := fmt.Sprintf("INSERT INTO %s.%s VALUES", qi(db), qi(table))
	var total uint64

	type acc struct {
		ids, qs, slugs, descs, imgs, icons, rsrcs, cats, amms, fees, denoms, raws                   []string
		outs, oprs, souts, mts, fts, rbys, umasts, umabs, umarws, ctids, gids, spmts, creats        []string
		cids, qids, mmas                                                                            [][]byte
		eds, sds, ueds, cts, cats_t, uats_t, iats                                                   []time.Time
		lns, vns, opticks, omins, scrs, spreads, ltps, bbids, basks, comps, lines                   []float64
		o1ds, o1hs, o1ws, o1ms, o1ys                                                                []float64
		acts, clss, archs, restrs, rdys, fnds, eobs, acpts, fpmms, negs, autos, rfqs                []uint8
		mfees, tfees, curs                                                                          []int32
		v24, v1w, v1m, v1y, v24a, v1wa, v1ma, v1ya, v24c, v1wc, v1mc, v1yc, vamm, vclob, liqa, liqc []float64
	}

	var a acc
	const batchSize = 1000

	add := func(m *polymarketgamma.Market, raw json.RawMessage) {
		a.ids = append(a.ids, m.ID)
		a.qs = append(a.qs, m.Question)
		a.cids = append(a.cids, h2b(m.ConditionID, 32))
		a.slugs = append(a.slugs, m.Slug)
		a.qids = append(a.qids, h2b(m.QuestionID, 32))
		a.descs = append(a.descs, m.Description)
		a.imgs = append(a.imgs, m.Image)
		a.icons = append(a.icons, m.Icon)
		a.rsrcs = append(a.rsrcs, m.ResolutionSource)
		a.eds = append(a.eds, nt(m.EndDate))
		a.sds = append(a.sds, nt(m.StartDate))
		a.ueds = append(a.ueds, nt(m.UMAEndDate))
		a.cts = append(a.cts, nt(m.ClosedTime))
		a.cats = append(a.cats, m.Category)
		a.amms = append(a.amms, m.AmmType)
		a.lns = append(a.lns, m.LiquidityNum)
		a.vns = append(a.vns, m.VolumeNum)
		a.fees = append(a.fees, m.Fee)
		a.denoms = append(a.denoms, m.DenominationToken)
		a.outs = append(a.outs, ja(m.Outcomes))
		a.oprs = append(a.oprs, ja(m.OutcomePrices))
		a.souts = append(a.souts, ja(m.ShortOutcomes))
		a.acts = append(a.acts, b8(m.Active))
		a.clss = append(a.clss, b8(m.Closed))
		a.archs = append(a.archs, b8(m.Archived))
		a.restrs = append(a.restrs, b8(m.Restricted))
		a.rdys = append(a.rdys, b8(m.Ready))
		a.fnds = append(a.fnds, b8(m.Funded))
		a.mts = append(a.mts, m.MarketType)
		a.fts = append(a.fts, m.FormatType)
		a.mmas = append(a.mmas, h2b(m.MarketMakerAddress, 20))
		a.cats_t = append(a.cats_t, nt(m.CreatedAt))
		a.uats_t = append(a.uats_t, nt(m.UpdatedAt))
		a.rbys = append(a.rbys, m.ResolvedBy)
		a.umasts = append(a.umasts, m.UMAResolutionStatus)
		a.umabs = append(a.umabs, m.UMABond)
		a.umarws = append(a.umarws, m.UMAReward)
		a.eobs = append(a.eobs, b8(m.EnableOrderBook))
		a.opticks = append(a.opticks, m.OrderPriceMinTickSize)
		a.omins = append(a.omins, m.OrderMinSize)
		a.mfees = append(a.mfees, int32(m.MakerBaseFee))
		a.tfees = append(a.tfees, int32(m.TakerBaseFee))
		a.acpts = append(a.acpts, b8(m.AcceptingOrders))
		a.scrs = append(a.scrs, m.Score)
		a.curs = append(a.curs, int32(m.CurationOrder))
		a.v24 = append(a.v24, m.Volume24hr)
		a.v1w = append(a.v1w, m.Volume1wk)
		a.v1m = append(a.v1m, m.Volume1mo)
		a.v1y = append(a.v1y, m.Volume1yr)
		a.v24a = append(a.v24a, m.Volume24hrAmm)
		a.v1wa = append(a.v1wa, m.Volume1wkAmm)
		a.v1ma = append(a.v1ma, m.Volume1moAmm)
		a.v1ya = append(a.v1ya, m.Volume1yrAmm)
		a.v24c = append(a.v24c, m.Volume24hrClob)
		a.v1wc = append(a.v1wc, m.Volume1wkClob)
		a.v1mc = append(a.v1mc, m.Volume1moClob)
		a.v1yc = append(a.v1yc, m.Volume1yrClob)
		a.vamm = append(a.vamm, m.VolumeAmm)
		a.vclob = append(a.vclob, m.VolumeClob)
		a.liqa = append(a.liqa, m.LiquidityAmm)
		a.liqc = append(a.liqc, m.LiquidityClob)
		a.spreads = append(a.spreads, m.Spread)
		a.ltps = append(a.ltps, m.LastTradePrice)
		a.bbids = append(a.bbids, m.BestBid)
		a.basks = append(a.basks, m.BestAsk)
		a.o1ds = append(a.o1ds, m.OneDayPriceChange)
		a.o1hs = append(a.o1hs, m.OneHourPriceChange)
		a.o1ws = append(a.o1ws, m.OneWeekPriceChange)
		a.o1ms = append(a.o1ms, m.OneMonthPriceChange)
		a.o1ys = append(a.o1ys, m.OneYearPriceChange)
		a.ctids = append(a.ctids, m.ClobTokenIDs)
		a.gids = append(a.gids, m.GameID)
		a.spmts = append(a.spmts, m.SportsMarketType)
		a.lines = append(a.lines, m.Line)
		a.fpmms = append(a.fpmms, b8(m.FPMMLive))
		a.negs = append(a.negs, b8(m.NegRiskOther))
		a.autos = append(a.autos, b8(m.AutomaticallyResolved))
		a.rfqs = append(a.rfqs, b8(m.RFQEnabled))
		a.creats = append(a.creats, m.Creator)
		a.comps = append(a.comps, m.Competitive)
		a.raws = append(a.raws, string(raw))
		a.iats = append(a.iats, time.Now().UTC())
	}

	flush := func() error {
		n := len(a.ids)
		if n == 0 {
			return nil
		}
		cols := []proto.InputColumn{
			ci("id", ss(a.ids)), ci("question", ss(a.qs)),
			ci("condition_id", fs(a.cids, 32)), ci("slug", ss(a.slugs)), ci("question_id", fs(a.qids, 32)),
			ci("description", ss(a.descs)), ci("image", ss(a.imgs)), ci("icon", ss(a.icons)),
			ci("resolution_source", ss(a.rsrcs)),
			ci("end_date", dt(a.eds)), ci("start_date", dt(a.sds)),
			ci("uma_end_date", dt(a.ueds)), ci("closed_time", dt(a.cts)),
			ci("category", ss(a.cats)), ci("amm_type", ss(a.amms)),
			ci("liquidity_num", f64(a.lns)), ci("volume_num", f64(a.vns)),
			ci("fee", ss(a.fees)), ci("denomination_token", ss(a.denoms)),
			ci("outcomes", ss(a.outs)), ci("outcome_prices", ss(a.oprs)), ci("short_outcomes", ss(a.souts)),
			ci("active", u8(a.acts)), ci("closed", u8(a.clss)), ci("archived", u8(a.archs)),
			ci("restricted", u8(a.restrs)), ci("ready", u8(a.rdys)), ci("funded", u8(a.fnds)),
			ci("market_type", ss(a.mts)), ci("format_type", ss(a.fts)),
			ci("market_maker_address", fs(a.mmas, 20)),
			ci("created_at", dt(a.cats_t)), ci("updated_at", dt(a.uats_t)),
			ci("resolved_by", ss(a.rbys)), ci("uma_resolution_status", ss(a.umasts)),
			ci("uma_bond", ss(a.umabs)), ci("uma_reward", ss(a.umarws)),
			ci("enable_order_book", u8(a.eobs)), ci("order_price_min_tick_size", f64(a.opticks)),
			ci("order_min_size", f64(a.omins)), ci("maker_base_fee", i32(a.mfees)),
			ci("taker_base_fee", i32(a.tfees)), ci("accepting_orders", u8(a.acpts)),
			ci("score", f64(a.scrs)), ci("curation_order", i32(a.curs)),
			ci("volume_24hr", f64(a.v24)), ci("volume_1wk", f64(a.v1w)),
			ci("volume_1mo", f64(a.v1m)), ci("volume_1yr", f64(a.v1y)),
			ci("volume_24hr_amm", f64(a.v24a)), ci("volume_1wk_amm", f64(a.v1wa)),
			ci("volume_1mo_amm", f64(a.v1ma)), ci("volume_1yr_amm", f64(a.v1ya)),
			ci("volume_24hr_clob", f64(a.v24c)), ci("volume_1wk_clob", f64(a.v1wc)),
			ci("volume_1mo_clob", f64(a.v1mc)), ci("volume_1yr_clob", f64(a.v1yc)),
			ci("volume_amm", f64(a.vamm)), ci("volume_clob", f64(a.vclob)),
			ci("liquidity_amm", f64(a.liqa)), ci("liquidity_clob", f64(a.liqc)),
			ci("spread", f64(a.spreads)), ci("last_trade_price", f64(a.ltps)),
			ci("best_bid", f64(a.bbids)), ci("best_ask", f64(a.basks)),
			ci("one_day_price_change", f64(a.o1ds)), ci("one_hour_price_change", f64(a.o1hs)),
			ci("one_week_price_change", f64(a.o1ws)), ci("one_month_price_change", f64(a.o1ms)),
			ci("one_year_price_change", f64(a.o1ys)),
			ci("clob_token_ids", ss(a.ctids)), ci("game_id", ss(a.gids)),
			ci("sports_market_type", ss(a.spmts)), ci("line", f64(a.lines)),
			ci("fpmm_live", u8(a.fpmms)), ci("neg_risk_other", u8(a.negs)),
			ci("automatically_resolved", u8(a.autos)), ci("rfq_enabled", u8(a.rfqs)),
			ci("creator", ss(a.creats)), ci("competitive", f64(a.comps)),
			ci("raw_json", ss(a.raws)),
			ci("inserted_at", dt(a.iats)),
		}
		if err := conn.Do(ctx, ch.Query{Body: insertSQL, Input: cols}); err != nil {
			return fmt.Errorf("insert %d: %w", n, err)
		}
		log.Printf("Inserted %d markets", n)
		a = acc{}
		return nil
	}

	if useKeyset {
		fetchStatus := func(closed bool) error {
			cursor := ""
			status := "open"
			if closed {
				status = "closed"
			}
			log.Printf("Fetching %s markets via complete Gamma keyset pagination...", status)
			var lastID string
			for {
				raws, next, err := fetchKeysetPage(httpClient, closed, cursor, ascending)
				if err != nil {
					return err
				}
				if len(raws) == 0 {
					break
				}
				skipped := 0
				reachedStop := false
				for _, raw := range raws {
					market, err := decodeGammaMarket(raw)
					if err != nil {
						skipped++
						continue
					}
					numericID, err := strconv.ParseUint(market.ID, 10, 64)
					if stopID > 0 && err == nil {
						if (ascending && numericID > stopID) || (!ascending && numericID <= stopID) {
							reachedStop = true
							break
						}
					}
					add(&market, raw)
					total++
					lastID = market.ID
				}
				if skipped > 0 {
					log.Printf("  skipped %d unparseable %s market(s)", skipped, status)
				}
				if len(a.ids) >= batchSize {
					if err := flush(); err != nil {
						return err
					}
				}
				if total%10000 < uint64(len(raws)) {
					log.Printf("  keyset progress: %d markets (last id %s)", total, lastID)
				}
				if reachedStop || next == "" || next == cursor {
					break
				}
				cursor = next
			}
			return flush()
		}
		if fetchActive {
			if err := fetchStatus(false); err != nil {
				return total, fmt.Errorf("open keyset: %w", err)
			}
		}
		if fetchClosed {
			if err := fetchStatus(true); err != nil {
				return total, fmt.Errorf("closed keyset: %w", err)
			}
		}
		return total, nil
	}

	const apiTime = "2006-01-02T15:04:05Z"

	// fetchOffsetPages walks offset pages (limit=100, the Gamma API maximum) for one
	// filter, up to the API's hard offset ceiling (2000). Returns the collected markets
	// and whether the result was truncated by that ceiling (more markets exist for this
	// filter than offset paging can reach).
	fetchOffsetPages := func(filt string) ([]polymarketgamma.Market, bool, error) {
		var out []polymarketgamma.Market
		for off := 0; off <= 2000; off += 100 {
			url := fmt.Sprintf("%s/markets?limit=100&offset=%d%s&order=id&ascending=true",
				"https://gamma-api.polymarket.com", off, filt)
			ms, rawCount, truncated, err := fetchPage(httpClient, url)
			if err != nil {
				return out, false, err
			}
			if truncated {
				return out, true, nil
			}
			out = append(out, ms...)
			// Terminate based on the API page size, not successfully decoded rows.
			// Otherwise one malformed record turns a full 100-row page into 99
			// decoded rows and silently stops pagination early.
			if rawCount < 100 {
				return out, false, nil
			}
			time.Sleep(120 * time.Millisecond) // be polite to the Gamma API
		}
		return out, true, nil
	}

	// fetchRange recursively bisects (minT,maxT] by end_date until each leaf fits under
	// the offset ceiling, guaranteeing complete coverage despite the 2000-offset cap.
	var fetchRange func(closed bool, minT, maxT time.Time) error
	fetchRange = func(closed bool, minT, maxT time.Time) error {
		cf := "true"
		if !closed {
			cf = "false"
		}
		filt := fmt.Sprintf("&closed=%s&end_date_min=%s&end_date_max=%s",
			cf, minT.Format(apiTime), maxT.Format(apiTime))
		markets, truncated, err := fetchOffsetPages(filt)
		if err != nil {
			return err
		}
		if truncated && maxT.Sub(minT) > time.Hour {
			mid := minT.Add(maxT.Sub(minT) / 2)
			if err := fetchRange(closed, minT, mid); err != nil {
				return err
			}
			return fetchRange(closed, mid, maxT)
		}
		for i := range markets {
			add(&markets[i], nil)
		}
		total += uint64(len(markets))
		if len(markets) > 0 {
			warn := ""
			if truncated {
				warn = " WARN:truncated-leaf(>2100 in <1h)"
			}
			log.Printf("  closed=%s [%s..%s] +%d (total %d)%s", cf,
				minT.Format("2006-01-02"), maxT.Format("2006-01-02"), len(markets), total, warn)
		}
		return flush()
	}

	if fetchActive {
		log.Printf("Fetching ACTIVE (closed=false) markets in [%s..%s] via adaptive end_date bisection...",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
		if err := fetchRange(false, from, to); err != nil {
			return total, fmt.Errorf("active markets: %w", err)
		}
	}
	if fetchClosed {
		log.Printf("Fetching CLOSED (closed=true) markets in [%s..%s] via adaptive end_date bisection...",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
		if err := fetchRange(true, from, to); err != nil {
			return total, fmt.Errorf("closed markets: %w", err)
		}
	}
	return total, flush()
}

func fetchPage(httpClient *http.Client, url string) ([]polymarketgamma.Market, int, bool, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "sqd-go/1.0")

	backoff := 2 * time.Second
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			continue
		}
		// Gamma rejects offset beyond ~2000 with 422 ("offset too large, use
		// /markets/keyset"); signal truncation so the caller bisects the date range.
		if resp.StatusCode == http.StatusUnprocessableEntity ||
			(resp.StatusCode == http.StatusBadRequest && strings.Contains(string(body), "offset")) {
			return nil, 0, true, nil
		}
		if resp.StatusCode != http.StatusOK {
			return nil, 0, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
		}
		// Decode per-market: some live markets carry placeholder / non-RFC3339
		// dates (e.g. endDate "NOW*()") that fail the typed time unmarshal. Isolate
		// the offending record instead of dropping the whole page.
		var raws []json.RawMessage
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, 0, false, fmt.Errorf("decode page: %w", err)
		}
		markets := make([]polymarketgamma.Market, 0, len(raws))
		skipped := 0
		for _, rm := range raws {
			var m polymarketgamma.Market
			if err := json.Unmarshal(rm, &m); err != nil {
				skipped++
				continue
			}
			markets = append(markets, m)
		}
		if skipped > 0 {
			log.Printf("  skipped %d unparseable market(s) on a page", skipped)
		}
		return markets, len(raws), false, nil
	}
	return nil, 0, false, fmt.Errorf("exhausted retries")
}

func fetchKeysetPage(
	httpClient *http.Client,
	closed bool,
	cursor string,
	ascending bool,
) ([]json.RawMessage, string, error) {
	params := url.Values{
		"limit":       {"100"},
		"closed":      {strconv.FormatBool(closed)},
		"order":       {"id"},
		"ascending":   {strconv.FormatBool(ascending)},
		"include_tag": {"true"},
	}
	if cursor != "" {
		params.Set("after_cursor", cursor)
	}
	endpoint := "https://gamma-api.polymarket.com/markets/keyset?" + params.Encode()

	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<min(attempt, 5)) * 250 * time.Millisecond
			time.Sleep(delay)
		}
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("User-Agent", "sqd-go-fetch-markets-v1/2.0")
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("HTTP %d: %s",
				resp.StatusCode, string(body[:min(len(body), 200)]))
		}
		var page struct {
			Markets    []json.RawMessage `json:"markets"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			lastErr = err
			continue
		}
		return page.Markets, page.NextCursor, nil
	}
	return nil, "", fmt.Errorf("exhausted retries: %w", lastErr)
}

func decodeGammaMarket(raw json.RawMessage) (polymarketgamma.Market, error) {
	var market polymarketgamma.Market
	if err := json.Unmarshal(raw, &market); err == nil {
		return market, nil
	}

	// Gamma occasionally emits placeholders such as "NOW*()" in date fields.
	// Preserve the untouched object in raw_json while zeroing only typed dates
	// for the structured projection. Nested objects are also retained in raw_json
	// and can be omitted from this projection if one of their dates is malformed.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return market, err
	}
	for _, key := range []string{
		"endDate", "startDate", "umaEndDate", "closedTime",
		"lowerBoundDate", "upperBoundDate", "createdAt", "updatedAt",
		"gameStartTime", "readyTimestamp", "fundedTimestamp",
		"acceptingOrdersTimestamp", "deployingTimestamp",
		"scheduledDeploymentTimestamp", "eventStartTime",
	} {
		object[key] = json.RawMessage("null")
	}
	delete(object, "events")
	delete(object, "categories")
	delete(object, "tags")
	delete(object, "imageOptimized")
	delete(object, "iconOptimized")

	sanitized, err := json.Marshal(object)
	if err != nil {
		return market, err
	}
	if err := json.Unmarshal(sanitized, &market); err != nil {
		return market, err
	}
	return market, nil
}

// ---- column constructors (always allocate new) ----

func ci(name string, d proto.ColInput) proto.InputColumn {
	return proto.InputColumn{Name: name, Data: d}
}
func ss(v []string) *proto.ColStr {
	c := new(proto.ColStr)
	for _, s := range v {
		c.Append(s)
	}
	return c
}
func fs(v [][]byte, size int) *proto.ColFixedStr {
	c := new(proto.ColFixedStr)
	c.SetSize(size)
	for _, b := range v {
		c.Append(b)
	}
	return c
}
func dt(v []time.Time) *proto.ColDateTime64 {
	c := new(proto.ColDateTime64)
	c.WithPrecision(proto.Precision(3))
	c.WithLocation(time.UTC)
	for _, t := range v {
		c.Append(t)
	}
	return c
}
func f64(v []float64) *proto.ColFloat64 {
	c := new(proto.ColFloat64)
	for _, x := range v {
		c.Append(x)
	}
	return c
}
func u8(v []uint8) *proto.ColUInt8 {
	c := new(proto.ColUInt8)
	for _, x := range v {
		c.Append(x)
	}
	return c
}
func i32(v []int32) *proto.ColInt32 {
	c := new(proto.ColInt32)
	for _, x := range v {
		c.Append(x)
	}
	return c
}

// ---- helpers ----

func h2b(hex string, size int) []byte {
	if hex == "" {
		return make([]byte, size)
	}
	b := common.FromHex(hex)
	if len(b) == 0 {
		return make([]byte, size)
	}
	if len(b) < size {
		p := make([]byte, size)
		copy(p[size-len(b):], b)
		return p
	}
	return b[:size]
}
func nt(t polymarketgamma.NormalizedTime) time.Time {
	tt := t.Time()
	if tt.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return tt.UTC()
}
func ja(a []string) string {
	if len(a) == 0 {
		return ""
	}
	b, _ := json.Marshal(a)
	return string(b)
}
func b8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
func qi(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseBoundary(value string) (time.Time, error) {
	layout := time.RFC3339
	if len(value) == len("2006-01-02") {
		layout = "2006-01-02"
	}
	parsed, err := time.Parse(layout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: %w", value, err)
	}
	return parsed.UTC(), nil
}
