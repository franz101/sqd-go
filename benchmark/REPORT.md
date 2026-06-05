# JSONL Pipeline Benchmark Report

## Data File
`exchange_events.jsonl`: 948 blocks, 20,583 logs, 15.8 MB  
Exchange contract events (OrderFilled + paired events)

## Pipeline
```
JSONL bytes → JSON tokenize → hex decode → ABI unpack → ring buffer
                                              ↓
                              [{blocknumber, Transfers[], Positions[], evtOrder[]}]
```

## Final Results (15.8 MB, i7-8700 @ 3.20GHz)

| Pipeline | ns/op | MB/s | B/op | allocs/op |
|----------|-------|------|------|-----------|
| ParseOnly (fastjson) | 11,710,820 | 1.35 GB/s | - | - |
| FastJSON (fastjson + decode + fill) | 13,843,275 | 1.14 GB/s | ~2.9M | ~3,800 |
| Sonic AST | 40,953,760 | 0.39 GB/s | 59M | 110K |
| Sonic Unmarshal (JIT) | 25,657,757 | 0.62 GB/s | 18M | 23K |
| Simdjson (SIMD tape) | 5,905,932 | 2.67 GB/s | 65M | 9.7K |
| EasyJSON Codegen | 9,203,324 | 1.72 GB/s | 3,712 | 18 |
| **EasyJSON Raw Lexer** | **4,137,782** | **3.82 GB/s** | **64** | **2** |

## Key Findings

### 1. Raw lexer dominates (3.8 GB/s, 2 allocs)
Walking `jlexer.Lexer` directly and extracting only needed fields avoids:
- Intermediate `jtypes.JSONLBlock` / `[]JSONLLog` allocation
- Parsing unused fields (hash, timestamp, txHash, txIndex, logIndex, address)
- The `easyjson.UnmarshalEasyJSON` dispatch overhead

The code parses exactly what it needs: `header.number` (block number), `logs[].data` (hex-encoded ABI), `logs[].topics` (indexed params).

### 2. Sonic fails on small-doc workloads
JIT compilation and AST allocation overhead never amortizes when parsing 948 separate ~16KB JSON objects.

### 3. Simdjson is competitive but allocates heavily
SIMD parsing matches speeds but copies every string on access. 64MB alloc per 15.8MB input.

### 4. Hex decode is no longer the bottleneck
256-entry lookup table + unrolled decode. Invisible against JSON tokenization.

## Optimizations Applied

1. **Raw jlexer** — manual token walking, skip unused fields, no intermediate structs
2. **256-entry hex lookup table** — branchless nibble decode
3. **Unrolled hexDecode32/hexDecode20** — 32/20 explicit loads with BCE hints
4. **Direct field decode** — ABI words → `[32]byte` struct fields, no intermediate buffer
5. **Preallocated ring buffer** — `make([]Transfer, 0, 256)` avoids append-growth
6. **Lexer reuse** — single `jlexer.Lexer{}`, `l.Data = line` per iteration
7. **Inline decode** — hex decode + ring fill inlined into the lexer loop

## Recommendation

Use **easyjson's raw `jlexer.Lexer`** to walk JSON tokens directly into the ring buffer. The pipeline achieves 3.8 GB/s single-core with 2 heap allocations per 15.8MB input.
