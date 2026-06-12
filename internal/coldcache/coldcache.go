// Package coldcache is a Pebble-backed cold tier sitting under the in-memory hot
// clock caches and above durable ClickHouse.
//
// Why it exists: on a hot-cache miss the generated state Get currently fires a
// synchronous ClickHouse point-SELECT (~1.9 ms round-trip). During a from-genesis
// backfill every such lookup is a dry run that returns nothing, capping the
// indexer at ~500 blk/s and hammering ClickHouse with one SELECT per evicted key.
// The cold tier serves evicted entries from local disk (~8 µs, ~220x faster) and,
// combined with the authoritative flag, lets a provably-new key skip ClickHouse
// entirely.
//
// Bounded memory (per pebble/docs/memory.md): Pebble's Block Cache and MemTables
// live OFF the Go heap and are hard-capped here, so RSS is bounded by construction
// and the Go heap stays tiny (it only ever sees the []byte we pass in/out).
//
// Ephemeral: the cold tier is NOT durable — ClickHouse is the source of truth. We
// run with DisableWAL and NoSync writes (a crash just discards it; resume rebuilds
// from ClickHouse). The directory is wiped on Open.
//
// Single-writer: the processor is one goroutine (same invariant the A3 flat index
// relies on), so no external locking is required.
package coldcache

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
)

// Default off-heap budgets. Both are hard caps; steady-state RSS ≈ Cache +
// MemTableSize + small bookkeeping (validated by the ./disk spike at ~88 MiB).
// SQD_COLDCACHE_MB overrides the block-cache cap (still a hard cap). The
// cache default scales with the machine: long live runs are cold-tier
// read-bound, and a larger block cache keeps the hot working set of state
// lookups off disk.
const (
	MinDefaultCacheBytes int64  = 256 << 20
	MaxDefaultCacheBytes int64  = 8 << 30
	DefaultMemTableSize  uint64 = 16 << 20
)

// defaultCacheBytes picks the block-cache cap when neither the caller nor
// SQD_COLDCACHE_MB sets one: 1/8 of total RAM, clamped to
// [MinDefaultCacheBytes, MaxDefaultCacheBytes]. If total RAM is unknown
// (non-Linux), it stays at the conservative minimum.
func defaultCacheBytes() int64 {
	total := totalRAMBytes()
	if total <= 0 {
		return MinDefaultCacheBytes
	}
	c := total / 8
	if c < MinDefaultCacheBytes {
		return MinDefaultCacheBytes
	}
	if c > MaxDefaultCacheBytes {
		return MaxDefaultCacheBytes
	}
	return c
}

// totalRAMBytes returns total system memory, or 0 if it can't be determined.
func totalRAMBytes() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb << 10
	}
	return 0
}

// Store is a single-writer raw byte-slice KV backed by Pebble.
type Store struct {
	db    *pebble.DB
	cache *pebble.Cache
	dir   string
}

// Open creates a fresh (wiped) Pebble store at dir with capped off-heap memory.
// cacheBytes/memTableBytes <= 0 fall back to the defaults.
func Open(dir string, cacheBytes int64, memTableBytes uint64) (*Store, error) {
	if cacheBytes <= 0 {
		if mb, err := strconv.ParseInt(os.Getenv("SQD_COLDCACHE_MB"), 10, 64); err == nil && mb > 0 {
			cacheBytes = mb << 20
		} else {
			cacheBytes = defaultCacheBytes()
		}
		log.Printf("cold tier: block cache %d MiB (override with SQD_COLDCACHE_MB)", cacheBytes>>20)
	}
	if memTableBytes == 0 {
		memTableBytes = DefaultMemTableSize
	}
	// Ephemeral: start clean so a stale dir can never feed wrong values into a
	// new run (it must stay consistent with ClickHouse, which we don't re-validate
	// here — wiping is the simple, safe choice).
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
		DisableWAL:                  true, // ephemeral: CH is the durable truth
	}
	// Per-sstable bloom filters: a Get for an absent key (the common case while
	// new entities stream in) is answered by an in-memory filter probe instead
	// of data-block reads across every level — point misses were ~16% of pipeline
	// CPU in raw pread syscalls without this. ~10 bits/key, counted against the
	// block cache budget, so memory stays capped.
	opts.EnsureDefaults()
	for i := range opts.Levels {
		opts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
	}
	db, err := pebble.Open(dir, opts)
	if err != nil {
		cache.Unref()
		return nil, err
	}
	return &Store{db: db, cache: cache, dir: dir}, nil
}

// Put stores value under key. Pebble copies both slices synchronously, so callers
// may pass transient (e.g. unsafe-aliased) slices. NoSync: the write is not
// fsync'd — durability comes from ClickHouse, not here.
func (s *Store) Put(key, value []byte) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Set(key, value, pebble.NoSync)
}

// GetInto copies the value stored under key into dst (up to len(dst) bytes)
// and reports whether it was found. Unlike Get it allocates nothing: callers
// with a fixed-size destination (the pointer-free hot-state entities) pass
// their stack struct's bytes directly.
func (s *Store) GetInto(dst []byte, key []byte) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	v, closer, err := s.db.Get(key)
	if err == pebble.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	copy(dst, v)
	closer.Close()
	return true, nil
}

// Get returns a COPY of the value stored under key (so it stays valid after the
// internal pebble closer is released), and whether it was found.
func (s *Store) Get(key []byte) ([]byte, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, nil
	}
	v, closer, err := s.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := make([]byte, len(v))
	copy(out, v)
	closer.Close()
	return out, true, nil
}

// Delete removes key (used when a hot entry is hard-deleted).
func (s *Store) Delete(key []byte) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Delete(key, pebble.NoSync)
}

// Close releases the database and its off-heap cache, then removes the directory.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	if s.cache != nil {
		s.cache.Unref()
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	s.db = nil
	return err
}
