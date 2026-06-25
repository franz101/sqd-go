package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
)

// These tests pin down the "--parallel-fetch looks slow" metric artifact and now
// validate the fix. In skipEmpties (includeAllBlocks=false) mode the portal returns
// only NON-EMPTY blocks plus a marker at the scanned high-water mark, so the replay
// buffer is sparse. The consumer advances its cursor across confirmed-empty gaps via
// ReplayBuffer.CeilBlock (ingestion.go:1060-1069) WITHOUT incrementing totalBlocks
// (which is bumped once per buffer-present block at ingestion.go:930). The old stat
// derived "+N blocks / blk/s" from totalBlocks, under-reporting true chain coverage
// by the empty-block ratio. The fix reports chain coverage from the consumer-cursor
// delta and relabels the totalBlocks counter as "non-empty" (logStats at
// ingestion.go:863-867).

// sparseFakePortal mimics a SQD portal queried with includeAllBlocks=false: for a
// request {fromBlock,toBlock} it returns ONLY the present (non-empty) block numbers
// that fall inside the requested window, plus a marker block at the scanned
// high-water mark (the clamped toBlock). Most block numbers in the range are absent,
// just like a real sparse stream. X-Sqd-Finalized-Head-Number is set very large so
// the whole requested range is treated as finalized/immutable.
//
// present must be sorted ascending. The marker block carries one matching log so the
// downstream typed-event pipeline has a row to decode, while the gap blocks have no
// lines at all (confirmed-empty, never fetched per-number).
func sparseFakePortal(present []uint64, finalizedHead uint64) *httptest.Server {
	// USDC contract + a Transfer event so the full-pipeline integration test decodes
	// rows. The header-only blocks (non-marker present blocks) stay log-less; the
	// marker block at the scanned high-water mark carries the matching log.
	const usdcAddr = "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	// keccak256("Transfer(address,address,uint256)")
	const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

	emitHeader := func(b *strings.Builder, n uint64) {
		fmt.Fprintf(b, `{"header":{"number":%d,"hash":"0x%064x","timestamp":%d}}`+"\n", n, n, 1700000000+n)
	}
	emitWithLog := func(b *strings.Builder, n uint64) {
		fmt.Fprintf(b,
			`{"header":{"number":%d,"hash":"0x%064x","timestamp":%d},`+
				`"logs":[{"address":"%s","transactionHash":"0x%064x","topics":["%s","0x%064x","0x%064x"],"data":"0x%064x","transactionIndex":0,"logIndex":0}]}`+"\n",
			n, n, 1700000000+n, usdcAddr, n, transferTopic, uint64(1), uint64(2), uint64(1000))
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			FromBlock uint64  `json:"fromBlock"`
			ToBlock   *uint64 `json:"toBlock"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("X-Sqd-Finalized-Head-Number", strconv.FormatUint(finalizedHead, 10))
		w.Header().Set("X-Sqd-Head-Number", strconv.FormatUint(finalizedHead, 10))

		// The scanned high-water mark for this response: the clamped toBlock. The
		// portal always returns a marker block there so the consumer learns every
		// number <= marker has been scanned (LatestBlock), enabling the empty-gap skip.
		marker := q.FromBlock
		if q.ToBlock != nil {
			marker = *q.ToBlock
		}

		var b strings.Builder
		for _, n := range present {
			if n < q.FromBlock || n > marker {
				continue
			}
			if n == marker {
				continue // emitted last, as the marker, possibly with a log
			}
			emitHeader(&b, n)
		}
		// Marker line at the high-water mark. If it's also a present block, give it
		// the matching log; otherwise a header-only marker so the consumer sees it
		// as the scanned high-water mark.
		markerPresent := false
		for _, n := range present {
			if n == marker {
				markerPresent = true
				break
			}
		}
		if markerPresent {
			emitWithLog(&b, marker)
		} else {
			emitHeader(&b, marker)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}))
}

// covHeaderNumberRe extracts block-header numbers from a raw JSONL page. Mirrors the
// production headerNumberRe but kept local so this test owns its parsing.
var covHeaderNumberRe = regexp.MustCompile(`"number":\s*(\d+)`)

// covParseBlockNumbers returns the distinct header block numbers present in a raw
// JSONL page (one header "number" per line; the first match per line is the header).
func covParseBlockNumbers(raw []byte) []uint64 {
	var out []uint64
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		m := covHeaderNumberRe.FindSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.ParseUint(string(m[1]), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// TestCoverageReflectsSkippedEmptyGaps replicates the consumer's advance loop exactly
// as production runs it over a sparse buffer, and proves chain coverage (consumer
// cursor delta) is ~100x the non-empty count (totalBlocks delta). This is the metric
// artifact: the old "+N blocks/blk/s" derived from non-empty under-reported true
// coverage by the empty ratio. ALWAYS RUNS (no ClickHouse, no network).
func TestCoverageReflectsSkippedEmptyGaps(t *testing.T) {
	rb := NewReplayBuffer(64)

	// Sparse present blocks across a 0..9999 range: 0,1000,...,9000 plus a marker at
	// 9999 — 11 present blocks, every other number is a confirmed-empty gap. Each has
	// empty events, mirroring includeAllBlocks=false where only the marker carries a
	// matching log.
	var present []uint64
	for n := uint64(0); n <= 9000; n += 1000 {
		present = append(present, n)
	}
	present = append(present, 9999) // marker at the scanned high-water mark
	for _, n := range present {
		rb.Write(1, n, "h", time.Time{}, nil, nil, nil, nil, false, "", n, nil)
	}

	if got, want := rb.Len(), len(present); got != want {
		t.Fatalf("buffer Len = %d, want %d present blocks", got, want)
	}
	if got := rb.LatestBlock(); got != 9999 {
		t.Fatalf("LatestBlock (scanned high-water mark) = %d, want 9999", got)
	}

	// Simulate the consumer EXACTLY like production:
	//   - GetBlock(consumer) hit -> nonEmpty++ (ingestion.go:930) and consumer=number+1
	//     (ingestion.go:1039)
	//   - miss -> CeilBlock(consumer) jumps across the confirmed-empty gap
	//     (ingestion.go:1060-1069) WITHOUT touching nonEmpty
	//   - stop once past the scanned high-water mark (LatestBlock)
	var nonEmpty uint64
	consumer := uint64(0)
	latest := rb.LatestBlock()
	for consumer <= latest {
		if entry, ok := rb.GetBlock(consumer); ok {
			nonEmpty++ // production increments totalBlocks once per present block
			consumer = entry.number + 1
			continue
		}
		next, ok := rb.CeilBlock(consumer)
		if !ok {
			break // nothing present at/after consumer: past the high-water mark
		}
		consumer = next // gap-skip: advance cursor without counting a block
	}

	// coverage == final consumer cursor == chain coverage (the honest "blk/s" signal).
	coverage := consumer
	if coverage < 9999 {
		t.Fatalf("coverage = %d, want consumer to advance to ~9999 (across the whole range)", coverage)
	}
	if nonEmpty != uint64(len(present)) {
		t.Fatalf("nonEmpty = %d, want %d (one per present block)", nonEmpty, len(present))
	}

	// THE ARTIFACT: coverage (~10000) is at least ~100x the non-empty count (~11).
	// The old totalBlocks-based stat therefore under-reported true coverage ~900x
	// here; chain-coverage from the consumer-cursor delta is the honest signal.
	if coverage < nonEmpty*100 {
		t.Fatalf("coverage %d is not >= 100x non-empty %d — the metric artifact is not reproduced",
			coverage, nonEmpty)
	}
	t.Logf("chain coverage = %d blocks, non-empty = %d blocks (ratio %.0fx): the old stat under-reported coverage by this ratio",
		coverage, nonEmpty, float64(coverage)/float64(nonEmpty))
}

// TestParallelPrefetcherSparseCoverageVsNonEmpty exercises the REAL parallel fetch
// path against the sparse fake portal: it drains the prefetcher over a wide range and
// confirms the pages SPAN the whole chain range while the count of present (non-empty)
// blocks is a small fraction. This is the prefetcher-level view of the same artifact:
// the chain is covered fast, but few blocks are "present". ALWAYS RUNS (no ClickHouse).
func TestParallelPrefetcherSparseCoverageVsNonEmpty(t *testing.T) {
	const start, end uint64 = 0, 100000
	const pageSize uint64 = 10000
	const workers = 4

	// Present (non-empty) blocks: one every 10000 across the range, plus the marker
	// blocks the portal emits at each page's scanned high-water mark. With ~11
	// "real" present numbers the non-empty fraction is tiny relative to the range.
	var present []uint64
	for n := start; n <= end; n += 10000 {
		present = append(present, n)
	}
	// The portal also emits a marker block at the clamped toBlock of each request;
	// for grid pages those land at page boundaries (e.g. 9999, 19999, ...). Add them
	// so sparseFakePortal returns them as present-with-marker lines.
	for from := start; from <= end; from += pageSize {
		to := from + uint64(pageSize) - 1
		if to > end {
			to = end
		}
		present = append(present, to)
	}
	// Deduplicate + sort (present must be ascending for sparseFakePortal).
	present = covUniqueSorted(present)

	srv := sparseFakePortal(present, 99_999_999)
	defer srv.Close()

	// noRateLimit (defined in parallel_fetch_test.go) keeps this hermetic test fast.
	p := newParallelPrefetcher(srv.URL, nil, false /*includeAllBlocks=false -> sparse*/, start, end, pageSize, workers, noRateLimit())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.launch(ctx)

	var nonEmpty uint64
	var coverage uint64 // max block number observed across all pages
	var pageHi uint64   // grid boundary coverage: pages are contiguous, ascending
	for {
		pg, ok := p.Next(ctx)
		if !ok {
			break
		}
		if pg.err != nil {
			t.Fatalf("unexpected page err [%d-%d]: %v", pg.from, pg.coveredTo, pg.err)
		}
		if pg.coveredTo > pageHi {
			pageHi = pg.coveredTo
		}
		for _, n := range covParseBlockNumbers(pg.raw) {
			nonEmpty++
			if n > coverage {
				coverage = n
			}
		}
	}

	// The prefetcher's pages must span ~the full configured range...
	if pageHi != end {
		t.Fatalf("grid coverage reached %d, want full range end %d", pageHi, end)
	}
	if coverage < end-pageSize {
		t.Fatalf("max present block %d does not span the range (end %d)", coverage, end)
	}
	// ...while the present (non-empty) count is a small fraction of that span. We
	// inserted ~21 present/marker numbers across 100001 block-numbers, so non-empty
	// should be well under 1% of coverage.
	if nonEmpty == 0 {
		t.Fatal("no present blocks parsed from sparse pages")
	}
	if nonEmpty*20 >= coverage {
		t.Fatalf("non-empty %d is not a small fraction of coverage %d (artifact not reproduced)", nonEmpty, coverage)
	}
	t.Logf("parallel prefetcher: chain coverage span = %d blocks, non-empty present = %d blocks (%.2f%%)",
		coverage, nonEmpty, 100*float64(nonEmpty)/float64(coverage))
}

func covUniqueSorted(in []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(in))
	var out []uint64
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	// Simple insertion sort: the slices here are tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// covStatsLineRe matches the new stats line format and captures the chain-coverage
// blk/s figure plus the non-empty delta, proving the relabeled format is emitted:
//
//	"... | +%d blocks, %.0f blk/s (avg %.0f) | +%d non-empty, +%d events in %s ..."
var covStatsLineRe = regexp.MustCompile(
	`\+(\d+) blocks, (\d+) blk/s \(avg \d+\) \| \+(\d+) non-empty, \+\d+ events`)

// TestIntegrationParallelFetchCoverageStat runs the full pipeline against the sparse
// fake portal in --parallel-fetch cursor mode, captures the stats log lines, and
// asserts the new format reports chain coverage much larger than the non-empty delta
// AND that checkpoint/next advanced across the configured range. CH-GATED: SKIPS when
// ClickHouse is unavailable (as in this environment); always COMPILES.
func TestIntegrationParallelFetchCoverageStat(t *testing.T) {
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}

	host, port, password := chEnv()
	dbName := fmt.Sprintf("coverage_metric_test_%d", time.Now().UnixNano())

	const startBlock uint64 = 0
	const endBlock uint64 = 300_000

	// Sparse present (non-empty) blocks across the range: one every 25k, plus the
	// per-page marker blocks the portal emits at each scanned high-water mark so the
	// consumer's empty-gap skip (CeilBlock) can advance. finalizedHead is huge so the
	// whole range is finalized and parallel fetch engages.
	var present []uint64
	for n := startBlock; n <= endBlock; n += 25_000 {
		present = append(present, n)
	}
	_, pageSize, _ := ParallelFetchSettings()
	for from := startBlock; from <= endBlock; from += uint64(pageSize) {
		to := from + uint64(pageSize) - 1
		if to > endBlock {
			to = endBlock
		}
		present = append(present, to)
	}
	present = covUniqueSorted(present)

	srv := sparseFakePortal(present, 99_999_999)
	defer srv.Close()
	t.Setenv("SQD_PORTAL_ENDPOINT", srv.URL)
	// The fake-portal run finishes in a couple of seconds, well under the 10s
	// default stats cadence; shorten it so periodic stats lines actually fire.
	t.Setenv("SQD_STATS_INTERVAL", "200ms")

	end := endBlock
	cfg := &config.Config{
		Name: dbName,
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: startBlock,
			EndBlock:   &end,
			Contracts: []config.ChainContractConfig{{
				Name:    "USDC",
				Address: config.Address{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"},
				Events: []config.EventConfig{
					{Event: "Transfer(address indexed from, address indexed to, uint256 value)"},
				},
			}},
		}},
		// StoreBlocks and StoreRawLogs left nil (=false) so parallelSkipEmpties engages
		// (ingestion.go:423) and the portal is fetched sparse (includeAllBlocks=false).
	}

	// Set up DB + base tables + the typed event table (mirrors integration_test.go).
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer setupCancel()

	store, err := database.NewClickHouse(setupCtx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("setup ClickHouse: %v", err)
	}
	if err := store.EnsureTablesWithOptions(setupCtx, true, database.EnsureTablesOptions{}); err != nil {
		t.Fatalf("ensure base tables: %v", err)
	}
	createTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.usdc_transfer_events (
		block_number UInt64,
		block_timestamp DateTime64(3, 'UTC'),
		transaction_index UInt64,
		log_index UInt64,
		from FixedString(20),
		to FixedString(20),
		value UInt256
	) ENGINE = MergeTree()
	ORDER BY (block_number, transaction_index, log_index)`, quoteIdentForTest(dbName))
	if err := store.Conn().Do(setupCtx, ch.Query{Body: createTable}); err != nil {
		t.Fatalf("create typed table: %v", err)
	}
	store.Close()

	// Capture log output (the stats lines) for assertion; restore afterward.
	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	runCtx, runCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer runCancel()

	opts := Options{
		ClickHouseHost:     host,
		ClickHousePort:     port,
		ClickHouseUser:     "default",
		ClickHousePassword: password,
		ClickHouseDatabase: dbName,
		// NOT Restart: ingestion.Run drops the whole database on restart, which
		// would delete the usdc_transfer_events table created above (no
		// GeneratedSQLDir here to recreate it). The per-run dbName is already fresh.
		Restart:       false,
		CursorMode:    true,
		ParallelFetch: true,
		PageSize:      0,
	}
	err = Run(runCtx, cfg, opts)
	if err != nil && runCtx.Err() == nil {
		// Restore output before failing so the message is visible.
		log.SetOutput(oldOut)
		t.Fatalf("ingestion.Run: %v", err)
	}

	// Restore output so test diagnostics are visible.
	log.SetOutput(oldOut)
	logged := buf.String()

	// Cleanup DB regardless of assertions below.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	defer func() {
		if err := database.DropClickHouseDatabase(cleanupCtx, host, port, "default", password, dbName); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()

	// (a) The new stats line format must be present and report chain coverage much
	// larger than the non-empty delta implies. Sum coverage and non-empty across all
	// stats ticks; if only the final tick fired, that single line still carries the
	// ratio.
	matches := covStatsLineRe.FindAllStringSubmatch(logged, -1)
	if len(matches) == 0 {
		t.Fatalf("no stats line matched the new format (got logs:\n%s)", logged)
	}
	var totalCoverage, totalNonEmpty uint64
	for _, m := range matches {
		cov, _ := strconv.ParseUint(m[1], 10, 64)
		ne, _ := strconv.ParseUint(m[3], 10, 64)
		totalCoverage += cov
		totalNonEmpty += ne
	}
	if totalCoverage == 0 {
		t.Fatalf("reported chain coverage is zero across %d stats lines", len(matches))
	}
	if totalNonEmpty == 0 {
		t.Fatalf("reported non-empty delta is zero across %d stats lines", len(matches))
	}
	if totalCoverage <= totalNonEmpty*5 {
		t.Fatalf("chain coverage %d not much larger than non-empty %d — fix not reflected in stats",
			totalCoverage, totalNonEmpty)
	}
	t.Logf("stats lines: summed chain coverage = %d, summed non-empty = %d (%.0fx)",
		totalCoverage, totalNonEmpty, float64(totalCoverage)/float64(totalNonEmpty))

	// (b) checkpoint/next advanced across the configured range. The final "next:" in
	// the captured logs should be near endBlock.
	if !covNextAdvanced(t, logged, endBlock) {
		t.Fatalf("consumer cursor (next:) did not advance across the configured range to ~%d; logs:\n%s",
			endBlock, logged)
	}
}

// covNextAdvanced scans the captured stats lines for the highest "next: N" value and
// asserts it advanced across most of the configured range (within one page of the end).
func covNextAdvanced(t *testing.T, logged string, endBlock uint64) bool {
	t.Helper()
	re := regexp.MustCompile(`next: (\d+)`)
	var maxNext uint64
	for _, m := range re.FindAllStringSubmatch(logged, -1) {
		n, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			continue
		}
		if n > maxNext {
			maxNext = n
		}
	}
	_, pageSize, _ := ParallelFetchSettings()
	return maxNext+uint64(pageSize) >= endBlock
}
