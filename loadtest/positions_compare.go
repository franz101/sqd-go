package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/shopspring/decimal"
)

type PositionCompareConfig struct {
	Host      string
	Port      int
	User      string
	Password  string
	Database  string
	Positions int
	Events    int
	Engine    string
	Insert    string
	ChunkSize int
}

type positionStepMetric struct {
	Duration time.Duration
	Bytes    uint64
	Mallocs  uint64
}

type positionResult struct {
	Engine   string
	Insert   string
	Update   positionStepMetric
	Ingest   positionStepMetric
	Inserted int
}

func RunPositionCompare(ctx context.Context, cfg PositionCompareConfig) error {
	if cfg.Positions <= 0 {
		return fmt.Errorf("positions must be > 0")
	}
	if cfg.Events <= 0 {
		return fmt.Errorf("events must be > 0")
	}
	if cfg.ChunkSize <= 0 {
		return fmt.Errorf("chunk-size must be > 0")
	}

	engines, err := expandPositionOption(cfg.Engine, "engine", []string{"proto", "shopspring"})
	if err != nil {
		return err
	}
	insertModes, err := expandPositionOption(cfg.Insert, "insert", []string{"batch", "stream"})
	if err != nil {
		return err
	}

	log.Printf("Position compare: positions=%d events=%d engine=%s insert=%s chunk=%d db=%s",
		cfg.Positions, cfg.Events, cfg.Engine, cfg.Insert, cfg.ChunkSize, cfg.Database)

	var conn *ch.Client
	if len(insertModes) > 0 {
		conn, err = ch.Dial(ctx, ch.Options{
			Address:  fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
			Database: "default",
			User:     cfg.User,
			Password: cfg.Password,
		})
		if err != nil {
			return fmt.Errorf("connect clickhouse: %w", err)
		}
		defer conn.Close()
		if err := ensurePositionCompareTable(ctx, conn, cfg.Database); err != nil {
			return err
		}
	}

	results := make([]positionResult, 0, len(engines)*maxInt(1, len(insertModes)))
	for _, engine := range engines {
		switch engine {
		case "proto":
			store := newPositionProtoStore(cfg.Positions)
			events := newPositionProtoEvents(cfg.Events, cfg.Positions)
			update, err := measurePositionStep(func() error {
				store.applyEvents(events, protomath.Decimal256Scale18)
				return nil
			})
			if err != nil {
				return err
			}
			if len(insertModes) == 0 {
				results = append(results, positionResult{Engine: engine, Insert: "none", Update: update})
				continue
			}
			for _, insertMode := range insertModes {
				rawCols := newPositionInsertRawColumns(cfg.ChunkSize, protomath.Decimal256Scale18)
				inserted := 0
				ingest, err := measurePositionStep(func() error {
					var err error
					inserted, err = insertProtoPositions(ctx, conn, cfg.Database, store, rawCols, insertMode, cfg.ChunkSize)
					return err
				})
				if err != nil {
					return err
				}
				results = append(results, positionResult{Engine: engine, Insert: insertMode, Update: update, Ingest: ingest, Inserted: inserted})
			}
		case "shopspring":
			store := newPositionShopStore(cfg.Positions)
			events := newPositionShopEvents(cfg.Events, cfg.Positions)
			update, err := measurePositionStep(func() error {
				store.applyEvents(events)
				return nil
			})
			if err != nil {
				return err
			}
			if len(insertModes) == 0 {
				results = append(results, positionResult{Engine: engine, Insert: "none", Update: update})
				continue
			}
			for _, insertMode := range insertModes {
				rawCols := newPositionInsertRawColumns(cfg.ChunkSize, protomath.Decimal256Scale18)
				inserted := 0
				ingest, err := measurePositionStep(func() error {
					var err error
					inserted, err = insertShopPositions(ctx, conn, cfg.Database, store, rawCols, insertMode, cfg.ChunkSize)
					return err
				})
				if err != nil {
					return err
				}
				results = append(results, positionResult{Engine: engine, Insert: insertMode, Update: update, Ingest: ingest, Inserted: inserted})
			}
		}
	}

	reportPositionResults(results)
	return nil
}

func expandPositionOption(raw, name string, values []string) ([]string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "none":
		if name == "insert" {
			return nil, nil
		}
	case "both":
		out := make([]string, len(values))
		copy(out, values)
		return out, nil
	default:
		for _, value := range values {
			if raw == value {
				return []string{raw}, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid %s %q", name, raw)
}

func measurePositionStep(fn func() error) (positionStepMetric, error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := fn()
	duration := time.Since(start)
	runtime.ReadMemStats(&after)
	return positionStepMetric{
		Duration: duration,
		Bytes:    after.TotalAlloc - before.TotalAlloc,
		Mallocs:  after.Mallocs - before.Mallocs,
	}, err
}

func reportPositionResults(results []positionResult) {
	for _, result := range results {
		total := result.Update.Duration + result.Ingest.Duration
		totalBytes := result.Update.Bytes + result.Ingest.Bytes
		totalMallocs := result.Update.Mallocs + result.Ingest.Mallocs
		log.Printf("[POSITIONS] engine=%s insert=%s inserted=%d task1_update=%s alloc=%s mallocs=%d task2_ingest=%s alloc=%s mallocs=%d e2e=%s alloc=%s mallocs=%d",
			result.Engine,
			result.Insert,
			result.Inserted,
			result.Update.Duration.Round(time.Microsecond),
			humanBytes(result.Update.Bytes),
			result.Update.Mallocs,
			result.Ingest.Duration.Round(time.Microsecond),
			humanBytes(result.Ingest.Bytes),
			result.Ingest.Mallocs,
			total.Round(time.Microsecond),
			humanBytes(totalBytes),
			totalMallocs,
		)
	}

	for _, protoResult := range results {
		if protoResult.Engine != "proto" {
			continue
		}
		for _, shopResult := range results {
			if shopResult.Engine != "shopspring" || shopResult.Insert != protoResult.Insert {
				continue
			}
			protoTotal := protoResult.Update.Duration + protoResult.Ingest.Duration
			shopTotal := shopResult.Update.Duration + shopResult.Ingest.Duration
			if protoTotal > 0 {
				log.Printf("[POSITIONS] improvement insert=%s speedup=%.2fx allocation_reduction=%.2fx malloc_reduction=%.2fx",
					protoResult.Insert,
					float64(shopTotal)/float64(protoTotal),
					ratioUint64(shopResult.Update.Bytes+shopResult.Ingest.Bytes, protoResult.Update.Bytes+protoResult.Ingest.Bytes),
					ratioUint64(shopResult.Update.Mallocs+shopResult.Ingest.Mallocs, protoResult.Update.Mallocs+protoResult.Ingest.Mallocs),
				)
			}
		}
	}
}

type positionProtoStore struct {
	index       map[uint64]int
	entityID    []uint64
	amount      []protomath.Decimal256
	totalBought []protomath.Decimal256
	avgPrice    []protomath.Decimal256
}

type positionProtoEvents struct {
	entityID []uint64
	delta    []protomath.Decimal256
	price    []protomath.Decimal256
}

func newPositionProtoStore(rows int) *positionProtoStore {
	scale := protomath.Decimal256Scale18
	store := &positionProtoStore{
		index:       make(map[uint64]int, rows),
		entityID:    make([]uint64, rows),
		amount:      make([]protomath.Decimal256, rows),
		totalBought: make([]protomath.Decimal256, rows),
		avgPrice:    make([]protomath.Decimal256, rows),
	}
	for i := 0; i < rows; i++ {
		id := uint64(i + 1)
		store.index[id] = i
		store.entityID[i] = id
		amount, _ := protomath.FromInt64(int64(10+i%90), scale)
		price := protomath.FromScaledInt64(350_000_000_000_000_000 + int64(i%200)*1_000_000_000_000_000)
		total, _ := amount.Mul(price, scale)
		store.amount[i] = amount
		store.avgPrice[i] = price
		store.totalBought[i] = total
	}
	return store
}

func newPositionProtoEvents(rows, positions int) positionProtoEvents {
	events := positionProtoEvents{
		entityID: make([]uint64, rows),
		delta:    make([]protomath.Decimal256, rows),
		price:    make([]protomath.Decimal256, rows),
	}
	for i := 0; i < rows; i++ {
		events.entityID[i] = uint64(i%positions + 1)
		events.delta[i] = protomath.FromScaledInt64(100_000_000_000_000_000 + int64(i%7)*10_000_000_000_000_000)
		events.price[i] = protomath.FromScaledInt64(420_000_000_000_000_000 + int64(i%300)*1_000_000_000_000_000)
	}
	return events
}

func (s *positionProtoStore) applyEvents(events positionProtoEvents, scale protomath.Decimal256Scale) {
	for i, entityID := range events.entityID {
		idx := s.index[entityID]
		delta := events.delta[i]
		price := events.price[i]
		amount, _ := s.amount[idx].Add(delta)
		bought, _ := delta.Mul(price, scale)
		total, _ := s.totalBought[idx].Add(bought)
		avg, _ := total.Div(amount, scale)
		s.amount[idx] = amount
		s.totalBought[idx] = total
		s.avgPrice[idx] = avg
	}
}

type positionShopStore struct {
	index       map[uint64]int
	entityID    []uint64
	amount      []decimal.Decimal
	totalBought []decimal.Decimal
	avgPrice    []decimal.Decimal
}

type positionShopEvents struct {
	entityID []uint64
	delta    []decimal.Decimal
	price    []decimal.Decimal
}

func newPositionShopStore(rows int) *positionShopStore {
	store := &positionShopStore{
		index:       make(map[uint64]int, rows),
		entityID:    make([]uint64, rows),
		amount:      make([]decimal.Decimal, rows),
		totalBought: make([]decimal.Decimal, rows),
		avgPrice:    make([]decimal.Decimal, rows),
	}
	for i := 0; i < rows; i++ {
		id := uint64(i + 1)
		store.index[id] = i
		store.entityID[i] = id
		amount := decimal.NewFromInt(int64(10 + i%90))
		price := decimal.New(350_000_000_000_000_000+int64(i%200)*1_000_000_000_000_000, -18)
		store.amount[i] = amount
		store.avgPrice[i] = price
		store.totalBought[i] = amount.Mul(price)
	}
	return store
}

func newPositionShopEvents(rows, positions int) positionShopEvents {
	events := positionShopEvents{
		entityID: make([]uint64, rows),
		delta:    make([]decimal.Decimal, rows),
		price:    make([]decimal.Decimal, rows),
	}
	for i := 0; i < rows; i++ {
		events.entityID[i] = uint64(i%positions + 1)
		events.delta[i] = decimal.New(100_000_000_000_000_000+int64(i%7)*10_000_000_000_000_000, -18)
		events.price[i] = decimal.New(420_000_000_000_000_000+int64(i%300)*1_000_000_000_000_000, -18)
	}
	return events
}

func (s *positionShopStore) applyEvents(events positionShopEvents) {
	for i, entityID := range events.entityID {
		idx := s.index[entityID]
		delta := events.delta[i]
		price := events.price[i]
		amount := s.amount[idx].Add(delta)
		total := s.totalBought[idx].Add(delta.Mul(price))
		avg, _ := total.QuoRem(amount, int32(protomath.Decimal256Scale18.Scale()))
		s.amount[idx] = amount
		s.totalBought[idx] = total
		s.avgPrice[idx] = avg
	}
}

type positionInsertRawColumns struct {
	scale          protomath.Decimal256Scale
	multiplier     decimal.Decimal
	colEntityID    proto.ColUInt64
	colAmount      loadtestRawColumn
	colTotalBought loadtestRawColumn
	colAvgPrice    loadtestRawColumn
	input          proto.Input
}

type loadtestRawColumn struct {
	proto.ColRaw
}

func (c *loadtestRawColumn) WriteColumn(w *proto.Writer) {
	w.ChainWrite(c.Data)
}

func newPositionInsertRawColumns(capacity int, scale protomath.Decimal256Scale) *positionInsertRawColumns {
	decimalType := proto.ColumnTypeDecimal256.With(fmt.Sprint(scale.Scale()))
	cols := &positionInsertRawColumns{
		scale:       scale,
		multiplier:  decimal.New(1, scale.Scale()),
		colEntityID: make(proto.ColUInt64, 0, capacity),
		colAmount: loadtestRawColumn{ColRaw: proto.ColRaw{
			T:    decimalType,
			Size: 32,
			Data: make([]byte, 0, capacity*32),
		}},
		colTotalBought: loadtestRawColumn{ColRaw: proto.ColRaw{
			T:    decimalType,
			Size: 32,
			Data: make([]byte, 0, capacity*32),
		}},
		colAvgPrice: loadtestRawColumn{ColRaw: proto.ColRaw{
			T:    decimalType,
			Size: 32,
			Data: make([]byte, 0, capacity*32),
		}},
	}
	cols.input = proto.Input{
		{Name: "entity_id", Data: &cols.colEntityID},
		{Name: "amount", Data: &cols.colAmount},
		{Name: "total_bought", Data: &cols.colTotalBought},
		{Name: "avg_price", Data: &cols.colAvgPrice},
	}
	return cols
}

func (c *positionInsertRawColumns) reset() {
	c.colEntityID.Reset()
	c.colAmount.Reset()
	c.colTotalBought.Reset()
	c.colAvgPrice.Reset()
}

func (c *positionInsertRawColumns) appendDecimal(col *loadtestRawColumn, value protomath.Decimal256) {
	var raw [32]byte
	value.PutLittleEndianBytes(&raw)
	col.Data = append(col.Data, raw[:]...)
	col.Count++
}

func (c *positionInsertRawColumns) fillProto(store *positionProtoStore, start, rows int) proto.Input {
	c.reset()
	end := minInt(start+rows, len(store.entityID))
	for i := start; i < end; i++ {
		c.colEntityID.Append(store.entityID[i])
		c.appendDecimal(&c.colAmount, store.amount[i])
		c.appendDecimal(&c.colTotalBought, store.totalBought[i])
		c.appendDecimal(&c.colAvgPrice, store.avgPrice[i])
	}
	return c.input
}

func (c *positionInsertRawColumns) fillShop(store *positionShopStore, start, rows int) proto.Input {
	c.reset()
	end := minInt(start+rows, len(store.entityID))
	for i := start; i < end; i++ {
		c.colEntityID.Append(store.entityID[i])
		c.appendDecimal(&c.colAmount, shopDecimalToProtoDecimal256(store.amount[i], c.multiplier))
		c.appendDecimal(&c.colTotalBought, shopDecimalToProtoDecimal256(store.totalBought[i], c.multiplier))
		c.appendDecimal(&c.colAvgPrice, shopDecimalToProtoDecimal256(store.avgPrice[i], c.multiplier))
	}
	return c.input
}

func insertProtoPositions(ctx context.Context, conn *ch.Client, db string, store *positionProtoStore, cols *positionInsertRawColumns, mode string, chunkSize int) (int, error) {
	if err := truncatePositionCompareTable(ctx, conn, db); err != nil {
		return 0, err
	}
	switch mode {
	case "batch":
		inserted := 0
		for start := 0; start < len(store.entityID); start += chunkSize {
			input := cols.fillProto(store, start, chunkSize)
			if err := conn.Do(ctx, ch.Query{Body: positionInsertQuery(db), Input: input}); err != nil {
				return inserted, err
			}
			inserted += input[0].Data.Rows()
		}
		return inserted, nil
	case "stream":
		processed := 0
		err := conn.Do(ctx, ch.Query{
			Body:  positionInsertQuery(db),
			Input: cols.input,
			OnInput: func(ctx context.Context) error {
				if processed >= len(store.entityID) {
					cols.reset()
					return io.EOF
				}
				cols.fillProto(store, processed, chunkSize)
				processed += cols.colEntityID.Rows()
				return nil
			},
		})
		return processed, err
	default:
		return 0, fmt.Errorf("invalid insert mode %q", mode)
	}
}

func insertShopPositions(ctx context.Context, conn *ch.Client, db string, store *positionShopStore, cols *positionInsertRawColumns, mode string, chunkSize int) (int, error) {
	if err := truncatePositionCompareTable(ctx, conn, db); err != nil {
		return 0, err
	}
	switch mode {
	case "batch":
		inserted := 0
		for start := 0; start < len(store.entityID); start += chunkSize {
			input := cols.fillShop(store, start, chunkSize)
			if err := conn.Do(ctx, ch.Query{Body: positionInsertQuery(db), Input: input}); err != nil {
				return inserted, err
			}
			inserted += input[0].Data.Rows()
		}
		return inserted, nil
	case "stream":
		processed := 0
		err := conn.Do(ctx, ch.Query{
			Body:  positionInsertQuery(db),
			Input: cols.input,
			OnInput: func(ctx context.Context) error {
				if processed >= len(store.entityID) {
					cols.reset()
					return io.EOF
				}
				cols.fillShop(store, processed, chunkSize)
				processed += cols.colEntityID.Rows()
				return nil
			},
		})
		return processed, err
	default:
		return 0, fmt.Errorf("invalid insert mode %q", mode)
	}
}

func shopDecimalToProtoDecimal256(value decimal.Decimal, multiplier decimal.Decimal) protomath.Decimal256 {
	coefficient := value.Mul(multiplier).BigInt()
	out, ok := protomath.FromDecimal256ScaledBigInt(coefficient)
	if !ok {
		panic("decimal value does not fit Decimal256")
	}
	return out
}

func ensurePositionCompareTable(ctx context.Context, conn *ch.Client, db string) error {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", db)}); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	query := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.loadtest_position_compare (
  entity_id UInt64,
  amount Decimal256(18),
  total_bought Decimal256(18),
  avg_price Decimal256(18)
) ENGINE = Memory`, db)
	if err := conn.Do(ctx, ch.Query{Body: query}); err != nil {
		return fmt.Errorf("create position compare table: %w", err)
	}
	return nil
}

func truncatePositionCompareTable(ctx context.Context, conn *ch.Client, db string) error {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("TRUNCATE TABLE %s.loadtest_position_compare", db)}); err != nil {
		return fmt.Errorf("truncate position compare table: %w", err)
	}
	return nil
}

func positionInsertQuery(db string) string {
	return fmt.Sprintf("INSERT INTO %s.loadtest_position_compare (entity_id, amount, total_bought, avg_price) VALUES", db)
}

func humanBytes(v uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case v >= gib:
		return fmt.Sprintf("%.2fGB", float64(v)/gib)
	case v >= mib:
		return fmt.Sprintf("%.2fMB", float64(v)/mib)
	case v >= kib:
		return fmt.Sprintf("%.2fKB", float64(v)/kib)
	default:
		return fmt.Sprintf("%dB", v)
	}
}

func ratioUint64(a, b uint64) float64 {
	if b == 0 {
		if a == 0 {
			return 1
		}
		return float64(a)
	}
	return float64(a) / float64(b)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
