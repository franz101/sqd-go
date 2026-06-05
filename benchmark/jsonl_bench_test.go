package main

import (
	"os"
	"testing"

	"github.com/mailru/easyjson/jlexer"
	"github.com/minio/simdjson-go"

	"github.com/franz101/sqd-go/internal/parser"
	"github.com/franz101/sqd-go/benchmark/jtypes"
)

// ─── RingBuffer: [{blocknumber, Transfers[], Positions[], evtOrderes[]}] ───

type Transfer struct {
	Taker   [20]byte
	Maker   [20]byte
	Amount  [32]byte
	AssetID [32]byte
	Fee     [32]byte
}

type Position struct {
	User    [20]byte
	TokenID [32]byte
	Amount  [32]byte
}

type BlockEntry struct {
	BlockNumber uint64
	Transfers   []Transfer
	Positions   []Position
	EvtOrder    []uint8
}

type RingBuffer struct {
	entries []BlockEntry
	head    int
	mask    int
}

func NewRingBuffer(size int) *RingBuffer {
	entries := make([]BlockEntry, size)
	for i := range entries {
		entries[i].Transfers = make([]Transfer, 0, 256)
		entries[i].Positions = make([]Position, 0, 256)
		entries[i].EvtOrder = make([]uint8, 0, 256)
	}
	return &RingBuffer{entries: entries, mask: size - 1}
}

func (rb *RingBuffer) Push(blockNum uint64) *BlockEntry {
	e := &rb.entries[rb.head&rb.mask]
	e.BlockNumber = blockNum
	e.Transfers = e.Transfers[:0]
	e.Positions = e.Positions[:0]
	e.EvtOrder = e.EvtOrder[:0]
	rb.head++
	return e
}

// ─── Pipelines ───

// FastJSON baseline
func pipelineFastJSON(data []byte, rb *RingBuffer) {
	p := parser.NewFastJSONLParser(1024)
	_ = p.Parse(data, func(block *parser.Block) error {
		e := rb.Push(block.Header.Number)
		for i := range block.Logs {
			lg := &block.Logs[i]
			// Inline fillFromLog to avoid extra function call
			if len(lg.Data) != 322 || len(lg.Topics) < 3 {
				continue
			}
			var tf Transfer
			var pos Position
			hexDecode20(&tf.Maker, lg.Topics[2])
			if len(lg.Topics) > 3 {
				hexDecode20(&tf.Taker, lg.Topics[3])
			}
			pos.User = tf.Taker
			hexDecode32(&tf.AssetID, lg.Data, 2)
			hexDecode32(&pos.TokenID, lg.Data, 2+64)
			hexDecode32(&tf.Amount, lg.Data, 2+128)
			hexDecode32(&pos.Amount, lg.Data, 2+192)
			hexDecode32(&tf.Fee, lg.Data, 2+256)
			e.Transfers = append(e.Transfers, tf)
			e.EvtOrder = append(e.EvtOrder, 1)
			e.Positions = append(e.Positions, pos)
			e.EvtOrder = append(e.EvtOrder, 2)
		}
		return nil
	})
}

// ─── easyjson codegen (existing champion) ───

func pipelineEasyJSONCodegen(data []byte, rb *RingBuffer) {
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

		e := rb.Push(block.Header.Number)
		for i := range block.Logs {
			lg := &block.Logs[i]
			if len(lg.Data) != 322 || len(lg.Topics) < 3 {
				continue
			}
			var tf Transfer
			var pos Position
			hexDecode20(&tf.Maker, lg.Topics[2])
			if len(lg.Topics) > 3 {
				hexDecode20(&tf.Taker, lg.Topics[3])
			}
			pos.User = tf.Taker
			hexDecode32(&tf.AssetID, lg.Data, 2)
			hexDecode32(&pos.TokenID, lg.Data, 2+64)
			hexDecode32(&tf.Amount, lg.Data, 2+128)
			hexDecode32(&pos.Amount, lg.Data, 2+192)
			hexDecode32(&tf.Fee, lg.Data, 2+256)
			e.Transfers = append(e.Transfers, tf)
			e.EvtOrder = append(e.EvtOrder, 1)
			e.Positions = append(e.Positions, pos)
			e.EvtOrder = append(e.EvtOrder, 2)
		}
	}
}

// ─── easyjson raw lexer: walk JSON tokens directly into ring buffer ───
// No intermediate jtypes.JSONLBlock struct. No []JSONLLog slice.

func pipelineEasyJSONRaw(data []byte, rb *RingBuffer) {
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

		// Parse: {"header": {"number": N, ...}, "logs": [{...}, ...]}
		var blockNum uint64
		var e *BlockEntry
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
					default:
						l.Skip()
					}
					l.WantComma()
				}
				l.Delim('}')
			case "logs":
				e = rb.Push(blockNum)
				l.Delim('[')
				for !l.IsDelim(']') {
					l.Delim('{')
					dataHex = ""
					var topicIdx int
					for !l.IsDelim('}') {
						lkey := l.UnsafeFieldName(false)
						l.WantColon()
						switch lkey {
						case "data":
							dataHex = string(l.UnsafeString())
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

					// Inline fillFromLog
					if len(dataHex) == 322 && topicIdx >= 3 {
						var tf Transfer
						var pos Position
						hexDecode20(&tf.Maker, topics[2])
						if topicIdx > 3 {
							hexDecode20(&tf.Taker, topics[3])
						}
						pos.User = tf.Taker
						hexDecode32(&tf.AssetID, dataHex, 2)
						hexDecode32(&pos.TokenID, dataHex, 2+64)
						hexDecode32(&tf.Amount, dataHex, 2+128)
						hexDecode32(&pos.Amount, dataHex, 2+192)
						hexDecode32(&tf.Fee, dataHex, 2+256)
						e.Transfers = append(e.Transfers, tf)
						e.EvtOrder = append(e.EvtOrder, 1)
						e.Positions = append(e.Positions, pos)
						e.EvtOrder = append(e.EvtOrder, 2)
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

// ─── simdjson NDJSON → direct ring buffer ───

func pipelineSimdjsonReused(pj *simdjson.ParsedJson, rb *RingBuffer) {
	_ = pj.ForEach(func(i simdjson.Iter) error {
		var blockNum uint64
		var dataHex string
		var topics [4]string

		objIter := i
		for {
			typ := objIter.Advance()
			if typ == simdjson.TypeNone {
				break
			}
			key, _ := objIter.String()
			var valIter simdjson.Iter
			objIter.AdvanceIter(&valIter)

			switch key {
			case "header":
				valIter.AdvanceInto()
				for {
					t := valIter.Advance()
					if t == simdjson.TypeNone {
						break
					}
					k, _ := valIter.String()
					valIter.Advance()
					if k == "number" {
						blockNum, _ = valIter.Uint()
					}
				}
			case "logs":
				arrIter := valIter
				arrIter.AdvanceInto()
				e := rb.Push(blockNum)
				for {
					at := arrIter.Advance()
					if at == simdjson.TypeNone {
						break
					}
					var logIter simdjson.Iter
					arrIter.AdvanceIter(&logIter)
					logIter.AdvanceInto()

					dataHex = ""
					var topicIdx int
					for {
						lt := logIter.Advance()
						if lt == simdjson.TypeNone {
							break
						}
						lk, _ := logIter.String()
						logIter.Advance()
						switch lk {
						case "data":
							dataHex, _ = logIter.String()
						case "topics":
							tarr := logIter
							tarr.AdvanceInto()
							topicIdx = 0
							for {
								tt := tarr.Advance()
								if tt == simdjson.TypeNone {
									break
								}
								if topicIdx < 4 {
									topics[topicIdx], _ = tarr.String()
								}
								topicIdx++
							}
						}
					}
					// Inline fillFromLog
					if len(dataHex) == 322 && topicIdx >= 3 {
						var tf Transfer
						var pos Position
						hexDecode20(&tf.Maker, topics[2])
						if topicIdx > 3 {
							hexDecode20(&tf.Taker, topics[3])
						}
						pos.User = tf.Taker
						hexDecode32(&tf.AssetID, dataHex, 2)
						hexDecode32(&pos.TokenID, dataHex, 2+64)
						hexDecode32(&tf.Amount, dataHex, 2+128)
						hexDecode32(&pos.Amount, dataHex, 2+192)
						hexDecode32(&tf.Fee, dataHex, 2+256)
						e.Transfers = append(e.Transfers, tf)
						e.EvtOrder = append(e.EvtOrder, 1)
						e.Positions = append(e.Positions, pos)
						e.EvtOrder = append(e.EvtOrder, 2)
					}
				}
			}
		}
		return nil
	})
}

// ─── Benchmarks ───

var sampleFile = "/home/dev/CODING/polymarket_lowram/sqd-go/samples/exchange_events.jsonl"

var simdjsonParsed *simdjson.ParsedJson

func init() {
	data, err := os.ReadFile(sampleFile)
	if err == nil {
		simdjsonParsed, _ = simdjson.ParseND(data, nil)
	}
}

func BenchmarkParseOnly(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := parser.NewFastJSONLParser(1024)
		var bc, lc int
		_ = p.Parse(data, func(b *parser.Block) error { bc++; lc += len(b.Logs); return nil })
		_, _ = bc, lc
	}
}

func BenchmarkFastJSON(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	rb := NewRingBuffer(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.head = 0
		pipelineFastJSON(data, rb)
	}
}

func BenchmarkEasyJSONCodegen(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	rb := NewRingBuffer(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.head = 0
		pipelineEasyJSONCodegen(data, rb)
	}
}

func BenchmarkEasyJSONRaw(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	rb := NewRingBuffer(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.head = 0
		pipelineEasyJSONRaw(data, rb)
	}
}

func BenchmarkSimdjson(b *testing.B) {
	if simdjsonParsed == nil {
		b.Skip("simdjson parse failed at init")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb := NewRingBuffer(1024)
		pipelineSimdjsonReused(simdjsonParsed, rb)
	}
}

// Alloc profiles

func BenchmarkEasyJSONCodegenAlloc(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	rb := NewRingBuffer(1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rb.head = 0
		pipelineEasyJSONCodegen(data, rb)
	}
}

func BenchmarkEasyJSONRawAlloc(b *testing.B) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		b.Skipf("sample file not found: %v", err)
	}
	rb := NewRingBuffer(1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rb.head = 0
		pipelineEasyJSONRaw(data, rb)
	}
}

func BenchmarkSimdjsonAlloc(b *testing.B) {
	if simdjsonParsed == nil {
		b.Skip("simdjson parse failed at init")
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rb := NewRingBuffer(1024)
		pipelineSimdjsonReused(simdjsonParsed, rb)
	}
}
