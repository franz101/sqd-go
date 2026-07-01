package coldcache

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
)

// Benchmarks the "limit concurrent compactions" lever in isolation against the
// production cold-tier baseline (coldcache.go Open, no optimization profile).
//
// Pebble v2's EnsureDefaults already sets CompactionConcurrencyRange to (1, 1)
// when unset (see pebble/v2 options.go), so the baseline is *already* running
// with a single background compaction goroutine — there is no
// MaxConcurrentCompactions field on this API (that name is from Pebble v1;
// this module is on v2.1.6). "current_default" and "explicit_cap_1" below
// exist to confirm that empirically rather than by reading the source.
type compactionProfile struct {
	name  string
	apply func(*pebble.Options)
}

var compactionProfiles = []compactionProfile{
	{name: "current_default", apply: func(o *pebble.Options) {}},
	{name: "explicit_cap_1", apply: func(o *pebble.Options) {
		o.CompactionConcurrencyRange = func() (int, int) { return 1, 1 }
	}},
	{name: "concurrency_2", apply: func(o *pebble.Options) {
		o.CompactionConcurrencyRange = func() (int, int) { return 1, 2 }
	}},
	{name: "concurrency_4", apply: func(o *pebble.Options) {
		o.CompactionConcurrencyRange = func() (int, int) { return 1, 4 }
	}},
	// Defers L0 compaction instead of capping concurrency (option 3 from the
	// suggestion). Pebble's own default is already L0CompactionThreshold=4 /
	// L0StopWritesThreshold=12, so this only changes anything because it goes
	// *above* those defaults.
	{name: "lazy_l0", apply: func(o *pebble.Options) {
		o.L0CompactionThreshold = 8
		o.L0StopWritesThreshold = 24
	}},

	// --- round 2: bloom / bigger memtable / aggressive L0 pacing ---
	//
	// "pebble.Options.FilterPolicy" doesn't exist — FilterPolicy lives on
	// LevelOptions (per-level, see options.go LevelOptions), which is what
	// withBloom (coldcache_optimized.go) already does correctly and what this
	// profile reuses verbatim, on top of the production baseline (Snappy).
	{name: "bloom", apply: func(o *pebble.Options) {
		o.Levels = withBloom(sstable.SnappyCompression)
	}},

	// MemTableSize's pebble library default is 4MB, not 64MB, and this repo's
	// own DefaultMemTableSize is already 16MB — 4x the library default. This
	// profile tests going further, to 128MB, holding MemTableStopWritesThreshold
	// at the production value (4) to isolate the size variable. Note this
	// works against coldcache.go's documented bounded-memory design (RSS is
	// hard-capped by construction) and against this project's history of
	// memory-budget-driven OOMs — worth weighing against any throughput win.
	{name: "bigmem_128", apply: func(o *pebble.Options) {
		o.MemTableSize = 128 << 20
	}},

	// Opposite direction from lazy_l0: compact L0 *more* eagerly (lower
	// thresholds than Pebble's defaults of 4 / 500) to keep sublevels — and
	// therefore read amplification — low, at whatever extra CPU cost that
	// takes.
	{name: "aggressive_l0", apply: func(o *pebble.Options) {
		o.L0CompactionThreshold = 2
		o.L0CompactionFileThreshold = 100
	}},

	// bloom and bigmem_128 independently won on every axis tested (CPU,
	// sublevels, hit/miss latency) and act on orthogonal mechanisms (filter
	// vs flush cadence) — combining them to check for negative interaction.
	{name: "bloom_bigmem_128", apply: func(o *pebble.Options) {
		o.Levels = withBloom(sstable.SnappyCompression)
		o.MemTableSize = 128 << 20
	}},
}

// BenchmarkCompactionConcurrency drives a sustained, single-goroutine write
// burst (matching the cold tier's single-writer invariant, see coldcache.go)
// through each profile and reports how much of the process's CPU time ended
// up inside Pebble's own cumulative compaction-duration metric.
//
// Isolated from any live data: each sub-benchmark opens its own Pebble store
// under b.TempDir() (tmpfs, auto-cleaned), never touching a real cold-tier
// directory.
func BenchmarkCompactionConcurrency(b *testing.B) {
	const (
		entries  = 5_000_000 // ~1GB raw (keySize+valueSize=204B), several memtable flushes
		cacheMiB = 1024      // matches SQD_COLDCACHE_MB used by the live polymarket run
	)

	for _, prof := range compactionProfiles {
		b.Run(prof.name, func(b *testing.B) {
			dir := b.TempDir()
			cache := pebble.NewCache(int64(cacheMiB) << 20)
			defer cache.Unref()

			opts := &pebble.Options{
				Cache:                       cache,
				MemTableSize:                DefaultMemTableSize,
				MemTableStopWritesThreshold: 4,
				DisableWAL:                  true,
			}
			prof.apply(opts)

			db, err := pebble.Open(dir, opts)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer db.Close()

			r := rand.New(rand.NewSource(42))

			var rusageStart, rusageEnd syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusageStart); err != nil {
				b.Fatalf("getrusage: %v", err)
			}
			wallStart := time.Now()

			b.ReportAllocs()

			for i := 0; i < entries; i++ {
				if err := db.Set(randKey(r), randValue(r), pebble.NoSync); err != nil {
					b.Fatalf("set: %v", err)
				}
			}
			if err := db.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			waitForCompactionsIdle(db, 60*time.Second)

			wall := time.Since(wallStart)
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusageEnd); err != nil {
				b.Fatalf("getrusage: %v", err)
			}
			cpu := rusageDelta(rusageStart, rusageEnd)

			m := db.Metrics()

			b.ReportMetric(wall.Seconds(), "wall_s")
			b.ReportMetric(cpu.Seconds(), "process_cpu_s")
			b.ReportMetric(m.Compact.Duration.Seconds(), "compact_s")
			b.ReportMetric(float64(m.Compact.Count), "compactions")
			b.ReportMetric(float64(m.Flush.Count), "flushes")
			b.ReportMetric(float64(entries)/wall.Seconds(), "entries/sec")
			var pctCPU float64
			if cpu > 0 {
				pctCPU = 100 * m.Compact.Duration.Seconds() / cpu.Seconds()
				b.ReportMetric(pctCPU, "compact_%cpu")
			}

			b.Logf("%-18s wall=%-9s cpu=%-9s compact=%-9s (%d compactions, %d flushes, debt=%dMiB) -> %.1f%% of process CPU",
				prof.name, wall.Round(time.Millisecond), cpu.Round(time.Millisecond),
				m.Compact.Duration.Round(time.Millisecond), m.Compact.Count, m.Flush.Count,
				m.Compact.EstimatedDebt>>20, pctCPU)
		})
	}
}

// waitForCompactionsIdle polls Metrics until no compaction has been in
// progress for a few consecutive checks (or timeout), so the comparison
// reflects compaction work triggered by the write burst, not just whatever
// happened to finish inside the write loop itself.
func waitForCompactionsIdle(db *pebble.DB, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	idleStreak := 0
	for time.Now().Before(deadline) {
		if db.Metrics().Compact.NumInProgress == 0 {
			idleStreak++
			if idleStreak >= 3 {
				return
			}
		} else {
			idleStreak = 0
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func rusageDelta(start, end syscall.Rusage) time.Duration {
	toDur := func(tv syscall.Timeval) time.Duration {
		return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
	}
	return (toDur(end.Utime) - toDur(start.Utime)) + (toDur(end.Stime) - toDur(start.Stime))
}

// BenchmarkWALMode quantifies what coldcache.go's DisableWAL:true is actually
// buying production (see the package doc: "ephemeral — CH is the durable
// truth, a crash just discards it").
//
//   - wal_off: today's production setting. No WAL at all; never durable.
//   - wal_on_nosync_batched / wal_on_sync_batched: WAL on, writes batched
//     16384/op (matching coldcache.go's writeBatchFlushCount) — isolates the
//     cost of having a WAL, and of fsync, when amortized over a batch.
//   - wal_on_sync_unbatched: WAL on, Sync, ONE key per Set call — what you'd
//     actually get if DisableWAL were flipped to false without also
//     restructuring the write path, since the real hot path (hot_state.go)
//     calls cold.Put once per event with no batching at all. Uses far fewer
//     entries: this is fsync-per-call, not CPU-bound, and would otherwise
//     dominate the run.
func BenchmarkWALMode(b *testing.B) {
	const cacheMiB = 1024

	profiles := []struct {
		name       string
		disableWAL bool
		sync       *pebble.WriteOptions
		entries    int
		batchSize  int // 1 = unbatched (one Set + Apply per entry)
	}{
		{name: "wal_off", disableWAL: true, sync: pebble.NoSync, entries: 500_000, batchSize: writeBatchFlushCount},
		{name: "wal_on_nosync_batched", disableWAL: false, sync: pebble.NoSync, entries: 500_000, batchSize: writeBatchFlushCount},
		{name: "wal_on_sync_batched", disableWAL: false, sync: pebble.Sync, entries: 500_000, batchSize: writeBatchFlushCount},
		{name: "wal_on_sync_unbatched", disableWAL: false, sync: pebble.Sync, entries: 5_000, batchSize: 1},
	}

	for _, prof := range profiles {
		b.Run(prof.name, func(b *testing.B) {
			// b.TempDir() resolves under os.TempDir() (/tmp), which is tmpfs on
			// this box — fsync there is nearly free since there's no real block
			// device. A WAL-sync benchmark on tmpfs would understate the actual
			// fsync cost, so this uses a real disk-backed dir instead (same
			// /dev/md2 ext4 filesystem the live cold tier runs on, just a
			// different, scratch subdirectory).
			dir := realDiskTempDir(b, prof.name)
			cache := pebble.NewCache(int64(cacheMiB) << 20)
			defer cache.Unref()

			opts := &pebble.Options{
				Cache:                       cache,
				MemTableSize:                DefaultMemTableSize,
				MemTableStopWritesThreshold: 4,
				DisableWAL:                  prof.disableWAL,
			}

			db, err := pebble.Open(dir, opts)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer db.Close()

			r := rand.New(rand.NewSource(42))

			var rusageStart, rusageEnd syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusageStart); err != nil {
				b.Fatalf("getrusage: %v", err)
			}
			wallStart := time.Now()

			b.ReportAllocs()

			wb := db.NewBatch()
			for i := 0; i < prof.entries; i++ {
				if err := wb.Set(randKey(r), randValue(r), nil); err != nil {
					b.Fatalf("batch set: %v", err)
				}
				if (i+1)%prof.batchSize == 0 {
					if err := db.Apply(wb, prof.sync); err != nil {
						b.Fatalf("apply: %v", err)
					}
					wb = db.NewBatch()
				}
			}
			if err := db.Apply(wb, prof.sync); err != nil {
				b.Fatalf("final apply: %v", err)
			}

			wall := time.Since(wallStart)
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusageEnd); err != nil {
				b.Fatalf("getrusage: %v", err)
			}
			cpu := rusageDelta(rusageStart, rusageEnd)

			b.ReportMetric(wall.Seconds(), "wall_s")
			b.ReportMetric(cpu.Seconds(), "process_cpu_s")
			b.ReportMetric(float64(prof.entries)/wall.Seconds(), "entries/sec")
			b.ReportMetric(wall.Seconds()/float64(prof.entries)*1e6, "us/entry")

			b.Logf("%-22s wall=%-9s cpu=%-9s %.0f entries/sec (%.2f us/entry)",
				prof.name, wall.Round(time.Millisecond), cpu.Round(time.Millisecond),
				float64(prof.entries)/wall.Seconds(), wall.Seconds()/float64(prof.entries)*1e6)
		})
	}
}

// BenchmarkCompactionConcurrencyReadAmp measures the other side of the
// lazy_l0 tradeoff: deferring L0 compaction caps CPU (see
// BenchmarkCompactionConcurrency) but leaves more overlapping L0 files for a
// point Get to probe, which is the cold tier's actual job (serving
// hot-cache-miss point lookups, see the coldcache.go package doc).
//
// Lookups are sampled periodically *while writes are still in flight*, not
// after draining to idle, since a live backfill never gets to drain — L0 is
// continuously replenished by flushes while compaction works through the
// backlog in the background. Each sample also records L0Metrics.Sublevels,
// which Pebble documents directly as L0's read-amplification factor.
func BenchmarkCompactionConcurrencyReadAmp(b *testing.B) {
	const (
		entries        = 3_000_000
		cacheMiB       = 1024
		sampleInterval = 100_000
		sampleSize     = 200
	)

	for _, prof := range compactionProfiles {
		b.Run(prof.name, func(b *testing.B) {
			dir := b.TempDir()
			cache := pebble.NewCache(int64(cacheMiB) << 20)
			defer cache.Unref()

			opts := &pebble.Options{
				Cache:                       cache,
				MemTableSize:                DefaultMemTableSize,
				MemTableStopWritesThreshold: 4,
				DisableWAL:                  true,
			}
			prof.apply(opts)

			db, err := pebble.Open(dir, opts)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer db.Close()

			r := rand.New(rand.NewSource(42))
			sampleR := rand.New(rand.NewSource(99))

			written := make([][]byte, 0, entries/50+1) // every 50th key, as a hit-lookup pool
			var hitLat, missLat []time.Duration
			var l0Sublevels, l0Tables []int32

			sampleNow := func() {
				m := db.Metrics()
				l0Sublevels = append(l0Sublevels, m.Levels[0].Sublevels)
				l0Tables = append(l0Tables, int32(m.Levels[0].TablesCount))

				for i := 0; i < sampleSize && len(written) > 0; i++ {
					k := written[sampleR.Intn(len(written))]
					start := time.Now()
					v, closer, err := db.Get(k)
					d := time.Since(start)
					if err != nil {
						b.Fatalf("hit get: %v", err)
					}
					_ = v
					closer.Close()
					hitLat = append(hitLat, d)
				}
				for i := 0; i < sampleSize; i++ {
					k := randKey(sampleR)
					start := time.Now()
					_, closer, err := db.Get(k)
					d := time.Since(start)
					if err == nil {
						closer.Close()
						continue // astronomically unlikely real collision with a written key; skip
					}
					if err != pebble.ErrNotFound {
						b.Fatalf("miss get: %v", err)
					}
					missLat = append(missLat, d)
				}
			}

			for i := 0; i < entries; i++ {
				key := randKey(r)
				if i%50 == 0 {
					written = append(written, key)
				}
				if err := db.Set(key, randValue(r), pebble.NoSync); err != nil {
					b.Fatalf("set: %v", err)
				}
				if i > 0 && i%sampleInterval == 0 {
					sampleNow()
				}
			}
			sampleNow()

			b.ReportMetric(float64(avgDur(hitLat).Nanoseconds()), "hit_avg_ns")
			b.ReportMetric(float64(pctDur(hitLat, 0.99).Nanoseconds()), "hit_p99_ns")
			b.ReportMetric(float64(avgDur(missLat).Nanoseconds()), "miss_avg_ns")
			b.ReportMetric(float64(pctDur(missLat, 0.99).Nanoseconds()), "miss_p99_ns")
			b.ReportMetric(avgI32(l0Sublevels), "avg_L0_sublevels")
			b.ReportMetric(float64(maxI32(l0Sublevels)), "max_L0_sublevels")
			b.ReportMetric(avgI32(l0Tables), "avg_L0_tables")

			b.Logf("%-18s hit avg=%-10s p99=%-10s | miss avg=%-10s p99=%-10s | L0 sublevels avg=%.1f max=%d, L0 tables avg=%.1f (n_hit=%d n_miss=%d)",
				prof.name,
				avgDur(hitLat), pctDur(hitLat, 0.99),
				avgDur(missLat), pctDur(missLat, 0.99),
				avgI32(l0Sublevels), maxI32(l0Sublevels), avgI32(l0Tables),
				len(hitLat), len(missLat))
		})
	}
}

func avgDur(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

func pctDur(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(a, c int) bool { return sorted[a] < sorted[c] })
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

func avgI32(xs []int32) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum int64
	for _, x := range xs {
		sum += int64(x)
	}
	return float64(sum) / float64(len(xs))
}

func maxI32(xs []int32) int32 {
	var m int32
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

// realDiskTempDir returns a fresh scratch dir on the repo's real (ext4)
// filesystem, gitignored and cleaned up on test exit — unlike b.TempDir(),
// which resolves under /tmp (tmpfs) and makes fsync nearly free.
func realDiskTempDir(b *testing.B, name string) string {
	b.Helper()
	dir := fmt.Sprintf("../tmp/bench-wal-%s-%d", name, os.Getpid())
	if err := os.RemoveAll(dir); err != nil {
		b.Fatalf("clean scratch dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatalf("create scratch dir: %v", err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
