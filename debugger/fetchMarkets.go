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
	"os"
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
	flag.Parse()

	ctx := context.Background()
	conn, err := ch.Dial(ctx, ch.Options{
		Address: fmt.Sprintf("%s:%d", *chHost, *chPort), Database: "default",
		User: *chUser, Password: *chPass,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", qi(*chDB))})
	ensureTable(ctx, conn, *chDB)
	log.Printf("Table %s.markets ready", *chDB)

	total, err := ingestAll(ctx, conn, *chDB)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Done. %d markets ingested", total)
}

func ensureTable(ctx context.Context, conn *ch.Client, db string) error {
	return conn.Do(ctx, ch.Query{Body: fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.markets (
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
			inserted_at DateTime64(3,'UTC') DEFAULT now64(3)
		) ENGINE = ReplacingMergeTree(inserted_at) ORDER BY id
	`, qi(db))})
}

func ingestAll(ctx context.Context, conn *ch.Client, db string) (uint64, error) {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	insertSQL := fmt.Sprintf("INSERT INTO %s.markets VALUES", qi(db))
	var total uint64

	type acc struct {
		ids, qs, slugs, descs, imgs, icons, rsrcs, cats, amms, fees, denoms                                                         []string
		outs, oprs, souts, mts, fts, rbys, umasts, umabs, umarws, ctids, gids, spmts, creats                                         []string
		cids, qids, mmas                                                                                                             [][]byte
		eds, sds, ueds, cts, cats_t, uats_t                                                                                          []time.Time
		lns, vns, opticks, omins, scrs, spreads, ltps, bbids, basks, comps, lines                                                    []float64
		o1ds, o1hs, o1ws, o1ms, o1ys                                                                                                []float64
		acts, clss, archs, restrs, rdys, fnds, eobs, acpts, fpmms, negs, autos, rfqs                                                 []uint8
		mfees, tfees, curs                                                                                                           []int32
		v24, v1w, v1m, v1y, v24a, v1wa, v1ma, v1ya, v24c, v1wc, v1mc, v1yc, vamm, vclob, liqa, liqc                                 []float64
	}

	var a acc
	const batchSize = 1000

	add := func(m *polymarketgamma.Market) {
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
		}
		if err := conn.Do(ctx, ch.Query{Body: insertSQL, Input: cols}); err != nil {
			return fmt.Errorf("insert %d: %w", n, err)
		}
		log.Printf("Inserted %d markets", n)
		a = acc{}
		return nil
	}

	// ---- Fetch active markets ----
	log.Printf("Fetching active (closed=false) markets...")
	if err := fetchChunk(httpClient, "", "&closed=false", 500, add, &total); err != nil {
		return total, fmt.Errorf("active markets: %w", err)
	}
	flush()

	// ---- Fetch closed markets by month ----
	log.Printf("Fetching closed markets by month (2020-2026)...")
	for year := 2020; year <= 2026; year++ {
		for month := 1; month <= 12; month++ {
			endMonth := month + 1
			endYear := year
			if endMonth > 12 {
				endMonth = 1
				endYear = year + 1
			}
			dateFilter := fmt.Sprintf("&closed=true&end_date_min=%d-%02d-01T00:00:00Z&end_date_max=%d-%02d-01T00:00:00Z",
				year, month, endYear, endMonth)
			if err := fetchChunk(httpClient, "", dateFilter, 500, add, &total); err != nil {
				log.Printf("Warning: %d-%02d: %v", year, month, err)
			}
		}
	}

	return total, flush()
}

func fetchChunk(httpClient *http.Client, baseParams, extraParams string, limit int,
	add func(*polymarketgamma.Market), total *uint64) error {

	const gammaBase = "https://gamma-api.polymarket.com"
	const maxOffset = 2000

	for offset := 0; offset <= maxOffset; offset += limit {
		url := fmt.Sprintf("%s/markets?limit=%d&offset=%d%s%s&order=id&ascending=true",
			gammaBase, limit, offset, baseParams, extraParams)
		markets, err := fetchPage(httpClient, url)
		if err != nil {
			return fmt.Errorf("offset %d: %w", offset, err)
		}
		if len(markets) == 0 {
			break
		}
		for i := range markets {
			add(&markets[i])
		}
		*total += uint64(len(markets))
		if len(markets) < limit {
			break
		}
	}
	return nil
}

func fetchPage(httpClient *http.Client, url string) ([]polymarketgamma.Market, error) {
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
		if resp.StatusCode == http.StatusTooManyRequests {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
		}
		var markets []polymarketgamma.Market
		if err := json.Unmarshal(body, &markets); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		return markets, nil
	}
	return nil, fmt.Errorf("exhausted retries")
}

// ---- column constructors (always allocate new) ----

func ci(name string, d proto.ColInput) proto.InputColumn { return proto.InputColumn{Name: name, Data: d} }
func ss(v []string) *proto.ColStr {
	c := new(proto.ColStr)
	for _, s := range v { c.Append(s) }
	return c
}
func fs(v [][]byte, size int) *proto.ColFixedStr {
	c := new(proto.ColFixedStr); c.SetSize(size)
	for _, b := range v { c.Append(b) }
	return c
}
func dt(v []time.Time) *proto.ColDateTime64 {
	c := new(proto.ColDateTime64); c.WithPrecision(proto.Precision(3)); c.WithLocation(time.UTC)
	for _, t := range v { c.Append(t) }
	return c
}
func f64(v []float64) *proto.ColFloat64 {
	c := new(proto.ColFloat64)
	for _, x := range v { c.Append(x) }
	return c
}
func u8(v []uint8) *proto.ColUInt8 {
	c := new(proto.ColUInt8)
	for _, x := range v { c.Append(x) }
	return c
}
func i32(v []int32) *proto.ColInt32 {
	c := new(proto.ColInt32)
	for _, x := range v { c.Append(x) }
	return c
}

// ---- helpers ----

func h2b(hex string, size int) []byte {
	if hex == "" { return make([]byte, size) }
	b := common.FromHex(hex)
	if len(b) == 0 { return make([]byte, size) }
	if len(b) < size {
		p := make([]byte, size)
		copy(p[size-len(b):], b)
		return p
	}
	return b[:size]
}
func nt(t polymarketgamma.NormalizedTime) time.Time {
	tt := t.Time()
	if tt.IsZero() { return time.Unix(0, 0).UTC() }
	return tt.UTC()
}
func ja(a []string) string {
	if len(a) == 0 { return "" }
	b, _ := json.Marshal(a)
	return string(b)
}
func b8(b bool) uint8 { if b { return 1 }; return 0 }
func qi(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func env(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }
