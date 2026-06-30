package polymarket

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

// BenchmarkResolverSyncConditions benchmarks synchronous condition resolver queries.
// This is the baseline before async optimization.
func BenchmarkResolverSyncConditions(b *testing.B) {
	conn, err := dialClickHouse(b)
	if err != nil {
		b.Fatalf("Failed to connect to ClickHouse: %v", err)
	}
	defer conn.Close()

	b.ResetTimer()
	b.ReportAllocs()

	var totalResolveTime time.Duration

	for i := 0; i < b.N; i++ {
		state := generated.NewHotState(100000)

		// Queue some condition keys
		for j := 0; j < 100; j++ {
			var condKey generated.ConditionsClockKey
			condKey.ID = common.HexToHash(fmt.Sprintf("0x%064x", j+0x1000))
			state.ConditionsResolver.Queue(condKey)
		}

		start := time.Now()
		err := state.ConditionsResolver.Resolve(context.Background(), conn, "polymarket")
		resolveTime := time.Since(start)
		totalResolveTime += resolveTime

		if err != nil {
			// Expected for test data - log but don't fail
		}

		b.ReportMetric(float64(resolveTime.Microseconds()), "μs/op")
	}

	avgResolveTime := totalResolveTime / time.Duration(b.N)
	b.ReportMetric(float64(avgResolveTime.Microseconds()), "μs/avg")
}

// BenchmarkResolverSyncAll benchmarks all entity resolvers sequentially.
// This simulates the current ResolveAllPending behavior.
func BenchmarkResolverSyncAll(b *testing.B) {
	conn, err := dialClickHouse(b)
	if err != nil {
		b.Fatalf("Failed to connect to ClickHouse: %v", err)
	}
	defer conn.Close()

	b.ResetTimer()
	b.ReportAllocs()

	var totalResolveTime time.Duration

	for i := 0; i < b.N; i++ {
		state := generated.NewHotState(100000)

		// Queue some keys for each resolver
		for j := 0; j < 50; j++ {
			var condKey generated.ConditionsClockKey
			condKey.ID = common.HexToHash(fmt.Sprintf("0x%064x", j))
			state.ConditionsResolver.Queue(condKey)

			var posKey generated.UserPositionsClockKey
			posKey.User = common.HexToAddress(fmt.Sprintf("0x%040x", j))
			posKey.TokenID = common.HexToHash(fmt.Sprintf("0x%064x", j))
			state.UserPositionsResolver.Queue(posKey)

			var marketKey generated.MarketsClockKey
			marketKey.ID = common.HexToHash(fmt.Sprintf("0x%064x", j+0x2000))
			state.MarketsResolver.Queue(marketKey)
		}

		start := time.Now()
		// This simulates ResolveAllPending - sequential resolver calls
		err1 := state.ConditionsResolver.Resolve(context.Background(), conn, "polymarket")
		err2 := state.UserPositionsResolver.Resolve(context.Background(), conn, "polymarket")
		err3 := state.MarketsResolver.Resolve(context.Background(), conn, "polymarket")
		resolveTime := time.Since(start)
		totalResolveTime += resolveTime

		if err1 != nil || err2 != nil || err3 != nil {
			// Expected for test data
		}

		b.ReportMetric(float64(resolveTime.Microseconds()), "μs/op")
	}

	avgResolveTime := totalResolveTime / time.Duration(b.N)
	b.ReportMetric(float64(avgResolveTime.Microseconds()), "μs/avg")
}

// BenchmarkResolverEmpty benchmarks the overhead of resolver calls with no pending work.
// This should be very fast (< 1μs).
func BenchmarkResolverEmpty(b *testing.B) {
	state := generated.NewHotState(100000)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		start := time.Now()
		pending := state.ConditionsResolver.Pending()
		resolveTime := time.Since(start)

		if pending != 0 {
			b.Fatalf("Expected 0 pending, got %d", pending)
		}

		b.ReportMetric(float64(resolveTime.Nanoseconds()), "ns/op")
	}
}

// BenchmarkResolverQueueOnly benchmarks just the Queue operation overhead.
func BenchmarkResolverQueueOnly(b *testing.B) {
	state := generated.NewHotState(100000)
	var keys []generated.ConditionsClockKey

	// Pre-generate keys
	for i := 0; i < 1000; i++ {
		var key generated.ConditionsClockKey
		key.ID = common.HexToHash(fmt.Sprintf("0x%064x", i))
		keys = append(keys, key)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		for _, key := range keys {
			state.ConditionsResolver.Queue(key)
		}

		b.ReportMetric(float64(len(keys)), "keys/op")
	}
}

// dialClickHouse creates a ClickHouse connection for benchmarking.
// Reads connection settings from environment (same as production).
func dialClickHouse(b *testing.B) (*ch.Client, error) {
	host := os.Getenv("CLICKHOUSE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}

	port := 9000
	if v := os.Getenv("CLICKHOUSE_NATIVE_PORT"); v != "" {
		if p, err := parsePort(v); err == nil {
			port = p
		}
	}

	user := os.Getenv("CLICKHOUSE_USER")
	if user == "" {
		user = "default"
	}

	password := os.Getenv("CLICKHOUSE_PASSWORD")
	if password == "" {
		password = "sqd-clickhouse"
	}

	database := "polymarket"
	if v := os.Getenv("CLICKHOUSE_DATABASE"); v != "" {
		database = v
	}

	return ch.Dial(b.Context(), ch.Options{
		Address:  fmt.Sprintf("%s:%d", host, port),
		Database: database,
		User:     user,
		Password: password,
	})
}

func parsePort(s string) (int, error) {
	var port int
	_, err := fmt.Sscanf(s, "%d", &port)
	return port, err
}
