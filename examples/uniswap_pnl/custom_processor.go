package uniswap_pnl

import (
	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/uniswap_pnl/generated"
	"github.com/franz101/sqd-go/internal/cli"
	"github.com/franz101/sqd-go/internal/ingestion"
)

// Custom schema explanation: you can access custom schema entities from state (e.g. state.UserPosition)
func Process(state *generated.State, block *generated.ParsedBlock) error {
	for ev := range block.EventsIter() {
		switch e := ev.(type) {
		case *generated.LBTCTransfer:
			// handleEventX(): process sender transfer
			zero := common.Address{}
			if e.From != zero {
				// ETL: retrieve current state or initialize new position
				pos, ok := state.UserPosition.Get(e.From)
				if !ok {
					pos = &generated.UserPosition{
						Address: e.From,
					}
				}
				// ETL: perform profit/loss / balance logic
				pos.Balance.Sub(&pos.Balance, &e.Value)
				pos.TotalOut.Add(&pos.TotalOut, &e.Value)

				// state persistence
				state.UserPosition.Save(pos, e.EventMeta)
			}

			// handleEventY(): process receiver transfer
			if e.To != zero {
				// ETL: retrieve current state or initialize new position
				pos, ok := state.UserPosition.Get(e.To)
				if !ok {
					pos = &generated.UserPosition{
						Address: e.To,
					}
				}
				// ETL: perform profit/loss / balance logic
				pos.Balance.Add(&pos.Balance, &e.Value)
				pos.TotalIn.Add(&pos.TotalIn, &e.Value)

				// state persistence
				state.UserPosition.Save(pos, e.EventMeta)
			}
		}
	}
	return nil
}

func init() {
	// link to cli
	generated.CustomProcessFn = Process
	cli.RegisterProcessor(generated.ProjectName, func() (ingestion.Processor, error) {
		return generated.NewProcessor()
	})

	// settings:
	// commit interval (every 4096 blocks).
	// To customize the state commit interval, set STATE_SNAPSHOT_INTERVAL=4096 in your .env file
}
