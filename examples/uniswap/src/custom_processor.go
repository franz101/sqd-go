package src

import (
	"context"
	"sync"

	"github.com/franz101/sqd-go/examples/uniswap/generated"
	"github.com/franz101/sqd-go/internal/cli"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
)

type LBTCProcessor struct {}

func (p *LBTCProcessor) Process(ctx context.Context, store *database.Store, logs []ingestion.CustomLog) error {
	return nil
}
func (p *LBTCProcessor) RestoreToBlock(blockNumber uint64) (uint64, error) { return blockNumber, nil }
func (p *LBTCProcessor) LoadFromDatabase(ctx context.Context, blockNumber uint64) error { return nil }

func (p *LBTCProcessor) ProcessJSONL(ctx context.Context, store *database.Store, data []byte) (uint64, error) {
	batches := generated.NewInsertBatches()
	events, err := generated.ParseJSONLV2(data, batches, nil, nil)
	if err != nil {
		return 0, err
	}
	if events > 0 {
		return events, batches.Insert(ctx, store.Conn(), store.DB())
	}
	return 0, nil
}

func (p *LBTCProcessor) ProcessJSONLWithInserts(ctx context.Context, store *database.Store, data []byte) (uint64, func(context.Context) error, error) {
	batches := generated.NewInsertBatches()
	ring, _ := generated.NewOrderedHistoricRingBuffer(16384)
	
	events, err := generated.ParseJSONLV2(data, batches, ring, func(block *generated.ParsedBlock) error {
		entities := &generated.Entities{
			BlockNumber:  block.BlockNumber,
			LBTCTransfer: block.LBTCTransfers,
		}
		return generated.CustomProcessing(ctx, store, entities)
	})
	if err != nil {
		return 0, nil, err
	}
	
	flush := func(ctx context.Context) error {
		if events > 0 {
			return batches.Insert(ctx, store.InsertConn(), store.DB())
		}
		return nil
	}
	return events, flush, nil
}

func init() {
	cli.RegisterProcessorV2("case_1_lbtc_event_only", func(protoMode bool) (ingestion.Processor, error) {
		return &LBTCProcessor{}, nil
	})
}

var ringPool = sync.Pool{
	New: func() interface{} {
		ring, _ := generated.NewOrderedHistoricRingBuffer(2048)
		return ring
	},
}

func (p *LBTCProcessor) SupportsBatchParse() bool {
	return true
}

func (p *LBTCProcessor) ParseBatchForInserts(store *database.Store, data []byte, endBlock uint64, onParsed func(ingestion.BatchParsedBlock) error) (uint64, func(context.Context) error, error) {
	batches := generated.NewInsertBatches()
	events, err := generated.ParseJSONLV2(data, batches, nil, func(block *generated.ParsedBlock) error {
		if endBlock > 0 && block.BlockNumber > endBlock {
			return nil
		}
		return onParsed(ingestion.BatchParsedBlock{
			Number: block.BlockNumber,
			Block:  block,
		})
	})
	if err != nil {
		return 0, nil, err
	}
	flush := func(ctx context.Context) error {
		if events > 0 {
			return batches.Insert(ctx, store.InsertConn(), store.DB())
		}
		return nil
	}
	return events, flush, nil
}

func (p *LBTCProcessor) ProcessParsedBlock(ctx context.Context, store *database.Store, block any) error {
	b := block.(*generated.ParsedBlock)
	for _, ev := range b.LBTCTransfers {
		generated.GlobalState.ApplyTransfer(ev.From, ev.To, &ev.Value, b.BlockNumber)
	}
	return nil
}

func (p *LBTCProcessor) CommitDerivedState(ctx context.Context, store *database.Store, block uint64) error {
	return generated.GlobalState.SyncToClickHouse(ctx, store, block)
}

func (p *LBTCProcessor) ReclaimParseBatches() {
	// Not safe to return to pool if consumer holds references indefinitely, but we can't easily track lifetimes.
	// Actually, the consumer processes blocks sequentially. We can just let GC handle them and use a small ring buffer size!
}
