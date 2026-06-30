package polymarket

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

func loadPolymarketBenchData(b *testing.B) []byte {
	b.Helper()
	path := os.Getenv("POLYMARKET_BENCH_FILE")
	if path == "" {
		b.Skip("set POLYMARKET_BENCH_FILE to a portal JSONL sample")
	}
	if !filepath.IsAbs(path) {
		if _, err := os.Stat(path); err != nil {
			_, currentFile, _, ok := runtime.Caller(0)
			if !ok {
				b.Fatalf("resolve relative benchmark path %q", path)
			}
			path = filepath.Join(filepath.Dir(currentFile), "..", "..", path)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatalf("read %s: %v", path, err)
	}
	return data
}

func parsePolymarketBenchBlocks(b *testing.B, data []byte) []*generated.ProtoEventBlock {
	b.Helper()
	ring, err := generated.NewProtoRingBuffer(1024)
	if err != nil {
		b.Fatal(err)
	}
	batches := generated.NewInsertBatches()
	var blocks []*generated.ProtoEventBlock
	events, err := generated.ParseJSONLProto(data, batches, ring, func(block *generated.ProtoEventBlock) error {
		blocks = append(blocks, block)
		return nil
	})
	if err != nil {
		b.Fatalf("parse proto: %v", err)
	}
	if events == 0 || len(blocks) == 0 {
		b.Fatalf("empty benchmark sample: events=%d blocks=%d", events, len(blocks))
	}
	return blocks
}

func BenchmarkPolymarketGeneratedParseProtoReuse(b *testing.B) {
	data := loadPolymarketBenchData(b)
	ring, err := generated.NewProtoRingBuffer(1024)
	if err != nil {
		b.Fatal(err)
	}
	batches := generated.NewInsertBatches()

	if _, err := generated.ParseJSONLProto(data, batches, ring, nil); err != nil {
		b.Fatalf("warm parse proto: %v", err)
	}
	batches.Reset()
	ring.Reset()

	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batches.Reset()
		ring.Reset()
		events, err := generated.ParseJSONLProto(data, batches, ring, nil)
		if err != nil {
			b.Fatalf("parse proto: %v", err)
		}
		if events == 0 {
			b.Fatal("empty parse")
		}
	}
}

func BenchmarkPolymarketProcessProtoWarmState(b *testing.B) {
	data := loadPolymarketBenchData(b)
	blocks := parsePolymarketBenchBlocks(b, data)
	ctx := context.Background()
	state := generated.NewState()

	for _, block := range blocks {
		if err := generated.CustomProcessingProto(ctx, nil, state, block); err != nil {
			b.Fatalf("warm process proto: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, block := range blocks {
			if err := generated.CustomProcessingProto(ctx, nil, state, block); err != nil {
				b.Fatalf("process proto: %v", err)
			}
		}
	}
}

func BenchmarkPolymarketParseAndProcessProtoReuse(b *testing.B) {
	data := loadPolymarketBenchData(b)
	ring, err := generated.NewProtoRingBuffer(1024)
	if err != nil {
		b.Fatal(err)
	}
	batches := generated.NewInsertBatches()
	ctx := context.Background()
	state := generated.NewState()

	if _, err := generated.ParseJSONLProto(data, batches, ring, func(block *generated.ProtoEventBlock) error {
		return generated.CustomProcessingProto(ctx, nil, state, block)
	}); err != nil {
		b.Fatalf("warm parse+process proto: %v", err)
	}
	batches.Reset()
	ring.Reset()

	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batches.Reset()
		ring.Reset()
		events, err := generated.ParseJSONLProto(data, batches, ring, func(block *generated.ProtoEventBlock) error {
			return generated.CustomProcessingProto(ctx, nil, state, block)
		})
		if err != nil {
			b.Fatalf("parse+process proto: %v", err)
		}
		if events == 0 {
			b.Fatal("empty parse+process")
		}
	}
}
