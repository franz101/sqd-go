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
	"errors"
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
	db  *pebble.DB
	dir string
	// ownedCache is non-nil only for a single-owner Store (opened via Open); it is
	// Closed with the Store. Stores opened via OpenWithCache share a caller-owned
	// SharedCache and leave it alone on Close.
	ownedCache *SharedCache
}

// resolveCacheBytes picks the off-heap block-cache budget: explicit bytes if >0,
// else SQD_COLDCACHE_MB, else defaultCacheBytes() (RAM/8, clamped).
func resolveCacheBytes(cacheBytes int64) int64 {
	if cacheBytes > 0 {
		return cacheBytes
	}
	if mb, err := strconv.ParseInt(os.Getenv("SQD_COLDCACHE_MB"), 10, 64); err == nil && mb > 0 {
		return mb << 20
	}
	return defaultCacheBytes()
}

// SharedCache is a Pebble block cache shared by several Stores so total off-heap
// cache memory is bounded by ONE budget no matter how many cold tiers are open
// (the hot state has one Store per entity — UserPositions, Conditions, ...). The
// working sets of every entity compete in a single unified LRU instead of each
// reserving its own SQD_COLDCACHE_MB. The caller owns it and must Close it after
// every Store opened against it has been closed.
type SharedCache struct {
	cache *pebble.Cache
	bytes int64
}

// NewSharedCache allocates a shared block cache sized like Open's default
// resolution. Off-heap (per pebble/docs/memory.md), so the Go heap is unaffected.
func NewSharedCache(cacheBytes int64) *SharedCache {
	b := resolveCacheBytes(cacheBytes)
	log.Printf("cold tier: shared block cache %d MiB across all entities (override with SQD_COLDCACHE_MB)", b>>20)
	return &SharedCache{cache: pebble.NewCache(b), bytes: b}
}

// Bytes reports the cache budget (for logging / RSS accounting).
func (sc *SharedCache) Bytes() int64 {
	if sc == nil {
		return 0
	}
	return sc.bytes
}

// Close releases the caller's reference to the cache. Pebble frees the backing
// memory once every DB sharing it has also been closed (it is ref-counted).
func (sc *SharedCache) Close() {
	if sc == nil || sc.cache == nil {
		return
	}
	sc.cache.Unref()
	sc.cache = nil
}

// Open creates a fresh (wiped) single-owner Pebble store at dir with capped
// off-heap memory. cacheBytes/memTableBytes <= 0 fall back to the defaults. Use
// OpenWithCache when several stores should share one block-cache budget.
func Open(dir string, cacheBytes int64, memTableBytes uint64) (*Store, error) {
	sc := NewSharedCache(cacheBytes)
	s, err := openStore(dir, sc, memTableBytes)
	if err != nil {
		sc.Close()
		return nil, err
	}
	s.ownedCache = sc
	return s, nil
}

// OpenWithCache creates a fresh (wiped) Pebble store at dir backed by a shared,
// caller-owned block cache. The Store does NOT own the cache: Close leaves it
// intact for the other stores and the owner.
func OpenWithCache(dir string, sc *SharedCache, memTableBytes uint64) (*Store, error) {
	if sc == nil || sc.cache == nil {
		return nil, errClosedSharedCache
	}
	return openStore(dir, sc, memTableBytes)
}

var errClosedSharedCache = errors.New("coldcache: OpenWithCache given a nil/closed SharedCache")

func openStore(dir string, sc *SharedCache, memTableBytes uint64) (*Store, error) {
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
	opts := &pebble.Options{
		Cache:                       sc.cache,
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
		return nil, err
	}
	return &Store{db: db, dir: dir}, nil
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

// Close releases the database (and, for a single-owner store, its off-heap
// cache), then removes the directory. Stores sharing a caller-owned SharedCache
// leave the cache untouched — the owner Closes it once all stores are closed.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	if s.ownedCache != nil {
		s.ownedCache.Close()
		s.ownedCache = nil
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	s.db = nil
	return err
}
