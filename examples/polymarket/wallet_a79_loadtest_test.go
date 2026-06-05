package polymarket

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
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
	// Deterministic, collision-free per i; offset away from the real wallet.
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

// TestLoadProcessorThroughput drives the V2 proto processor through a large,
// locally-generated workload (duplicated synthetic wallets + the real A79 events
// sprinkled across blocks, in order) to measure throughput + GC under a ~2 GB
// processed volume WITHOUT being bottlenecked by portal HTTP, and confirms no
// data loss: the real wallet still computes realized -$13.93 / open $3.00.
//
// Gated behind LOAD_TEST=1 (heavy). Size via LOAD_TEST_BYTES (default 256 MiB;
// set 2147483648 for ~2 GB). Duplicate wallets per A79 block via LOAD_TEST_FANOUT.
func TestLoadProcessorThroughput(t *testing.T) {
	if os.Getenv("LOAD_TEST") != "1" {
		t.Skip("set LOAD_TEST=1 to run the load test")
	}
	ctx := context.Background()
	store := newPolymarketIntegrationStore(t, ctx)

	targetBytes := int64(256 << 20)
	if v := os.Getenv("LOAD_TEST_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			targetBytes = n
		}
	}
	fanout := uint64(2000)
	if v := os.Getenv("LOAD_TEST_FANOUT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			fanout = n
		}
	}

	a79Logs := loadWalletCustomLogs(t, "../../tests/wallet_0xa79af3b_all.jsonl")
	tmpl := findTemplateOrderFilled(t, a79Logs)

	// Group the real A79 logs by their original block so we can replay them
	// block-by-block, in order, sprinkled through the synthetic stream.
	type a79Block struct {
		logs []ingestion.CustomLog
	}
	var a79Blocks []a79Block
	{
		var cur *a79Block
		var curNum uint64
		for _, lg := range a79Logs {
			if cur == nil || lg.BlockNumber != curNum {
				a79Blocks = append(a79Blocks, a79Block{})
				cur = &a79Blocks[len(a79Blocks)-1]
				curNum = lg.BlockNumber
			}
			cur.logs = append(cur.logs, lg)
		}
	}

	proc, err := generated.NewProcessor(true) // V2 proto
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	// Backfill behaviour: disable fork-recovery snapshots (the ingestion layer does
	// this in non-cursor mode). Set LOAD_TEST_SNAPSHOTS=1 to measure with them on.
	if os.Getenv("LOAD_TEST_SNAPSHOTS") != "1" {
		proc.SetSnapshotsEnabled(false)
	}

	// approxLogBytes: rough wire size per CustomLog for the byte budget.
	approxLogBytes := func(lg ingestion.CustomLog) int64 {
		n := len(lg.BlockHash) + len(lg.ContractAddress) + len(lg.TransactionHash) + len(lg.Data)
		for _, tp := range lg.Topics {
			n += len(tp)
		}
		return int64(n) + 64
	}

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	start := time.Now()

	var (
		block        uint64 = 1
		wallet       uint64
		bytesEmitted int64
		blocksProc   uint64
		a79Idx       int
		// emit one real A79 block every `sprinkle` synthetic blocks, preserving order.
		sprinkle = 64
	)

	// Process in contiguous block batches; each batch is one Process call.
	const batchBlocks = 256
	for bytesEmitted < targetBytes {
		var batch []ingestion.CustomLog
		for bb := 0; bb < batchBlocks; bb++ {
			// Sprinkle the next real A79 block (in order) periodically.
			if a79Idx < len(a79Blocks) && int(block)%sprinkle == 0 {
				for _, lg := range a79Blocks[a79Idx].logs {
					nl := lg
					nl.BlockNumber = block
					nl.BlockHash = fmt.Sprintf("0x%064x", block)
					batch = append(batch, nl)
					bytesEmitted += approxLogBytes(nl)
				}
				a79Idx++
			} else {
				// One synthetic duplicate-wallet OrderFilled.
				dl := cloneOrderFilledForWallet(tmpl, wallet, block)
				wallet++
				batch = append(batch, dl)
				bytesEmitted += approxLogBytes(dl)
			}
			block++
		}
		if err := proc.Process(ctx, store, batch); err != nil {
			t.Fatalf("processor.Process at block %d: %v", block, err)
		}
		blocksProc += batchBlocks
		_ = fanout
	}

	// Drain any remaining real A79 blocks so the wallet's full history is applied
	// in order (no data loss even if the byte budget cut the stream short).
	for ; a79Idx < len(a79Blocks); a79Idx++ {
		var batch []ingestion.CustomLog
		for _, lg := range a79Blocks[a79Idx].logs {
			nl := lg
			nl.BlockNumber = block
			nl.BlockHash = fmt.Sprintf("0x%064x", block)
			batch = append(batch, nl)
		}
		if err := proc.Process(ctx, store, batch); err != nil {
			t.Fatalf("processor.Process (A79 drain) at block %d: %v", block, err)
		}
		block++
		blocksProc++
	}

	if err := proc.State.Commit(ctx, store); err != nil {
		t.Fatalf("final commit: %v", err)
	}

	elapsed := time.Since(start)
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	mib := func(b uint64) float64 { return float64(b) / (1 << 20) }
	t.Logf("[LOAD] processed=%d blocks (%.1f MiB) in %s => %.0f blk/s",
		blocksProc, float64(bytesEmitted)/(1<<20), elapsed.Round(time.Millisecond),
		float64(blocksProc)/elapsed.Seconds())
	t.Logf("[LOAD][GC] TotalAlloc=%.0f MiB Mallocs=%d NumGC=%d PauseTotal=%s HeapInuse=%.0f MiB",
		mib(memAfter.TotalAlloc-memBefore.TotalAlloc),
		memAfter.Mallocs-memBefore.Mallocs,
		memAfter.NumGC-memBefore.NumGC,
		time.Duration(memAfter.PauseTotalNs-memBefore.PauseTotalNs).Round(time.Microsecond),
		mib(memAfter.HeapInuse))

	// No-data-loss check: recover A79 positions from ClickHouse, assert PnL.
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
	t.Logf("[LOAD][A79] positions=%d realized=$%s open=$%s", n, pnl.StringFixed(4), openVal.StringFixed(4))
	if n == 0 {
		t.Fatal("no A79 positions persisted under load")
	}
	if pnl.Sub(decimal.NewFromFloat(-13.93)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("under load realized PnL = %s, want -13.93 (data loss?)", pnl.StringFixed(4))
	}
	if openVal.Sub(decimal.NewFromFloat(3.00)).Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		t.Errorf("under load open value = %s, want 3.00 (data loss?)", openVal.StringFixed(4))
	}
}
