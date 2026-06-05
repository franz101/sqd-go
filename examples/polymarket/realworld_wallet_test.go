package polymarket

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/shopspring/decimal"
)

func TestRealWorldWallet0xa0932d9Integration(t *testing.T) {
	// Read the real-world JSONL fixture for wallet 0xa0932d9aa1ca003376d1237c799efacb302a1198
	data, err := os.ReadFile("../../tests/wallet_0xa0932d9_all.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	state := generated.NewState()
	p := parser.NewFastJSONLParser(2048)
	var processedEvents int

	walletAddress := common.HexToAddress("0xa0932d9aa1ca003376d1237c799efacb302a1198")

	if err := p.Parse(data, func(block *parser.Block) error {
		for _, lg := range block.Logs {
			decoded, err := generated.UnpackLog(lg.Address, lg.Topics, common.FromHex(lg.Data))
			if err != nil {
				return fmt.Errorf("decode block=%d tx=%s log=%d: %w", block.Header.Number, lg.TransactionHash, lg.LogIndex, err)
			}
			if decoded == nil || decoded.Value == nil {
				continue
			}

			meta := generated.EventMeta{
				BlockNumber:      block.Header.Number,
				BlockTimestamp:   time.Unix(int64(block.Header.Timestamp), 0).UTC(),
				BlockHash:        common.HexToHash(block.Header.Hash),
				ContractAddress:  common.HexToAddress(lg.Address),
				TransactionHash:  common.HexToHash(lg.TransactionHash),
				TransactionIndex: lg.TransactionIndex,
				LogIndex:         lg.LogIndex,
			}

			switch ev := decoded.Value.(type) {
			case *generated.ConditionalTokensConditionPreparation:
				handleConditionPreparation(state, ev)
				processedEvents++

			case *generated.ConditionalTokensConditionResolution:
				handleConditionResolution(state, ev)
				processedEvents++

			case *generated.ConditionalTokensPositionSplit:
				if ev.Stakeholder == walletAddress {
					ev.EventMeta = meta
					handlePositionSplit(state, ev)
					processedEvents++
				}

			case *generated.ConditionalTokensPositionsMerge:
				if ev.Stakeholder == walletAddress {
					ev.EventMeta = meta
					handlePositionsMerge(state, ev)
					processedEvents++
				}

			case *generated.ExchangeOrderFilled:
				if ev.Maker == walletAddress || ev.Taker == walletAddress {
					ev.EventMeta = meta
					handleOrderFilled(state, ev)
					processedEvents++
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	t.Logf("Processed %d events from real-world wallet JSONL fixture.", processedEvents)

	// Assert on the resulting positions of our target wallet
	var positionsCount int
	var nonZeroPositions int
	totalRealizedPnL := decimal.Zero

	state.HotState.UserPositions.Range(func(key generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.User == walletAddress {
			positionsCount++
			totalRealizedPnL = totalRealizedPnL.Add(toDecimal(pos.RealizedPnL))
			if !pos.Amount.IsZero() {
				nonZeroPositions++
			}
			t.Logf("Position: TokenID=%s Amount=%s AvgPrice=%s RealizedPnL=%s",
				pos.TokenID.Hex(),
				toDecimal(pos.Amount).String(),
				toDecimal(pos.AvgPrice).String(),
				toDecimal(pos.RealizedPnL).String())
		}
		return true
	})

	if positionsCount == 0 {
		t.Fatal("expected at least one processed user position for the target wallet")
	}

	t.Logf("Total Realized PnL for target wallet: %s", totalRealizedPnL.String())
}

func TestRealWorldWallet0xa0932d9BatchFixtureIntegration(t *testing.T) {
	logs, summary := readWalletFixtureLogs(t, "../../tests/wallet_0xa0932d9_all.jsonl")

	if summary.orderFilledMaker != 13 {
		t.Fatalf("Exchange OrderFilled maker count = %d, want 13", summary.orderFilledMaker)
	}
	if summary.positionSplit != 1 {
		t.Fatalf("CTF PositionSplit count = %d, want 1", summary.positionSplit)
	}
	if summary.positionsMerge != 9 {
		t.Fatalf("CTF PositionsMerge count = %d, want 9", summary.positionsMerge)
	}
	if summary.conditionPreparation != 6 {
		t.Fatalf("ConditionPreparation count = %d, want 6", summary.conditionPreparation)
	}

	state := processWalletFixtureLogs(t, logs)

	walletAddress := common.HexToAddress("0xa0932d9aa1ca003376d1237c799efacb302a1198")
	var positionsCount int
	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, pos generated.MemoryUserPosition) bool {
		if pos.User == walletAddress {
			positionsCount++
		}
		return true
	})
	if positionsCount == 0 {
		t.Fatal("expected batch fixture path to create wallet positions")
	}
}

func processWalletFixtureLogs(t *testing.T, logs []ingestion.CustomLog) *generated.State {
	t.Helper()

	ring, err := generated.NewOrderedHistoricRingBuffer(8192)
	if err != nil {
		t.Fatalf("new ring: %v", err)
	}
	state := generated.NewState()
	var curBlock uint64
	var curHash string
	var group []generated.DecodedLog
	flush := func() {
		if len(group) == 0 {
			return
		}
		ring.Push(curBlock, curHash, group)
		block, ok := ring.GetParsedBlock(curBlock)
		if !ok {
			t.Fatalf("block %d not found after push", curBlock)
		}
		if err := generated.CustomProcessing(context.Background(), generated.Store(nil), state, block); err != nil {
			t.Fatalf("custom processing block %d: %v", curBlock, err)
		}
		group = group[:0]
	}

	for _, lg := range logs {
		if len(group) > 0 && lg.BlockNumber != curBlock {
			flush()
		}
		if len(group) == 0 {
			curBlock = lg.BlockNumber
			curHash = lg.BlockHash
		}

		meta := generated.EventMeta{
			BlockNumber:      lg.BlockNumber,
			BlockTimestamp:   lg.BlockTimestamp,
			BlockHash:        common.HexToHash(lg.BlockHash),
			ContractAddress:  common.HexToAddress(lg.ContractAddress),
			TransactionHash:  common.HexToHash(lg.TransactionHash),
			TransactionIndex: lg.TransactionIndex,
			LogIndex:         lg.LogIndex,
		}
		decoded, err := generated.UnpackLogWithMeta(lg.ContractAddress, lg.Topics, common.FromHex(lg.Data), meta)
		if err != nil {
			t.Fatalf("decode block=%d tx=%s log=%d: %v", lg.BlockNumber, lg.TransactionHash, lg.LogIndex, err)
		}
		if decoded != nil && decoded.Value != nil {
			group = append(group, *decoded)
		}
	}
	flush()
	return state
}

type walletFixtureSummary struct {
	orderFilledMaker     int
	positionSplit        int
	positionsMerge       int
	conditionPreparation int
}

func readWalletFixtureLogs(t *testing.T, path string) ([]ingestion.CustomLog, walletFixtureSummary) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	walletAddress := common.HexToAddress("0xa0932d9aa1ca003376d1237c799efacb302a1198")
	p := parser.NewFastJSONLParser(2048)
	var logs []ingestion.CustomLog
	var summary walletFixtureSummary

	if err := p.Parse(data, func(block *parser.Block) error {
		for _, lg := range block.Logs {
			// parser reuses the Topics backing array across blocks; clone since
			// these logs are retained past the callback.
			topics := append([]string(nil), lg.Topics...)
			logs = append(logs, ingestion.CustomLog{
				BlockNumber:      block.Header.Number,
				BlockTimestamp:   time.Unix(int64(block.Header.Timestamp), 0).UTC(),
				BlockHash:        block.Header.Hash,
				ContractAddress:  lg.Address,
				TransactionHash:  lg.TransactionHash,
				TransactionIndex: lg.TransactionIndex,
				LogIndex:         lg.LogIndex,
				Topics:           topics,
				Data:             lg.Data,
			})

			decoded, err := generated.UnpackLog(lg.Address, lg.Topics, common.FromHex(lg.Data))
			if err != nil {
				return fmt.Errorf("decode block=%d tx=%s log=%d: %w", block.Header.Number, lg.TransactionHash, lg.LogIndex, err)
			}
			if decoded == nil || decoded.Value == nil {
				continue
			}
			switch ev := decoded.Value.(type) {
			case *generated.ConditionalTokensConditionPreparation:
				summary.conditionPreparation++
			case *generated.ConditionalTokensPositionSplit:
				if ev.Stakeholder == walletAddress {
					summary.positionSplit++
				}
			case *generated.ConditionalTokensPositionsMerge:
				if ev.Stakeholder == walletAddress {
					summary.positionsMerge++
				}
			case *generated.ExchangeOrderFilled:
				if ev.Maker == walletAddress {
					summary.orderFilledMaker++
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return logs, summary
}
