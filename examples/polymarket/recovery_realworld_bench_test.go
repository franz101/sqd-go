// Real-world ClickHouse recovery benchmarks — READ ONLY.
//
// These benchmark the state-recovery query shapes against the live `polymarket`
// database. They NEVER mutate data: every statement is a SELECT (wrapped in
// `SELECT count() FROM (...)`), the only tables they create are ephemeral
// per-query external tables (`_resolver_keys`), and rwAssertReadOnly rejects any
// query containing a mutating keyword as defense-in-depth.
//
// Run (guarded by RW_BENCH=1 so normal `go test ./...` skips them):
//
//	RW_BENCH=1 CLICKHOUSE_NATIVE_PORT=9003 CLICKHOUSE_PASSWORD=sqd-clickhouse \
//	  go test ./examples/polymarket/ -run '^$' -bench 'BenchmarkRW' -benchtime 1x -timeout 1200s -v
//
// Each sub-benchmark reports read_rows and read_mb (ClickHouse server-side
// progress) via b.ReportMetric, which is the headline metric: the optimized
// shapes read far fewer rows for identical results.
//
// Captured against the live polymarket.memory_user_positions (~135.8M rows),
// 2026-06-15 (12-core box, indexer running concurrently):
//
//	RecoveryShape (full table, RW_BENCH_MAX_MEM=2GB):
//	  argMax_groupby        MEMORY_LIMIT_EXCEEDED after 6.75M rows  (cannot recover in 2GB)
//	  readinorder_limit1by  135.8M rows in 16.4s                    (streams in bounded RAM)
//	ResolverJoinVsIn (1500 scattered keys):
//	  inner_join  383 ms   2888 read_mb   135,703,647 read_rows
//	  tuple_in    108 ms    192 read_mb     3,809,248 read_rows    (35.6x fewer rows, 3.5x faster)
//	ParallelBuckets (4 buckets, 17.3M rows, identical read_rows):
//	  sequential_1conn  2.22 s
//	  parallel_4conn    1.19 s                                     (1.86x on a busy box)
package polymarket

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	chgo "github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

// ---- config / guards -------------------------------------------------------

func rwBenchEnabled(tb testing.TB) {
	tb.Helper()
	if os.Getenv("RW_BENCH") != "1" {
		tb.Skip("set RW_BENCH=1 to run real-world ClickHouse benchmarks")
	}
	if testing.Short() {
		tb.Skip("skipping real-world benchmark in -short mode")
	}
}

func rwEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func rwEnvIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// rwMutatingKeyword matches mutating SQL statements as whole words. Word
// boundaries avoid false positives on read-only settings and identifiers that
// merely contain a keyword as a substring (e.g. the setting
// "optimize_read_in_order" or the column "updated_at" / "created_at").
var rwMutatingKeyword = regexp.MustCompile(`(?i)\b(INSERT|ALTER|DROP|TRUNCATE|DELETE|UPDATE|CREATE|RENAME|ATTACH|DETACH|OPTIMIZE|REPLACE)\b`)

// rwAssertReadOnly panics if a query contains a mutating statement. Defense in
// depth so a future edit can't accidentally write to the live database.
func rwAssertReadOnly(sql string) {
	if m := rwMutatingKeyword.FindString(sql); m != "" {
		panic(fmt.Sprintf("rwAssertReadOnly: refusing to run query containing %q: %s", strings.ToUpper(m), sql))
	}
}

const (
	rwTable       = "memory_user_positions"
	rwUserSize    = 20
	rwTokenIDSize = 32
)

func rwDB() string { return rwEnvOr("CLICKHOUSE_DB", "polymarket") }

func rwDial(ctx context.Context, tb testing.TB) *chgo.Client {
	tb.Helper()
	addr := fmt.Sprintf("%s:%d", rwEnvOr("CLICKHOUSE_HOST", "127.0.0.1"), rwEnvIntOr("CLICKHOUSE_NATIVE_PORT", 9003))
	c, err := chgo.Dial(ctx, chgo.Options{
		Address:  addr,
		Database: rwEnvOr("CLICKHOUSE_DB", "polymarket"),
		User:     rwEnvOr("CLICKHOUSE_USER", "default"),
		Password: rwEnvOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse"),
	})
	if err != nil {
		tb.Skipf("ch.Dial %s: %v (is ClickHouse up on the native port?)", addr, err)
	}
	return c
}

// rwProgress accumulates server-side read counters for one query.
type rwProgress struct {
	rows  uint64
	bytes uint64
}

// rwCount runs `SELECT count() FROM (inner)` and returns how many rows/bytes the
// server actually read. Optionally attaches an external table.
func rwCount(ctx context.Context, conn *chgo.Client, inner, extName string, ext []proto.InputColumn) (rwProgress, time.Duration, error) {
	body := "SELECT count() AS c FROM (" + inner + ")"
	rwAssertReadOnly(body)
	var (
		cnt proto.ColUInt64
		p   rwProgress
	)
	settings := []chgo.Setting{
		{Key: "max_query_size", Value: "104857600"},
		// Disable external (on-disk) GROUP BY so argMax must hold its whole hash
		// table in RAM — at full table scale that is exactly what blows up.
		{Key: "max_bytes_before_external_group_by", Value: "0"},
	}
	// Optional RAM cap: with it set, full-table argMax errors ("memory limit
	// exceeded") while the streaming read-in-order form survives — the headline
	// recovery win made visible.
	if cap := os.Getenv("RW_BENCH_MAX_MEM"); cap != "" {
		settings = append(settings, chgo.Setting{Key: "max_memory_usage", Value: cap})
	}
	q := chgo.Query{
		Body:     body,
		Result:   proto.Results{{Name: "c", Data: &cnt}},
		Settings: settings,
		OnProgress: func(_ context.Context, pr proto.Progress) error {
			p.rows += uint64(pr.Rows)
			p.bytes += uint64(pr.Bytes)
			return nil
		},
	}
	if extName != "" {
		q.ExternalTable = extName
		q.ExternalData = ext
	}
	start := time.Now()
	err := conn.Do(ctx, q)
	return p, time.Since(start), err
}

// rwFirstByteRange builds a predicate restricting a FixedString(size) column to
// first byte in [loByte, hiByte) — used to bound a scan to a slice of the
// keyspace so a benchmark op completes in seconds, not minutes.
func rwFirstByteRange(col string, loByte, hiByte, size int) string {
	loHex := fmt.Sprintf("%02x%s", loByte, strings.Repeat("00", size-1))
	lo := fmt.Sprintf("toFixedString(unhex('%s'), %d)", loHex, size)
	if hiByte >= 256 {
		return fmt.Sprintf("`%s` >= %s", col, lo)
	}
	hiHex := fmt.Sprintf("%02x%s", hiByte, strings.Repeat("00", size-1))
	hi := fmt.Sprintf("toFixedString(unhex('%s'), %d)", hiHex, size)
	return fmt.Sprintf("`%s` >= %s AND `%s` < %s", col, lo, col, hi)
}

func rwReport(b *testing.B, p rwProgress) {
	b.ReportMetric(float64(p.rows), "read_rows")
	b.ReportMetric(float64(p.bytes)/(1<<20), "read_mb")
}

// ---- Benchmark 1: recovery query shape ------------------------------------
//
// argMax(...) GROUP BY  vs  ORDER BY ... LIMIT 1 BY ... (optimize_read_in_order).
// Both return the latest row per (user, token_id). The read-in-order form
// streams in primary-key order and avoids the full hash-aggregation + sort that
// argMax/GROUP BY needs (which spilled ~51GB to disk over the full table).
// Bounded to a first-byte slice of `user` so each op is a few seconds.
func BenchmarkRW_RecoveryShape(b *testing.B) {
	rwBenchEnabled(b)
	ctx := context.Background()
	conn := rwDial(ctx, b)
	defer conn.Close()

	hiByte := rwEnvIntOr("RW_BENCH_PREFIX_BYTES", 8) // 8/256 = 1/32 of the keyspace
	where := rwFirstByteRange("user", 0, hiByte, rwUserSize)
	db := rwDB()

	argMax := fmt.Sprintf(
		"SELECT user, token_id, "+
			"argMax(amount, (block_number, transaction_index, log_index)) AS amount "+
			"FROM %s.%s WHERE %s GROUP BY user, token_id",
		db, rwTable, where)

	readInOrder := fmt.Sprintf(
		"SELECT user, token_id, amount FROM %s.%s WHERE %s "+
			"ORDER BY user DESC, token_id DESC, block_number DESC, transaction_index DESC, log_index DESC "+
			"LIMIT 1 BY user, token_id SETTINGS optimize_read_in_order = 1",
		db, rwTable, where)

	for _, v := range []struct{ name, sql string }{
		{"argMax_groupby", argMax},
		{"readinorder_limit1by", readInOrder},
	} {
		b.Run(v.name, func(b *testing.B) {
			var last rwProgress
			var runErr error
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p, _, err := rwCount(ctx, conn, v.sql, "", nil)
				last, runErr = p, err
				// A memory-limit error is the expected outcome for argMax under
				// RW_BENCH_MAX_MEM — report it rather than failing the bench.
				if err != nil && !rwIsMemoryLimit(err) {
					b.Fatalf("%s: %v", v.name, err)
				}
			}
			b.StopTimer()
			if runErr != nil {
				b.Logf("%s: DID NOT COMPLETE under cap: %v", v.name, runErr)
				b.ReportMetric(1, "mem_exceeded")
			}
			rwReport(b, last)
		})
	}
}

func rwIsMemoryLimit(err error) bool {
	s := strings.ToUpper(err.Error())
	return strings.Contains(s, "MEMORY_LIMIT") || strings.Contains(s, "MEMORY LIMIT")
}

// ---- Benchmark 2: resolver prefetch — INNER JOIN vs tuple-IN ---------------
//
// The live lazy-prefetch resolves a scattered set of (user, token_id) keys
// against the 130M-row table. INNER JOIN against the keys streams the WHOLE left
// table through a hash table (read_rows ~= full table). The tuple-IN form uses
// the set as a primary-key prefilter and prunes granules (read_rows ~= a few
// granules per key). Same result, orders of magnitude fewer rows read — this was
// the live bottleneck fix (133M -> ~110K rows/query observed).
func BenchmarkRW_ResolverJoinVsIn(b *testing.B) {
	rwBenchEnabled(b)
	ctx := context.Background()
	conn := rwDial(ctx, b)
	defer conn.Close()

	nKeys := rwEnvIntOr("RW_BENCH_KEYS", 2000)
	keyUser, keyToken := rwSampleScatteredKeys(ctx, b, conn, nKeys)
	b.Logf("sampled %d scattered keys", keyUser.Rows())

	ext := []proto.InputColumn{
		{Name: "user", Data: keyUser},
		{Name: "token_id", Data: keyToken},
	}
	db := rwDB()

	join := fmt.Sprintf(
		"SELECT t.user FROM %s.%s AS t "+
			"INNER JOIN _resolver_keys AS k ON t.user = k.user AND t.token_id = k.token_id",
		db, rwTable)

	in := fmt.Sprintf(
		"SELECT t.user FROM %s.%s AS t "+
			"WHERE (t.user, t.token_id) IN (SELECT user, token_id FROM _resolver_keys)",
		db, rwTable)

	for _, v := range []struct{ name, sql string }{
		{"inner_join", join},
		{"tuple_in", in},
	} {
		b.Run(v.name, func(b *testing.B) {
			var last rwProgress
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p, _, err := rwCount(ctx, conn, v.sql, "_resolver_keys", ext)
				if err != nil {
					b.Fatalf("%s: %v", v.name, err)
				}
				last = p
			}
			b.StopTimer()
			rwReport(b, last)
		})
	}
}

// rwSampleScatteredKeys reads nKeys (user, token_id) pairs spread across the
// whole primary-key range (hash modulo, so they are scattered, not clustered).
// Read-only.
func rwSampleScatteredKeys(ctx context.Context, tb testing.TB, conn *chgo.Client, nKeys int) (*proto.ColFixedStr, *proto.ColFixedStr) {
	tb.Helper()
	stride := rwEnvIntOr("RW_BENCH_SAMPLE_STRIDE", 2000)
	sql := fmt.Sprintf(
		"SELECT user, token_id FROM %s.%s WHERE cityHash64(user, token_id) %% %d = 0 LIMIT %d",
		rwDB(), rwTable, stride, nKeys)
	rwAssertReadOnly(sql)

	var (
		colUser  proto.ColFixedStr
		colToken proto.ColFixedStr
		outUser  = new(proto.ColFixedStr)
		outToken = new(proto.ColFixedStr)
	)
	colUser.SetSize(rwUserSize)
	colToken.SetSize(rwTokenIDSize)
	outUser.SetSize(rwUserSize)
	outToken.SetSize(rwTokenIDSize)

	err := conn.Do(ctx, chgo.Query{
		Body: sql,
		Result: proto.Results{
			{Name: "user", Data: &colUser},
			{Name: "token_id", Data: &colToken},
		},
		OnResult: func(_ context.Context, block proto.Block) error {
			for i := 0; i < block.Rows; i++ {
				outUser.Append(colUser.Row(i))
				outToken.Append(colToken.Row(i))
			}
			return nil
		},
	})
	if err != nil {
		tb.Fatalf("sample keys: %v", err)
	}
	if outUser.Rows() == 0 {
		tb.Skip("no keys sampled (is the table populated?)")
	}
	return outUser, outToken
}

// ---- Benchmark 3: sequential vs parallel-bucket recovery -------------------
//
// Recovery splits the keyspace into first-byte buckets. This compares scanning
// `nbuckets` buckets one connection at a time vs one connection per bucket in
// parallel. Same total work and same read_rows; parallel wall-clock ~= 1/nbuckets.
func BenchmarkRW_ParallelBuckets(b *testing.B) {
	rwBenchEnabled(b)
	ctx := context.Background()
	nbuckets := rwEnvIntOr("RW_BENCH_BUCKETS", 4)
	db := rwDB()

	// Bound the scan to the first 1/8 of the keyspace, split into nbuckets.
	const span = 32 // first-byte ceiling (32/256 = 1/8)
	width := span / nbuckets
	bucketSQL := func(bk int) string {
		lo := bk * width
		hi := lo + width
		where := rwFirstByteRange("user", lo, hi, rwUserSize)
		return fmt.Sprintf(
			"SELECT user, token_id, amount FROM %s.%s WHERE %s "+
				"ORDER BY user DESC, token_id DESC, block_number DESC, transaction_index DESC, log_index DESC "+
				"LIMIT 1 BY user, token_id SETTINGS optimize_read_in_order = 1",
			db, rwTable, where)
	}

	b.Run("sequential_1conn", func(b *testing.B) {
		conn := rwDial(ctx, b)
		defer conn.Close()
		var total rwProgress
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			total = rwProgress{}
			for bk := 0; bk < nbuckets; bk++ {
				p, _, err := rwCount(ctx, conn, bucketSQL(bk), "", nil)
				if err != nil {
					b.Fatalf("bucket %d: %v", bk, err)
				}
				total.rows += p.rows
				total.bytes += p.bytes
			}
		}
		b.StopTimer()
		rwReport(b, total)
	})

	b.Run(fmt.Sprintf("parallel_%dconn", nbuckets), func(b *testing.B) {
		conns := make([]*chgo.Client, nbuckets)
		for i := range conns {
			conns[i] = rwDial(ctx, b)
			defer conns[i].Close()
		}
		var total rwProgress
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var (
				mu  sync.Mutex
				wg  sync.WaitGroup
				bad error
			)
			total = rwProgress{}
			for bk := 0; bk < nbuckets; bk++ {
				wg.Add(1)
				go func(bk int) {
					defer wg.Done()
					p, _, err := rwCount(ctx, conns[bk], bucketSQL(bk), "", nil)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						bad = err
						return
					}
					total.rows += p.rows
					total.bytes += p.bytes
				}(bk)
			}
			wg.Wait()
			if bad != nil {
				b.Fatalf("parallel bucket: %v", bad)
			}
		}
		b.StopTimer()
		rwReport(b, total)
	})
}
