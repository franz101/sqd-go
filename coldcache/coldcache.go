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
	"sync/atomic"

	"github.com/cockroachdb/pebble"
)

// Default off-heap budgets. Both are hard caps; steady-state RSS ≈ Cache +
// MemTableSize + small bookkeeping (validated by the ./disk spike at ~88 MiB).
// The block-cache cap defaults to a fraction of system RAM (see
// defaultCacheBytes) and can be overridden with SQD_COLDCACHE_MB.
const (
	MinDefaultCacheBytes int64  = 256 << 20
	MaxDefaultCacheBytes int64  = 8 << 30
	DefaultMemTableSize  uint64 = 16 << 20
)

// defaultCacheBytes picks the block-cache cap when neither the caller nor
// SQD_COLDCACHE_MB sets one: 1/8 of total RAM, clamped to
// [MinDefaultCacheBytes, MaxDefaultCacheBytes]. If total RAM is unknown
// (non-Linux), it stays at the conservative minimum. Long live runs are
// cold-tier read-bound, so a small fixed cap left bigger boxes paying pread
// I/O for no reason.
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

// totalRAMBytes returns total system memory, or 0 if it can't be determined
// (e.g. non-Linux, where /proc/meminfo is absent).
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

	// neg is an optional in-memory negative-lookup Bloom filter (the V3 cold-tier
	// optimization). When set, a Get whose key is provably absent skips Pebble
	// entirely. nil => V2 behaviour (every miss probes Pebble). See filter.go.
	neg        *negFilter
	filterHits atomic.Uint64 // Pebble Gets skipped because the filter proved the key absent
}

// EnableNegativeFilter attaches a fixed-size in-memory negative-lookup Bloom
// filter sized to at least bitBudget bits (rounded up to a power of two). Call
// once, right after Open and before any Put/Get. This is what distinguishes the
// V3 cold tier from V2.
func (s *Store) EnableNegativeFilter(bitBudget uint64) {
	if s == nil {
		return
	}
	s.neg = newNegFilter(bitBudget)
}

func defaultNegativeFilterBits() uint64 {
	if v := os.Getenv("SQD_COLDFILTER_BITS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	if v := os.Getenv("SQD_BLOOM_KEYS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n * 10
		}
	}
	return 1 << 31
}

// FilterSkips returns how many Pebble Gets the negative filter has avoided.
func (s *Store) FilterSkips() uint64 {
	if s == nil {
		return 0
	}
	return s.filterHits.Load()
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
	db, err := pebble.Open(dir, opts)
	if err != nil {
		cache.Unref()
		return nil, err
	}
	s := &Store{db: db, cache: cache, dir: dir}
	if bits := defaultNegativeFilterBits(); bits > 0 {
		s.EnableNegativeFilter(bits)
		log.Printf("cold tier: negative Bloom filter %d MiB (SQD_COLDFILTER_BITS=0 disables)", bits>>23)
	}
	return s, nil
}

// Put stores value under key. Pebble copies both slices synchronously, so callers
// may pass transient (e.g. unsafe-aliased) slices. NoSync: the write is not
// fsync'd — durability comes from ClickHouse, not here.
func (s *Store) Put(key, value []byte) error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.neg != nil {
		s.neg.add(key)
	}
	return s.db.Set(key, value, pebble.NoSync)
}

const writeBatchFlushCount = 16384

type WriteBatch struct {
	db    *pebble.DB
	b     *pebble.Batch
	neg   *negFilter
	count int
}

func (s *Store) NewWriteBatch() *WriteBatch {
	if s == nil || s.db == nil {
		return &WriteBatch{}
	}
	return &WriteBatch{db: s.db, b: s.db.NewBatch(), neg: s.neg}
}

func (w *WriteBatch) Put(key, value []byte) error {
	if w == nil || w.b == nil {
		return nil
	}
	if err := w.b.Set(key, value, nil); err != nil {
		return err
	}
	if w.neg != nil {
		w.neg.add(key)
	}
	w.count++
	if w.count >= writeBatchFlushCount {
		return w.flush()
	}
	return nil
}

func (w *WriteBatch) flush() error {
	if w == nil || w.b == nil || w.count == 0 {
		return nil
	}
	if err := w.db.Apply(w.b, pebble.NoSync); err != nil {
		return err
	}
	w.b.Reset()
	w.count = 0
	return nil
}

func (w *WriteBatch) Close() error {
	if w == nil || w.b == nil {
		return nil
	}
	err := w.flush()
	_ = w.b.Close()
	w.b = nil
	return err
}

func (s *Store) MightContain(key []byte) bool {
	if s == nil || s.neg == nil {
		return true
	}
	if !s.neg.mayContain(key) {
		s.filterHits.Add(1)
		return false
	}
	return true
}

func (s *Store) GetInto(dst []byte, key []byte) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	if !s.MightContain(key) {
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
	// Negative filter (V3): if the key was never written to the cold store, skip
	// the Pebble round-trip. No false negatives, so this is always correct.
	if !s.MightContain(key) {
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
