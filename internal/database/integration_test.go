package database

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
)

// chEnv resolves ClickHouse connection parameters from the environment,
// matching the convention established in internal/ingestion/integration_test.go
// and internal/monitoring/live_test.go. Defaults match CI's docker-compose
// service (native port 9000); override CLICKHOUSE_NATIVE_PORT for local runs
// against the host-mapped port (e.g. 9003).
func chEnv() (host string, port int, password string) {
	host = os.Getenv("CLICKHOUSE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port = 9000
	if p := os.Getenv("CLICKHOUSE_NATIVE_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	password = os.Getenv("CLICKHOUSE_PASSWORD")
	if password == "" {
		password = "sqd-clickhouse"
	}
	return
}

// clickhouseAvailable reports whether a live ClickHouse is reachable using
// chEnv()'s connection parameters. Used to skip integration tests cleanly in
// environments (including the build-test CI job) where no ClickHouse service
// is running.
func clickhouseAvailable() bool {
	host, port, password := chEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", host, port),
		Database: "default",
		User:     "default",
		Password: password,
	})
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// newTestStore skips the calling test if no live ClickHouse is reachable;
// otherwise it creates a uniquely-named throwaway database, returns a *Store
// connected to it, and registers cleanup to close the store and drop the
// database when the test finishes.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	if !clickhouseAvailable() {
		t.Skip("ClickHouse not available; set CLICKHOUSE_HOST/CLICKHOUSE_NATIVE_PORT or run with docker")
	}

	host, port, password := chEnv()
	dbName := fmt.Sprintf("db_test_%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := NewClickHouse(ctx, host, port, "default", password, dbName)
	if err != nil {
		t.Fatalf("NewClickHouse(%s): %v", dbName, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if err := DropClickHouseDatabase(dropCtx, host, port, "default", password, dbName); err != nil {
			t.Logf("cleanup: DropClickHouseDatabase(%s): %v", dbName, err)
		}
	})
	return store
}
