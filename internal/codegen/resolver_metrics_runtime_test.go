package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGeneratedResolverMetricsRuntime(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}

	root := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("config.yaml", `name: resolver_metric_fixture
chains:
  - id: 137
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address taker, uint256 makerAssetId)
`)
	writeFile("custom_schema.go", `package resolver_metric_fixture

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// pk: User, TokenID
type MemoryUserPositionSchema struct {
	User           common.Address
	TokenID        common.Hash
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}
`)
	writeFile("go.mod", "module resolvermetricfixture\n\ngo 1.25\n\nrequire github.com/franz101/sqd-go v0.0.0\n\nreplace github.com/franz101/sqd-go => "+filepath.ToSlash(repoRoot(t))+"\n")

	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	testPath := filepath.Join(root, "generated", "resolver_metrics_test.go")
	testSource := `package generated

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestResolverMetricDeltasResetAndWrap(t *testing.T) {
	hot := NewHotState(8)
	resolver := hot.UserPositionsResolver
	resolver.EnableMetrics(true)
	key := UserPositionsClockKey{
		User: common.HexToAddress("0x1"),
		TokenID: common.HexToHash("0x11"),
	}
	hot.UserPositions.SetByKey(key, MemoryUserPosition{User: key.User, TokenID: key.TokenID})

	if _, ok := resolver.Lookup(key); !ok {
		t.Fatal("hot lookup missed")
	}
	missing := UserPositionsClockKey{
		User: common.HexToAddress("0x2"),
		TokenID: common.HexToHash("0x22"),
	}
	resolver.Queue(missing)
	resolver.Queue(missing)

	got := resolver.SnapshotAndResetMetrics()
	if got.HotHits != 1 || got.QueuedMisses != 2 {
		t.Fatalf("metrics = %+v, want hot_hits=1 queued_misses=2", got)
	}
	if resolver.Pending() != 2 {
		t.Fatalf("pending = %d, want 2", resolver.Pending())
	}
	if reset := resolver.Metrics(); reset != (BatchResolverMetrics{}) {
		t.Fatalf("metrics after reset = %+v, want zero", reset)
	}

	resolver.metrics.HotHits = ^uint64(0)
	if _, ok := resolver.Lookup(key); !ok {
		t.Fatal("wrap lookup missed")
	}
	if wrapped := resolver.Metrics().HotHits; wrapped != 0 {
		t.Fatalf("wrapped hot hits = %d, want 0", wrapped)
	}
}

func TestResolverMetricColdHit(t *testing.T) {
	t.Setenv("SQD_COLDFILTER_BITS", "0")
	hot := NewHotState(1)
	hot.UserPositionsResolver.EnableMetrics(true)
	if err := hot.EnableColdCache(t.TempDir(), false, 1<<20, 1<<20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hot.CloseColdCache(); err != nil {
			t.Error(err)
		}
	})

	first := UserPositionsClockKey{
		User: common.HexToAddress("0x1"),
		TokenID: common.HexToHash("0x11"),
	}
	second := UserPositionsClockKey{
		User: common.HexToAddress("0x2"),
		TokenID: common.HexToHash("0x22"),
	}
	hot.UserPositions.SetByKey(first, MemoryUserPosition{User: first.User, TokenID: first.TokenID})
	hot.UserPositions.SetByKey(second, MemoryUserPosition{User: second.User, TokenID: second.TokenID})

	if _, ok := hot.UserPositionsResolver.Lookup(first); !ok {
		t.Fatal("cold lookup missed")
	}
	if got := hot.UserPositionsResolver.Metrics(); got.ColdHits != 1 {
		t.Fatalf("metrics = %+v, want cold_hits=1", got)
	}
}
`
	if err := os.WriteFile(testPath, []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}

	if output, err := runGo(goBin, root, os.Environ(), "test", "./generated", "-run", "^TestResolverMetric", "-count=1"); err != nil {
		t.Fatalf("generated resolver metric tests failed: %v\n%s", err, output)
	}
}
