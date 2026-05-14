package ingestion

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/parser"
)

type Options struct {
	ClickHouseHost     string
	ClickHousePort     int
	ClickHouseUser     string
	ClickHousePassword string
	ClickHouseDatabase string
	PageSize           uint64
	StartBlock         uint64
	BlockCount         uint64
	Restart            bool
	GeneratedSQLDir    string
	CursorMode         bool
}

func Run(ctx context.Context, cfg *config.Config, opts Options) error {
	if opts.Restart {
		if err := database.DropClickHouseDatabase(ctx, opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseUser, opts.ClickHousePassword, opts.ClickHouseDatabase); err != nil {
			return fmt.Errorf("drop clickhouse database: %w", err)
		}
	}
	store, err := database.NewClickHouse(ctx, opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseUser, opts.ClickHousePassword, opts.ClickHouseDatabase)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer store.Close()

	if opts.GeneratedSQLDir != "" {
		if err := store.ApplySQLFile(ctx, filepath.Join(opts.GeneratedSQLDir, "schema.sql")); err != nil {
			return fmt.Errorf("apply generated schema: %w", err)
		}
		if err := store.ApplySQLFile(ctx, filepath.Join(opts.GeneratedSQLDir, "views.sql")); err != nil {
			return fmt.Errorf("apply generated views: %w", err)
		}
	} else if err := store.EnsureTables(ctx); err != nil {
		return fmt.Errorf("ensure tables: %w", err)
	}
	log.Printf("ClickHouse connected: %s:%d/%s", opts.ClickHouseHost, opts.ClickHousePort, opts.ClickHouseDatabase)

	for _, chain := range cfg.Chains {
		if err := processChain(ctx, store, &chain, opts.PageSize, opts.StartBlock, opts.BlockCount, opts.CursorMode); err != nil {
			log.Printf("chain %d error: %v", chain.ID, err)
		}
	}
	log.Println("Done.")
	return nil
}

func processChain(ctx context.Context, store *database.Store, chain *config.Chain, pageSize, flagStartBlock, blockCountLimit uint64, cursorMode bool) error {
	if len(chain.Contracts) == 0 {
		return fmt.Errorf("no contracts defined for chain %d", chain.ID)
	}
	log.Printf("Chain %d: building event decoders from %d contracts...", chain.ID, len(chain.Contracts))
	decoders, filters, err := parser.BuildEventDecoder(chain.Contracts)
	if err != nil {
		return fmt.Errorf("build decoders: %w", err)
	}
	typedTables, err := buildTypedTableIndex(chain)
	if err != nil {
		return fmt.Errorf("build typed tables: %w", err)
	}
	log.Printf("Chain %d: %d event types, %d filter(s)", chain.ID, len(decoders), len(filters))

	currentBlock := chain.StartBlock
	if flagStartBlock > 0 {
		currentBlock = flagStartBlock
	}
	last, hasLast, err := store.LastBlock(ctx, chain.ID)
	if err != nil {
		return fmt.Errorf("read last block: %w", err)
	}
	if hasLast {
		if err := store.TruncateAfterBlock(ctx, chain.ID, last); err != nil {
			return fmt.Errorf("truncate after checkpoint %d: %w", last, err)
		}
		if last >= currentBlock {
			currentBlock = last + 1
		}
	}
	effectiveEndBlock := chain.EndBlock
	if blockCountLimit > 0 {
		end := currentBlock + blockCountLimit - 1
		effectiveEndBlock = minEndBlock(effectiveEndBlock, end)
	}
	if cursorMode {
		if effectiveEndBlock != nil {
			log.Printf("Chain %d: starting from block %d (cursor mode, local stop at %d)", chain.ID, currentBlock, *effectiveEndBlock)
		} else {
			log.Printf("Chain %d: starting from block %d (cursor mode)", chain.ID, currentBlock)
		}
	} else {
		if pageSize == 0 {
			pageSize = 1000
		}
		log.Printf("Chain %d: starting from block %d", chain.ID, currentBlock)
	}

	sqd := client.New(chainEndpoint(chain.ID))
	defer sqd.Close()
	jsonl := parser.NewFastJSONLParser(1024)
	inserter := store.NewInserter()
	totalBlocks, totalEvents := uint64(0), uint64(0)
	startTime := time.Now()

	// profiling accumulators
	var profFetch, profParse, profDecode, profMarshal, profInsert time.Duration
	profIters := 0

	for {
		select {
		case <-ctx.Done():
			log.Printf("Chain %d: interrupted at block %d", chain.ID, currentBlock)
			// print profile
			printProfile(profFetch, profParse, profDecode, profMarshal, profInsert, profIters, totalBlocks, totalEvents, startTime)
			return ctx.Err()
		default:
		}

		requestStartBlock := currentBlock
		toBlockPtr, rangeLabel, ok := nextRequestRange(currentBlock, pageSize, effectiveEndBlock, cursorMode)
		if !ok {
			break
		}
		toBlock := uint64(0)
		if toBlockPtr != nil {
			toBlock = *toBlockPtr
		}

		fetchStart := time.Now()
		raw, err := sqd.FetchRaw(ctx, currentBlock, toBlockPtr, filters)
		profFetch += time.Since(fetchStart)
		if err != nil {
			log.Printf("Chain %d: fetch %s error: %v", chain.ID, rangeLabel, err)
			time.Sleep(5 * time.Second)
			continue
		}

		var decodedEvents []parser.DecodedEvent
		responseBlockCount := uint64(0)
		eventBlockCount := uint64(0)

		lastProcessed := toBlock
		if len(raw) == 0 {
			if cursorMode {
				if effectiveEndBlock != nil {
					lastProcessed = *effectiveEndBlock
					if err := store.UpdateSyncState(ctx, chain.ID, lastProcessed); err != nil {
						return fmt.Errorf("update sync state %d: %w", lastProcessed, err)
					}
				}
				log.Printf("Chain %d: empty response %s, stopping", chain.ID, rangeLabel)
				break
			}
			if err := store.UpdateSyncState(ctx, chain.ID, lastProcessed); err != nil {
				return fmt.Errorf("update sync state %d: %w", lastProcessed, err)
			}
			log.Printf("Chain %d: empty response %s, advancing", chain.ID, rangeLabel)
		} else {
			typedEvents := make(map[string][]parser.DecodedEvent)
			typedSpecs := make(map[string]database.TypedEventTable)
			var blockRows []database.BlockRow
			maxSeenBlock := uint64(0)
			seenBeyondEnd := false

			parseStart := time.Now()
			var decodeDur, marshalDur time.Duration
			err = jsonl.Parse(raw, func(block *parser.Block) error {
				if block.Header.Number > maxSeenBlock {
					maxSeenBlock = block.Header.Number
				}
				if effectiveEndBlock != nil && block.Header.Number > *effectiveEndBlock {
					seenBeyondEnd = true
					return nil
				}
				responseBlockCount++
				blockHash := strings.Clone(block.Header.Hash)
				blockTS := time.Unix(int64(block.Header.Timestamp), 0).UTC()
				blockRow := database.BlockRow{
					ChainID:        chain.ID,
					BlockNumber:    block.Header.Number,
					BlockTimestamp: blockTS,
					BlockHash:      blockHash,
				}
				blockHasEvents := false

				for _, lg := range block.Logs {
					if len(lg.Topics) == 0 {
						continue
					}
					d0 := time.Now()
					topic0 := common.HexToHash(lg.Topics[0])
					def, ok := decoders[topic0]
					if !ok {
						decodeDur += time.Since(d0)
						continue
					}
					if !def.MatchesAddress(lg.Address) {
						decodeDur += time.Since(d0)
						continue
					}
					ev, err := def.Decode(lg.Address, lg.Topics, common.FromHex(lg.Data))
					decodeDur += time.Since(d0)
					if err != nil {
						log.Printf("decode %s log in block %d: %v", def.EventName(), block.Header.Number, err)
						continue
					}
					ev.ChainID = chain.ID
					ev.BlockNumber = block.Header.Number
					ev.BlockTimestamp = blockTS
					ev.BlockHash = blockHash
					ev.TxHash = strings.Clone(lg.TransactionHash)
					ev.TxIndex = lg.TransactionIndex
					ev.LogIndex = lg.LogIndex
					ev.Address = strings.Clone(lg.Address)
					decodedEvents = append(decodedEvents, *ev)
					blockHasEvents = true
					if table, ok := typedTables.lookup(ev.Address, ev.EventName); ok {
						typedEvents[table.Name] = append(typedEvents[table.Name], *ev)
						typedSpecs[table.Name] = table
					}
				}
				if blockHasEvents {
					blockRows = append(blockRows, blockRow)
					eventBlockCount++
				}
				return nil
			})
			profParse += time.Since(parseStart)
			profDecode += decodeDur
			profMarshal += marshalDur
			profIters++

			if err != nil {
				log.Printf("Chain %d: parse %s error: %v", chain.ID, rangeLabel, err)
				time.Sleep(5 * time.Second)
				continue
			}

			if len(decodedEvents) > 0 {
				insertStart := time.Now()
				if err := inserter.InsertLogs(ctx, decodedEvents); err != nil {
					profInsert += time.Since(insertStart)
					log.Printf("Chain %d: insert %s error: %v", chain.ID, rangeLabel, err)
					time.Sleep(5 * time.Second)
					continue
				}
				profInsert += time.Since(insertStart)
			}
			typedInsertFailed := false
			for tableName, events := range typedEvents {
				insertStart := time.Now()
				if err := inserter.InsertTypedLogs(ctx, typedSpecs[tableName], events); err != nil {
					profInsert += time.Since(insertStart)
					log.Printf("Chain %d: insert typed %s %s error: %v", chain.ID, tableName, rangeLabel, err)
					time.Sleep(5 * time.Second)
					typedInsertFailed = true
					break
				}
				profInsert += time.Since(insertStart)
			}
			if typedInsertFailed {
				continue
			}
			if len(blockRows) > 0 {
				insertStart := time.Now()
				if err := inserter.InsertBlocks(ctx, blockRows); err != nil {
					profInsert += time.Since(insertStart)
					log.Printf("Chain %d: insert blocks %s error: %v", chain.ID, rangeLabel, err)
					time.Sleep(5 * time.Second)
					continue
				}
				profInsert += time.Since(insertStart)
			}

			if cursorMode {
				lastProcessed = maxSeenBlock
				if effectiveEndBlock != nil && (seenBeyondEnd || lastProcessed > *effectiveEndBlock) {
					lastProcessed = *effectiveEndBlock
				}
			}
			if err := store.UpdateSyncState(ctx, chain.ID, lastProcessed); err != nil {
				return fmt.Errorf("update sync state %d: %w", lastProcessed, err)
			}
			if profIters%10 == 0 {
				if err := store.TruncateSyncState(ctx, chain.ID, lastProcessed); err != nil {
					log.Printf("Chain %d: truncate sync state error: %v", chain.ID, err)
				}
			}

			totalEvents += uint64(len(decodedEvents))
		}


		scanned := uint64(0)
		if lastProcessed >= requestStartBlock {
			scanned = lastProcessed - requestStartBlock + 1
		}
		totalBlocks += scanned
		elapsed := time.Since(startTime)
		rate := float64(totalBlocks) / elapsed.Seconds()
		if len(raw) > 0 {
			log.Printf("Chain %d: %s scanned %d blocks, response headers: %d, event blocks: %d, events: %d | checkpoint: %d | total: %d blocks, %d events | %.1f blk/s",
				chain.ID, rangeLabel, scanned, responseBlockCount, eventBlockCount, len(decodedEvents), lastProcessed, totalBlocks, totalEvents, rate)
		} else {
			log.Printf("Chain %d: %s scanned %d blocks, empty response | checkpoint: %d | total: %d blocks, %d events | %.1f blk/s",
				chain.ID, rangeLabel, scanned, lastProcessed, totalBlocks, totalEvents, rate)
		}

		currentBlock = lastProcessed + 1

		if effectiveEndBlock != nil && currentBlock > *effectiveEndBlock {
			break
		}
	}


	printProfile(profFetch, profParse, profDecode, profMarshal, profInsert, profIters, totalBlocks, totalEvents, startTime)
	return nil
}

func printProfile(fetch, parse, decode, marshal, insert time.Duration, iters int, totalBlocks, totalEvents uint64, startTime time.Time) {
	if iters == 0 {
		return
	}
	total := fetch + parse + decode + marshal + insert
	elapsed := time.Since(startTime)
	log.Println("")
	log.Println("═══ PROFILE ═══")
	log.Printf("  FETCH:  %v (%.0f%%)", fetch, pct(fetch, total))
	log.Printf("  PARSE:  %v (%.0f%%), %d iterations, avg %v/iter", parse, pct(parse, total), iters, parse/time.Duration(iters))
	log.Printf("  DECODE: %v (%.0f%%)", decode, pct(decode, total))
	log.Printf("  MARSHAL:%v (%.0f%%)", marshal, pct(marshal, total))
	log.Printf("  INSERT: %v (%.0f%%)", insert, pct(insert, total))
	log.Printf("  ─────────────────")
	log.Printf("  TOTAL:  %v (wall %v, %.0f%% accounted)", total, elapsed, pct(total, elapsed))
	log.Printf("  Throughput: %d blocks, %d events, avg %.0f µs/event", totalBlocks, totalEvents, float64(total.Microseconds())/float64(max(totalEvents, 1)))
	log.Println("════════════════")
}

func pct(part, total time.Duration) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func nextRequestRange(currentBlock, pageSize uint64, effectiveEndBlock *uint64, cursorMode bool) (*uint64, string, bool) {
	if cursorMode {
		return nil, fmt.Sprintf("[%d-tail]", currentBlock), true
	}
	toBlock := currentBlock + pageSize - 1
	if effectiveEndBlock != nil && toBlock > *effectiveEndBlock {
		toBlock = *effectiveEndBlock
	}
	if toBlock < currentBlock {
		return nil, "", false
	}
	return &toBlock, fmt.Sprintf("[%d-%d]", currentBlock, toBlock), true
}

func minEndBlock(current *uint64, candidate uint64) *uint64 {
	if current != nil && *current < candidate {
		return current
	}
	return &candidate
}

func chainEndpoint(chainID uint64) string {
	switch chainID {
	case 1:
		return "https://portal.sqd.dev/datasets/ethereum-mainnet/finalized-stream"
	case 137:
		return "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream"
	default:
		return "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream"
	}
}

type typedTableIndex struct {
	byAddressEvent map[string]database.TypedEventTable
	byEvent        map[string]database.TypedEventTable
}

func (i typedTableIndex) lookup(address, eventName string) (database.TypedEventTable, bool) {
	if table, ok := i.byAddressEvent[strings.ToLower(address)+"|"+eventName]; ok {
		return table, true
	}
	table, ok := i.byEvent[eventName]
	return table, ok
}

func buildTypedTableIndex(chain *config.Chain) (typedTableIndex, error) {
	index := typedTableIndex{
		byAddressEvent: make(map[string]database.TypedEventTable),
		byEvent:        make(map[string]database.TypedEventTable),
	}
	used := make(map[string]int)
	for _, contract := range chain.Contracts {
		addresses := configAddresses(contract.Address)
		for _, event := range contract.Events {
			name, args, err := parseEventArgs(event.Event)
			if err != nil {
				return index, fmt.Errorf("%s.%s: %w", contract.Name, event.Event, err)
			}
			viewName := uniqueLower(used, toSnake(contract.Name+"_"+name))
			table := database.TypedEventTable{
				Name: viewName + "_events",
				Args: args,
			}
			if len(addresses) == 0 {
				index.byEvent[name] = table
				continue
			}
			for _, address := range addresses {
				index.byAddressEvent[strings.ToLower(address)+"|"+name] = table
			}
		}
	}
	return index, nil
}

func parseEventArgs(sig string) (string, []database.TypedEventArg, error) {
	sig = strings.TrimSpace(strings.TrimPrefix(sig, "event "))
	open := strings.IndexByte(sig, '(')
	close := strings.LastIndexByte(sig, ')')
	if open <= 0 || close <= open {
		return "", nil, fmt.Errorf("invalid event signature")
	}
	name := strings.TrimSpace(sig[:open])
	inputs := splitEventArgs(sig[open+1 : close])
	parsed, err := abi.JSON(strings.NewReader(eventABIJSON(name, inputs)))
	if err != nil {
		return "", nil, err
	}
	ev, ok := parsed.Events[name]
	if !ok {
		return "", nil, fmt.Errorf("event not found after parsing")
	}
	args := make([]database.TypedEventArg, 0, len(ev.Inputs))
	for idx, input := range ev.Inputs {
		argName := input.Name
		if strings.TrimSpace(argName) == "" {
			argName = fmt.Sprintf("p%d", idx)
		}
		solType := input.Type.String()
		args = append(args, database.TypedEventArg{
			Name:           argName,
			ColumnName:     argName,
			SolidityType:   solType,
			ClickHouseType: clickHouseType(solType),
		})
	}
	return name, args, nil
}

func eventABIJSON(name string, args []string) string {
	inputs := make([]string, 0, len(args))
	for i, arg := range args {
		parts := strings.Fields(strings.TrimSpace(arg))
		if len(parts) == 0 {
			continue
		}
		typ := normalizeSolidityType(parts[0])
		indexed := false
		paramName := fmt.Sprintf("p%d", i)
		for _, part := range parts[1:] {
			if part == "indexed" {
				indexed = true
				continue
			}
			paramName = part
		}
		inputs = append(inputs, fmt.Sprintf(`{"indexed":%t,"name":%q,"type":%q}`, indexed, paramName, typ))
	}
	return fmt.Sprintf(`[{"anonymous":false,"inputs":[%s],"name":%q,"type":"event"}]`, strings.Join(inputs, ","), name)
}

func splitEventArgs(args string) []string {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}
	var out []string
	start := 0
	depth := 0
	for i, r := range args {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(args[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(args[start:]))
	return out
}

func configAddresses(addr any) []string {
	switch v := addr.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func uniqueLower(used map[string]int, base string) string {
	if base == "" {
		base = "event"
	}
	count := used[base]
	used[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}

func toSnake(s string) string {
	parts := identifierParts(s)
	if len(parts) == 0 {
		return "event"
	}
	for i := range parts {
		parts[i] = strings.ToLower(parts[i])
	}
	return strings.Join(parts, "_")
}

func identifierParts(s string) []string {
	var parts []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			parts = append(parts, b.String())
			b.Reset()
		}
	}
	var prevLower bool
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if unicode.IsUpper(r) && prevLower {
				flush()
			}
			b.WriteRune(r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
			continue
		}
		flush()
		prevLower = false
	}
	flush()
	return parts
}

func normalizeSolidityType(typ string) string {
	switch typ {
	case "uint":
		return "uint256"
	case "int":
		return "int256"
	default:
		return typ
	}
}

func clickHouseType(solType string) string {
	if strings.Contains(solType, "[") {
		return "String"
	}
	switch {
	case solType == "bool":
		return "UInt8"
	case solType == "address":
		return "FixedString(20)"
	case solType == "bytes32":
		return "FixedString(32)"
	case isBytesN(solType):
		return fmt.Sprintf("FixedString(%d)", bytesNSize(solType))
	case solType == "bytes", solType == "string":
		return "String"
	case strings.HasPrefix(solType, "uint"):
		return "UInt256"
	case strings.HasPrefix(solType, "int"):
		return "Int256"
	default:
		return "String"
	}
}

func isBytesN(solType string) bool {
	if !strings.HasPrefix(solType, "bytes") || solType == "bytes" {
		return false
	}
	n := bytesNSize(solType)
	return n > 0 && n <= 32
}

func bytesNSize(solType string) int {
	var n int
	fmt.Sscanf(strings.TrimPrefix(solType, "bytes"), "%d", &n)
	return n
}
