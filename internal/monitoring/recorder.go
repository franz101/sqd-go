// Package monitoring writes indexer runtime and throughput metrics into a
// dedicated ClickHouse table so they can be graphed in Grafana through the same
// ClickHouse data source used for the server's own system tables.
//
// It is opt-in: nothing runs unless SQD_METRICS_CH is set. When enabled, a
// single background goroutine owns its own ch.Client (a ch.Client is not safe
// for concurrent Do, and the ingestion hot path must never block on a metrics
// insert) and, on a ticker, snapshots runtime.MemStats + process CPU and writes
// one row per chain. The ingestion loop only calls Observe, which is a cheap
// non-blocking snapshot update.
package monitoring

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/internal/envconfig"
)

const (
	metricsDatabase = "monitoring"
	metricsTable    = "indexer_metrics"
	defaultInterval = 5 * time.Second
)

// Config holds the ClickHouse connection parameters for the metrics writer.
// They mirror the ingestion connection so the writer reaches the same server.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
}

// snapshot is the latest set of per-chain counters handed in by Observe.
type snapshot struct {
	blocksTotal uint64
	eventsTotal uint64
	head        uint64
	checkpoint  uint64
}

// Recorder owns the metrics connection and the writer goroutine.
type Recorder struct {
	conn   *ch.Client
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	snaps map[uint64]snapshot // keyed by chain ID

	// Rate-derivation state. Only the writer goroutine touches these, so no lock.
	prevBlocks map[uint64]uint64
	prevEvents map[uint64]uint64
	prevMallocs uint64
	prevFrees   uint64
	prevCPUNanos int64
	prevTime     time.Time
	primed       bool
}

var (
	globalMu sync.Mutex
	global   *Recorder
)

// Start connects to ClickHouse and starts the writer goroutine when
// SQD_METRICS_CH is set; otherwise it is a no-op and Observe stays inert.
// SQD_METRICS_CH_INTERVAL (e.g. "2s") overrides the 5s sampling cadence.
func Start(ctx context.Context, cfg Config) {
	if !envconfig.MetricsCHEnabled() {
		return
	}
	interval := envconfig.MetricsCHFlushInterval()

	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Database: "default",
		User:     cfg.User,
		Password: cfg.Password,
	})
	if err != nil {
		log.Printf("monitoring: disabled (connect clickhouse: %v)", err)
		return
	}
	if err := ensureSchema(ctx, conn); err != nil {
		log.Printf("monitoring: disabled (ensure schema: %v)", err)
		conn.Close()
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	r := &Recorder{
		conn:       conn,
		cancel:     cancel,
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	globalMu.Lock()
	global = r
	globalMu.Unlock()

	go r.loop(runCtx, interval)
	log.Printf("monitoring: writing indexer metrics to %s.%s every %s (SQD_METRICS_CH)", metricsDatabase, metricsTable, interval)
}

// Observe records the latest cumulative counters for a chain. It is safe to call
// from the ingestion loop: it only updates an in-memory snapshot under a short
// lock and never touches the network. No-op when monitoring is disabled.
func Observe(chainID, blocksTotal, eventsTotal, head, checkpoint uint64) {
	globalMu.Lock()
	r := global
	globalMu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.snaps[chainID] = snapshot{blocksTotal: blocksTotal, eventsTotal: eventsTotal, head: head, checkpoint: checkpoint}
	r.mu.Unlock()
}

// Stop halts the writer goroutine and closes the connection. Safe to call when
// monitoring was never started.
func Stop() {
	globalMu.Lock()
	r := global
	global = nil
	globalMu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
	if r.conn != nil {
		r.conn.Close()
	}
}

func (r *Recorder) loop(ctx context.Context, interval time.Duration) {
	defer close(r.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.flush(ctx); err != nil && ctx.Err() == nil {
				log.Printf("monitoring: insert failed: %v", err)
			}
		}
	}
}

// flush snapshots process-global runtime/CPU stats once, then writes one row per
// observed chain combining those with the chain's cumulative counters.
func (r *Recorder) flush(ctx context.Context) error {
	r.mu.Lock()
	snaps := make(map[uint64]snapshot, len(r.snaps))
	for k, v := range r.snaps {
		snaps[k] = v
	}
	r.mu.Unlock()
	if len(snaps) == 0 {
		return nil
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	now := time.Now()

	cpuNanos := processCPUNanos()
	var elapsed float64
	if r.primed {
		elapsed = now.Sub(r.prevTime).Seconds()
	}

	// Per-process gauges shared across every chain row this tick.
	var cpuCores, mallocsPerSec, freesPerSec float64
	if r.primed && elapsed > 0 {
		cpuCores = float64(cpuNanos-r.prevCPUNanos) / 1e9 / elapsed
		mallocsPerSec = float64(ms.Mallocs-r.prevMallocs) / elapsed
		freesPerSec = float64(ms.Frees-r.prevFrees) / elapsed
	}
	lastPause := ms.PauseNs[(ms.NumGC+255)%256]

	var (
		colTS         proto.ColDateTime64
		colChain      proto.ColUInt64
		colBlocks     proto.ColUInt64
		colEvents     proto.ColUInt64
		colBlocksRate proto.ColFloat64
		colEventsRate proto.ColFloat64
		colHead       proto.ColUInt64
		colCheckpoint proto.ColUInt64
		colLag        proto.ColUInt64
		colHeapAlloc  proto.ColUInt64
		colHeapSys    proto.ColUInt64
		colHeapObjs   proto.ColUInt64
		colSys        proto.ColUInt64
		colStackSys   proto.ColUInt64
		colMallocRate proto.ColFloat64
		colFreeRate   proto.ColFloat64
		colNumGC      proto.ColUInt64
		colPause      proto.ColUInt64
		colGCFraction proto.ColFloat64
		colNextGC     proto.ColUInt64
		colGoroutine  proto.ColUInt32
		colCPUCores   proto.ColFloat64
	)
	colTS = *colTS.WithPrecision(proto.PrecisionMilli)
	goroutines := uint32(runtime.NumGoroutine())

	for chainID, s := range snaps {
		var blocksRate, eventsRate float64
		if r.primed && elapsed > 0 {
			if prev, ok := r.prevBlocks[chainID]; ok {
				blocksRate = float64(s.blocksTotal-prev) / elapsed
			}
			if prev, ok := r.prevEvents[chainID]; ok {
				eventsRate = float64(s.eventsTotal-prev) / elapsed
			}
		}
		var lag uint64
		if s.head > s.checkpoint {
			lag = s.head - s.checkpoint
		}

		colTS.Append(now)
		colChain.Append(chainID)
		colBlocks.Append(s.blocksTotal)
		colEvents.Append(s.eventsTotal)
		colBlocksRate.Append(blocksRate)
		colEventsRate.Append(eventsRate)
		colHead.Append(s.head)
		colCheckpoint.Append(s.checkpoint)
		colLag.Append(lag)
		colHeapAlloc.Append(ms.HeapAlloc)
		colHeapSys.Append(ms.HeapSys)
		colHeapObjs.Append(ms.HeapObjects)
		colSys.Append(ms.Sys)
		colStackSys.Append(ms.StackSys)
		colMallocRate.Append(mallocsPerSec)
		colFreeRate.Append(freesPerSec)
		colNumGC.Append(uint64(ms.NumGC))
		colPause.Append(lastPause)
		colGCFraction.Append(ms.GCCPUFraction)
		colNextGC.Append(ms.NextGC)
		colGoroutine.Append(goroutines)
		colCPUCores.Append(cpuCores)

		r.prevBlocks[chainID] = s.blocksTotal
		r.prevEvents[chainID] = s.eventsTotal
	}

	err := r.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf("INSERT INTO %s.%s (ts, chain_id, blocks_total, events_total, blocks_per_sec, events_per_sec, head, checkpoint, checkpoint_lag, heap_alloc_bytes, heap_sys_bytes, heap_objects, sys_bytes, stack_sys_bytes, mallocs_per_sec, frees_per_sec, num_gc, gc_pause_last_ns, gc_cpu_fraction, next_gc_bytes, num_goroutine, cpu_cores_used) VALUES", metricsDatabase, metricsTable),
		Input: []proto.InputColumn{
			{Name: "ts", Data: &colTS},
			{Name: "chain_id", Data: &colChain},
			{Name: "blocks_total", Data: &colBlocks},
			{Name: "events_total", Data: &colEvents},
			{Name: "blocks_per_sec", Data: &colBlocksRate},
			{Name: "events_per_sec", Data: &colEventsRate},
			{Name: "head", Data: &colHead},
			{Name: "checkpoint", Data: &colCheckpoint},
			{Name: "checkpoint_lag", Data: &colLag},
			{Name: "heap_alloc_bytes", Data: &colHeapAlloc},
			{Name: "heap_sys_bytes", Data: &colHeapSys},
			{Name: "heap_objects", Data: &colHeapObjs},
			{Name: "sys_bytes", Data: &colSys},
			{Name: "stack_sys_bytes", Data: &colStackSys},
			{Name: "mallocs_per_sec", Data: &colMallocRate},
			{Name: "frees_per_sec", Data: &colFreeRate},
			{Name: "num_gc", Data: &colNumGC},
			{Name: "gc_pause_last_ns", Data: &colPause},
			{Name: "gc_cpu_fraction", Data: &colGCFraction},
			{Name: "next_gc_bytes", Data: &colNextGC},
			{Name: "num_goroutine", Data: &colGoroutine},
			{Name: "cpu_cores_used", Data: &colCPUCores},
		},
	})
	if err != nil {
		return err
	}

	r.prevMallocs = ms.Mallocs
	r.prevFrees = ms.Frees
	r.prevCPUNanos = cpuNanos
	r.prevTime = now
	r.primed = true
	return nil
}

// processCPUNanos returns cumulative user+system CPU time for this process in
// nanoseconds via getrusage. Returns 0 on platforms where it is unavailable, in
// which case cpu_cores_used stays 0.
func processCPUNanos() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return ru.Utime.Nano() + ru.Stime.Nano()
}

func ensureSchema(ctx context.Context, conn *ch.Client) error {
	if err := conn.Do(ctx, ch.Query{Body: "CREATE DATABASE IF NOT EXISTS " + metricsDatabase}); err != nil {
		return err
	}
	ddl := `CREATE TABLE IF NOT EXISTS ` + metricsDatabase + `.` + metricsTable + ` (
	ts                DateTime64(3),
	chain_id          UInt64,
	blocks_total      UInt64,
	events_total      UInt64,
	blocks_per_sec    Float64,
	events_per_sec    Float64,
	head              UInt64,
	checkpoint        UInt64,
	checkpoint_lag    UInt64,
	heap_alloc_bytes  UInt64,
	heap_sys_bytes    UInt64,
	heap_objects      UInt64,
	sys_bytes         UInt64,
	stack_sys_bytes   UInt64,
	mallocs_per_sec   Float64,
	frees_per_sec     Float64,
	num_gc            UInt64,
	gc_pause_last_ns  UInt64,
	gc_cpu_fraction   Float64,
	next_gc_bytes     UInt64,
	num_goroutine     UInt32,
	cpu_cores_used    Float64
) ENGINE = MergeTree
ORDER BY (chain_id, ts)
TTL toDateTime(ts) + INTERVAL ` + ttlDays() + ` DAY`
	return conn.Do(ctx, ch.Query{Body: ddl})
}

// ttlDays returns the retention window for the metrics table (default 7 days),
// overridable via SQD_METRICS_CH_TTL_DAYS.
func ttlDays() string {
	ttl := envconfig.MetricsCHTTL() / (24 * time.Hour)
	if ttl <= 0 {
		return "7"
	}
	return strconv.Itoa(int(ttl))
}
