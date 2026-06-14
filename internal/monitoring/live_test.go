package monitoring

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveWriter exercises the recorder against a real ClickHouse instance.
// It is skipped unless SQD_METRICS_CH_LIVE=1 so normal `go test` stays hermetic.
//
//	SQD_METRICS_CH_LIVE=1 go test ./internal/monitoring/ -run TestLiveWriter -v
//
// Connection defaults match the local compose stack (.env): native port 9003,
// user default / password sqd-clickhouse. Override via CLICKHOUSE_* env.
func TestLiveWriter(t *testing.T) {
	if os.Getenv("SQD_METRICS_CH_LIVE") == "" {
		t.Skip("set SQD_METRICS_CH_LIVE=1 to run against a live ClickHouse")
	}
	// Start() gates on SQD_METRICS_CH; set it and a fast cadence for the test.
	os.Setenv("SQD_METRICS_CH", "1")
	os.Setenv("SQD_METRICS_CH_INTERVAL", "500ms")

	cfg := Config{
		Host:     getenv("CLICKHOUSE_HOST", "127.0.0.1"),
		Port:     getenvInt("CLICKHOUSE_NATIVE_PORT", 9003),
		User:     getenv("CLICKHOUSE_USER", "default"),
		Password: getenv("CLICKHOUSE_PASSWORD", "sqd-clickhouse"),
	}

	ctx := context.Background()
	Start(ctx, cfg)
	if global == nil {
		t.Fatal("recorder did not start (connect failed?)")
	}

	// Feed two snapshots so the second flush computes non-zero rates.
	Observe(137, 1000, 8000, 1000, 990)
	time.Sleep(700 * time.Millisecond)
	Observe(137, 3500, 28000, 3500, 3490) // +2500 blocks, +20000 events
	time.Sleep(900 * time.Millisecond)
	Stop()

	t.Log("recorder ran; verify rows with: docker exec sqd-go-clickhouse-1 clickhouse-client --password sqd-clickhouse -q \"SELECT * FROM monitoring.indexer_metrics ORDER BY ts\"")
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				return def
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return def
}
