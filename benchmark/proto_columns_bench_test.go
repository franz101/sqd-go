package main

import (
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/franz101/sqd-go/benchmark/jtypes"
	"github.com/mailru/easyjson/jlexer"
)

// ─── Proto-Column Block Layout ───

// ProtoExchangeBlock holds parsed Exchange events in columnar proto format.
// This replaces the Transfer/Position structs with ClickHouse-native columns.
type ProtoExchangeBlock struct {
	// Metadata columns
	BlockNumber    proto.ColUInt64
	BlockTimestamp  proto.ColDateTime64
	TxIndex         proto.ColUInt64
	LogIndex        proto.ColUInt64

	// Transfer event columns
	Maker       proto.ColFixedStr // 20 bytes
	Taker       proto.ColFixedStr // 20 bytes
	AssetID     proto.ColUInt256
	Amount      proto.ColUInt256
	Fee         proto.ColUInt256

	// For tracking event order (Transfer vs Position)
	EventType   proto.ColUInt8 // 1 = Transfer, 2 = Position
}

func (b *ProtoExchangeBlock) Init() {
	b.Maker.SetSize(20)
	b.Taker.SetSize(20)
	b.BlockTimestamp.WithPrecision(proto.Precision(3))
	b.BlockTimestamp.WithLocation(time.UTC)
}

func (b *ProtoExchangeBlock) Reset() {
	b.BlockNumber.Reset()
	b.BlockTimestamp.Reset()
	b.TxIndex.Reset()
	b.LogIndex.Reset()
	b.Maker.Reset()
	b.Taker.Reset()
	b.AssetID.Reset()
	b.Amount.Reset()
	b.Fee.Reset()
	b.EventType.Reset()
}

// ─── Pipeline: jlexer → proto columns ───

func pipelineJLexerToProto(data []byte, block *ProtoExchangeBlock) {
	l := &jlexer.Lexer{}
	var topics [4]string
	var dataHex string

	rest := data
	for len(rest) > 0 {
		lineEnd := 0
		for lineEnd < len(rest) && rest[lineEnd] != '\n' {
			lineEnd++
		}
		line := rest[:lineEnd]
		if lineEnd < len(rest) {
			rest = rest[lineEnd+1:]
		} else {
			rest = nil
		}
		if len(line) == 0 {
			continue
		}

		l.Data = line

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
				l.Delim('[')
				for !l.IsDelim(']') {
					l.Delim('{')
					dataHex = ""
					var topicIdx int
					var txIndex, logIndex uint64

					for !l.IsDelim('}') {
						lkey := l.UnsafeFieldName(false)
						l.WantColon()
						switch lkey {
						case "data":
							dataHex = l.UnsafeString()
						case "transactionIndex":
							txIndex = l.Uint64()
						case "logIndex":
							logIndex = l.Uint64()
						case "topics":
							l.Delim('[')
							topicIdx = 0
							for !l.IsDelim(']') {
								if topicIdx < 4 {
									topics[topicIdx] = l.UnsafeString()
								} else {
									l.Skip()
								}
								topicIdx++
								l.WantComma()
							}
							l.Delim(']')
						default:
							l.Skip()
						}
						l.WantComma()
					}
					l.Delim('}')

					// Parse Transfer events: data is 322 chars, topics has address
					if len(dataHex) == 322 && topicIdx >= 3 {
						var maker [20]byte
						var taker [20]byte

						hexDecode20(&maker, topics[2])
						if topicIdx > 3 {
							hexDecode20(&taker, topics[3])
						}

						// Write directly to proto columns
						block.BlockNumber.Append(blockNum)
						block.BlockTimestamp.Append(time.Unix(int64(blockTimestamp), 0))
						block.TxIndex.Append(txIndex)
						block.LogIndex.Append(logIndex)

						// Transfer event
						block.EventType.Append(1)
						block.Maker.Append(maker[:])
						block.Taker.Append(taker[:])
						block.AssetID.Append(uint256FromHex(dataHex, 2))
						block.Amount.Append(uint256FromHex(dataHex, 2+128))
						block.Fee.Append(uint256FromHex(dataHex, 2+256))

						// Position event (shares same data, different interpretation)
						block.EventType.Append(2)
						// Position: Taker is User, TokenID is AssetID
						block.Maker.Append(maker[:])
						block.Taker.Append(taker[:]) // User
						block.AssetID.Append(uint256FromHex(dataHex, 2+64)) // TokenID
						block.Amount.Append(uint256FromHex(dataHex, 2+192)) // Position Amount
						block.Fee.Append(proto.UInt256{})
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
	}
}

// ─── Benchmarks ───

func BenchmarkJLexerToProtoColumns(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	block := &ProtoExchangeBlock{}
	block.Init()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		block.Reset()
		pipelineJLexerToProto(data, block)
	}
}

func BenchmarkJLexerToProtoColumnsAlloc(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	block := &ProtoExchangeBlock{}
	block.Init()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		block.Reset()
		pipelineJLexerToProto(data, block)
	}
}

// ─── Standard Pipeline Layout ───

type EventMeta struct {
	BlockNumber    uint64
	BlockTimestamp time.Time
	TxIndex        uint64
	LogIndex       uint64
}

type TransferEvent struct {
	Maker   [20]byte
	Taker   [20]byte
	AssetID proto.UInt256
	Amount  proto.UInt256
	Fee     proto.UInt256
}

type PositionEvent struct {
	User    [20]byte
	TokenID proto.UInt256
	Amount  proto.UInt256
}

type TransferBatch struct {
	BlockNumber    proto.ColUInt64
	BlockTimestamp proto.ColDateTime64
	TxIndex        proto.ColUInt64
	LogIndex       proto.ColUInt64
	Maker          proto.ColFixedStr
	Taker          proto.ColFixedStr
	AssetID        proto.ColUInt256
	Amount         proto.ColUInt256
	Fee            proto.ColUInt256
}

func (b *TransferBatch) Init() {
	b.Maker.SetSize(20)
	b.Taker.SetSize(20)
	b.BlockTimestamp.WithPrecision(proto.Precision(3))
	b.BlockTimestamp.WithLocation(time.UTC)
}

func (b *TransferBatch) Reset() {
	b.BlockNumber.Reset()
	b.BlockTimestamp.Reset()
	b.TxIndex.Reset()
	b.LogIndex.Reset()
	b.Maker.Reset()
	b.Taker.Reset()
	b.AssetID.Reset()
	b.Amount.Reset()
	b.Fee.Reset()
}

func (b *TransferBatch) Append(meta EventMeta, value any) bool {
	ev, ok := value.(*TransferEvent)
	if !ok {
		return false
	}
	b.BlockNumber.Append(meta.BlockNumber)
	b.BlockTimestamp.Append(meta.BlockTimestamp)
	b.TxIndex.Append(meta.TxIndex)
	b.LogIndex.Append(meta.LogIndex)
	b.Maker.Append(ev.Maker[:])
	b.Taker.Append(ev.Taker[:])
	b.AssetID.Append(ev.AssetID)
	b.Amount.Append(ev.Amount)
	b.Fee.Append(ev.Fee)
	return true
}

type PositionBatch struct {
	BlockNumber    proto.ColUInt64
	BlockTimestamp proto.ColDateTime64
	TxIndex        proto.ColUInt64
	LogIndex       proto.ColUInt64
	User           proto.ColFixedStr
	TokenID        proto.ColUInt256
	Amount         proto.ColUInt256
}

func (b *PositionBatch) Init() {
	b.User.SetSize(20)
	b.BlockTimestamp.WithPrecision(proto.Precision(3))
	b.BlockTimestamp.WithLocation(time.UTC)
}

func (b *PositionBatch) Reset() {
	b.BlockNumber.Reset()
	b.BlockTimestamp.Reset()
	b.TxIndex.Reset()
	b.LogIndex.Reset()
	b.User.Reset()
	b.TokenID.Reset()
	b.Amount.Reset()
}

func (b *PositionBatch) Append(meta EventMeta, value any) bool {
	ev, ok := value.(*PositionEvent)
	if !ok {
		return false
	}
	b.BlockNumber.Append(meta.BlockNumber)
	b.BlockTimestamp.Append(meta.BlockTimestamp)
	b.TxIndex.Append(meta.TxIndex)
	b.LogIndex.Append(meta.LogIndex)
	b.User.Append(ev.User[:])
	b.TokenID.Append(ev.TokenID)
	b.Amount.Append(ev.Amount)
	return true
}

func pipelineStandard(data []byte, tfBatch *TransferBatch, posBatch *PositionBatch) {
	var block jtypes.JSONLBlock
	l := &jlexer.Lexer{}

	rest := data
	for len(rest) > 0 {
		lineEnd := 0
		for lineEnd < len(rest) && rest[lineEnd] != '\n' {
			lineEnd++
		}
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

		l.Data = line
		block.UnmarshalEasyJSON(l)
		if !l.Ok() {
			continue
		}

		blockMeta := EventMeta{
			BlockNumber:    block.Header.Number,
			BlockTimestamp: time.Unix(int64(block.Header.Timestamp), 0),
		}

		for i := range block.Logs {
			lg := &block.Logs[i]
			if len(lg.Data) != 322 || len(lg.Topics) < 3 {
				continue
			}

			// Simulated ABI Decode: parses into Go structs
			var tf TransferEvent
			hexDecode20(&tf.Maker, lg.Topics[2])
			if len(lg.Topics) > 3 {
				hexDecode20(&tf.Taker, lg.Topics[3])
			}
			tf.AssetID = uint256FromHex(lg.Data, 2)
			tf.Amount = uint256FromHex(lg.Data, 2+128)
			tf.Fee = uint256FromHex(lg.Data, 2+256)

			var pos PositionEvent
			pos.User = tf.Taker
			pos.TokenID = uint256FromHex(lg.Data, 2+64)
			pos.Amount = uint256FromHex(lg.Data, 2+192)

			// Standard framework logic: append to typed batches via any-based Append
			meta := EventMeta{
				BlockNumber:    blockMeta.BlockNumber,
				BlockTimestamp: blockMeta.BlockTimestamp,
				TxIndex:        lg.TransactionIndex,
				LogIndex:       lg.LogIndex,
			}
			tfBatch.Append(meta, &tf)
			posBatch.Append(meta, &pos)
		}
	}
}

func BenchmarkStandardPipeline(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	tfBatch := &TransferBatch{}
	tfBatch.Init()
	posBatch := &PositionBatch{}
	posBatch.Init()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tfBatch.Reset()
		posBatch.Reset()
		pipelineStandard(data, tfBatch, posBatch)
	}
}

func BenchmarkStandardPipelineAlloc(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	tfBatch := &TransferBatch{}
	tfBatch.Init()
	posBatch := &PositionBatch{}
	posBatch.Init()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tfBatch.Reset()
		posBatch.Reset()
		pipelineStandard(data, tfBatch, posBatch)
	}
}

// ─── Preallocated Ring Buffer Slot & Lex-to-Insert Pipeline ───

type TargetTransfer struct {
	Maker   [20]byte
	Taker   [20]byte
	AssetID proto.UInt256
	Amount  proto.UInt256
	Fee     proto.UInt256
}

type TargetPosition struct {
	User    [20]byte
	TokenID proto.UInt256
	Amount  proto.UInt256
}

type ParsedBlockSlot struct {
	BlockNumber uint64
	Transfers   []TargetTransfer
	Positions   []TargetPosition
}

func (s *ParsedBlockSlot) Reset() {
	s.Transfers = s.Transfers[:0]
	s.Positions = s.Positions[:0]
}

func pipelineLexToRingBufferAndInsert(data []byte, slot *ParsedBlockSlot, tfBatch *TransferBatch, posBatch *PositionBatch) {
	l := &jlexer.Lexer{}
	var topics [4]string
	var dataHex string

	rest := data
	for len(rest) > 0 {
		lineEnd := 0
		for lineEnd < len(rest) && rest[lineEnd] != '\n' {
			lineEnd++
		}
		line := rest[:lineEnd]
		if lineEnd < len(rest) {
			rest = rest[lineEnd+1:]
		} else {
			rest = nil
		}
		if len(line) == 0 {
			continue
		}

		l.Data = line
		slot.Reset()

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
				slot.BlockNumber = blockNum
				l.Delim('[')
				for !l.IsDelim(']') {
					l.Delim('{')
					dataHex = ""
					var topicIdx int
					var txIndex, logIndex uint64
					_ = txIndex
					_ = logIndex

					for !l.IsDelim('}') {
						lkey := l.UnsafeFieldName(false)
						l.WantColon()
						switch lkey {
						case "data":
							dataHex = l.UnsafeString()
						case "transactionIndex":
							txIndex = l.Uint64()
						case "logIndex":
							logIndex = l.Uint64()
						case "topics":
							l.Delim('[')
							topicIdx = 0
							for !l.IsDelim(']') {
								if topicIdx < 4 {
									topics[topicIdx] = l.UnsafeString()
								} else {
									l.Skip()
								}
								topicIdx++
								l.WantComma()
							}
							l.Delim(']')
						default:
							l.Skip()
						}
						l.WantComma()
					}
					l.Delim('}')

					// Lex directly into preallocated Ring Buffer slot
					if len(dataHex) == 322 && topicIdx >= 3 {
						var tf TargetTransfer
						hexDecode20(&tf.Maker, topics[2])
						if topicIdx > 3 {
							hexDecode20(&tf.Taker, topics[3])
						}
						tf.AssetID = uint256FromHex(dataHex, 2)
						tf.Amount = uint256FromHex(dataHex, 2+128)
						tf.Fee = uint256FromHex(dataHex, 2+256)

						slot.Transfers = append(slot.Transfers, tf)

						var pos TargetPosition
						pos.User = tf.Taker
						pos.TokenID = uint256FromHex(dataHex, 2+64)
						pos.Amount = uint256FromHex(dataHex, 2+192)

						slot.Positions = append(slot.Positions, pos)
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

		// Now write directly from the ringbuffer slot to ClickHouse columns
		meta := EventMeta{
			BlockNumber:    slot.BlockNumber,
			BlockTimestamp: time.Unix(int64(blockTimestamp), 0),
		}

		for i := range slot.Transfers {
			tf := &slot.Transfers[i]
			tfBatch.BlockNumber.Append(meta.BlockNumber)
			tfBatch.BlockTimestamp.Append(meta.BlockTimestamp)
			tfBatch.TxIndex.Append(meta.TxIndex)
			tfBatch.LogIndex.Append(meta.LogIndex)
			tfBatch.Maker.Append(tf.Maker[:])
			tfBatch.Taker.Append(tf.Taker[:])
			tfBatch.AssetID.Append(tf.AssetID)
			tfBatch.Amount.Append(tf.Amount)
			tfBatch.Fee.Append(tf.Fee)
		}

		for i := range slot.Positions {
			pos := &slot.Positions[i]
			posBatch.BlockNumber.Append(meta.BlockNumber)
			posBatch.BlockTimestamp.Append(meta.BlockTimestamp)
			posBatch.TxIndex.Append(meta.TxIndex)
			posBatch.LogIndex.Append(meta.LogIndex)
			posBatch.User.Append(pos.User[:])
			posBatch.TokenID.Append(pos.TokenID)
			posBatch.Amount.Append(pos.Amount)
		}
	}
}

func BenchmarkLexToRingBufferAndInsert(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	slot := &ParsedBlockSlot{
		Transfers: make([]TargetTransfer, 0, 1024),
		Positions: make([]TargetPosition, 0, 1024),
	}
	tfBatch := &TransferBatch{}
	tfBatch.Init()
	posBatch := &PositionBatch{}
	posBatch.Init()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slot.Reset()
		tfBatch.Reset()
		posBatch.Reset()
		pipelineLexToRingBufferAndInsert(data, slot, tfBatch, posBatch)
	}
}

func BenchmarkLexToRingBufferAndInsertAlloc(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	slot := &ParsedBlockSlot{
		Transfers: make([]TargetTransfer, 0, 1024),
		Positions: make([]TargetPosition, 0, 1024),
	}
	tfBatch := &TransferBatch{}
	tfBatch.Init()
	posBatch := &PositionBatch{}
	posBatch.Init()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		slot.Reset()
		tfBatch.Reset()
		posBatch.Reset()
		pipelineLexToRingBufferAndInsert(data, slot, tfBatch, posBatch)
	}
}
