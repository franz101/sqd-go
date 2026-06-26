# FETCH10X: Strategies for 10x Faster Indexing

This specification outlines the technical approach to achieving a 10x speedup in fetching and parsing data within the `sqd-go` indexer, specifically tailored to the Uniswap example and eventually the core code generator.

## 1. 10x Faster Parsing (Generated Code Modifications)

The current `ParseJSONL` implementation sequentially scans lines and uses `jlexer` for tokenization. We can achieve massive speedups by applying the following optimizations directly in the generated `parser.go`:

### A. Fast-Path Newline Scanning
Currently, newlines are found using a manual Go loop:
```go
for lineEnd < len(rest) && rest[lineEnd] != '\n' { lineEnd++ }
```
**Fix:** Replace this with `bytes.IndexByte(rest, '\n')`, which leverages highly optimized, SIMD-accelerated assembly in the Go standard library, speeding up line splitting by ~10x.

### B. Pre-filtering with `bytes.Contains` (Sparse Event Optimization)
For rare events (like LBTC Transfers), 99% of the blocks fetched might contain irrelevant logs. 
Instead of fully parsing the `logs` JSON array for every block, we can do an ultra-fast subset string search before parsing the logs:
```go
// Pre-calculate target topics as bytes
var lbtcTopic = []byte("ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

// Inside the line loop, before parsing the 'logs' array:
hasEvent := bytes.Contains(line, lbtcTopic)
if !hasEvent {
    // Skip parsing the logs array entirely; only parse the block header for state sync
    l.SkipRecursive() 
}
```
This turns a heavy JSON tokenization process into a CPU cache-friendly linear scan.

### C. Zero-Allocation Hex Decoding
Current hex decoding converts `[]byte` to `string` and uses reflection/allocation-heavy methods. We can use a direct hex-to-byte decoder that avoids `string` allocations entirely when reading `address`, `topics`, and `data`.

### D. Parallel Chunk Parsing
When the parallel fetcher delivers a massive 100K-block page (which can be megabytes of JSONL), we can split the `data []byte` by newlines into $N$ chunks and process them in parallel using `runtime.GOMAXPROCS(0)` goroutines. The results can be merged synchronously before inserting into ClickHouse.

---

## 2. 10x Faster Fetching (Network & Pipeline)

The `UNISWAP_FAST_PROFILING_REPORT.md` indicates fetching accounts for 75-80% of total time. Since `portal.sqd.dev` limits requests to ~5 req/s, the only way to scale throughput by 10x is to maximize data per request and optimize decompression.

### A. Maximizing Page Size
- **Current:** ~10,000 blocks per parallel fetch page.
- **10x Strategy:** Increase `SQD_PARALLEL_PAGE_SIZE` to 100,000 or 250,000. Because the rate limit is static (5 req/s), fetching larger spans per request linearly scales throughput. 

### B. Concurrent `zstd` Decompression
Currently, the `client.go` uses `zstd.WithDecoderConcurrency(0)`. We can configure it to use multiple CPU cores (`WithDecoderConcurrency(runtime.GOMAXPROCS(0))`), significantly reducing the decompression time overhead for massive 100K-block payloads.

### C. Pipelined Parsing & Fetching
Instead of waiting for a 10MB block to decompress and THEN parsing it, we can stream the decompressed bytes directly into the parallel chunk parser, overlapping network I/O, zstd CPU work, and JSON parsing CPU work perfectly.

---

## Next Steps for Validation

1. **Modify `examples/uniswap/generated/parser.go` directly** to implement `bytes.IndexByte` and the `bytes.Contains` pre-filter.
2. Run `make uniswap-fast` to profile the exact parsing speedup.
3. Test larger `SQD_PARALLEL_PAGE_SIZE` settings.
4. If successful, backport these direct modifications into `internal/codegen/` so all future generated code benefits from 10x parsing.
