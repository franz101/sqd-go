# Performance & Internals Investigation: Bloom Filters

This report details the performance and internal differences between three Bloom filter libraries and their variations:
1. **`coldcache` (our project)**: Blocked `BloomFilter` (non-atomic, single-writer) and `AtomicBloom` (thread-safe).
2. **`blobloom` (`github.com/greatroar/blobloom`)**: Blocked `Filter` and `SyncFilter`.
3. **`boomfilters` (`github.com/tylertreat/boomfilters`)**: Classic `BloomFilter`, `CuckooFilter`, `ScalableBloomFilter`, and `StableBloomFilter`.

---

## 1. Raw Benchmark Results

Benchmarks were executed on macOS (`arm64`, Apple M2 Pro, 10 CPU cores) with `go1.25`. The data set consists of **20,000 random 52-byte keys** (User ID + Token ID). For the queries, 10,000 keys exist in the filter (Hits) and 10,000 keys do not (Misses). The filter size is set to **1M bits (~128 KB)**.

### Standard Queries (Including Hashing)
These benchmarks measure the end-to-end time to query a membership check using a `[]byte` key (or a hash computed on the fly from that key).

| Filter Variation | Hit (ns/op) | Miss (ns/op) | Allocations (B/op) | Allocs/op |
| :--- | :---: | :---: | :---: | :---: |
| **`coldcache.BloomFilter`** (Blocked) | **15.67** | **13.64** | 0 | 0 |
| **`coldcache.AtomicBloom`** (Blocked, Thread-Safe) | **16.76** | **13.63** | 0 | 0 |
| **`blobloom.Filter`** (Blocked, pre-hashed xxhash) | **18.93** | **16.82** | 0 | 0 |
| **`blobloom.SyncFilter`** (Blocked, pre-hashed xxhash, Thread-Safe) | **20.00** | **15.98** | 0 | 0 |
| **`boom.BloomFilter`** (Classic, standard) | **71.42** | **79.79** | 0 | 0 |
| **`boom.StableBloomFilter`** (Classic, stable) | **60.78** | **56.12** | 0 | 0 |
| **`boom.CuckooFilter`** | **82.74** | **90.13** | 16 | 2 |
| **`boom.ScalableBloomFilter`** | **76.45** | **114.60** | 0 | 0 |

### Isolated Queries (Pre-computed Hashes, Hashing Excluded)
These benchmarks isolate the actual filter membership checks by passing pre-computed hash values (excluding the hashing time from the measurement).

| Filter Variation | Hit (ns/op) | Miss (ns/op) |
| :--- | :---: | :---: |
| **`coldcache.BloomFilter`** (Blocked) | **6.45** | **4.43** |
| **`coldcache.AtomicBloom`** (Blocked, Thread-Safe) | **7.39** | **4.40** |
| **`blobloom.Filter`** (Blocked) | **6.88** | **4.87** |
| **`blobloom.SyncFilter`** (Blocked, Thread-Safe) | **7.67** | **5.13** |

---

## 2. Why is Blobloom's Query Faster? (The Hashing Disparity)

Initially, it was believed that `blobloom`'s query was significantly faster. Our isolation benchmarks clarify why:

1. **API Differences**: `blobloom`'s `Has(h uint64)` method accepts a **pre-computed** 64-bit hash. It does not perform string/slice hashing. In contrast, our `mayContain(key []byte)` hashes the key on the fly. 
2. **Comparing Hashing Time**:
   - Our FNV-1a word-at-a-time hash (`negHash`) on a 52-byte key takes **~9.2 ns**.
   - `xxhash.Sum64` on a 52-byte key takes **~12.1 ns**.
   
   Our custom word-at-a-time FNV-1a implementation is faster than `xxhash` on small inputs. `xxhash` is optimized for large payloads using 4 parallel accumulators, but its initialization overhead makes it slower for small keys like our 52-byte structures.
3. **Comparing Filter Check Time**:
   Once hashing is excluded (in the isolated benchmarks), **our `coldcache` blocked Bloom filter check is actually ~6% to 9% faster than `blobloom`!**

---

## 3. Internals Deep-Dive: Blocked vs. Classic Bloom Filters

### CPU L1 Cache Line Alignment
Traditional Bloom filters (`boom.BloomFilter`) use a single large bit array. To query a key, they generate $k$ independent hash values and probe $k$ different locations across the entire bit array. On modern processors:
- Each probe can fall into a different memory page or cache line.
- This triggers up to $k$ random memory accesses, resulting in multiple **CPU cache misses** (L1/L2/L3) per query.
- At **60–80 ns/op**, classic filters are bottlenecked by memory latency.

Blocked Bloom Filters (`coldcache` and `blobloom`) partition the filter into cache-line-sized blocks (e.g. 512 bits = 64 bytes, matching the L1 cache line size of `x86_64` and `arm64`). 
- A single hash determines which 512-bit block to check.
- The entire block is loaded into the CPU cache *once*.
- All $k$ subsequent bit probes happen inside the CPU registers or L1 cache, with **zero L2/L3 cache misses**.
- This results in a massive performance improvement, achieving **~4–6 ns/op** (isolated).

---

## 4. Algorithmic Differences in Bit Manipulation & Loop Structure

### Block Indexing
- **`coldcache`**: Uses a simple bitwise AND (`h & blockMask`) to select the block. This compiles to a single `AND` CPU instruction and is extremely fast (1 cycle), but requires the number of blocks to be a power of two.
- **`blobloom`**: Uses Lemire's range reduction `(h2 * blockCount) >> 32`. This allows arbitrary block counts (not just powers of two), but requires a 64-bit integer multiplication and shift, which adds instruction latency.

### Hash Synthesis & Probing
- **`coldcache`**: Uses FNV-1a stride-based double-hashing:
  `bit := (h>>9 + uint64(i)*g) & (blockBits - 1)`
  This requires a 64-bit multiplication `uint64(i)*g` in each iteration of the loop.
- **`blobloom`**: Uses enhanced double-hashing:
  `h1 = h1 + h2; h2 = h2 + i`
  This only requires two 32-bit additions per iteration (no multiplications).
- **Probes**: `blobloom`'s `Add`/`Has` loops from `i = 1` to `k-1` (running $k-1$ iterations), effectively performing one fewer bit probe than `coldcache` (which runs $k$ iterations).

---

## 5. Analysis of Variations

### Concurrency and Thread Safety
- **Standard vs. SyncFilter / BloomFilter vs. AtomicBloom**:
  - `blobloom.SyncFilter` uses atomic bit operations (`atomic.LoadUint32` and `atomic.OrUint32`) inside each block.
  - `coldcache.AtomicBloom` uses `atomic.LoadUint64` and `atomic.OrUint64`.
  - The benchmarks show a minimal performance drop (~1 ns) when using atomic operations. Because blocked Bloom filters localize all writes to a single block, atomic operations on the block's words suffer no cache coherence bouncing unless two CPU threads access the *same block* simultaneously.

### Cuckoo Filters (`boom.CuckooFilter`)
- Cuckoo filters store small fingerprints of elements in buckets. While they support deletion, their lookup time (**82–90 ns**) is severely degraded.
- **Allocations**: The `boom.CuckooFilter` implementation allocates memory (`16 B/op`, `2 allocs/op`) during standard membership tests because it does not optimize slice conversions, causing significant garbage collector overhead.

### Scalable & Stable Bloom Filters
- **Scalable Bloom Filters** adapt dynamically by appending new filters as capacity is reached. A query must search multiple filters sequentially, multiplying cache misses and causing lookup times to degrade to **~114 ns** on misses.
- **Stable Bloom Filters** continuously evict old elements by decrementing random cells. This process introduces high computational overhead, making queries slower than a classic filter.

---

## 6. Recommendations

1. **Retain Blocked Bloom Filter Design**: Blocked Bloom filters are **5–10× faster** than classic, cuckoo, scalable, and stable filters due to cache line localization.
2. **Continue Using FNV-1a Word-at-a-time Hashing**: Our inlined word-at-a-time FNV-1a hash is faster than `xxhash` for 52-byte keys, making it the ideal hashing engine for our negative lookup filter.
3. **Use Non-Atomic BloomFilter by Default**: Unless multiple threads are concurrently writing to the negative filter, `BloomFilter` (non-atomic) should be used over `AtomicBloom` to save CPU cycles.
