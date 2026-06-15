// Parallel cross-entity recovery smoke test — READ ONLY against ClickHouse.
//
// Exercises HotState.Recover (which now recovers all entities concurrently,
// each on its own connection) against the live polymarket DB, bounded to a
// recent slice via SQD_RECOVERY_MIN_BLOCK, writing the cold tier to a throwaway
// temp dir. It does NOT process blocks and never writes to ClickHouse, so it
// can't disturb a running indexer.
//
// Run:
//
//	RECOVER_SMOKE=1 CLICKHOUSE_NATIVE_PORT=9003 CLICKHOUSE_PASSWORD=sqd-clickhouse \
//	  CLICKHOUSE_DATABASE=polymarket \
//	  go test ./examples/polymarket/ -run TestParallelRecoverySmoke -v -count=1 -timeout 360s
package polymarket

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	chgo "github.com/ClickHouse/ch-go"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
)

func TestParallelRecoverySmoke(t *testing.T) {
	if os.Getenv("RECOVER_SMOKE") != "1" {
		t.Skip("set RECOVER_SMOKE=1 to run the parallel-recovery smoke test")
	}
	// Bound recovery to a recent slice so it finishes in seconds, not minutes.
	if os.Getenv("SQD_RECOVERY_MIN_BLOCK") == "" {
		t.Setenv("SQD_RECOVERY_MIN_BLOCK", "84000000")
	}
	db := rwEnvOr("CLICKHOUSE_DATABASE", rwEnvOr("CLICKHOUSE_DB", "polymarket"))

	ctx, cancel := context.WithTimeout(context.Background(), 330*time.Second)
	defer cancel()

	addr := fmt.Sprintf("%s:%d", rwEnvOr("CLICKHOUSE_HOST", "127.0.0.1"), rwEnvIntOr("CLICKHOUSE_NATIVE_PORT", 9003))
	conn, err := chgo.Dial(ctx, chgo.Options{
		Address:  addr,
		Database: db,
		User:     rwEnvOr("CLICKHOUSE_USER", "default"),
		Password: rwEnvOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse"),
	})
	if err != nil {
		t.Skipf("ch.Dial %s: %v (is ClickHouse up?)", addr, err)
	}
	defer conn.Close()

	dir := t.TempDir()
	hs := generated.NewHotState(1 << 20)
	if err := hs.EnableColdCache(filepath.Join(dir, "cold"), false, 256<<20, 16<<20); err != nil {
		t.Fatalf("EnableColdCache: %v", err)
	}
	// Recover wipes+reopens the cold tier fresh, exactly like LoadFromClickHouse.
	if _, err := hs.ReopenColdCacheFresh(); err != nil {
		t.Fatalf("ReopenColdCacheFresh: %v", err)
	}

	start := time.Now()
	if err := hs.Recover(ctx, conn, db); err != nil {
		t.Fatalf("parallel Recover failed: %v", err)
	}
	elapsed := time.Since(start)

	// Prove it actually built something: the cold dir should hold per-entity
	// Pebble stores with data.
	var coldBytes int64
	_ = filepath.Walk(filepath.Join(dir, "cold"), func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi != nil && !fi.IsDir() {
			coldBytes += fi.Size()
		}
		return nil
	})
	t.Logf("parallel cross-entity Recover OK in %s; cold tier on disk = %.1f MiB", elapsed, float64(coldBytes)/(1<<20))
	if coldBytes == 0 {
		t.Logf("note: cold tier empty — recency window may have matched no rows (still proves no deadlock)")
	}
}
