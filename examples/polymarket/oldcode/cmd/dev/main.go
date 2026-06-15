package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
)

var ringBuffer, _ = generated.NewOrderedHistoricRingBuffer(1024)
var state = generated.NewState()

func main() {
	var (
		projectPath = flag.String("project", "examples/polymarket", "project directory or config file")
		restart     = flag.Bool("restart", false, "drop ClickHouse database before indexing")
		startBlock  = flag.Uint64("start-block", 0, "override configured start block")
		endBlock    = flag.Uint64("end-block", 0, "override configured end block; 0 means tail")
		blockchain  = flag.String("blockchain", "", "override chain id or name")
		cpuprofile  = flag.String("cpuprofile", "", "write cpu profile to file")
	)
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			exitf("create cpu profile: %v", err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	project, err := config.LoadProject(*projectPath)
	if err != nil {
		exitf("load config: %v", err)
	}
	loadEnv(filepath.Join(project.Root, ".env"))
	applyOverrides(project.Config, *startBlock, *endBlock, *blockchain)

	opts := ingestion.Options{
		ClickHouseHost:     envOrDefault("CLICKHOUSE_HOST", "127.0.0.1"),
		ClickHousePort:     envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9000),
		ClickHouseUser:     envOrDefault("CLICKHOUSE_USER", "default"),
		ClickHousePassword: envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse"),
		ClickHouseDatabase: project.Config.Name,
		Restart:            *restart,
		GeneratedSQLDir:    filepath.Join(project.Root, ".sqd", "generated"),
		CursorMode:         true,
		CustomProcessor:    processPolymarketBatch,
		StateRestorer: func(b uint64) error {
			_, err := state.RestoreToBlock(b)
			return err
		},
		StateLoader: func(ctx context.Context, blockNumber uint64) error {
			return state.LoadFromClickHouse(ctx, blockNumber)
		},
	}
	if err := ingestion.Run(context.Background(), project.Config, opts); err != nil {
		exitf("ingestion: %v", err)
	}
}

func processPolymarketBatch(ctx context.Context, store *database.Store, logs []ingestion.CustomLog) error {
	if len(logs) == 0 {
		return nil
	}

	type blockGroup struct {
		blockNum  uint64
		blockHash string
		logs      []generated.DecodedLog
	}
	var groups []blockGroup
	var curGroup *blockGroup

	for _, lg := range logs {
		decoded, err := generated.UnpackLog(lg.ContractAddress, lg.Topics, common.FromHex(lg.Data))
		if err != nil {
			return fmt.Errorf("unpack log block=%d tx=%s log=%d: %w", lg.BlockNumber, lg.TransactionHash, lg.LogIndex, err)
		}
		if decoded == nil || decoded.Value == nil {
			continue
		}
		meta := generated.EventMeta{
			BlockNumber:      lg.BlockNumber,
			BlockTimestamp:   lg.BlockTimestamp,
			TransactionIndex: lg.TransactionIndex,
			LogIndex:         lg.LogIndex,
		}
		setEventMeta(decoded, meta)

		if curGroup == nil || curGroup.blockNum != lg.BlockNumber {
			groups = append(groups, blockGroup{
				blockNum:  lg.BlockNumber,
				blockHash: lg.BlockHash,
			})
			curGroup = &groups[len(groups)-1]
		}
		curGroup.logs = append(curGroup.logs, *decoded)
	}

	for _, g := range groups {
		ringBuffer.Push(g.blockNum, g.blockHash, g.logs)
		block, ok := ringBuffer.GetParsedBlock(g.blockNum)
		if !ok {
			return fmt.Errorf("block %d not found after push", g.blockNum)
		}
		if err := generated.CustomProcessing(ctx, store, state, block); err != nil {
			return fmt.Errorf("custom processing block %d failed: %w", g.blockNum, err)
		}
	}

	return nil
}

func setEventMeta(decoded *generated.DecodedLog, meta generated.EventMeta) {
	if decoded == nil || decoded.Value == nil {
		return
	}
	switch ev := decoded.Value.(type) {
	case *generated.ConditionalTokensConditionPreparation:
		ev.EventMeta = meta
	case *generated.ConditionalTokensConditionResolution:
		ev.EventMeta = meta
	case *generated.ConditionalTokensPositionSplit:
		ev.EventMeta = meta
	case *generated.ConditionalTokensPositionsMerge:
		ev.EventMeta = meta
	case *generated.ConditionalTokensPayoutRedemption:
		ev.EventMeta = meta
	case *generated.ExchangeOrderFilled:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterMarketPrepared:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterQuestionPrepared:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPositionSplit:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPositionsMerge:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPositionsConverted:
		ev.EventMeta = meta
	case *generated.NegRiskAdapterPayoutRedemption:
		ev.EventMeta = meta
	}
}

func applyOverrides(cfg *config.Config, startBlock, endBlock uint64, chain string) {
	if cfg == nil {
		return
	}
	if chain != "" {
		if id, ok := chainID(chain); ok {
			for i := range cfg.Chains {
				cfg.Chains[i].ID = id
			}
		}
	}
	if startBlock > 0 {
		for i := range cfg.Chains {
			cfg.Chains[i].StartBlock = startBlock
		}
	}
	if endBlock > 0 {
		for i := range cfg.Chains {
			end := endBlock
			cfg.Chains[i].EndBlock = &end
		}
	}
}

func chainID(v string) (uint64, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "polygon", "polygon-mainnet":
		return 137, true
	case "ethereum", "ethereum-mainnet", "mainnet":
		return 1, true
	default:
		id, err := strconv.ParseUint(v, 10, 64)
		return id, err == nil
	}
}

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		if strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") {
			v = strings.Trim(v, "\"")
		}
		os.Setenv(k, v)
	}
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envOrDefaultInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func exitf(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
	os.Exit(1)
}
