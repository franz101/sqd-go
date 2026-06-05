// Package optidisk benchmarks the fastest path to serialize/deserialize
// position state as ch-go proto blobs (disk-backed without ClickHouse).
//
// Three serialization paths benchmarked:
//   1. RAW: custom LE binary blob (4B header + N×104B rows) — no ch-go dependency
//   2. PROTO: ch-go proto columns → EncodeBlock (ClickHouse native wire format)
//   3. BUFFER: ch-go OnInput streaming pattern with proto columns
//
// Each path is measured for serialize, deserialize, file I/O roundtrip,
// and in-memory roundtrip. Blobs are written to ./tmp/.
//
// Run:
//   go test -bench=. -benchmem ./experiment/optidisk/
//   go test -bench=Serialize -benchmem ./experiment/optidisk/
//   POLYMARKET_POSITIONS=100000 go test -bench=. -benchmem ./experiment/optidisk/
package optidisk

import (
	"bytes"
	"encoding/binary"
	"os"
	"strconv"
	"testing"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/drafts/protomath"
)

var scale18 = protomath.Decimal256Scale18

// Sinks (prevent compiler optimization)
var (
	sinkInput proto.Input
	sinkBytes []byte
	sinkRows  int
	sinkDec   protomath.Decimal256
	sinkPos   Position
)

// ── Position ───────────────────────────────────────────────────────────

type Position struct {
	EntityID    uint64
	Amount      protomath.Decimal256
	TotalBought protomath.Decimal256
	AvgPrice    protomath.Decimal256
}

// ── Proto columns (ch-go buffer pattern) ─────────────────────────────

type PosCols struct {
	EntityID    proto.ColUInt64
	Amount      proto.ColDecimal256
	TotalBought proto.ColDecimal256
	AvgPrice    proto.ColDecimal256
}

func NewPosCols(cap int) *PosCols {
	return &PosCols{
		EntityID:    make(proto.ColUInt64, 0, cap),
		Amount:      make(proto.ColDecimal256, 0, cap),
		TotalBought: make(proto.ColDecimal256, 0, cap),
		AvgPrice:    make(proto.ColDecimal256, 0, cap),
	}
}

func (c *PosCols) Reset() {
	c.EntityID.Reset()
	c.Amount.Reset()
	c.TotalBought.Reset()
	c.AvgPrice.Reset()
}

func (c *PosCols) Append(p Position) {
	c.EntityID.Append(p.EntityID)
	c.Amount.Append(p.Amount.Proto())
	c.TotalBought.Append(p.TotalBought.Proto())
	c.AvgPrice.Append(p.AvgPrice.Proto())
}

func (c *PosCols) Input() proto.Input {
	return proto.Input{
		{Name: "entity_id", Data: &c.EntityID},
		{Name: "amount", Data: &c.Amount},
		{Name: "total_bought", Data: &c.TotalBought},
		{Name: "avg_price", Data: &c.AvgPrice},
	}
}

func (c *PosCols) Results() proto.Results {
	return proto.Results{
		{Name: "entity_id", Data: &c.EntityID},
		{Name: "amount", Data: &c.Amount},
		{Name: "total_bought", Data: &c.TotalBought},
		{Name: "avg_price", Data: &c.AvgPrice},
	}
}

// ToPositions extracts proto columns → Position slice.
func (c *PosCols) ToPositions(dst []Position) []Position {
	rows := c.EntityID.Rows()
	if cap(dst) < rows {
		dst = make([]Position, rows)
	} else {
		dst = dst[:rows]
	}
	for i := 0; i < rows; i++ {
		dst[i] = Position{
			EntityID:    c.EntityID.Row(i),
			Amount:      protomath.Decimal256(c.Amount.Row(i)),
			TotalBought: protomath.Decimal256(c.TotalBought.Row(i)),
			AvgPrice:    protomath.Decimal256(c.AvgPrice.Row(i)),
		}
	}
	return dst
}

// ── RAW blob (custom binary, no ch-go dependency) ────────────────────

const rawRowSize = 8 + 32 + 32 + 32 // 104 bytes: entityID + 3×Decimal256

func serializeRaw(positions []Position) []byte {
	buf := make([]byte, 4+len(positions)*rawRowSize)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(positions)))
	off := 4
	for i := range positions {
		binary.LittleEndian.PutUint64(buf[off:off+8], positions[i].EntityID)
		off += 8
		positions[i].Amount.PutLittleEndianBytes((*[32]byte)(buf[off : off+32]))
		off += 32
		positions[i].TotalBought.PutLittleEndianBytes((*[32]byte)(buf[off : off+32]))
		off += 32
		positions[i].AvgPrice.PutLittleEndianBytes((*[32]byte)(buf[off : off+32]))
		off += 32
	}
	return buf
}

func deserializeRaw(data []byte) []Position {
	count := int(binary.LittleEndian.Uint32(data[0:4]))
	positions := make([]Position, count)
	off := 4
	for i := 0; i < count; i++ {
		positions[i].EntityID = binary.LittleEndian.Uint64(data[off : off+8])
		off += 8
		positions[i].Amount, _ = protomath.FromDecimal256LittleEndianBytes(data[off : off+32])
		off += 32
		positions[i].TotalBought, _ = protomath.FromDecimal256LittleEndianBytes(data[off : off+32])
		off += 32
		positions[i].AvgPrice, _ = protomath.FromDecimal256LittleEndianBytes(data[off : off+32])
		off += 32
	}
	return positions
}

// ── PROTO encode/decode (ch-go native wire format) ────────────────────

// encodeProto encodes proto.Input to ClickHouse native wire bytes.
func encodeProto(input proto.Input, rows int) ([]byte, error) {
	var buf proto.Buffer
	block := proto.Block{Columns: len(input), Rows: rows}
	if err := block.EncodeBlock(&buf, proto.Version, input); err != nil {
		return nil, err
	}
	return buf.Buf, nil
}

// decodeProto decodes ClickHouse native wire bytes back into PosCols.
func decodeProto(data []byte, cols *PosCols) (int, error) {
	r := proto.NewReader(bytes.NewReader(data))
	var block proto.Block
	results := cols.Results()
	if err := block.DecodeBlock(r, proto.Version, results); err != nil {
		return 0, err
	}
	return block.Rows, nil
}

// ── Test data ─────────────────────────────────────────────────────────

func makePositions(n int) []Position {
	positions := make([]Position, n)
	for i := 0; i < n; i++ {
		amount, _ := protomath.FromInt64(int64(10+i%90), scale18)
		price := protomath.FromScaledInt64(350_000_000_000_000_000 + int64(i%200)*1_000_000_000_000_000)
		total, _ := amount.Mul(price, scale18)
		positions[i] = Position{
			EntityID:    uint64(i + 1),
			Amount:      amount,
			TotalBought: total,
			AvgPrice:    price,
		}
	}
	return positions
}

func benchRows(env string, defaultVal int) int {
	raw := os.Getenv(env)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultVal
	}
	return v
}

func benchPositions() int { return benchRows("POLYMARKET_POSITIONS", 100_000) }

// ── Size test ─────────────────────────────────────────────────────────

func TestPrintSizes(t *testing.T) {
	positions := makePositions(1000)

	// RAW
	raw := serializeRaw(positions)
	t.Logf("RAW:    %d rows → %d bytes (%.1f KB, %d B/row)", len(positions), len(raw), float64(len(raw))/1024, len(raw)/len(positions))

	// PROTO
	cols := NewPosCols(len(positions))
	for i := range positions {
		cols.Append(positions[i])
	}
	wire, err := encodeProto(cols.Input(), cols.EntityID.Rows())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PROTO:  %d rows → %d bytes (%.1f KB, %d B/row)", len(positions), len(wire), float64(len(wire))/1024, len(wire)/len(positions))
	t.Logf("PROTO overhead vs RAW: %.1f%%", float64(len(wire)-len(raw))/float64(len(raw))*100)

	// Write to ./tmp for inspection
	os.MkdirAll("./tmp", 0755)
	os.WriteFile("./tmp/positions_raw.bin", raw, 0644)
	os.WriteFile("./tmp/positions_proto.bin", wire, 0644)
	t.Logf("Wrote ./tmp/positions_{raw,proto}.bin")
}

func TestPrintSizesFull(t *testing.T) {
	positions := makePositions(benchPositions())

	raw := serializeRaw(positions)
	t.Logf("RAW:    %d rows → %d bytes (%.2f MB)", len(positions), len(raw), float64(len(raw))/(1024*1024))

	cols := NewPosCols(len(positions))
	for i := range positions {
		cols.Append(positions[i])
	}
	wire, err := encodeProto(cols.Input(), cols.EntityID.Rows())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PROTO:  %d rows → %d bytes (%.2f MB)", len(positions), len(wire), float64(len(wire))/(1024*1024))

	// Verify roundtrip
	decoded := deserializeRaw(raw)
	if decoded[0].EntityID != positions[0].EntityID {
		t.Fatal("raw roundtrip mismatch")
	}

	readCols := NewPosCols(len(positions))
	rows, err := decodeProto(wire, readCols)
	if err != nil {
		t.Fatal(err)
	}
	if rows != len(positions) {
		t.Fatalf("proto decode rows: got %d want %d", rows, len(positions))
	}
	if readCols.EntityID.Row(0) != positions[0].EntityID {
		t.Fatal("proto roundtrip mismatch")
	}
	t.Logf("Roundtrip verified ✓")
}

// ── SERIALIZE ────────────────────────────────────────────────────────

// BenchmarkSerialize_Raw: positions → raw LE blob.
func BenchmarkSerialize_Raw(b *testing.B) {
	positions := makePositions(benchPositions())
	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		sinkBytes = serializeRaw(positions)
	}
}

// BenchmarkSerialize_ProtoCols: positions → proto columns (append only, no encode).
func BenchmarkSerialize_ProtoCols(b *testing.B) {
	positions := makePositions(benchPositions())
	cols := NewPosCols(len(positions))
	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		cols.Reset()
		for i := range positions {
			cols.Append(positions[i])
		}
		sinkInput = cols.Input()
	}
}

// BenchmarkSerialize_ProtoWire: positions → proto columns → EncodeBlock (full wire).
func BenchmarkSerialize_ProtoWire(b *testing.B) {
	positions := makePositions(benchPositions())
	cols := NewPosCols(len(positions))
	for i := range positions {
		cols.Append(positions[i])
	}
	input := cols.Input()
	rows := cols.EntityID.Rows()

	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var err error
		sinkBytes, err = encodeProto(input, rows)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// ── DESERIALIZE ──────────────────────────────────────────────────────

// BenchmarkDeserialize_Raw: raw LE blob → positions.
func BenchmarkDeserialize_Raw(b *testing.B) {
	positions := makePositions(benchPositions())
	blob := serializeRaw(positions)

	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		sinkPos = deserializeRaw(blob)[0]
	}
}

// BenchmarkDeserialize_ProtoWire: proto wire bytes → PosCols → Position slice.
func BenchmarkDeserialize_ProtoWire(b *testing.B) {
	positions := makePositions(benchPositions())
	cols := NewPosCols(len(positions))
	for i := range positions {
		cols.Append(positions[i])
	}
	blob, err := encodeProto(cols.Input(), cols.EntityID.Rows())
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		readCols := NewPosCols(len(positions))
		if _, err := decodeProto(blob, readCols); err != nil {
			b.Fatal(err)
		}
		sinkRows = readCols.EntityID.Rows()
	}
}

// ── ROUNDTRIP (in-memory) ────────────────────────────────────────────

func BenchmarkRoundtrip_Raw(b *testing.B) {
	positions := makePositions(benchPositions())
	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		blob := serializeRaw(positions)
		result := deserializeRaw(blob)
		sinkPos = result[0]
	}
}

func BenchmarkRoundtrip_Proto(b *testing.B) {
	positions := makePositions(benchPositions())
	cols := NewPosCols(len(positions))
	for i := range positions {
		cols.Append(positions[i])
	}
	input := cols.Input()
	rows := cols.EntityID.Rows()

	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		blob, err := encodeProto(input, rows)
		if err != nil {
			b.Fatal(err)
		}
		readCols := NewPosCols(len(positions))
		if _, err := decodeProto(blob, readCols); err != nil {
			b.Fatal(err)
		}
		sinkRows = readCols.EntityID.Rows()
	}
}

// ── FILE I/O ROUNDTRIP (disk) ─────────────────────────────────────

const tmpDir = "./tmp"

func init() { os.MkdirAll(tmpDir, 0755) }

func BenchmarkFile_Raw(b *testing.B) {
	positions := makePositions(benchPositions())
	blob := serializeRaw(positions)
	path := tmpDir + "/raw.bin"

	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ReportMetric(float64(len(blob)), "bytes")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if err := os.WriteFile(path, blob, 0644); err != nil {
			b.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		sinkPos = deserializeRaw(data)[0]
	}
}

func BenchmarkFile_Proto(b *testing.B) {
	positions := makePositions(benchPositions())
	cols := NewPosCols(len(positions))
	for i := range positions {
		cols.Append(positions[i])
	}
	blob, err := encodeProto(cols.Input(), cols.EntityID.Rows())
	if err != nil {
		b.Fatal(err)
	}
	path := tmpDir + "/proto.bin"

	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "rows")
	b.ReportMetric(float64(len(blob)), "bytes")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if err := os.WriteFile(path, blob, 0644); err != nil {
			b.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		readCols := NewPosCols(len(positions))
		if _, err := decodeProto(data, readCols); err != nil {
			b.Fatal(err)
		}
		sinkRows = readCols.EntityID.Rows()
	}
}

// ── BUFFER / STREAMING PATTERN ─────────────────────────────────────

// BenchmarkBuffer_Chunked: simulate ch-go OnInput streaming — serialize
// positions in 10k-row blocks using reusable proto columns.
func BenchmarkBuffer_Chunked(b *testing.B) {
	positions := makePositions(benchPositions())
	const blockSize = 10000

	b.ReportAllocs()
	b.ReportMetric(float64(len(positions)), "total_rows")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		cols := NewPosCols(blockSize)
		for start := 0; start < len(positions); start += blockSize {
			cols.Reset()
			end := start + blockSize
			if end > len(positions) {
				end = len(positions)
			}
			for i := start; i < end; i++ {
				cols.Append(positions[i])
			}
			_, err := encodeProto(cols.Input(), cols.EntityID.Rows())
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

// ── SLICE MATH (baseline: in-memory position updates) ──────────────

type sliceStore struct {
	index       map[uint64]int
	entityID    []uint64
	amount      []protomath.Decimal256
	totalBought []protomath.Decimal256
	avgPrice    []protomath.Decimal256
}

func newSliceStore(n int) *sliceStore {
	s := &sliceStore{
		index:       make(map[uint64]int, n),
		entityID:    make([]uint64, n),
		amount:      make([]protomath.Decimal256, n),
		totalBought: make([]protomath.Decimal256, n),
		avgPrice:    make([]protomath.Decimal256, n),
	}
	positions := makePositions(n)
	for i, p := range positions {
		s.index[p.EntityID] = i
		s.entityID[i] = p.EntityID
		s.amount[i] = p.Amount
		s.totalBought[i] = p.TotalBought
		s.avgPrice[i] = p.AvgPrice
	}
	return s
}

func (s *sliceStore) apply(entityID uint64, delta, price protomath.Decimal256) {
	idx := s.index[entityID]
	amount, _ := s.amount[idx].Add(delta)
	bought, _ := delta.Mul(price, scale18)
	total, _ := s.totalBought[idx].Add(bought)
	avg, _ := total.Div(amount, scale18)
	s.amount[idx] = amount
	s.totalBought[idx] = total
	s.avgPrice[idx] = avg
}

func BenchmarkSliceMath(b *testing.B) {
	positions := benchPositions()
	events := benchRows("POLYMARKET_EVENTS", 200_000)
	store := newSliceStore(positions)

	type ev struct {
		id    uint64
		delta protomath.Decimal256
		price protomath.Decimal256
	}
	evs := make([]ev, events)
	for i := 0; i < events; i++ {
		evs[i] = ev{
			id:    uint64(i%positions + 1),
			delta: protomath.FromScaledInt64(100_000_000_000_000_000 + int64(i%7)*10_000_000_000_000_000),
			price: protomath.FromScaledInt64(420_000_000_000_000_000 + int64(i%300)*1_000_000_000_000_000),
		}
	}

	b.ReportAllocs()
	b.ReportMetric(float64(positions), "positions")
	b.ReportMetric(float64(events), "events")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for _, e := range evs {
			store.apply(e.id, e.delta, e.price)
		}
	}
}
