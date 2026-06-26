package src

import (
	"context"

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
	
	events, err := generated.ParseJSONLV2(data, batches, nil, func(block *generated.ParsedBlock) error {
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
			return batches.Insert(ctx, store.Conn(), store.DB())
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
