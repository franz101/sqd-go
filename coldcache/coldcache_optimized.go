package coldcache

import (
	"log"
	"os"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/franz101/sqd-go/internal/envconfig"
)

// Optimized configurations to benchmark
type OptimConfig string

const (
	ConfigBaseline      OptimConfig = "baseline"   // Current settings
	ConfigNoCompression OptimConfig = "nocomp"     // No compression (faster writes)
	ConfigFastReads     OptimConfig = "fastreads"  // Optimized for reads
	ConfigLargeMem      OptimConfig = "largemem"   // Larger memtables
	ConfigAggressive    OptimConfig = "aggressive" // Aggressive compaction

	// ConfigBloom keeps Snappy but adds a table-level Bloom filter on every
	// level. The baseline sets no FilterPolicy, so a point Get for an existing
	// key reads a data block in every level whose key range covers the key
	// (only one level actually holds it). The Bloom filter lets Pebble skip the
	// data-block read in the levels that don't, which is the dominant cost of a
	// cache-miss point lookup.
	ConfigBloom OptimConfig = "bloom" // Snappy + Bloom filter (read-optimized)
	// ConfigBloomNoComp pairs no compression with the Bloom filter: fewer bytes
	// to memcpy per block on a hit, plus the same miss-skipping benefit.
	ConfigBloomNoComp OptimConfig = "bloom_nocomp" // No compression + Bloom filter

	// ConfigSmallBlock shrinks the data block from the 4 KiB default to 2 KiB.
	// On a cache-miss point lookup the engine reads and decodes one whole data
	// block to return a single 152 B value, so a smaller block means less work
	// per miss (at the cost of a larger, but still cached, index). Bloom is on
	// too, since they target the same path.
	ConfigSmallBlock OptimConfig = "smallblock" // Snappy + 2KiB blocks + Bloom

	// ConfigReadOptimal targets the actual cache-miss-read bottleneck: a burst
	// of writes outruns compaction and leaves L0 with ~100 overlapping files,
	// and a point Get for an existing key must probe every overlapping L0 file.
	// Reads are ~4x faster once the LSM is compacted (few overlapping runs), so
	// this profile keeps L0 small during the burst with bigger memtables (fewer
	// flushes => fewer L0 files), an early L0 compaction trigger, and more
	// compaction concurrency to drain L0. Codec stays Snappy (fastest on this
	// data once the shape is good; see BenchmarkCodecCacheMiss).
	ConfigReadOptimal OptimConfig = "readoptimal"

	// ConfigMinLZ uses MinLZ block compression + the read-skipping Bloom filter.
	// MinLZ (Pebble v2, table format v6+) decompresses faster than Snappy at a
	// similar ratio, so it can win on the decode-bound cache-miss read path.
	ConfigMinLZ OptimConfig = "minlz"
)

// smallBlockBytes is the data block target for ConfigSmallBlock.
const smallBlockBytes = 2 << 10

// bloomBitsPerKey is the Bloom filter size. 10 bits/key gives ~1% false
// positive rate at ~1.25 bytes/key of (off-heap, cached) filter overhead.
const bloomBitsPerKey = 10

// compFn wraps a compression profile in the func() value the Pebble v2
// LevelOptions.Compression field expects.
func compFn(p *sstable.CompressionProfile) func() *sstable.CompressionProfile {
	return func() *sstable.CompressionProfile { return p }
}

// withBloom returns 7 LevelOptions using the given compression and a
// table-level Bloom filter. FilterType is left at its default; EnsureDefaults
// promotes it to TableFilter (preferred) once a FilterPolicy is set.
func withBloom(compression *sstable.CompressionProfile) [7]pebble.LevelOptions {
	var levels [7]pebble.LevelOptions
	for i := range levels {
		levels[i].Compression = compFn(compression)
		levels[i].FilterPolicy = bloom.FilterPolicy(bloomBitsPerKey)
	}
	return levels
}

// OpenOptimized opens a Pebble store with the specified optimization profile
func OpenOptimized(dir string, cacheBytes int64, memTableBytes uint64, config OptimConfig) (*Store, error) {
	if cacheBytes <= 0 {
		cacheBytes = envconfig.ColdCacheSize()
		if cacheBytes <= 0 {
			cacheBytes = defaultCacheBytes()
		}
	}
	if memTableBytes == 0 {
		memTableBytes = DefaultMemTableSize
	}

	// Wipe directory
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	cache := pebble.NewCache(cacheBytes)
	opts := &pebble.Options{
		Cache:                       cache,
		MemTableSize:                memTableBytes,
		MemTableStopWritesThreshold: 4,
		DisableWAL:                  true,
	}

	// Apply optimization profile
	switch config {
	case ConfigNoCompression:
		// No compression = faster writes, more disk usage
		for i := range opts.Levels {
			opts.Levels[i].Compression = compFn(sstable.NoCompression)
		}
		log.Printf("cold tier: NO compression (fast writes)")

	case ConfigFastReads:
		// Optimized for read-heavy workloads
		for i := range opts.Levels {
			opts.Levels[i].Compression = compFn(sstable.NoCompression)
		}
		log.Printf("cold tier: read-optimized (no compression)")

	case ConfigLargeMem:
		// Larger memtables = fewer flushes
		opts.MemTableSize = 64 << 20 // 64MB
		opts.MemTableStopWritesThreshold = 2
		log.Printf("cold tier: 64MB memtables, aggressive flush")

	case ConfigAggressive:
		// Aggressive compaction to keep L0 small
		opts.L0CompactionThreshold = 2
		opts.L0CompactionFileThreshold = 4
		opts.CompactionConcurrencyRange = func() (int, int) { return 1, 2 }
		log.Printf("cold tier: aggressive compaction")

	case ConfigBloom:
		// Snappy + table-level Bloom filter on all levels.
		opts.Levels = withBloom(sstable.SnappyCompression)
		log.Printf("cold tier: Snappy + Bloom filter (read-optimized)")

	case ConfigBloomNoComp:
		// No compression + table-level Bloom filter on all levels.
		opts.Levels = withBloom(sstable.NoCompression)
		log.Printf("cold tier: no compression + Bloom filter")

	case ConfigSmallBlock:
		// Snappy + 2KiB data blocks + Bloom filter.
		opts.Levels = withBloom(sstable.SnappyCompression)
		for i := range opts.Levels {
			opts.Levels[i].BlockSize = smallBlockBytes
		}
		log.Printf("cold tier: Snappy + 2KiB blocks + Bloom filter")

	case ConfigReadOptimal:
		// Keep L0 shallow so cache-miss point reads probe few overlapping runs.
		opts.MemTableSize = 64 << 20 // 4x fewer flushes => 4x fewer L0 files
		opts.MemTableStopWritesThreshold = 4
		opts.L0CompactionThreshold = 2     // start compacting L0 early
		opts.L0CompactionFileThreshold = 4 // and on file count
		opts.CompactionConcurrencyRange = func() (int, int) { return 1, 4 }
		log.Printf("cold tier: read-optimal (shallow L0: 64MB memtables + aggressive L0 compaction)")

	case ConfigMinLZ:
		// MinLZ block compression (faster decode than Snappy) + Bloom filter.
		// MinLZ requires the v6+ on-disk table format; the cold tier is ephemeral
		// (wiped on Open) so adopting the newest format is free.
		opts.FormatMajorVersion = pebble.FormatNewest
		opts.Levels = withBloom(sstable.MinLZCompression)
		log.Printf("cold tier: MinLZ + Bloom filter (fast-decode codec)")

	case ConfigBaseline:
		// Default settings (Snappy compression)
		log.Printf("cold tier: baseline (Snappy compression)")

	default:
		log.Printf("cold tier: unknown config, using baseline")
	}

	db, err := pebble.Open(dir, opts)
	if err != nil {
		cache.Unref()
		return nil, err
	}

	s := &Store{db: db, cache: cache, dir: dir}
	if bits := defaultNegativeFilterBits(); bits > 0 {
		s.EnableNegativeFilter(bits)
	}
	return s, nil
}
