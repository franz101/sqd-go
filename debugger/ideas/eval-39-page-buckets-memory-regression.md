# Eval #39 — Page Buckets: Throughput Win, Memory Regression

## What I did
Replaced sort.Slice with O(N) page grouping via pre-allocated `[][]rawTransfer` array (312,500 slices, ~750KB). During parse loop, append each transfer to `e.buckets[posID>>4]`. Track touched pages in `e.touchedPages`. Process touched pages in order: one ReadAt, apply all bucket transfers, one WriteAt. Reset via `[:0]` after each block.

## Score
**273.28** — Regression from 302.36 (eval #38). Throughput: 680.3k tx/s. Memory: 20.9 MB.

## Why it regressed
The page bucket array `buckets [][]rawTransfer` with 312,500 slice headers costs 7.5MB alone (312,500 × 24 bytes). Backing arrays for touched buckets hold ~500K transfer copies over 50 blocks, adding ~8MB. Total: ~16MB bucket overhead + <5MB base memory = 20.9MB.

The score formula is `throughput / memory^0.3`. The throughput gain (468k→680k, +45%) was swamped by memory increase (4.3→20.9MB, 4.9×). At 4.3MB, 680k tx/s would score 439.

## The real win
The 45% throughput gain came from **inline byte-level processing** (direct byte buffer manipulation, no Position struct, no dirty map, no interface dispatch), NOT from eliminating the sort. The sort was ~2μs per block at 10K elements — negligible.

## Next: Eval #40 — Keep throughput, fix memory
Replace per-page bucket array with `sort.Slice` by posID. Keep the inline byte processing, custom JSON parser, and ReadAt/WriteAt-per-page pattern. Memory drops from 20.9MB → ~4.5MB. Expected score: 680/4.3^0.3 ≈ 439.
