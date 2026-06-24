package protomath

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/shopspring/decimal"
)

var (
	positionsProtoInputSink proto.Input
	positionsRowsSink       int
)

type protoPositionStore struct {
	index       map[uint64]int
	entityID    []uint64
	amount      []Decimal256
	totalBought []Decimal256
	avgPrice    []Decimal256

	colEntityID    proto.ColUInt64
	colAmount      proto.ColDecimal256
	colTotalBought proto.ColDecimal256
	colAvgPrice    proto.ColDecimal256
}

type protoOrderEvents struct {
	entityID []uint64
	delta    []Decimal256
	price    []Decimal256
}

type shopPositionStore struct {
	index       map[uint64]int
	entityID    []uint64
	amount      []decimal.Decimal
	totalBought []decimal.Decimal
	avgPrice    []decimal.Decimal

	colEntityID    proto.ColUInt64
	colAmount      proto.ColDecimal256
	colTotalBought proto.ColDecimal256
	colAvgPrice    proto.ColDecimal256
}

type shopOrderEvents struct {
	entityID []uint64
	delta    []decimal.Decimal
	price    []decimal.Decimal
}

func BenchmarkPositionsTask1_Update_ProtoMath_MapSlices(b *testing.B) {
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	events := benchRows("PROTO_MATH_EVENTS", 200_000)
	store := newProtoPositionStore(positions)
	orderEvents := newProtoOrderEvents(events, positions)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		store.applyEventsMapSlices(orderEvents, Decimal256Scale18)
	}
}

func BenchmarkPositionsTask1_Update_ProtoMath_MapId(b *testing.B) {
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	events := benchRows("PROTO_MATH_EVENTS", 200_000)
	store := newProtoPositionStore(positions)
	orderEvents := newProtoOrderEvents(events, positions)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range orderEvents.entityID {
			store.applyEventMapId(orderEvents.entityID[i], orderEvents.delta[i], orderEvents.price[i], Decimal256Scale18)
		}
	}
}

func BenchmarkPositionsTask1_Update_ShopDecimal_MapSlices(b *testing.B) {
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	events := benchRows("PROTO_MATH_EVENTS", 200_000)
	store := newShopPositionStore(positions)
	orderEvents := newShopOrderEvents(events, positions)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		store.applyEventsMapSlices(orderEvents)
	}
}

func BenchmarkPositionsTask2_BuildInsert_ProtoMath(b *testing.B) {
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	store := newProtoPositionStore(positions)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		positionsProtoInputSink = store.buildInsertInput()
	}
}

func BenchmarkPositionsTask2_BuildInsert_ShopDecimal(b *testing.B) {
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	store := newShopPositionStore(positions)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		positionsProtoInputSink = store.buildInsertInput(Decimal256Scale18)
	}
}

func BenchmarkPositionsE2E_ProtoMath_MapSlices(b *testing.B) {
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	events := benchRows("PROTO_MATH_EVENTS", 200_000)
	store := newProtoPositionStore(positions)
	orderEvents := newProtoOrderEvents(events, positions)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		store.applyEventsMapSlices(orderEvents, Decimal256Scale18)
		positionsProtoInputSink = store.buildInsertInput()
	}
}

func BenchmarkPositionsE2E_ShopDecimal_MapSlices(b *testing.B) {
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	events := benchRows("PROTO_MATH_EVENTS", 200_000)
	store := newShopPositionStore(positions)
	orderEvents := newShopOrderEvents(events, positions)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		store.applyEventsMapSlices(orderEvents)
		positionsProtoInputSink = store.buildInsertInput(Decimal256Scale18)
	}
}

func BenchmarkPositionsE2E_ProtoMath_ClickHouseInsert(b *testing.B) {
	if os.Getenv("PROTO_MATH_CLICKHOUSE_BENCH") != "1" {
		b.Skip("set PROTO_MATH_CLICKHOUSE_BENCH=1 to run ClickHouse insert benchmark")
	}
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	events := benchRows("PROTO_MATH_EVENTS", 200_000)
	store := newProtoPositionStore(positions)
	orderEvents := newProtoOrderEvents(events, positions)
	ctx := context.Background()
	conn, db := setupProtoMathClickHouseBench(b, ctx)
	defer conn.Close()
	defer dropProtoMathClickHouseBenchDB(ctx, conn, db)
	createPositionsInsertTable(b, ctx, conn, db)
	rawCols := newPositionsRawInsertColumns(positions, Decimal256Scale18)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		store.applyEventsMapSlices(orderEvents, Decimal256Scale18)
		if err := conn.Do(ctx, ch.Query{
			Body:  positionsInsertQuery(db),
			Input: rawCols.fillProto(store),
		}); err != nil {
			b.Fatalf("insert proto positions: %v", err)
		}
	}
}

func BenchmarkPositionsE2E_ShopDecimal_ClickHouseInsert(b *testing.B) {
	if os.Getenv("PROTO_MATH_CLICKHOUSE_BENCH") != "1" {
		b.Skip("set PROTO_MATH_CLICKHOUSE_BENCH=1 to run ClickHouse insert benchmark")
	}
	positions := benchRows("PROTO_MATH_POSITIONS", 100_000)
	events := benchRows("PROTO_MATH_EVENTS", 200_000)
	store := newShopPositionStore(positions)
	orderEvents := newShopOrderEvents(events, positions)
	ctx := context.Background()
	conn, db := setupProtoMathClickHouseBench(b, ctx)
	defer conn.Close()
	defer dropProtoMathClickHouseBenchDB(ctx, conn, db)
	createPositionsInsertTable(b, ctx, conn, db)
	rawCols := newPositionsRawInsertColumns(positions, Decimal256Scale18)

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events/op")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		store.applyEventsMapSlices(orderEvents)
		if err := conn.Do(ctx, ch.Query{
			Body:  positionsInsertQuery(db),
			Input: rawCols.fillShop(store),
		}); err != nil {
			b.Fatalf("insert shopspring positions: %v", err)
		}
	}
}

func newProtoPositionStore(rows int) *protoPositionStore {
	scale := Decimal256Scale18
	store := &protoPositionStore{
		index:       make(map[uint64]int, rows),
		entityID:    make([]uint64, rows),
		amount:      make([]Decimal256, rows),
		totalBought: make([]Decimal256, rows),
		avgPrice:    make([]Decimal256, rows),
		colEntityID: make(proto.ColUInt64, 0, rows),
		colAmount:   make(proto.ColDecimal256, 0, rows),
	}
	store.colTotalBought = make(proto.ColDecimal256, 0, rows)
	store.colAvgPrice = make(proto.ColDecimal256, 0, rows)

	for i := 0; i < rows; i++ {
		id := uint64(i + 1)
		store.index[id] = i
		store.entityID[i] = id
		amount, _ := FromInt64(int64(10+i%90), scale)
		price := FromScaledInt64(350_000_000_000_000_000 + int64(i%200)*1_000_000_000_000_000)
		total, _ := amount.Mul(price, scale)
		store.amount[i] = amount
		store.avgPrice[i] = price
		store.totalBought[i] = total
	}
	return store
}

func newProtoOrderEvents(rows, positions int) protoOrderEvents {
	events := protoOrderEvents{
		entityID: make([]uint64, rows),
		delta:    make([]Decimal256, rows),
		price:    make([]Decimal256, rows),
	}
	for i := 0; i < rows; i++ {
		events.entityID[i] = uint64(i%positions + 1)
		events.delta[i] = FromScaledInt64(100_000_000_000_000_000 + int64(i%7)*10_000_000_000_000_000)
		events.price[i] = FromScaledInt64(420_000_000_000_000_000 + int64(i%300)*1_000_000_000_000_000)
	}
	return events
}

func (s *protoPositionStore) applyEventsMapSlices(events protoOrderEvents, scale Decimal256Scale) {
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

func (s *protoPositionStore) applyEventMapId(entityID uint64, delta, price Decimal256, scale Decimal256Scale) {
	idx := s.index[entityID]
	amount, _ := s.amount[idx].Add(delta)
	bought, _ := delta.Mul(price, scale)
	total, _ := s.totalBought[idx].Add(bought)
	avg, _ := total.Div(amount, scale)
	s.amount[idx] = amount
	s.totalBought[idx] = total
	s.avgPrice[idx] = avg
}

func (s *protoPositionStore) buildInsertInput() proto.Input {
	s.colEntityID.Reset()
	s.colAmount.Reset()
	s.colTotalBought.Reset()
	s.colAvgPrice.Reset()
	for i, entityID := range s.entityID {
		s.colEntityID.Append(entityID)
		s.colAmount.Append(s.amount[i].Proto())
		s.colTotalBought.Append(s.totalBought[i].Proto())
		s.colAvgPrice.Append(s.avgPrice[i].Proto())
	}
	positionsRowsSink = s.colEntityID.Rows()
	return proto.Input{
		{Name: "entity_id", Data: &s.colEntityID},
		{Name: "amount", Data: &s.colAmount},
		{Name: "total_bought", Data: &s.colTotalBought},
		{Name: "avg_price", Data: &s.colAvgPrice},
	}
}

func newShopPositionStore(rows int) *shopPositionStore {
	store := &shopPositionStore{
		index:       make(map[uint64]int, rows),
		entityID:    make([]uint64, rows),
		amount:      make([]decimal.Decimal, rows),
		totalBought: make([]decimal.Decimal, rows),
		avgPrice:    make([]decimal.Decimal, rows),
		colEntityID: make(proto.ColUInt64, 0, rows),
		colAmount:   make(proto.ColDecimal256, 0, rows),
	}
	store.colTotalBought = make(proto.ColDecimal256, 0, rows)
	store.colAvgPrice = make(proto.ColDecimal256, 0, rows)

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

func newShopOrderEvents(rows, positions int) shopOrderEvents {
	events := shopOrderEvents{
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

func (s *shopPositionStore) applyEventsMapSlices(events shopOrderEvents) {
	for i, entityID := range events.entityID {
		idx := s.index[entityID]
		delta := events.delta[i]
		price := events.price[i]
		amount := s.amount[idx].Add(delta)
		total := s.totalBought[idx].Add(delta.Mul(price))
		avg, _ := total.QuoRem(amount, int32(Decimal256Scale18.Scale()))
		s.amount[idx] = amount
		s.totalBought[idx] = total
		s.avgPrice[idx] = avg
	}
}

func (s *shopPositionStore) buildInsertInput(scale Decimal256Scale) proto.Input {
	s.colEntityID.Reset()
	s.colAmount.Reset()
	s.colTotalBought.Reset()
	s.colAvgPrice.Reset()
	multiplier := decimal.New(1, scale.Scale())
	for i, entityID := range s.entityID {
		s.colEntityID.Append(entityID)
		s.colAmount.Append(shopDecimalToProtoDecimal256(s.amount[i], multiplier).Proto())
		s.colTotalBought.Append(shopDecimalToProtoDecimal256(s.totalBought[i], multiplier).Proto())
		s.colAvgPrice.Append(shopDecimalToProtoDecimal256(s.avgPrice[i], multiplier).Proto())
	}
	positionsRowsSink = s.colEntityID.Rows()
	return proto.Input{
		{Name: "entity_id", Data: &s.colEntityID},
		{Name: "amount", Data: &s.colAmount},
		{Name: "total_bought", Data: &s.colTotalBought},
		{Name: "avg_price", Data: &s.colAvgPrice},
	}
}

func shopDecimalToProtoDecimal256(value decimal.Decimal, multiplier decimal.Decimal) Decimal256 {
	coefficient := value.Mul(multiplier).BigInt()
	out, ok := FromDecimal256ScaledBigInt(coefficient)
	if !ok {
		panic("decimal value does not fit Decimal256")
	}
	return out
}

type positionsRawInsertColumns struct {
	scale          Decimal256Scale
	multiplier     decimal.Decimal
	colEntityID    proto.ColUInt64
	colAmount      insertRawColumn
	colTotalBought insertRawColumn
	colAvgPrice    insertRawColumn
	input          proto.Input
}

type insertRawColumn struct {
	proto.ColRaw
}

func (c *insertRawColumn) WriteColumn(w *proto.Writer) {
	w.ChainWrite(c.Data)
}

func newPositionsRawInsertColumns(rows int, scale Decimal256Scale) *positionsRawInsertColumns {
	decimalType := proto.ColumnTypeDecimal256.With(fmt.Sprint(scale.Scale()))
	cols := &positionsRawInsertColumns{
		scale:       scale,
		multiplier:  decimal.New(1, scale.Scale()),
		colEntityID: make(proto.ColUInt64, 0, rows),
		colAmount: insertRawColumn{ColRaw: proto.ColRaw{
			T:    decimalType,
			Size: 32,
			Data: make([]byte, 0, rows*32),
		}},
		colTotalBought: insertRawColumn{ColRaw: proto.ColRaw{
			T:    decimalType,
			Size: 32,
			Data: make([]byte, 0, rows*32),
		}},
		colAvgPrice: insertRawColumn{ColRaw: proto.ColRaw{
			T:    decimalType,
			Size: 32,
			Data: make([]byte, 0, rows*32),
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

func (c *positionsRawInsertColumns) fillProto(store *protoPositionStore) proto.Input {
	c.reset()
	for i, entityID := range store.entityID {
		c.colEntityID.Append(entityID)
		c.appendDecimal(&c.colAmount, store.amount[i])
		c.appendDecimal(&c.colTotalBought, store.totalBought[i])
		c.appendDecimal(&c.colAvgPrice, store.avgPrice[i])
	}
	positionsRowsSink = c.colEntityID.Rows()
	return c.input
}

func (c *positionsRawInsertColumns) fillShop(store *shopPositionStore) proto.Input {
	c.reset()
	for i, entityID := range store.entityID {
		c.colEntityID.Append(entityID)
		c.appendDecimal(&c.colAmount, shopDecimalToProtoDecimal256(store.amount[i], c.multiplier))
		c.appendDecimal(&c.colTotalBought, shopDecimalToProtoDecimal256(store.totalBought[i], c.multiplier))
		c.appendDecimal(&c.colAvgPrice, shopDecimalToProtoDecimal256(store.avgPrice[i], c.multiplier))
	}
	positionsRowsSink = c.colEntityID.Rows()
	return c.input
}

func (c *positionsRawInsertColumns) reset() {
	c.colEntityID.Reset()
	c.colAmount.Reset()
	c.colTotalBought.Reset()
	c.colAvgPrice.Reset()
}

func (c *positionsRawInsertColumns) appendDecimal(col *insertRawColumn, value Decimal256) {
	var raw [32]byte
	value.PutLittleEndianBytes(&raw)
	col.Data = append(col.Data, raw[:]...)
	col.Count++
}

func setupProtoMathClickHouseBench(tb testing.TB, ctx context.Context) (*ch.Client, string) {
	host := os.Getenv("CLICKHOUSE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := benchRows("CLICKHOUSE_NATIVE_PORT", 9003)
	user := os.Getenv("CLICKHOUSE_USER")
	if user == "" {
		user = "default"
	}
	password := os.Getenv("CLICKHOUSE_PASSWORD")
	if password == "" {
		password = "sqd-clickhouse"
	}
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", host, port),
		Database: "default",
		User:     user,
		Password: password,
	})
	if err != nil {
		tb.Fatalf("connect clickhouse: %v", err)
	}
	db := fmt.Sprintf("protomath_positions_bench_%d", os.Getpid())
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", db)}); err != nil {
		conn.Close()
		tb.Fatalf("create database: %v", err)
	}
	return conn, db
}

func createPositionsInsertTable(tb testing.TB, ctx context.Context, conn *ch.Client, db string) {
	query := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.positions_insert (
  entity_id UInt64,
  amount Decimal256(18),
  total_bought Decimal256(18),
  avg_price Decimal256(18)
) ENGINE = Memory`, db)
	if err := conn.Do(ctx, ch.Query{Body: query}); err != nil {
		tb.Fatalf("create positions table: %v", err)
	}
}

func positionsInsertQuery(db string) string {
	return fmt.Sprintf("INSERT INTO %s.positions_insert (entity_id, amount, total_bought, avg_price) VALUES", db)
}

func dropProtoMathClickHouseBenchDB(ctx context.Context, conn *ch.Client, db string) {
	_ = conn.Do(ctx, ch.Query{Body: fmt.Sprintf("DROP DATABASE IF EXISTS %s SYNC", db)})
}
