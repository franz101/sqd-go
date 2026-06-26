package coldcache

import (
	"log"
	"os"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/sstable"
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
)

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
		opts.Levels = make([]pebble.LevelOptions, 7)
		for i := range opts.Levels {
			opts.Levels[i].Compression = sstable.NoCompression
		}
		log.Printf("cold tier: NO compression (fast writes)")

	case ConfigFastReads:
		// Optimized for read-heavy workloads
		opts.Levels = make([]pebble.LevelOptions, 7)
		for i := range opts.Levels {
			opts.Levels[i].Compression = sstable.NoCompression
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
		opts.MaxConcurrentCompactions = func() int { return 2 }
		log.Printf("cold tier: aggressive compaction")

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
