package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/benchmark/jtypes"
	"github.com/mailru/easyjson/jlexer"
)

const e2eOrderFilledTopic0 = "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"

type e2eOrderRow struct {
	blockNumber       uint64
	blockTimestamp    time.Time
	txIndex           uint64
	logIndex          uint64
	maker             [20]byte
	taker             [20]byte
	makerAssetID      proto.UInt256
	takerAssetID      proto.UInt256
	makerAmountFilled proto.UInt256
	takerAmountFilled proto.UInt256
	fee               proto.UInt256
}

type e2ePositionRow struct {
	blockNumber    uint64
	blockTimestamp time.Time
	txIndex        uint64
	logIndex       uint64
	user           [20]byte
	tokenID        proto.UInt256
	amount         proto.UInt256
}

type e2eRowSlot struct {
	orders    []e2eOrderRow
	positions []e2ePositionRow
}

func (s *e2eRowSlot) reset() {
	s.orders = s.orders[:0]
	s.positions = s.positions[:0]
}

type e2eRowRing struct {
	slots []e2eRowSlot
	head  uint64
	mask  uint64
}

func newE2ERowRing(size int, rowsPerBlock int) *e2eRowRing {
	r := &e2eRowRing{slots: make([]e2eRowSlot, size), mask: uint64(size - 1)}
	for i := range r.slots {
		r.slots[i].orders = make([]e2eOrderRow, 0, rowsPerBlock)
		r.slots[i].positions = make([]e2ePositionRow, 0, rowsPerBlock)
	}
	return r
}

func (r *e2eRowRing) next() *e2eRowSlot {
	slot := &r.slots[r.head&r.mask]
	r.head++
	slot.reset()
	return slot
}

type e2eOrderColumns struct {
	blockNumber       proto.ColUInt64
	blockTimestamp    proto.ColDateTime64
	txIndex           proto.ColUInt64
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

func newE2EOrderColumns() *e2eOrderColumns {
	c := &e2eOrderColumns{}
	c.blockTimestamp.WithPrecision(proto.Precision(3))
	c.blockTimestamp.WithLocation(time.UTC)
	c.maker.SetSize(20)
	c.taker.SetSize(20)
	c.input = proto.Input{
		{Name: "block_number", Data: &c.blockNumber},
		{Name: "block_timestamp", Data: &c.blockTimestamp},
		{Name: "transaction_index", Data: &c.txIndex},
		{Name: "log_index", Data: &c.logIndex},
		{Name: "maker", Data: &c.maker},
		{Name: "taker", Data: &c.taker},
		{Name: "makerAssetId", Data: &c.makerAssetID},
		{Name: "takerAssetId", Data: &c.takerAssetID},
		{Name: "makerAmountFilled", Data: &c.makerAmountFilled},
		{Name: "takerAmountFilled", Data: &c.takerAmountFilled},
		{Name: "fee", Data: &c.fee},
	}
	return c
}

func (c *e2eOrderColumns) reserve(rows int) {
	c.blockNumber = make(proto.ColUInt64, 0, rows)
	c.blockTimestamp.Data = make([]proto.DateTime64, 0, rows)
	c.txIndex = make(proto.ColUInt64, 0, rows)
	c.logIndex = make(proto.ColUInt64, 0, rows)
	c.maker.Buf = make([]byte, 0, rows*c.maker.Size)
	c.taker.Buf = make([]byte, 0, rows*c.taker.Size)
	c.makerAssetID = make(proto.ColUInt256, 0, rows)
	c.takerAssetID = make(proto.ColUInt256, 0, rows)
	c.makerAmountFilled = make(proto.ColUInt256, 0, rows)
	c.takerAmountFilled = make(proto.ColUInt256, 0, rows)
	c.fee = make(proto.ColUInt256, 0, rows)
}

func (c *e2eOrderColumns) reset() {
	c.input.Reset()
}

func (c *e2eOrderColumns) appendRow(row e2eOrderRow) {
	c.blockNumber.Append(row.blockNumber)
	c.blockTimestamp.Append(row.blockTimestamp)
	c.txIndex.Append(row.txIndex)
	c.logIndex.Append(row.logIndex)
	c.maker.Append(row.maker[:])
	c.taker.Append(row.taker[:])
	c.makerAssetID.Append(row.makerAssetID)
	c.takerAssetID.Append(row.takerAssetID)
	c.makerAmountFilled.Append(row.makerAmountFilled)
	c.takerAmountFilled.Append(row.takerAmountFilled)
	c.fee.Append(row.fee)
}

type e2ePositionColumns struct {
	blockNumber    proto.ColUInt64
	blockTimestamp proto.ColDateTime64
	txIndex        proto.ColUInt64
	logIndex       proto.ColUInt64
	user           proto.ColFixedStr
	tokenID        proto.ColUInt256
	amount         proto.ColUInt256
	input          proto.Input
}

func newE2EPositionColumns() *e2ePositionColumns {
	c := &e2ePositionColumns{}
	c.blockTimestamp.WithPrecision(proto.Precision(3))
	c.blockTimestamp.WithLocation(time.UTC)
	c.user.SetSize(20)
	c.input = proto.Input{
		{Name: "block_number", Data: &c.blockNumber},
		{Name: "block_timestamp", Data: &c.blockTimestamp},
		{Name: "transaction_index", Data: &c.txIndex},
		{Name: "log_index", Data: &c.logIndex},
		{Name: "user", Data: &c.user},
		{Name: "token_id", Data: &c.tokenID},
		{Name: "amount", Data: &c.amount},
	}
	return c
}

func (c *e2ePositionColumns) reserve(rows int) {
	c.blockNumber = make(proto.ColUInt64, 0, rows)
	c.blockTimestamp.Data = make([]proto.DateTime64, 0, rows)
	c.txIndex = make(proto.ColUInt64, 0, rows)
	c.logIndex = make(proto.ColUInt64, 0, rows)
	c.user.Buf = make([]byte, 0, rows*c.user.Size)
	c.tokenID = make(proto.ColUInt256, 0, rows)
	c.amount = make(proto.ColUInt256, 0, rows)
}

func (c *e2ePositionColumns) reset() {
	c.input.Reset()
}

func (c *e2ePositionColumns) appendRow(row e2ePositionRow) {
	c.blockNumber.Append(row.blockNumber)
	c.blockTimestamp.Append(row.blockTimestamp)
	c.txIndex.Append(row.txIndex)
	c.logIndex.Append(row.logIndex)
	c.user.Append(row.user[:])
	c.tokenID.Append(row.tokenID)
	c.amount.Append(row.amount)
}

type e2eProtoSlot struct {
	orders    *e2eOrderColumns
	positions *e2ePositionColumns
}

func newE2EProtoSlot(rowsPerSlot int) e2eProtoSlot {
	slot := e2eProtoSlot{
		orders:    newE2EOrderColumns(),
		positions: newE2EPositionColumns(),
	}
	slot.orders.reserve(rowsPerSlot)
	slot.positions.reserve(rowsPerSlot)
	return slot
}

func (s *e2eProtoSlot) reset() {
	s.orders.reset()
	s.positions.reset()
}

type e2eProtoRing struct {
	slots []e2eProtoSlot
	head  uint64
	mask  uint64
}

func newE2EProtoRing(size int, rowsPerSlot int) *e2eProtoRing {
	r := &e2eProtoRing{slots: make([]e2eProtoSlot, size), mask: uint64(size - 1)}
	for i := range r.slots {
		r.slots[i] = newE2EProtoSlot(rowsPerSlot)
	}
	return r
}

func (r *e2eProtoRing) next() *e2eProtoSlot {
	slot := &r.slots[r.head&r.mask]
	r.head++
	slot.reset()
	return slot
}

type e2eInsertBuffers struct {
	orders    *e2eOrderColumns
	positions *e2ePositionColumns
}

func newE2EInsertBuffers(rowsPerBatch int) *e2eInsertBuffers {
	buffers := &e2eInsertBuffers{
		orders:    newE2EOrderColumns(),
		positions: newE2EPositionColumns(),
	}
	buffers.orders.reserve(rowsPerBatch)
	buffers.positions.reserve(rowsPerBatch)
	return buffers
}

func (b *e2eInsertBuffers) reset() {
	b.orders.reset()
	b.positions.reset()
}

func (b *e2eInsertBuffers) appendRows(slot *e2eRowSlot) {
	for _, row := range slot.orders {
		b.orders.appendRow(row)
	}
	for _, row := range slot.positions {
		b.positions.appendRow(row)
	}
}

type e2eDBDigest struct {
	orderRows    int
	positionRows int
	orderHash    uint64
	positionHash uint64
}

func (d *e2eDBDigest) consumeOrderColumns(cols *e2eOrderColumns) {
	rows := cols.blockNumber.Rows()
	d.orderRows += rows
	h := d.orderHash
	for i := 0; i < rows; i++ {
		h = e2eMixUint64(h, cols.blockNumber.Row(i))
		h = e2eMixInt64(h, cols.blockTimestamp.Row(i).UnixMilli())
		h = e2eMixUint64(h, cols.txIndex.Row(i))
		h = e2eMixUint64(h, cols.logIndex.Row(i))
		h = e2eMixBytes(h, cols.maker.Row(i))
		h = e2eMixBytes(h, cols.taker.Row(i))
		h = e2eMixUInt256(h, cols.makerAssetID.Row(i))
		h = e2eMixUInt256(h, cols.takerAssetID.Row(i))
		h = e2eMixUInt256(h, cols.makerAmountFilled.Row(i))
		h = e2eMixUInt256(h, cols.takerAmountFilled.Row(i))
		h = e2eMixUInt256(h, cols.fee.Row(i))
	}
	d.orderHash = h
	runtime.KeepAlive(cols.input)
}

func (d *e2eDBDigest) consumePositionColumns(cols *e2ePositionColumns) {
	rows := cols.blockNumber.Rows()
	d.positionRows += rows
	h := d.positionHash
	for i := 0; i < rows; i++ {
		h = e2eMixUint64(h, cols.blockNumber.Row(i))
		h = e2eMixInt64(h, cols.blockTimestamp.Row(i).UnixMilli())
		h = e2eMixUint64(h, cols.txIndex.Row(i))
		h = e2eMixUint64(h, cols.logIndex.Row(i))
		h = e2eMixBytes(h, cols.user.Row(i))
		h = e2eMixUInt256(h, cols.tokenID.Row(i))
		h = e2eMixUInt256(h, cols.amount.Row(i))
	}
	d.positionHash = h
	runtime.KeepAlive(cols.input)
}

func e2ePipelineEasyJSONRowRingToProtoDB(data []byte, ring *e2eRowRing, insert *e2eInsertBuffers, digest *e2eDBDigest) {
	var block jtypes.JSONLBlock

	rest := data
	for len(rest) > 0 {
		lineEnd := e2eLineEnd(rest)
		line := rest[:lineEnd]
		if lineEnd < len(rest) {
			rest = rest[lineEnd+1:]
		} else {
			rest = nil
		}
		if len(line) == 0 {
			continue
		}

		block.Header = jtypes.JSONLHeader{}
		block.Logs = block.Logs[:0]
		l := jlexer.Lexer{Data: line}
		block.UnmarshalEasyJSON(&l)
		if !l.Ok() {
			continue
		}

		slot := ring.next()
		blockTimestamp := time.Unix(int64(block.Header.Timestamp), 0).UTC()
		for i := range block.Logs {
			lg := &block.Logs[i]
			if !e2eIsOrderFilled(lg.Data, lg.Topics) {
				continue
			}
			row := e2eDecodeOrderRow(block.Header.Number, blockTimestamp, lg.TransactionIndex, lg.LogIndex, lg.Data, lg.Topics)
			slot.orders = append(slot.orders, row)
			slot.positions = append(slot.positions, e2ePositionFromOrder(row))
		}

		insert.reset()
		insert.appendRows(slot)
		digest.consumeOrderColumns(insert.orders)
		digest.consumePositionColumns(insert.positions)
	}
}

func e2ePipelineRawLexerRowRingToProtoDB(data []byte, ring *e2eRowRing, insert *e2eInsertBuffers, digest *e2eDBDigest) {
	var topics [4]string
	var dataHex string

	rest := data
	for len(rest) > 0 {
		lineEnd := e2eLineEnd(rest)
		line := rest[:lineEnd]
		if lineEnd < len(rest) {
			rest = rest[lineEnd+1:]
		} else {
			rest = nil
		}
		if len(line) == 0 {
			continue
		}

		l := jlexer.Lexer{Data: line}
		slot := ring.next()
		var blockNum uint64
		var blockTimestamp uint64

		l.Delim('{')
		for !l.IsDelim('}') {
			key := l.UnsafeFieldName(false)
			l.WantColon()
			switch key {
			case "header":
				l.Delim('{')
				for !l.IsDelim('}') {
					hkey := l.UnsafeFieldName(false)
					l.WantColon()
					switch hkey {
					case "number":
						blockNum = l.Uint64()
					case "timestamp":
						blockTimestamp = l.Uint64()
					default:
						l.Skip()
					}
					l.WantComma()
				}
				l.Delim('}')
			case "logs":
				ts := time.Unix(int64(blockTimestamp), 0).UTC()
				l.Delim('[')
				for !l.IsDelim(']') {
					topicCount, txIndex, logIndex := e2eReadJSONLLog(&l, &topics, &dataHex)
					if topicCount >= 4 && len(dataHex) == 322 && topics[0] == e2eOrderFilledTopic0 {
						row := e2eDecodeOrderRow(blockNum, ts, txIndex, logIndex, dataHex, topics[:])
						slot.orders = append(slot.orders, row)
						slot.positions = append(slot.positions, e2ePositionFromOrder(row))
					}
					l.WantComma()
				}
				l.Delim(']')
			default:
				l.Skip()
			}
			l.WantComma()
		}
		l.Delim('}')
		if !l.Ok() {
			continue
		}

		insert.reset()
		insert.appendRows(slot)
		digest.consumeOrderColumns(insert.orders)
		digest.consumePositionColumns(insert.positions)
	}
}

func e2ePipelineGeneratedLexerProtoRingPointerDB(data []byte, ring *e2eProtoRing, digest *e2eDBDigest) {
	var topics [4]string
	var dataHex string

	rest := data
	for len(rest) > 0 {
		lineEnd := e2eLineEnd(rest)
		line := rest[:lineEnd]
		if lineEnd < len(rest) {
			rest = rest[lineEnd+1:]
		} else {
			rest = nil
		}
		if len(line) == 0 {
			continue
		}

		l := jlexer.Lexer{Data: line}
		slot := ring.next()
		var blockNum uint64
		var blockTimestamp uint64

		l.Delim('{')
		for !l.IsDelim('}') {
			key := l.UnsafeFieldName(false)
			l.WantColon()
			switch key {
			case "header":
				l.Delim('{')
				for !l.IsDelim('}') {
					hkey := l.UnsafeFieldName(false)
					l.WantColon()
					switch hkey {
					case "number":
						blockNum = l.Uint64()
					case "timestamp":
						blockTimestamp = l.Uint64()
					default:
						l.Skip()
					}
					l.WantComma()
				}
				l.Delim('}')
			case "logs":
				ts := time.Unix(int64(blockTimestamp), 0).UTC()
				l.Delim('[')
				for !l.IsDelim(']') {
					topicCount, txIndex, logIndex := e2eReadJSONLLog(&l, &topics, &dataHex)
					if topicCount >= 4 && len(dataHex) == 322 && topics[0] == e2eOrderFilledTopic0 {
						e2eAppendOrderAndPositionToProto(slot, blockNum, ts, txIndex, logIndex, dataHex, topics)
					}
					l.WantComma()
				}
				l.Delim(']')
			default:
				l.Skip()
			}
			l.WantComma()
		}
		l.Delim('}')
		if !l.Ok() {
			continue
		}

		digest.consumeOrderColumns(slot.orders)
		digest.consumePositionColumns(slot.positions)
	}
}

func e2eReadJSONLLog(l *jlexer.Lexer, topics *[4]string, dataHex *string) (topicCount int, txIndex uint64, logIndex uint64) {
	*dataHex = ""
	l.Delim('{')
	for !l.IsDelim('}') {
		key := l.UnsafeFieldName(false)
		l.WantColon()
		switch key {
		case "data":
			*dataHex = l.UnsafeString()
		case "transactionIndex":
			txIndex = l.Uint64()
		case "logIndex":
			logIndex = l.Uint64()
		case "topics":
			l.Delim('[')
			topicCount = 0
			for !l.IsDelim(']') {
				if topicCount < len(topics) {
					topics[topicCount] = l.UnsafeString()
				} else {
					l.Skip()
				}
				topicCount++
				l.WantComma()
			}
			l.Delim(']')
		default:
			l.Skip()
		}
		l.WantComma()
	}
	l.Delim('}')
	return topicCount, txIndex, logIndex
}

func e2eAppendOrderAndPositionToProto(slot *e2eProtoSlot, blockNum uint64, ts time.Time, txIndex, logIndex uint64, dataHex string, topics [4]string) {
	var maker, taker [20]byte
	e2eHexDecodeTopicAddress(&maker, topics[2])
	e2eHexDecodeTopicAddress(&taker, topics[3])

	makerAssetID := e2eUInt256FromHex(dataHex, 2)
	takerAssetID := e2eUInt256FromHex(dataHex, 2+64)
	makerAmountFilled := e2eUInt256FromHex(dataHex, 2+128)
	takerAmountFilled := e2eUInt256FromHex(dataHex, 2+192)
	fee := e2eUInt256FromHex(dataHex, 2+256)

	orders := slot.orders
	orders.blockNumber.Append(blockNum)
	orders.blockTimestamp.Append(ts)
	orders.txIndex.Append(txIndex)
	orders.logIndex.Append(logIndex)
	orders.maker.Append(maker[:])
	orders.taker.Append(taker[:])
	orders.makerAssetID.Append(makerAssetID)
	orders.takerAssetID.Append(takerAssetID)
	orders.makerAmountFilled.Append(makerAmountFilled)
	orders.takerAmountFilled.Append(takerAmountFilled)
	orders.fee.Append(fee)

	tokenID, amount := e2ePositionTokenAmount(makerAssetID, takerAssetID, makerAmountFilled, takerAmountFilled)
	positions := slot.positions
	positions.blockNumber.Append(blockNum)
	positions.blockTimestamp.Append(ts)
	positions.txIndex.Append(txIndex)
	positions.logIndex.Append(logIndex)
	positions.user.Append(maker[:])
	positions.tokenID.Append(tokenID)
	positions.amount.Append(amount)
}

func e2eDecodeOrderRow(blockNum uint64, ts time.Time, txIndex, logIndex uint64, dataHex string, topics []string) e2eOrderRow {
	var row e2eOrderRow
	row.blockNumber = blockNum
	row.blockTimestamp = ts
	row.txIndex = txIndex
	row.logIndex = logIndex
	e2eHexDecodeTopicAddress(&row.maker, topics[2])
	e2eHexDecodeTopicAddress(&row.taker, topics[3])
	row.makerAssetID = e2eUInt256FromHex(dataHex, 2)
	row.takerAssetID = e2eUInt256FromHex(dataHex, 2+64)
	row.makerAmountFilled = e2eUInt256FromHex(dataHex, 2+128)
	row.takerAmountFilled = e2eUInt256FromHex(dataHex, 2+192)
	row.fee = e2eUInt256FromHex(dataHex, 2+256)
	return row
}

func e2ePositionFromOrder(row e2eOrderRow) e2ePositionRow {
	tokenID, amount := e2ePositionTokenAmount(row.makerAssetID, row.takerAssetID, row.makerAmountFilled, row.takerAmountFilled)
	return e2ePositionRow{
		blockNumber:    row.blockNumber,
		blockTimestamp: row.blockTimestamp,
		txIndex:        row.txIndex,
		logIndex:       row.logIndex,
		user:           row.maker,
		tokenID:        tokenID,
		amount:         amount,
	}
}

func e2ePositionTokenAmount(makerAssetID, takerAssetID, makerAmountFilled, takerAmountFilled proto.UInt256) (proto.UInt256, proto.UInt256) {
	if e2eUInt256IsZero(makerAssetID) {
		return takerAssetID, takerAmountFilled
	}
	return makerAssetID, makerAmountFilled
}

func e2eIsOrderFilled(data string, topics []string) bool {
	return len(data) == 322 && len(topics) >= 4 && topics[0] == e2eOrderFilledTopic0
}

func e2eLineEnd(data []byte) int {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i
	}
	return len(data)
}

func e2eHexDecodeTopicAddress(dst *[20]byte, src string) {
	if len(src) < 42 {
		return
	}
	off := 2
	if len(src) >= 66 {
		off = len(src) - 40
	}
	for i := 0; i < 20; i++ {
		dst[i] = e2eHexTable[src[off+i*2]]<<4 | e2eHexTable[src[off+i*2+1]]
	}
}

func e2eUInt256FromHex(src string, off int) proto.UInt256 {
	var be [32]byte
	if off+64 > len(src) {
		return proto.UInt256{}
	}
	for i := 0; i < 32; i++ {
		be[i] = e2eHexTable[src[off+i*2]]<<4 | e2eHexTable[src[off+i*2+1]]
	}
	return proto.UInt256{
		Low: proto.UInt128{
			Low:  binary.BigEndian.Uint64(be[24:32]),
			High: binary.BigEndian.Uint64(be[16:24]),
		},
		High: proto.UInt128{
			Low:  binary.BigEndian.Uint64(be[8:16]),
			High: binary.BigEndian.Uint64(be[0:8]),
		},
	}
}

func e2eUInt256IsZero(v proto.UInt256) bool {
	return v.Low.Low == 0 && v.Low.High == 0 && v.High.Low == 0 && v.High.High == 0
}

func e2eMixInt64(h uint64, v int64) uint64 {
	return e2eMixUint64(h, uint64(v))
}

func e2eMixUInt256(h uint64, v proto.UInt256) uint64 {
	h = e2eMixUint64(h, v.Low.Low)
	h = e2eMixUint64(h, v.Low.High)
	h = e2eMixUint64(h, v.High.Low)
	return e2eMixUint64(h, v.High.High)
}

func e2eMixBytes(h uint64, b []byte) uint64 {
	for _, v := range b {
		h ^= uint64(v)
		h *= 1099511628211
	}
	return h
}

func e2eMixUint64(h uint64, v uint64) uint64 {
	h ^= v
	h *= 1099511628211
	h ^= v >> 32
	h *= 1099511628211
	return h
}

var e2eHexTable = func() [256]byte {
	var table [256]byte
	for i := range table {
		table[i] = 0xff
	}
	for c := byte('0'); c <= '9'; c++ {
		table[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		table[c] = c - 'a' + 10
	}
	for c := byte('A'); c <= 'F'; c++ {
		table[c] = c - 'A' + 10
	}
	return table
}()

func e2eLoadJSONLFixture(tb testing.TB, path string) []byte {
	tb.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("fixture not found: %v", err)
	}
	return data
}

func e2eTargetRepeats(tb testing.TB, dataLen int) int {
	tb.Helper()
	raw := os.Getenv("LOADTEST_E2E_TARGET_BYTES")
	if raw == "" {
		return 1
	}
	target, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || target <= 0 {
		tb.Fatalf("invalid LOADTEST_E2E_TARGET_BYTES=%q", raw)
	}
	repeats := int((target + int64(dataLen) - 1) / int64(dataLen))
	if repeats < 1 {
		return 1
	}
	return repeats
}

func e2eTotalBytes(dataLen, repeats int) int64 {
	return int64(dataLen) * int64(repeats)
}

func e2eRunEasyJSON(data []byte) e2eDBDigest {
	ring := newE2ERowRing(1024, 256)
	insert := newE2EInsertBuffers(256)
	var digest e2eDBDigest
	e2ePipelineEasyJSONRowRingToProtoDB(data, ring, insert, &digest)
	return digest
}

func e2eRunRawRowRing(data []byte) e2eDBDigest {
	ring := newE2ERowRing(1024, 256)
	insert := newE2EInsertBuffers(256)
	var digest e2eDBDigest
	e2ePipelineRawLexerRowRingToProtoDB(data, ring, insert, &digest)
	return digest
}

func e2eRunProtoRing(data []byte) e2eDBDigest {
	ring := newE2EProtoRing(1024, 256)
	var digest e2eDBDigest
	e2ePipelineGeneratedLexerProtoRingPointerDB(data, ring, &digest)
	return digest
}

type e2eGCMetrics struct {
	digest       e2eDBDigest
	duration     time.Duration
	totalAlloc   uint64
	mallocs      uint64
	numGC        uint32
	pauseTotalNS uint64
	heapAlloc    uint64
}

func e2eMeasureGC(fn func(*e2eDBDigest), digest *e2eDBDigest) e2eGCMetrics {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	fn(digest)
	duration := time.Since(start)
	runtime.ReadMemStats(&after)
	return e2eGCMetrics{
		digest:       *digest,
		duration:     duration,
		totalAlloc:   after.TotalAlloc - before.TotalAlloc,
		mallocs:      after.Mallocs - before.Mallocs,
		numGC:        after.NumGC - before.NumGC,
		pauseTotalNS: after.PauseTotalNs - before.PauseTotalNs,
		heapAlloc:    after.HeapAlloc,
	}
}

func TestProtoColumnRingE2ELargeLoadGC(t *testing.T) {
	if os.Getenv("LOADTEST_E2E_GC") != "1" {
		t.Skip("set LOADTEST_E2E_GC=1 and LOADTEST_E2E_TARGET_BYTES to run large-load GC confirmation")
	}
	data := e2eLoadJSONLFixture(t, "../sqd/testdata/exchange_events.jsonl")
	repeats := e2eTargetRepeats(t, len(data))
	totalBytes := e2eTotalBytes(len(data), repeats)

	easyRing := newE2ERowRing(1024, 256)
	easyInsert := newE2EInsertBuffers(256)
	var easyDigest e2eDBDigest
	easy := e2eMeasureGC(func(digest *e2eDBDigest) {
		for i := 0; i < repeats; i++ {
			e2ePipelineEasyJSONRowRingToProtoDB(data, easyRing, easyInsert, digest)
		}
	}, &easyDigest)

	protoRing := newE2EProtoRing(1024, 256)
	var protoDigest e2eDBDigest
	protoResult := e2eMeasureGC(func(digest *e2eDBDigest) {
		for i := 0; i < repeats; i++ {
			e2ePipelineGeneratedLexerProtoRingPointerDB(data, protoRing, digest)
		}
	}, &protoDigest)

	t.Logf("large load bytes=%d repeats=%d", totalBytes, repeats)
	t.Logf("easyjson rows orders=%d positions=%d duration=%s total_alloc=%d mallocs=%d num_gc=%d pause=%s heap_alloc=%d",
		easy.digest.orderRows,
		easy.digest.positionRows,
		easy.duration.Round(time.Millisecond),
		easy.totalAlloc,
		easy.mallocs,
		easy.numGC,
		time.Duration(easy.pauseTotalNS).Round(time.Microsecond),
		easy.heapAlloc,
	)
	t.Logf("proto_ring rows orders=%d positions=%d duration=%s total_alloc=%d mallocs=%d num_gc=%d pause=%s heap_alloc=%d",
		protoResult.digest.orderRows,
		protoResult.digest.positionRows,
		protoResult.duration.Round(time.Millisecond),
		protoResult.totalAlloc,
		protoResult.mallocs,
		protoResult.numGC,
		time.Duration(protoResult.pauseTotalNS).Round(time.Microsecond),
		protoResult.heapAlloc,
	)

	if easy.digest.orderRows != protoResult.digest.orderRows || easy.digest.positionRows != protoResult.digest.positionRows {
		t.Fatalf("row count mismatch: easyjson=%+v proto=%+v", easy.digest, protoResult.digest)
	}
	if protoResult.numGC != 0 {
		t.Fatalf("proto ring triggered GC during large load: num_gc=%d", protoResult.numGC)
	}
	if protoResult.totalAlloc > 1024 || protoResult.mallocs > 8 {
		t.Fatalf("proto ring allocated unexpectedly during large load: total_alloc=%d mallocs=%d", protoResult.totalAlloc, protoResult.mallocs)
	}
}

func TestProtoColumnRingE2EParity(t *testing.T) {
	data := e2eLoadJSONLFixture(t, "../sqd/testdata/5blocks.jsonl")
	easy := e2eRunEasyJSON(data)
	raw := e2eRunRawRowRing(data)
	protoRing := e2eRunProtoRing(data)

	if easy != raw {
		t.Fatalf("raw lexer row-ring digest mismatch\n easyjson: %+v\n raw:      %+v", easy, raw)
	}
	if easy != protoRing {
		t.Fatalf("proto ring digest mismatch\n easyjson: %+v\n proto:    %+v", easy, protoRing)
	}
	if protoRing.orderRows == 0 || protoRing.positionRows == 0 {
		t.Fatalf("fixture produced no typed rows: %+v", protoRing)
	}
	t.Logf("typed rows parity: orders=%d positions=%d orderHash=%016x positionHash=%016x", protoRing.orderRows, protoRing.positionRows, protoRing.orderHash, protoRing.positionHash)
}

func BenchmarkE2E_EasyJSONDecodeRingToProtoDB(b *testing.B) {
	data := e2eLoadJSONLFixture(b, "../sqd/testdata/exchange_events.jsonl")
	repeats := e2eTargetRepeats(b, len(data))
	ring := newE2ERowRing(1024, 256)
	insert := newE2EInsertBuffers(256)
	var digest e2eDBDigest

	b.SetBytes(e2eTotalBytes(len(data), repeats))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		digest = e2eDBDigest{}
		ring.head = 0
		for r := 0; r < repeats; r++ {
			e2ePipelineEasyJSONRowRingToProtoDB(data, ring, insert, &digest)
		}
	}
	runtime.KeepAlive(digest)
	b.ReportMetric(float64(digest.orderRows), "order_rows/op")
	b.ReportMetric(float64(digest.positionRows), "position_rows/op")
}

func BenchmarkE2E_RawLexerRowRingToProtoDB(b *testing.B) {
	data := e2eLoadJSONLFixture(b, "../sqd/testdata/exchange_events.jsonl")
	repeats := e2eTargetRepeats(b, len(data))
	ring := newE2ERowRing(1024, 256)
	insert := newE2EInsertBuffers(256)
	var digest e2eDBDigest

	b.SetBytes(e2eTotalBytes(len(data), repeats))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		digest = e2eDBDigest{}
		ring.head = 0
		for r := 0; r < repeats; r++ {
			e2ePipelineRawLexerRowRingToProtoDB(data, ring, insert, &digest)
		}
	}
	runtime.KeepAlive(digest)
	b.ReportMetric(float64(digest.orderRows), "order_rows/op")
	b.ReportMetric(float64(digest.positionRows), "position_rows/op")
}

func BenchmarkE2E_GeneratedLexerProtoRingPointerDB(b *testing.B) {
	data := e2eLoadJSONLFixture(b, "../sqd/testdata/exchange_events.jsonl")
	repeats := e2eTargetRepeats(b, len(data))
	ring := newE2EProtoRing(1024, 256)
	var digest e2eDBDigest

	b.SetBytes(e2eTotalBytes(len(data), repeats))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		digest = e2eDBDigest{}
		ring.head = 0
		for r := 0; r < repeats; r++ {
			e2ePipelineGeneratedLexerProtoRingPointerDB(data, ring, &digest)
		}
	}
	runtime.KeepAlive(digest)
	b.ReportMetric(float64(digest.orderRows), "order_rows/op")
	b.ReportMetric(float64(digest.positionRows), "position_rows/op")
}
