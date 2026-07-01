package lbtc

import (
	"github.com/ethereum/go-ethereum/common"

	// generated is THIS project's own generated package. The import path is
	// module-relative ("<module>/generated"); here the module is sqd-go itself,
	// so it resolves to .../examples/lbtc/generated. `sqd-go codegen` writes
	// this package — do not edit it by hand. If your editor flags this import
	// before the first codegen run, run `sqd-go codegen .` (or
	// `go run . codegen examples/lbtc`) to create the package.
	generated "github.com/franz101/sqd-go/examples/lbtc/generated"

	// sqd is the PUBLIC facade. A custom processor imports this, never the
	// module's internal/ packages, so an indexer project can build as its own
	// standalone Go module. See docs/GO_MODULES.md.
	"github.com/franz101/sqd-go/sqd"
)

// Process runs once per parsed block. It receives the derived State (the
// hot-state entities declared in custom_schema.go) and the decoded ParsedBlock.
// Iterate block.EventsIter() and type-switch on the generated event structs.
// Every event embeds the always-present EventMeta fields (BlockNumber,
// BlockTimestamp, TransactionIndex, LogIndex, ...) — see docs/EVENT_FIELDS.md.
//
// This processor tracks, per address, the cumulative LBTC sent and received.
func Process(state *generated.State, block *generated.ParsedBlock) error {
	var zero common.Address
	for ev := range block.EventsIter() {
		e, ok := ev.(*generated.LBTCTransfer)
		if !ok {
			continue
		}
		// Debit the sender. Save() stamps UpdatedAtBlock / UpdatedAt from meta.
		if e.From != zero {
			pos := userPosition(state, e.From)
			pos.TotalOut.Add(&pos.TotalOut, &e.Value)
			pos.TransferCount++
			state.UserPosition.Save(pos, e.EventMeta)
		}
		// Credit the receiver.
		if e.To != zero {
			pos := userPosition(state, e.To)
			pos.TotalIn.Add(&pos.TotalIn, &e.Value)
			pos.TransferCount++
			state.UserPosition.Save(pos, e.EventMeta)
		}
	}
	return nil
}

// ProcessProto keeps the same example logic under the default proto decoder.
// A production processor can replace this bridge with direct proto-view access.
func ProcessProto(state *generated.State, block *generated.ProtoEventBlock) error {
	return Process(state, block.ToParsedBlock())
}

// userPosition returns the current position for addr, initializing a fresh one
// (with its primary key set) when this is the first time we've seen the account.
func userPosition(state *generated.State, addr common.Address) *generated.UserPosition {
	pos, ok := state.UserPosition.Get(addr)
	if !ok {
		pos = &generated.UserPosition{Address: addr}
	}
	return pos
}

func init() {
	// Link the processor to the generated runtime, and register it under the
	// project name. Always use generated.ProjectName (derived from config.yaml's
	// `name:`) so the registered name can never drift from the config.
	generated.CustomProcessFn = Process
	generated.CustomProcessProtoFn = ProcessProto
	sqd.RegisterProcessor(generated.ProjectName, func() (sqd.Processor, error) {
		return generated.NewProcessor(sqd.GetProtoMode())
	})
}
