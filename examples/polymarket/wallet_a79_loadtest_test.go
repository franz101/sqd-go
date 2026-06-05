package polymarket

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/shopspring/decimal"
)

// exchangeOrderFilledTopic0 is the OrderFilled event signature on the Exchange /
// NegRiskExchange contracts (topics = [sig, orderHash, maker, taker]).
const exchangeOrderFilledTopic0 = "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"

// makerTopicForWallet builds a 32-byte indexed-address topic (left-padded) for a
// synthetic wallet index, so each duplicate produces a distinct (user, token_id)
// position — growing the UserPositions cache to stress GC / memory layout.
func makerTopicForWallet(i uint64) string {
	var addr [20]byte
	binPut := func(b []byte, v uint64) {
		for k := 0; k < 8; k++ {
			b[len(b)-1-k] = byte(v >> (8 * k))
		}
	}
	binPut(addr[12:], i+1) // last 8 bytes carry i; first 12 stay zero
	return "0x000000000000000000000000" + common.BytesToAddress(addr[:]).Hex()[2:]
}

// findTemplateOrderFilled returns the first OrderFilled CustomLog (clean, known to
// process), used as the clone template for synthetic duplicate wallets.
func findTemplateOrderFilled(t *testing.T, logs []ingestion.CustomLog) ingestion.CustomLog {
	t.Helper()
	want := common.HexToHash(exchangeOrderFilledTopic0)
	for _, lg := range logs {
		if len(lg.Topics) == 4 && common.HexToHash(lg.Topics[0]) == want {
			return lg
		}
	}
	t.Fatal("no OrderFilled template log found in A79 fixture")
	return ingestion.CustomLog{}
}

// cloneOrderFilledForWallet clones the template OrderFilled into a distinct wallet
// at the given block, preserving all non-maker fields so it processes cleanly.
func cloneOrderFilledForWallet(tmpl ingestion.CustomLog, wallet, block uint64) ingestion.CustomLog {
	c := tmpl
	c.Topics = make([]string, len(tmpl.Topics))
	copy(c.Topics, tmpl.Topics)
	c.Topics[2] = makerTopicForWallet(wallet) // rewrite maker (indexed) -> distinct user
	c.BlockNumber = block
	c.BlockTimestamp = time.Unix(int64(block), 0).UTC()
	c.BlockHash = fmt.Sprintf("0x%064x", block)
	c.TransactionHash = fmt.Sprintf("0x%064x", wallet)
	c.LogIndex = 0
	c.TransactionIndex = 0
	return c
}

// loadStats captures the throughput, GC, and CPU-profile outcome of one workload run.
type loadStats struct {
	version     int
	cold        bool
	blocks      uint64
	bytes       int64
	elapsed     time.Duration
	totalAlloc  uint64
	mallocs     uint64
	numGC       uint32
	pauseTotal  time.Duration
	heapInuse   uint64
	cpuProfPath string
}

func (s loadStats) blkPerSec() float64 {
	if s.elapsed <= 0 {
		return 0
	}
	return float64(s.blocks) / s.elapsed.Seconds()
}

// syntheticStream is a deterministic, memory-bounded generator of CustomLog
// batches: synthetic duplicate-wallet OrderFilled logs (each a distinct
// (user, token_id) position to grow the UserPositions ring) with the real A79
// wallet's events sprinkled through in their original order. It yields one batch
// at a time so a 2 GB (or 20 GB) workload never has to be fully resident — both
// V1 and V2 replay the *same* stream by re-seeding from the same A79 fixture.
type syntheticStream struct {
	tmpl       ingestion.CustomLog
	a79Blocks  [][]ingestion.CustomLog
	targetByte int64
	batchSize  int // blocks per Process() call
	sprinkle   int // emit one real A79 block every `sprinkle` synthetic blocks

	block   uint64
	wallet  uint64
	emitted int64
	a79Idx  int
	done    bool
}

func approxLogBytes(lg ingestion.CustomLog) int64 {
	n := len(lg.BlockHash) + len(lg.ContractAddress) + len(lg.TransactionHash) + len(lg.Data)
	for _, tp := range lg.Topics {
		n += len(tp)
	}
	return int64(n) + 64
}

func newSyntheticStream(t *testing.T, targetBytes int64) *syntheticStream {
	t.Helper()
	a79Logs := loadWalletCustomLogs(t, "../../tests/wallet_0xa79af3b_all.jsonl")
	tmpl := findTemplateOrderFilled(t, a79Logs)

	// Group the real A79 logs by their original block, preserving order.
	var blocks [][]ingestion.CustomLog
	var curNum uint64
	for _, lg := range a79Logs {
		if len(blocks) == 0 || lg.BlockNumber != curNum {
			blocks = append(blocks, nil)
			curNum = lg.BlockNumber
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], lg)
	}

	return &syntheticStream{
		tmpl:       tmpl,
		a79Blocks:  blocks,
		targetByte: targetBytes,
		batchSize:  256,
		sprinkle:   64,
		block:      1,
	}
}

// next returns the next batch of CustomLogs, or (nil, false) when the byte budget
// is met AND every real A79 block has been drained (so no A79 history is lost).
func (s *syntheticStream) next() ([]ingestion.CustomLog, bool) {
	if s.done {
		return nil, false
	}
	if s.emitted >= s.targetByte && s.a79Idx >= len(s.a79Blocks) {
		s.done = true
		return nil, false
	}
	var batch []ingestion.CustomLog
	for bb := 0; bb < s.batchSize; bb++ {
		if s.a79Idx < len(s.a79Blocks) && (s.emitted >= s.targetByte || int(s.block)%s.sprinkle == 0) {
			for _, lg := range s.a79Blocks[s.a79Idx] {
				nl := lg
				nl.BlockNumber = s.block
				nl.BlockHash = fmt.Sprintf("0x%064x", s.block)
				batch = append(batch, nl)
				s.emitted += approxLogBytes(nl)
			}
			s.a79Idx++
		} else if s.emitted < s.targetByte {
			dl := cloneOrderFilledForWallet(s.tmpl, s.wallet, s.block)
			s.wallet++
			batch = append(batch, dl)
			s.emitted += approxLogBytes(dl)
		}
		s.block++
		if s.emitted >= s.targetByte && s.a79Idx >= len(s.a79Blocks) {
			break
		}
	}
	if len(batch) == 0 {
		s.done = true
		return nil, false
	}
	return batch, true
}

// runProcessorWorkload drives the synthetic stream through one processor version
// (protoMode=false => V1 parsed path, true => V2 proto path) and measures
// throughput + GC. If cpuProfPath != "", a CPU profile of the whole run is written
// there. Snapshots are disabled (finalized-backfill behaviour) unless
// LOAD_TEST_SNAPSHOTS=1. The store is used for periodic hot-state commits and the
// final flush; per-position lazy loads don't fire because every synthetic wallet
// is new.
func runProcessorWorkload(t *testing.T, ctx context.Context, store *database.Store, protoMode, coldCache bool, targetBytes int64, cpuProfPath string) loadStats {
	t.Helper()
	proc, err := generated.NewProcessor(protoMode)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if os.Getenv("LOAD_TEST_SNAPSHOTS") != "1" {
		proc.SetSnapshotsEnabled(false)
	}
	// Cold tier: the store is a fresh (empty) ClickHouse, so the cold cache is
	// authoritative — a hot+cold miss is provably new and the per-position lazy
	// SELECT is skipped entirely (the V1≈V2 bottleneck). Pebble buffers are
	// off-heap and hard-capped, so the Go heap stays tiny.
	if coldCache {
		coldDir := filepath.Join(t.TempDir(), "coldcache")
		if err := proc.State.HotState.EnableColdCache(coldDir, true, 0, 0); err != nil {
			t.Fatalf("enable cold cache: %v", err)
		}
		defer func() { _ = proc.State.HotState.CloseColdCache() }()
	}

	stream := newSyntheticStream(t, targetBytes)

	var cpuFile *os.File
	if cpuProfPath != "" {
		cpuFile, err = os.Create(cpuProfPath)
		if err != nil {
			t.Fatalf("create cpu profile: %v", err)
		}
		if err := pprof.StartCPUProfile(cpuFile); err != nil {
			t.Fatalf("start cpu profile: %v", err)
		}
	}

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	start := time.Now()

	var blocks uint64
	for {
		batch, ok := stream.next()
		if !ok {
			break
		}
		if err := proc.Process(ctx, store, batch); err != nil {
			if cpuProfPath != "" {
				pprof.StopCPUProfile()
				cpuFile.Close()
			}
			t.Fatalf("processor.Process (v=%v): %v", protoMode, err)
		}
		// batch spans <= batchSize block numbers; count distinct blocks processed.
		blocks += uint64(stream.batchSize)
	}
	if err := proc.State.Commit(ctx, store); err != nil {
		t.Fatalf("final commit: %v", err)
	}

	elapsed := time.Since(start)
	if cpuProfPath != "" {
		pprof.StopCPUProfile()
		cpuFile.Close()
	}

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	version := 1
	if protoMode {
		version = 2
	}
	return loadStats{
		version:     version,
		cold:        coldCache,
		blocks:      blocks,
		bytes:       stream.emitted,
		elapsed:     elapsed,
		totalAlloc:  memAfter.TotalAlloc - memBefore.TotalAlloc,
		mallocs:     memAfter.Mallocs - memBefore.Mallocs,
		numGC:       memAfter.NumGC - memBefore.NumGC,
		pauseTotal:  time.Duration(memAfter.PauseTotalNs - memBefore.PauseTotalNs),
		heapInuse:   memAfter.HeapInuse,
		cpuProfPath: cpuProfPath,
	}
}

func logLoadStats(t *testing.T, s loadStats) {
	t.Helper()
	mib := func(b uint64) float64 { return float64(b) / (1 << 20) }
	t.Logf("[LOAD][V%d] cold=%v processed=%d blocks (%.1f MiB) in %s => %.0f blk/s",
		s.version, s.cold, s.blocks, float64(s.bytes)/(1<<20), s.elapsed.Round(time.Millisecond), s.blkPerSec())
	t.Logf("[LOAD][V%d][GC] TotalAlloc=%.0f MiB Mallocs=%d NumGC=%d PauseTotal=%s HeapInuse=%.0f MiB",
		s.version, mib(s.totalAlloc), s.mallocs, s.numGC, s.pauseTotal.Round(time.Microsecond), mib(s.heapInuse))
	if s.cpuProfPath != "" {
		t.Logf("[LOAD][V%d][CPU] profile written to %s (analyze: go tool pprof -top %s)", s.version, s.cpuProfPath, s.cpuProfPath)
	}
}

// assertA79UnderLoad recovers the real wallet's positions from ClickHouse and
// asserts the known-answer PnL, proving the duplicated bulk didn't perturb or
// drop A79's events (no data loss under load).
func assertA79UnderLoad(t *testing.T, ctx context.Context, store *database.Store, tag string) {
	t.Helper()
	fresh := generated.NewState()
	if err := fresh.HotState.UserPositions.Recover(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("recover positions: %v", err)
	}
	wal := common.HexToAddress("0xa79af3bab636f41f1f7bd1c568857dbdf4650beb")
	million := decimal.NewFromInt(1_000_000)
	realized := decimal.Zero
	openValueHalf := decimal.Zero
	var n int
	fresh.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.User != wal {
			return true
		}
		n++
		realized = realized.Add(toDecimal(pos.RealizedPnL))
		if !pos.Amount.IsZero() {
			openValueHalf = openValueHalf.Add(toDecimal(pos.Amount).Mul(decimal.NewFromFloat(0.5)))
		}
		return true
	})
	pnl := realized.Div(million)
	openVal := openValueHalf.Div(million)
	t.Logf("[LOAD][%s][A79] positions=%d realized=$%s open=$%s", tag, n, pnl.StringFixed(4), openVal.StringFixed(4))
	if n == 0 {
		t.Fatalf("[%s] no A79 positions persisted under load", tag)
	}
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("[%s] under load realized PnL = %s, want -13.93 (data loss?)", tag, pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("[%s] under load open value = %s, want 3.00 (data loss?)", tag, openVal.StringFixed(4))
	}
}

// loadTargetBytes resolves the byte budget (default 256 MiB; set 2147483648 for ~2 GB).
func loadTargetBytes() int64 {
	targetBytes := int64(256 << 20)
	if v := os.Getenv("LOAD_TEST_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			targetBytes = n
		}
	}
	return targetBytes
}

// TestLoadProcessorThroughput drives the V2 proto processor through the synthetic
// workload (~256 MiB default, set LOAD_TEST_BYTES=2147483648 for ~2 GB) to measure
// throughput + GC without portal HTTP, and confirms no data loss (A79 = -13.93/3.00).
// Gated behind LOAD_TEST=1. Set LOAD_TEST_CPUPROFILE=1 to also write a CPU profile.
func TestLoadProcessorThroughput(t *testing.T) {
	if os.Getenv("LOAD_TEST") != "1" {
		t.Skip("set LOAD_TEST=1 to run the load test")
	}
	ctx := context.Background()
	store := newPolymarketIntegrationStore(t, ctx)

	prof := ""
	if os.Getenv("LOAD_TEST_CPUPROFILE") == "1" {
		prof = filepath.Join(t.TempDir(), "cpu_v2.prof")
	}
	stats := runProcessorWorkload(t, ctx, store, true, true, loadTargetBytes(), prof)
	logLoadStats(t, stats)
	assertA79UnderLoad(t, ctx, store, "V2")
}

// TestLoadV1VsV2 runs the identical synthetic workload through both the V1 parsed
// path and the V2 proto path, on separate ClickHouse databases, writing a CPU
// profile for each so the per-version bottlenecks (string/hex conversion, hot-state
// commit, GC) can be compared directly. Both must reproduce A79 = -13.93/3.00.
//
// Gated behind LOAD_TEST=1. Profiles land in LOAD_TEST_PROFDIR (default a temp dir
// echoed in the log) as cpu_v1.prof / cpu_v2.prof.
func TestLoadV1VsV2(t *testing.T) {
	if os.Getenv("LOAD_TEST") != "1" {
		t.Skip("set LOAD_TEST=1 to run the V1-vs-V2 load comparison")
	}
	ctx := context.Background()
	target := loadTargetBytes()

	profDir := os.Getenv("LOAD_TEST_PROFDIR")
	if profDir == "" {
		profDir = t.TempDir()
	}

	// V1 (parsed) and V2 (proto) each get a fresh DB so their recovered A79 state
	// is independent; the synthetic stream is regenerated identically for each.
	// V1 is the legacy baseline (no cold tier). V2 enables the cold tier — it is
	// part of the V2 opt-in bundle — so the comparison shows V2 beating V1 by
	// removing the per-miss ClickHouse SELECT storm.
	storeV1 := newPolymarketIntegrationStore(t, ctx)
	v1 := runProcessorWorkload(t, ctx, storeV1, false, false, target, filepath.Join(profDir, "cpu_v1.prof"))
	logLoadStats(t, v1)
	assertA79UnderLoad(t, ctx, storeV1, "V1")

	storeV2 := newPolymarketIntegrationStore(t, ctx)
	v2 := runProcessorWorkload(t, ctx, storeV2, true, true, target, filepath.Join(profDir, "cpu_v2.prof"))
	logLoadStats(t, v2)
	assertA79UnderLoad(t, ctx, storeV2, "V2")

	t.Logf("[LOAD][CMP] V1=%.0f blk/s  V2=%.0f blk/s  (V2/V1=%.2fx)  | V1 alloc=%.0fMiB V2 alloc=%.0fMiB | profiles in %s",
		v1.blkPerSec(), v2.blkPerSec(), v2.blkPerSec()/v1.blkPerSec(),
		float64(v1.totalAlloc)/(1<<20), float64(v2.totalAlloc)/(1<<20), profDir)
}
