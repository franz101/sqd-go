package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/bits"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/shopspring/decimal"
)

const protoBenchScale = int64(1_000_000)

type decimalPositionBench struct {
	User           [20]byte
	TokenID        [32]byte
	Amount         decimal.Decimal
	AvgPrice       decimal.Decimal
	RealizedPnL    decimal.Decimal
	TotalBought    decimal.Decimal
	UpdatedAtBlock uint64
	BlockNumber    uint64
	TxIndex        uint64
	LogIndex       uint64
}

type protoPositionBlock struct {
	User           proto.ColFixedStr
	TokenID        proto.ColFixedStr
	Amount         proto.ColDecimal128
	AvgPrice       proto.ColDecimal128
	RealizedPnL    proto.ColDecimal128
	TotalBought    proto.ColDecimal128
	UpdatedAtBlock proto.ColUInt64
	BlockNumber    proto.ColUInt64
	TxIndex        proto.ColUInt64
	LogIndex       proto.ColUInt64
}

func TestProtoColUInt256Div1e8(t *testing.T) {
	values := []*big.Int{
		big.NewInt(0),
		big.NewInt(1),
		big.NewInt(99_999_999),
		big.NewInt(100_000_000),
		big.NewInt(123_456_789),
		new(big.Int).SetUint64(math.MaxUint64),
		new(big.Int).Lsh(big.NewInt(1), 64),
		new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(123_456_789)),
		new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(987_654_321)),
		new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 200), new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 120), big.NewInt(123_456_789_012_345))),
		new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)),
	}

	var col proto.ColUInt256
	for _, value := range values {
		col.Append(protoUInt256FromBig(value))
	}

	divisor := uint64(100_000_000)
	bigDivisor := new(big.Int).SetUint64(divisor)
	for i, raw := range col {
		gotQuotient, gotRemainder := protoUInt256DivUint64(raw, divisor)

		wantQuotient := new(big.Int)
		wantRemainder := new(big.Int)
		wantQuotient.QuoRem(values[i], bigDivisor, wantRemainder)

		if got := protoUInt256ToBig(gotQuotient); got.Cmp(wantQuotient) != 0 {
			t.Fatalf("row %d quotient mismatch\n got: %s\nwant: %s", i, got.String(), wantQuotient.String())
		}
		if gotRemainder != wantRemainder.Uint64() {
			t.Fatalf("row %d remainder mismatch: got %d want %d", i, gotRemainder, wantRemainder.Uint64())
		}
	}
}

func BenchmarkExp1_GCScan_DecimalSnapshots(b *testing.B) {
	rows := benchIntEnv("LOADTEST_BENCH_ROWS", 50_000)
	snapshots := benchIntEnv("LOADTEST_BENCH_SNAPSHOTS", 32)
	src := makeDecimalPositionsBench(rows)
	ring := make([][]decimalPositionBench, snapshots)
	for i := range ring {
		ring[i] = append(ring[i], src...)
	}

	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()

	b.ReportAllocs()
	b.ReportMetric(float64(rows*snapshots*4), "decimal_ptrs")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.GC()
	}
	runtime.KeepAlive(ring)
}

func BenchmarkExp1_GCScan_ProtoBlockSnapshots(b *testing.B) {
	rows := benchIntEnv("LOADTEST_BENCH_ROWS", 50_000)
	snapshots := benchIntEnv("LOADTEST_BENCH_SNAPSHOTS", 32)
	src := makeProtoPositionBlockBench(rows)
	ring := make([]protoPositionBlock, snapshots)
	for i := range ring {
		copyProtoPositionBlock(&ring[i], src)
	}

	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()

	b.ReportAllocs()
	b.ReportMetric(float64(snapshots*10), "slice_headers")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.GC()
	}
	runtime.KeepAlive(ring)
}

func BenchmarkExp1_Copy_DecimalSnapshot(b *testing.B) {
	rows := benchIntEnv("LOADTEST_BENCH_ROWS", 50_000)
	src := makeDecimalPositionsBench(rows)
	dst := make([]decimalPositionBench, 0, rows)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = append(dst[:0], src...)
	}
	runtime.KeepAlive(dst)
}

func BenchmarkExp1_Copy_ProtoBlockSnapshot(b *testing.B) {
	rows := benchIntEnv("LOADTEST_BENCH_ROWS", 50_000)
	src := makeProtoPositionBlockBench(rows)
	var dst protoPositionBlock
	copyProtoPositionBlock(&dst, src)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copyProtoPositionBlock(&dst, src)
	}
	runtime.KeepAlive(dst)
}

func BenchmarkExp2_PositionUpdate_DecimalStruct(b *testing.B) {
	rows := benchIntEnv("LOADTEST_BENCH_ROWS", 50_000)
	positions := makeDecimalPositionsBench(rows)
	prices, deltas := makeDecimalUpdatesBench(rows)

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range positions {
			decimalBuyUpdate(&positions[i], prices[i], deltas[i])
		}
	}
	runtime.KeepAlive(positions)
}

func BenchmarkExp2_PositionUpdate_ProtoColumns(b *testing.B) {
	rows := benchIntEnv("LOADTEST_BENCH_ROWS", 50_000)
	block := makeProtoPositionBlockBench(rows)
	prices, deltas := makeProtoUpdatesBench(rows)

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		amounts := block.Amount
		avgPrices := block.AvgPrice
		totalBought := block.TotalBought
		for i := range amounts {
			protoBuyUpdate(&amounts[i], &avgPrices[i], &totalBought[i], prices[i], deltas[i])
		}
	}
	runtime.KeepAlive(block)
}

func BenchmarkExp2_UInt256Div1e8_ProtoCol(b *testing.B) {
	rows := benchIntEnv("LOADTEST_BENCH_ROWS", 50_000)
	var col proto.ColUInt256
	col.AppendArr(makeProtoUInt256Values(rows))
	out := make([]proto.UInt256, rows)
	rem := make([]uint64, rows)

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i, value := range col {
			out[i], rem[i] = protoUInt256DivUint64(value, 100_000_000)
		}
	}
	runtime.KeepAlive(out)
	runtime.KeepAlive(rem)
}

func BenchmarkExp3_Insert_PerBatch(b *testing.B) {
	if os.Getenv("LOADTEST_CLICKHOUSE_BENCH") != "1" {
		b.Skip("set LOADTEST_CLICKHOUSE_BENCH=1 to run ClickHouse insert benchmark")
	}
	rows := benchIntEnv("LOADTEST_INSERT_ROWS", 200_000)
	chunkSize := benchIntEnv("LOADTEST_INSERT_CHUNK", 10_000)
	ctx := context.Background()
	conn, db := setupClickHouseInsertBench(b, ctx)
	defer conn.Close()
	defer dropClickHouseBenchDB(ctx, conn, db)

	createOrderInsertTable(b, ctx, conn, db)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		truncateOrderInsertTable(b, ctx, conn, db)
		if err := insertOrderRowsPerBatch(ctx, conn, db, rows, chunkSize); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExp3_Insert_OnInputStreaming(b *testing.B) {
	if os.Getenv("LOADTEST_CLICKHOUSE_BENCH") != "1" {
		b.Skip("set LOADTEST_CLICKHOUSE_BENCH=1 to run ClickHouse insert benchmark")
	}
	rows := benchIntEnv("LOADTEST_INSERT_ROWS", 200_000)
	chunkSize := benchIntEnv("LOADTEST_INSERT_CHUNK", 10_000)
	ctx := context.Background()
	conn, db := setupClickHouseInsertBench(b, ctx)
	defer conn.Close()
	defer dropClickHouseBenchDB(ctx, conn, db)

	createOrderInsertTable(b, ctx, conn, db)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		truncateOrderInsertTable(b, ctx, conn, db)
		if err := insertOrderRowsStreaming(ctx, conn, db, rows, chunkSize); err != nil {
			b.Fatal(err)
		}
	}
}

func makeDecimalPositionsBench(rows int) []decimalPositionBench {
	out := make([]decimalPositionBench, rows)
	for i := range out {
		fillFixedBytes(out[i].User[:], uint64(i))
		fillFixedBytes(out[i].TokenID[:], uint64(i*17))
		amount := decimal.NewFromInt(int64(1_000_000 + i%10_000)).Div(decimal.NewFromInt(protoBenchScale))
		avgPrice := decimal.NewFromInt(int64(400_000 + i%200_000)).Div(decimal.NewFromInt(protoBenchScale))
		out[i].Amount = amount
		out[i].AvgPrice = avgPrice
		out[i].RealizedPnL = decimal.Zero
		out[i].TotalBought = amount
		out[i].UpdatedAtBlock = uint64(i)
		out[i].BlockNumber = uint64(i)
	}
	return out
}

func makeProtoPositionBlockBench(rows int) *protoPositionBlock {
	block := &protoPositionBlock{}
	block.User.SetSize(20)
	block.TokenID.SetSize(32)
	for i := 0; i < rows; i++ {
		var user [20]byte
		var token [32]byte
		fillFixedBytes(user[:], uint64(i))
		fillFixedBytes(token[:], uint64(i*17))
		block.User.Append(user[:])
		block.TokenID.Append(token[:])
		amount := int64(1_000_000 + i%10_000)
		avgPrice := int64(400_000 + i%200_000)
		block.Amount.Append(protoDec128FromI64(amount))
		block.AvgPrice.Append(protoDec128FromI64(avgPrice))
		block.RealizedPnL.Append(protoDec128FromI64(0))
		block.TotalBought.Append(protoDec128FromI64(amount))
		block.UpdatedAtBlock.Append(uint64(i))
		block.BlockNumber.Append(uint64(i))
		block.TxIndex.Append(0)
		block.LogIndex.Append(0)
	}
	return block
}

func copyProtoPositionBlock(dst *protoPositionBlock, src *protoPositionBlock) {
	dst.User.Size = src.User.Size
	dst.TokenID.Size = src.TokenID.Size
	dst.User.Buf = append(dst.User.Buf[:0], src.User.Buf...)
	dst.TokenID.Buf = append(dst.TokenID.Buf[:0], src.TokenID.Buf...)
	dst.Amount = append(dst.Amount[:0], src.Amount...)
	dst.AvgPrice = append(dst.AvgPrice[:0], src.AvgPrice...)
	dst.RealizedPnL = append(dst.RealizedPnL[:0], src.RealizedPnL...)
	dst.TotalBought = append(dst.TotalBought[:0], src.TotalBought...)
	dst.UpdatedAtBlock = append(dst.UpdatedAtBlock[:0], src.UpdatedAtBlock...)
	dst.BlockNumber = append(dst.BlockNumber[:0], src.BlockNumber...)
	dst.TxIndex = append(dst.TxIndex[:0], src.TxIndex...)
	dst.LogIndex = append(dst.LogIndex[:0], src.LogIndex...)
}

func makeDecimalUpdatesBench(rows int) ([]decimal.Decimal, []decimal.Decimal) {
	prices := make([]decimal.Decimal, rows)
	deltas := make([]decimal.Decimal, rows)
	scale := decimal.NewFromInt(protoBenchScale)
	for i := range prices {
		prices[i] = decimal.NewFromInt(int64(300_000 + i%400_000)).Div(scale)
		deltas[i] = decimal.NewFromInt(int64(10_000 + i%500)).Div(scale)
	}
	return prices, deltas
}

func makeProtoUpdatesBench(rows int) ([]proto.Decimal128, []proto.Decimal128) {
	prices := make([]proto.Decimal128, rows)
	deltas := make([]proto.Decimal128, rows)
	for i := range prices {
		prices[i] = protoDec128FromI64(int64(300_000 + i%400_000))
		deltas[i] = protoDec128FromI64(int64(10_000 + i%500))
	}
	return prices, deltas
}

func decimalBuyUpdate(pos *decimalPositionBench, price, delta decimal.Decimal) {
	denom := pos.Amount.Add(delta)
	if !denom.IsZero() {
		pos.AvgPrice = pos.AvgPrice.Mul(pos.Amount).Add(price.Mul(delta)).Div(denom)
	}
	pos.Amount = denom
	pos.TotalBought = pos.TotalBought.Add(delta)
}

func protoBuyUpdate(amount, avgPrice, totalBought *proto.Decimal128, price, delta proto.Decimal128) {
	amountRaw := protoDec128ToI64(*amount)
	avgRaw := protoDec128ToI64(*avgPrice)
	priceRaw := protoDec128ToI64(price)
	deltaRaw := protoDec128ToI64(delta)
	denom := amountRaw + deltaRaw
	if denom != 0 {
		*avgPrice = protoDec128FromI64((avgRaw*amountRaw + priceRaw*deltaRaw) / denom)
	}
	*amount = protoDec128FromI64(denom)
	*totalBought = protoDec128FromI64(protoDec128ToI64(*totalBought) + deltaRaw)
}

func protoDec128FromI64(v int64) proto.Decimal128 {
	if v < 0 {
		return proto.Decimal128{Low: uint64(v), High: math.MaxUint64}
	}
	return proto.Decimal128{Low: uint64(v), High: 0}
}

func protoDec128ToI64(v proto.Decimal128) int64 {
	return int64(v.Low)
}

func makeProtoUInt256Values(rows int) []proto.UInt256 {
	values := make([]proto.UInt256, rows)
	for i := range values {
		values[i] = proto.UInt256{
			Low: proto.UInt128{
				Low:  uint64(100_000_000 + i),
				High: uint64(i) * 17,
			},
			High: proto.UInt128{
				Low:  uint64(i/7) * 31,
				High: uint64(i/13) * 43,
			},
		}
	}
	return values
}

func protoUInt256DivUint64(value proto.UInt256, divisor uint64) (proto.UInt256, uint64) {
	if divisor == 0 {
		panic("divide by zero")
	}
	var quotient proto.UInt256
	var remainder uint64
	quotient.High.High, remainder = divWord(remainder, value.High.High, divisor)
	quotient.High.Low, remainder = divWord(remainder, value.High.Low, divisor)
	quotient.Low.High, remainder = divWord(remainder, value.Low.High, divisor)
	quotient.Low.Low, remainder = divWord(remainder, value.Low.Low, divisor)
	return quotient, remainder
}

func divWord(remainder, limb, divisor uint64) (uint64, uint64) {
	quotient, nextRemainder := bits.Div64(remainder, limb, divisor)
	return quotient, nextRemainder
}

func protoUInt256FromBig(value *big.Int) proto.UInt256 {
	if value.Sign() < 0 || value.BitLen() > 256 {
		panic("value out of UInt256 range")
	}
	var buf [32]byte
	raw := value.Bytes()
	copy(buf[len(buf)-len(raw):], raw)
	return proto.UInt256{
		Low: proto.UInt128{
			Low:  binary.BigEndian.Uint64(buf[24:32]),
			High: binary.BigEndian.Uint64(buf[16:24]),
		},
		High: proto.UInt128{
			Low:  binary.BigEndian.Uint64(buf[8:16]),
			High: binary.BigEndian.Uint64(buf[0:8]),
		},
	}
}

func protoUInt256ToBig(value proto.UInt256) *big.Int {
	out := new(big.Int).SetUint64(value.High.High)
	out.Lsh(out, 64).Add(out, new(big.Int).SetUint64(value.High.Low))
	out.Lsh(out, 64).Add(out, new(big.Int).SetUint64(value.Low.High))
	out.Lsh(out, 64).Add(out, new(big.Int).SetUint64(value.Low.Low))
	return out
}

func fillFixedBytes(dst []byte, v uint64) {
	for i := len(dst) - 1; i >= 0 && v != 0; i-- {
		dst[i] = byte(v)
		v >>= 8
	}
}

func benchIntEnv(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

type orderInsertColumns struct {
	blockNumber       proto.ColUInt64
	blockTimestamp    proto.ColDateTime64
	transactionIndex  proto.ColUInt64
	logIndex          proto.ColUInt64
	maker             proto.ColFixedStr
	taker             proto.ColFixedStr
	makerAssetID      proto.ColUInt256
	takerAssetID      proto.ColUInt256
	makerAmountFilled proto.ColUInt256
	takerAmountFilled proto.ColUInt256
	fee               proto.ColUInt256
	input             proto.Input
}

func newOrderInsertColumns() *orderInsertColumns {
	cols := &orderInsertColumns{}
	cols.blockTimestamp.WithPrecision(proto.Precision(3))
	cols.blockTimestamp.WithLocation(time.UTC)
	cols.maker.SetSize(20)
	cols.taker.SetSize(20)
	cols.input = proto.Input{
		{Name: "block_number", Data: &cols.blockNumber},
		{Name: "block_timestamp", Data: &cols.blockTimestamp},
		{Name: "transaction_index", Data: &cols.transactionIndex},
		{Name: "log_index", Data: &cols.logIndex},
		{Name: "maker", Data: &cols.maker},
		{Name: "taker", Data: &cols.taker},
		{Name: "makerAssetId", Data: &cols.makerAssetID},
		{Name: "takerAssetId", Data: &cols.takerAssetID},
		{Name: "makerAmountFilled", Data: &cols.makerAmountFilled},
		{Name: "takerAmountFilled", Data: &cols.takerAmountFilled},
		{Name: "fee", Data: &cols.fee},
	}
	return cols
}

func (c *orderInsertColumns) fill(start, rows int) {
	c.input.Reset()
	now := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < rows; i++ {
		idx := start + i
		var maker [20]byte
		var taker [20]byte
		fillFixedBytes(maker[:], uint64(idx))
		fillFixedBytes(taker[:], uint64(idx*7))
		c.blockNumber.Append(uint64(idx / 1000))
		c.blockTimestamp.Append(now.Add(time.Duration(idx/1000) * time.Second))
		c.transactionIndex.Append(uint64(idx % 1000))
		c.logIndex.Append(uint64(idx))
		c.maker.Append(maker[:])
		c.taker.Append(taker[:])
		c.makerAssetID.Append(proto.UInt256FromUInt64(uint64(idx + 1)))
		c.takerAssetID.Append(proto.UInt256FromUInt64(uint64(idx + 2)))
		c.makerAmountFilled.Append(proto.UInt256FromUInt64(uint64(100 + idx%1000)))
		c.takerAmountFilled.Append(proto.UInt256FromUInt64(uint64(200 + idx%1000)))
		c.fee.Append(proto.UInt256{})
	}
}

func insertOrderRowsPerBatch(ctx context.Context, conn *ch.Client, db string, rows, chunkSize int) error {
	cols := newOrderInsertColumns()
	for start := 0; start < rows; start += chunkSize {
		n := chunkSize
		if start+n > rows {
			n = rows - start
		}
		cols.fill(start, n)
		if err := conn.Do(ctx, ch.Query{
			Body:  orderInsertQuery(db),
			Input: cols.input,
		}); err != nil {
			return err
		}
	}
	return nil
}

func insertOrderRowsStreaming(ctx context.Context, conn *ch.Client, db string, rows, chunkSize int) error {
	cols := newOrderInsertColumns()
	processed := 0
	return conn.Do(ctx, ch.Query{
		Body:  orderInsertQuery(db),
		Input: cols.input,
		OnInput: func(ctx context.Context) error {
			if processed >= rows {
				cols.input.Reset()
				return io.EOF
			}
			n := chunkSize
			if processed+n > rows {
				n = rows - processed
			}
			cols.fill(processed, n)
			processed += n
			return nil
		},
	})
}

func orderInsertQuery(db string) string {
	return fmt.Sprintf("INSERT INTO %s.loadtest_proto_order_insert (block_number, block_timestamp, transaction_index, log_index, maker, taker, makerAssetId, takerAssetId, makerAmountFilled, takerAmountFilled, fee) VALUES", db)
}

func setupClickHouseInsertBench(tb testing.TB, ctx context.Context) (*ch.Client, string) {
	host := os.Getenv("CLICKHOUSE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := benchIntEnv("CLICKHOUSE_NATIVE_PORT", 9003)
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
	db := fmt.Sprintf("loadtest_proto_bench_%d", os.Getpid())
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", db)}); err != nil {
		conn.Close()
		tb.Fatalf("create db: %v", err)
	}
	return conn, db
}

func createOrderInsertTable(tb testing.TB, ctx context.Context, conn *ch.Client, db string) {
	query := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.loadtest_proto_order_insert (
  block_number UInt64,
  block_timestamp DateTime64(3, 'UTC'),
  transaction_index UInt64,
  log_index UInt64,
  maker FixedString(20),
  taker FixedString(20),
  makerAssetId UInt256,
  takerAssetId UInt256,
  makerAmountFilled UInt256,
  takerAmountFilled UInt256,
  fee UInt256
) ENGINE = Memory`, db)
	if err := conn.Do(ctx, ch.Query{Body: query}); err != nil {
		tb.Fatalf("create table: %v", err)
	}
}

func truncateOrderInsertTable(tb testing.TB, ctx context.Context, conn *ch.Client, db string) {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("TRUNCATE TABLE %s.loadtest_proto_order_insert", db)}); err != nil {
		tb.Fatalf("truncate table: %v", err)
	}
}

func dropClickHouseBenchDB(ctx context.Context, conn *ch.Client, db string) {
	_ = conn.Do(ctx, ch.Query{Body: fmt.Sprintf("DROP DATABASE IF EXISTS %s SYNC", db)})
}
