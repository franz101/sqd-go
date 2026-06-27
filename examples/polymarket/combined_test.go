package polymarket

// combined_test.go — the "combine it all" measurement that the isolated parser
// and isolated protomath benchmarks miss. It answers the real question:
//
//   The byte-scanner hits 8M ev/s on 6 cores in isolation, but the production
//   pipeline after the SPSC buffer is single-threaded: parse on one core in the
//   producer, the ORDERED custom processor on one core in the consumer. With the
//   processor at ~54% of wall time, Amdahl caps the whole pipeline at ~1.5x no
//   matter how fast the parser gets. The ONLY lever that breaks that ceiling is
//   parallelizing the custom processor itself.
//
// The custom processor's OrderFilled handler mutates exactly one entity key,
// Position(maker, tokenID) (see custom_processor.go handleOrderFilledValues ->
// updateUserPositionWithBuy/SellD256). State is fully keyed, there are no global
// accumulators, and the only intra-block coupling is same-(user,tokenID)
// ordering. That is the precondition for ECS/DB-style key-sharding: hash the
// entity key to a shard, preserve order WITHIN a shard, run shards in parallel.
//
// This file proves it on the REAL corpus distribution (real maker skew, not the
// uniform i%positions of positions_e2e_bench_test.go) with the REAL arithmetic
// (protomath Decimal256, the exact avg-price/PnL formula extracted faithfully
// below). It measures the combined single-core ceiling, the sharded multi-core
// speedup, the realistic skew-bound on that speedup, and asserts the sharded
// final state is BIT-IDENTICAL to the serial final state.

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/abiunpack"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
)

// ofPosKey mirrors the production position key (user, tokenID) — the unit of
// state the OrderFilled handler mutates and therefore the unit we shard by.
type ofPosKey struct {
	user    common.Address
	tokenID common.Hash
}

// ofEvent is a decoded OrderFilled carrying exactly the fields
// handleOrderFilledValues consumes.
type ofEvent struct {
	maker        common.Address
	makerAssetID uint256.Int
	takerAssetID uint256.Int
	makerAmt     uint256.Int
	takerAmt     uint256.Int
}

// shardOf hashes (user, tokenID) the same way the production processor's
// hashPositionKey does: 8 bytes off two keccak-derived fields, low bits already
// uniform. A key always maps to the same shard, so per-key order is preserved by
// construction.
func shardOf(k ofPosKey, nShards int) int {
	h := binary.LittleEndian.Uint64(k.user[:8]) ^ binary.LittleEndian.Uint64(k.tokenID[:8])
	return int(h % uint64(nShards))
}

// ---- faithful extraction of the production OrderFilled state mutation ----
// applyBuyD256 / applySellD256 are line-for-line copies of
// updateUserPositionWithBuyD256 / updateUserPositionWithSellD256 (custom_processor.go),
// with state.Position.Get/Save replaced by direct map access so the final state
// is complete (unbounded, no hot-ring eviction) and bit-comparable. The
// arithmetic is the production arithmetic.

func applyBuyD256(up *generated.Position, price, amount protomath.Decimal256) {
	if amount.IsZero() {
		return
	}
	scale := protomath.Decimal256Scale18
	if denom, ok := up.Amount.Add(amount); ok && !denom.IsZero() {
		numerA, okA := up.AvgPrice.Mul(up.Amount, scale)
		numerB, okB := price.Mul(amount, scale)
		if okA && okB {
			if numer, okN := numerA.Add(numerB); okN {
				if avg, okD := numer.Div(denom, scale); okD {
					up.AvgPrice = avg
				}
			}
		}
	}
	if v, ok := up.Amount.Add(amount); ok {
		up.Amount = v
	}
	if v, ok := up.TotalBought.Add(amount); ok {
		up.TotalBought = v
	}
}

func applySellD256(up *generated.Position, price, amount protomath.Decimal256) {
	adjAmt := amount
	if adjAmt.Gt(up.Amount) {
		adjAmt = up.Amount
	}
	if adjAmt.IsZero() {
		return
	}
	scale := protomath.Decimal256Scale18
	if spread, ok := price.Sub(up.AvgPrice); ok {
		if pnl, ok := adjAmt.Mul(spread, scale); ok {
			if v, ok := up.RealizedPnL.Add(pnl); ok {
				up.RealizedPnL = v
			}
		}
	}
	if v, ok := up.Amount.Sub(adjAmt); ok {
		up.Amount = v
	}
}

// applyOrderFilled is handleOrderFilledValues against a plain map. Faithful to
// custom_processor.go:675.
func applyOrderFilled(positions map[ofPosKey]generated.Position, ev *ofEvent) {
	makerFilled, okM := usdcRawToDec18(&ev.makerAmt)
	takerFilled, okT := usdcRawToDec18(&ev.takerAmt)
	if !okM || !okT {
		return
	}
	var tokenID uint256.Int
	var baseAmount, quoteAmount protomath.Decimal256
	isBuy := ev.makerAssetID.IsZero()
	if isBuy {
		tokenID = ev.takerAssetID
		baseAmount = takerFilled
		quoteAmount = makerFilled
	} else {
		tokenID = ev.makerAssetID
		baseAmount = makerFilled
		quoteAmount = takerFilled
	}
	var price protomath.Decimal256
	if !baseAmount.IsZero() {
		var ok bool
		if price, ok = quoteAmount.Div(baseAmount, protomath.Decimal256Scale18); !ok {
			return
		}
	}
	key := ofPosKey{user: ev.maker, tokenID: tokenIDHash(tokenID)}
	if isBuy {
		up, ok := positions[key]
		if !ok {
			up = generated.Position{User: ev.maker, TokenID: key.tokenID}
		}
		applyBuyD256(&up, price, baseAmount)
		positions[key] = up
	} else {
		up, ok := positions[key]
		if !ok {
			return // sell on an unseen position is a no-op (matches production)
		}
		applySellD256(&up, price, baseAmount)
		positions[key] = up
	}
}

// collectOrderFilledV2 byte-scans a page and appends every Exchange /
// NegRiskExchange OrderFilled as a decoded ofEvent (same scan as
// ParseOrderFilledV2; header is irrelevant to position math so it is skipped).
func collectOrderFilledV2(data []byte, dst []ofEvent) []ofEvent {
	var dataBytes []byte
	var topics [4][]byte
	rest := data
	for len(rest) > 0 {
		var line []byte
		if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
			line, rest = rest[:nl], rest[nl+1:]
		} else {
			line, rest = rest, nil
		}
		li := bytes.Index(line, v2Logs)
		if li < 0 {
			continue
		}
		p := line[li+len(v2Logs):]
		for {
			i := bytes.Index(p, v2LogIndexKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2LogIndexKey):]
			_, p = scanUintAdv(p)

			i = bytes.Index(p, v2TxIndexKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2TxIndexKey):]
			_, p = scanUintAdv(p)

			i = bytes.Index(p, v2AddrKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2AddrKey):]
			var addr []byte
			addr, p = scanQuotedAdv(p)

			i = bytes.Index(p, v2DataKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2DataKey):]
			var dataHex []byte
			dataHex, p = scanQuotedAdv(p)

			i = bytes.Index(p, v2TopicsKey)
			if i < 0 {
				break
			}
			p = p[i+len(v2TopicsKey):]
			topics[0], topics[1], topics[2], topics[3] = nil, nil, nil, nil
			for idx := 0; idx < 4; idx++ {
				rb := bytes.IndexByte(p, ']')
				qs := bytes.IndexByte(p, '"')
				if qs < 0 || (rb >= 0 && rb < qs) {
					break
				}
				p = p[qs+1:]
				qe := bytes.IndexByte(p, '"')
				if qe < 0 {
					break
				}
				topics[idx] = p[:qe]
				p = p[qe+1:]
			}
			if rb := bytes.IndexByte(p, ']'); rb >= 0 {
				p = p[rb+1:]
			}

			if !lowerEq(topics[0], v2OrderFilledT0) {
				continue
			}
			isExchange := lowerEq(addr, v2ExchangeAddr)
			isNegRisk := !isExchange && lowerEq(addr, v2NegRiskAddr)
			if !isExchange && !isNegRisk {
				continue
			}
			var ev ofEvent
			ev.maker = abiunpack.DecodeTopicAddress(bstr(topics[2]))
			dataBytes = abiunpack.AppendHexBytes(dataBytes[:0], bstr(dataHex))
			if w, ok := abiunpack.Word(dataBytes, 0); ok {
				ev.makerAssetID.SetBytes32(w)
			}
			if w, ok := abiunpack.Word(dataBytes, 1); ok {
				ev.takerAssetID.SetBytes32(w)
			}
			if w, ok := abiunpack.Word(dataBytes, 2); ok {
				ev.makerAmt.SetBytes32(w)
			}
			if w, ok := abiunpack.Word(dataBytes, 3); ok {
				ev.takerAmt.SetBytes32(w)
			}
			dst = append(dst, ev)
		}
	}
	return dst
}

// d256eq compares two Decimal256 by their canonical 32-byte little-endian form.
func d256eq(a, b protomath.Decimal256) bool {
	var ra, rb [32]byte
	a.PutLittleEndianBytes(&ra)
	b.PutLittleEndianBytes(&rb)
	return ra == rb
}

func posEq(a, b generated.Position) bool {
	return a.User == b.User && a.TokenID == b.TokenID &&
		d256eq(a.Amount, b.Amount) && d256eq(a.AvgPrice, b.AvgPrice) &&
		d256eq(a.RealizedPnL, b.RealizedPnL) && d256eq(a.TotalBought, b.TotalBought)
}

// TestCombinedParseProcessSharded is the headline measurement for the goal:
// combine parse + the real ordered processor, find the single-core ceiling, then
// show key-sharding parallelizes the processor with a bit-identical result.
func TestCombinedParseProcessSharded(t *testing.T) {
	pages := loadOFCorpus(t)
	if len(pages) == 0 {
		t.Skip("empty corpus")
	}

	// Decode the whole corpus once into a flat, globally-ordered event slice.
	var events []ofEvent
	for _, pg := range pages {
		events = collectOrderFilledV2(pg, events)
	}
	if len(events) == 0 {
		t.Fatal("no OrderFilled events decoded")
	}
	t.Logf("corpus: %d pages, %d OrderFilled events", len(pages), len(events))

	// ---- serial: the production single-core combined behaviour ----
	serial := make(map[ofPosKey]generated.Position, len(events)/2)
	for i := range events {
		applyOrderFilled(serial, &events[i])
	}
	t.Logf("distinct positions: %d (%.1f events/position)", len(serial), float64(len(events))/float64(len(serial)))

	// Timed serial process (state-math only; this is the 54% slice in isolation).
	const K = 8
	serialEvps := bestEvps(len(events), K, func() {
		m := make(map[ofPosKey]generated.Position, len(serial))
		for i := range events {
			applyOrderFilled(m, &events[i])
		}
		_ = m
	})
	t.Logf("PROCESS serial (1 core): %.2fM ev/s", serialEvps)

	// ---- sharded process: hash(key) -> shard, fold each shard independently ----
	// Pre-partition once (deterministic, order-preserving within a shard) so the
	// timed section measures pure parallel state-math, the ceiling for this lever.
	maxShards := runtime.NumCPU()
	for _, nShards := range shardCounts(maxShards) {
		buckets := make([][]int, nShards) // event indices per shard, in order
		for i := range events {
			ev := &events[i]
			// Recompute the routing key exactly as applyOrderFilled does.
			var tokenID uint256.Int
			if ev.makerAssetID.IsZero() {
				tokenID = ev.takerAssetID
			} else {
				tokenID = ev.makerAssetID
			}
			k := ofPosKey{user: ev.maker, tokenID: tokenIDHash(tokenID)}
			s := shardOf(k, nShards)
			buckets[s] = append(buckets[s], i)
		}

		// Skew report: the hottest shard bounds the achievable speedup.
		sizes := make([]int, nShards)
		maxSize := 0
		for s := range buckets {
			sizes[s] = len(buckets[s])
			if sizes[s] > maxSize {
				maxSize = sizes[s]
			}
		}
		idealFrac := 1.0 / float64(nShards)
		hottestFrac := float64(maxSize) / float64(len(events))
		skewBound := 1.0 / (hottestFrac * float64(nShards)) // speedup ceiling vs ideal nShards x

		shardMaps := make([]map[ofPosKey]generated.Position, nShards)
		shardedEvps := bestEvps(len(events), K, func() {
			var wg sync.WaitGroup
			for s := 0; s < nShards; s++ {
				wg.Add(1)
				go func(s int) {
					defer wg.Done()
					m := make(map[ofPosKey]generated.Position, len(buckets[s]))
					for _, i := range buckets[s] {
						applyOrderFilled(m, &events[i])
					}
					shardMaps[s] = m
				}(s)
			}
			wg.Wait()
		})

		// ---- correctness gate: merged shards == serial, bit-identical ----
		merged := make(map[ofPosKey]generated.Position, len(serial))
		for _, m := range shardMaps {
			for k, v := range m {
				if _, dup := merged[k]; dup {
					t.Fatalf("shard=%d key collision across shards (routing bug)", nShards)
				}
				merged[k] = v
			}
		}
		if len(merged) != len(serial) {
			t.Fatalf("shard=%d position count mismatch: sharded %d vs serial %d", nShards, len(merged), len(serial))
		}
		for k, want := range serial {
			got, ok := merged[k]
			if !ok || !posEq(got, want) {
				t.Fatalf("shard=%d STATE MISMATCH at key %x/%x: sharded != serial", nShards, k.user, k.tokenID)
			}
		}

		t.Logf("PROCESS sharded x%-2d: %5.2fM ev/s  (%.2fx serial)  hottest shard %.1f%% (ideal %.1f%%, skew-bound %.2fx)  STATE OK",
			nShards, shardedEvps, shardedEvps/serialEvps, hottestFrac*100, idealFrac*100, skewBound*float64(nShards))
	}
}

// TestCombinedEndToEnd measures the full combined wall: serial(parse+process) vs
// parallel-parse + sharded-process, the real "combine it all" speedup.
func TestCombinedEndToEnd(t *testing.T) {
	pages := loadOFCorpus(t)
	if len(pages) == 0 {
		t.Skip("empty corpus")
	}
	nCPU := runtime.NumCPU()
	nShards := nCPU
	const K = 8

	// Decode once for the router-only measurement and event count.
	var events []ofEvent
	for _, pg := range pages {
		events = collectOrderFilledV2(pg, events)
	}
	total := len(events)

	// ---- serial parse + serial process, one core: the production ceiling ----
	serialEvps := bestEvps(total, K, func() {
		m := make(map[ofPosKey]generated.Position, total/2)
		var evs []ofEvent
		for _, pg := range pages {
			evs = collectOrderFilledV2(pg, evs[:0])
			for i := range evs {
				applyOrderFilled(m, &evs[i])
			}
		}
		_ = m
	})
	t.Logf("COMBINED serial parse+process (1 core): %.2fM ev/s  [the production pipeline today]", serialEvps)

	// ---- isolated parallel stages (to expose the barrier tax) ----
	// Stage A: parallel parse, one worker per contiguous page range.
	parseEvps := bestEvps(total, K, func() {
		var wg sync.WaitGroup
		for w := 0; w < nCPU; w++ {
			lo, hi := w*len(pages)/nCPU, (w+1)*len(pages)/nCPU
			if lo >= hi {
				continue
			}
			wg.Add(1)
			go func(lo, hi int) {
				defer wg.Done()
				var evs []ofEvent
				for pi := lo; pi < hi; pi++ {
					evs = collectOrderFilledV2(pages[pi], evs[:0])
				}
				_ = evs
			}(lo, hi)
		}
		wg.Wait()
	})

	// Router-only: hash every event and scatter into shard buckets. NO compute.
	// This is the serial hop between parse and a sharded consumer; if it is much
	// faster than the per-shard compute, the pipelined ceiling is min(parse,process),
	// not router-bound.
	routerEvps := bestEvps(total, K, func() {
		buckets := make([][]ofEvent, nShards)
		for i := range events {
			ev := &events[i]
			var tok uint256.Int
			if ev.makerAssetID.IsZero() {
				tok = ev.takerAssetID
			} else {
				tok = ev.makerAssetID
			}
			s := shardOf(ofPosKey{user: ev.maker, tokenID: tokenIDHash(tok)}, nShards)
			buckets[s] = append(buckets[s], *ev)
		}
		_ = buckets
	})

	// Pre-partition for the sharded-process-only measurement.
	procBuckets := make([][]int, nShards)
	for i := range events {
		ev := &events[i]
		var tok uint256.Int
		if ev.makerAssetID.IsZero() {
			tok = ev.takerAssetID
		} else {
			tok = ev.makerAssetID
		}
		s := shardOf(ofPosKey{user: ev.maker, tokenID: tokenIDHash(tok)}, nShards)
		procBuckets[s] = append(procBuckets[s], i)
	}
	procEvps := bestEvps(total, K, func() {
		var wg sync.WaitGroup
		for s := 0; s < nShards; s++ {
			wg.Add(1)
			go func(s int) {
				defer wg.Done()
				m := make(map[ofPosKey]generated.Position, len(procBuckets[s]))
				for _, i := range procBuckets[s] {
					applyOrderFilled(m, &events[i])
				}
			}(s)
		}
		wg.Wait()
	})

	t.Logf("  isolated stages: parse(%d) %.2fM ev/s | router(1, hash+scatter) %.2fM ev/s | process(%d shards) %.2fM ev/s",
		nCPU, parseEvps, routerEvps, nShards, procEvps)

	// ---- BARRIER: parallel parse -> sharded process with a barrier between ----
	// Wall = parse-then-process, so throughput is the harmonic sum of the two
	// parallel stages: cores idle during whichever stage they are not in.
	barrierEvps := bestEvps(total, K, func() {
		pageEvents := make([][]ofEvent, len(pages))
		var wg sync.WaitGroup
		for pi := range pages {
			wg.Add(1)
			go func(pi int) {
				defer wg.Done()
				pageEvents[pi] = collectOrderFilledV2(pages[pi], nil)
			}(pi)
		}
		wg.Wait()
		buckets := make([][]ofEvent, nShards)
		for pi := range pageEvents {
			for i := range pageEvents[pi] {
				ev := &pageEvents[pi][i]
				var tok uint256.Int
				if ev.makerAssetID.IsZero() {
					tok = ev.takerAssetID
				} else {
					tok = ev.makerAssetID
				}
				s := shardOf(ofPosKey{user: ev.maker, tokenID: tokenIDHash(tok)}, nShards)
				buckets[s] = append(buckets[s], *ev)
			}
		}
		var wg2 sync.WaitGroup
		for s := 0; s < nShards; s++ {
			wg2.Add(1)
			go func(s int) {
				defer wg2.Done()
				m := make(map[ofPosKey]generated.Position, len(buckets[s]))
				for i := range buckets[s] {
					applyOrderFilled(m, &buckets[s][i])
				}
			}(s)
		}
		wg2.Wait()
	})
	harmonicPred := 1.0 / (1.0/parseEvps + 1.0/procEvps)
	t.Logf("COMBINED barrier (parallel parse -> sharded process): %.2fM ev/s  (%.2fx serial)  [harmonic-sum of stages, predicted %.2fM]",
		barrierEvps, barrierEvps/serialEvps, harmonicPred)

	// ---- PIPELINED: all three stages overlap, no barrier ----
	// Parse workers run ahead (up to nCPU pages concurrently), landing results in
	// ordered slots. An order-preserving router consumes slot pi as soon as it is
	// ready and scatters to S shard channels (router does only hash+scatter, the
	// 21M-ev/s stage). Shard workers drain concurrently. Parse(page N+k),
	// route(page N+1), and process(page N) all run at once -> steady-state
	// throughput approaches the SLOWER single stage, min(parse, process), NOT the
	// harmonic sum. This is the producer||consumer ring with BOTH sides parallel.
	const batchSz = 512
	pipeEvps := bestEvps(total, K, func() {
		pageEvents := make([][]ofEvent, len(pages))
		ready := make([]chan struct{}, len(pages))
		for pi := range pages {
			ready[pi] = make(chan struct{})
		}
		// parse pool, bounded to nCPU, results in ordered slots
		sem := make(chan struct{}, nCPU)
		for pi := range pages {
			go func(pi int) {
				sem <- struct{}{}
				pageEvents[pi] = collectOrderFilledV2(pages[pi], nil)
				<-sem
				close(ready[pi])
			}(pi)
		}
		// shard workers
		chans := make([]chan []ofEvent, nShards)
		var wg sync.WaitGroup
		for s := 0; s < nShards; s++ {
			chans[s] = make(chan []ofEvent, 8)
			wg.Add(1)
			go func(s int) {
				defer wg.Done()
				m := make(map[ofPosKey]generated.Position)
				for batch := range chans[s] {
					for i := range batch {
						applyOrderFilled(m, &batch[i])
					}
				}
			}(s)
		}
		// router: consume pages in order as they become ready, hash+scatter only
		bufs := make([][]ofEvent, nShards)
		for s := range bufs {
			bufs[s] = make([]ofEvent, 0, batchSz)
		}
		for pi := range pages {
			<-ready[pi]
			for i := range pageEvents[pi] {
				ev := &pageEvents[pi][i]
				var tok uint256.Int
				if ev.makerAssetID.IsZero() {
					tok = ev.takerAssetID
				} else {
					tok = ev.makerAssetID
				}
				s := shardOf(ofPosKey{user: ev.maker, tokenID: tokenIDHash(tok)}, nShards)
				bufs[s] = append(bufs[s], *ev)
				if len(bufs[s]) >= batchSz {
					chans[s] <- bufs[s]
					bufs[s] = make([]ofEvent, 0, batchSz)
				}
			}
			pageEvents[pi] = nil
		}
		for s := 0; s < nShards; s++ {
			if len(bufs[s]) > 0 {
				chans[s] <- bufs[s]
			}
			close(chans[s])
		}
		wg.Wait()
	})
	pipeCeil := parseEvps
	if procEvps < pipeCeil {
		pipeCeil = procEvps
	}
	t.Logf("COMBINED pipelined (parallel parse || router || sharded process, no barrier): %.2fM ev/s  (%.2fx serial)  [ceiling min(parse,process)=%.2fM]",
		pipeEvps, pipeEvps/serialEvps, pipeCeil)
}

// bestEvps runs fn K times (after one warmup) and returns the BEST throughput in
// M ev/s. Best-of-K is the standard estimator for a micro-benchmark's achievable
// rate: it discards runs contaminated by thermal throttling, GC, and E-core
// scheduling jitter (this is a 10-core P+E laptop; absolute clocks vary, the peak
// is the stable, reproducible quantity).
func bestEvps(total, K int, fn func()) float64 {
	fn() // warmup (JIT-free Go, but warms caches / grows scratch)
	best := 0.0
	for k := 0; k < K; k++ {
		t0 := time.Now()
		fn()
		evps := float64(total) / time.Since(t0).Seconds() / 1e6
		if evps > best {
			best = evps
		}
	}
	return best
}

func shardCounts(maxShards int) []int {
	cands := []int{1, 2, 4, 6, 8, 12, 16}
	var out []int
	for _, c := range cands {
		if c <= maxShards {
			out = append(out, c)
		}
	}
	if len(out) == 0 || out[len(out)-1] != maxShards {
		out = append(out, maxShards)
	}
	sort.Ints(out)
	// dedup
	uniq := out[:0]
	prev := -1
	for _, c := range out {
		if c != prev {
			uniq = append(uniq, c)
			prev = c
		}
	}
	return uniq
}
