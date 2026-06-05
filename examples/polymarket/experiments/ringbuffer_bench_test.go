package experiments

import (
	"bytes"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/franz101/sqd-go/benchmark/jtypes"
	"github.com/mailru/easyjson/jlexer"
)

var hexTable [256]uint8

func init() {
	for i := range hexTable {
		hexTable[i] = 0xFF
	}
	for c := byte('0'); c <= '9'; c++ {
		hexTable[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		hexTable[c] = c - 'a' + 10
	}
	for c := byte('A'); c <= 'F'; c++ {
		hexTable[c] = c - 'A' + 10
	}
}

func hexDecode32(dst *[32]byte, src string, off int) {
	_ = src[off+63]
	dst[0] = hexTable[src[off+0]]<<4 | hexTable[src[off+1]]
	dst[1] = hexTable[src[off+2]]<<4 | hexTable[src[off+3]]
	dst[2] = hexTable[src[off+4]]<<4 | hexTable[src[off+5]]
	dst[3] = hexTable[src[off+6]]<<4 | hexTable[src[off+7]]
	dst[4] = hexTable[src[off+8]]<<4 | hexTable[src[off+9]]
	dst[5] = hexTable[src[off+10]]<<4 | hexTable[src[off+11]]
	dst[6] = hexTable[src[off+12]]<<4 | hexTable[src[off+13]]
	dst[7] = hexTable[src[off+14]]<<4 | hexTable[src[off+15]]
	dst[8] = hexTable[src[off+16]]<<4 | hexTable[src[off+17]]
	dst[9] = hexTable[src[off+18]]<<4 | hexTable[src[off+19]]
	dst[10] = hexTable[src[off+20]]<<4 | hexTable[src[off+21]]
	dst[11] = hexTable[src[off+22]]<<4 | hexTable[src[off+23]]
	dst[12] = hexTable[src[off+24]]<<4 | hexTable[src[off+25]]
	dst[13] = hexTable[src[off+26]]<<4 | hexTable[src[off+27]]
	dst[14] = hexTable[src[off+28]]<<4 | hexTable[src[off+29]]
	dst[15] = hexTable[src[off+30]]<<4 | hexTable[src[off+31]]
	dst[16] = hexTable[src[off+32]]<<4 | hexTable[src[off+33]]
	dst[17] = hexTable[src[off+34]]<<4 | hexTable[src[off+35]]
	dst[18] = hexTable[src[off+36]]<<4 | hexTable[src[off+37]]
	dst[19] = hexTable[src[off+38]]<<4 | hexTable[src[off+39]]
	dst[20] = hexTable[src[off+40]]<<4 | hexTable[src[off+41]]
	dst[21] = hexTable[src[off+42]]<<4 | hexTable[src[off+43]]
	dst[22] = hexTable[src[off+44]]<<4 | hexTable[src[off+45]]
	dst[23] = hexTable[src[off+46]]<<4 | hexTable[src[off+47]]
	dst[24] = hexTable[src[off+48]]<<4 | hexTable[src[off+49]]
	dst[25] = hexTable[src[off+50]]<<4 | hexTable[src[off+51]]
	dst[26] = hexTable[src[off+52]]<<4 | hexTable[src[off+53]]
	dst[27] = hexTable[src[off+54]]<<4 | hexTable[src[off+55]]
	dst[28] = hexTable[src[off+56]]<<4 | hexTable[src[off+57]]
	dst[29] = hexTable[src[off+58]]<<4 | hexTable[src[off+59]]
	dst[30] = hexTable[src[off+60]]<<4 | hexTable[src[off+61]]
	dst[31] = hexTable[src[off+62]]<<4 | hexTable[src[off+63]]
}

func hexDecode20(dst *[20]byte, s string) {
	if len(s) < 42 {
		return
	}
	_ = s[41]
	dst[0] = hexTable[s[2+0]]<<4 | hexTable[s[3+0]]
	dst[1] = hexTable[s[2+2]]<<4 | hexTable[s[3+2]]
	dst[2] = hexTable[s[2+4]]<<4 | hexTable[s[3+4]]
	dst[3] = hexTable[s[2+6]]<<4 | hexTable[s[3+6]]
	dst[4] = hexTable[s[2+8]]<<4 | hexTable[s[3+8]]
	dst[5] = hexTable[s[2+10]]<<4 | hexTable[s[3+10]]
	dst[6] = hexTable[s[2+12]]<<4 | hexTable[s[3+12]]
	dst[7] = hexTable[s[2+14]]<<4 | hexTable[s[3+14]]
	dst[8] = hexTable[s[2+16]]<<4 | hexTable[s[3+16]]
	dst[9] = hexTable[s[2+18]]<<4 | hexTable[s[3+18]]
	dst[10] = hexTable[s[2+20]]<<4 | hexTable[s[3+20]]
	dst[11] = hexTable[s[2+22]]<<4 | hexTable[s[3+22]]
	dst[12] = hexTable[s[2+24]]<<4 | hexTable[s[3+24]]
	dst[13] = hexTable[s[2+26]]<<4 | hexTable[s[3+26]]
	dst[14] = hexTable[s[2+28]]<<4 | hexTable[s[3+28]]
	dst[15] = hexTable[s[2+30]]<<4 | hexTable[s[3+30]]
	dst[16] = hexTable[s[2+32]]<<4 | hexTable[s[3+32]]
	dst[17] = hexTable[s[2+34]]<<4 | hexTable[s[3+34]]
	dst[18] = hexTable[s[2+36]]<<4 | hexTable[s[3+36]]
	dst[19] = hexTable[s[2+38]]<<4 | hexTable[s[3+38]]
}

func readTestData(b *testing.B) []byte {
	data, err := os.ReadFile("../../../sqd/testdata/exchange_events.jsonl")
	if err != nil {
		b.Fatalf("failed to read test data: %v", err)
	}
	return data
}

func BenchmarkExperiment1_FastJSON_ABIDecode(b *testing.B) {
	data := readTestData(b)
	rb, _ := generated.NewOrderedHistoricRingBuffer(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := parser.NewFastJSONLParser(1024)
		_ = p.Parse(data, func(block *parser.Block) error {
			var decodedLogs []generated.DecodedLog
			for _, lg := range block.Logs {
				dataBytes := common.FromHex(lg.Data)
				decoded, err := generated.UnpackLog(lg.Address, lg.Topics, dataBytes)
				if err != nil || decoded == nil {
					continue
				}
				decodedLogs = append(decodedLogs, *decoded)
			}
			if len(decodedLogs) > 0 {
				rb.Push(block.Header.Number, block.Header.Hash, decodedLogs)
			}
			return nil
		})
	}
}

func BenchmarkExperiment2_EasyJSON_Codegen(b *testing.B) {
	data := readTestData(b)
	rb, _ := generated.NewOrderedHistoricRingBuffer(1024)
	lexer := &jlexer.Lexer{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rest := data
		for len(rest) > 0 {
			lineEnd := bytes.IndexByte(rest, '\n')
			var line []byte
			if lineEnd >= 0 {
				line = rest[:lineEnd]
				rest = rest[lineEnd+1:]
			} else {
				line = rest
				rest = nil
			}
			if len(line) == 0 {
				continue
			}

			var block jtypes.JSONLBlock
			lexer.Data = line
			block.UnmarshalEasyJSON(lexer)
			if !lexer.Ok() {
				continue
			}

			var decodedLogs []generated.DecodedLog
			for i := range block.Logs {
				lg := &block.Logs[i]
				dataBytes := common.FromHex(lg.Data)
				decoded, err := generated.UnpackLog(lg.Address, lg.Topics, dataBytes)
				if err != nil || decoded == nil {
					continue
				}
				decodedLogs = append(decodedLogs, *decoded)
			}
			if len(decodedLogs) > 0 {
				rb.Push(block.Header.Number, block.Header.Hash, decodedLogs)
			}
		}
	}
}

func BenchmarkExperiment3_EasyJSON_RawLexer(b *testing.B) {
	data := readTestData(b)
	rb, _ := generated.NewOrderedHistoricRingBuffer(1024)
	lexer := &jlexer.Lexer{}
	var topics [4]string
	
	// Pre-parse the target address and topic0 hex strings into lookup values
	targetAddr := "0x4bfb41d5b3570defd03c39a9a4d8de6bd8b8982e"
	targetTopic0 := "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rest := data
		for len(rest) > 0 {
			lineEnd := bytes.IndexByte(rest, '\n')
			var line []byte
			if lineEnd >= 0 {
				line = rest[:lineEnd]
				rest = rest[lineEnd+1:]
			} else {
				line = rest
				rest = nil
			}
			if len(line) == 0 {
				continue
			}

			lexer.Data = line
			var blockNum uint64
			var blockHash string
			var block *generated.ParsedBlock

			lexer.Delim('{')
			for !lexer.IsDelim('}') {
				key := lexer.UnsafeFieldName(false)
				lexer.WantColon()
				switch key {
				case "header":
					lexer.Delim('{')
					for !lexer.IsDelim('}') {
						hkey := lexer.UnsafeFieldName(false)
						lexer.WantColon()
						switch hkey {
						case "number":
							blockNum = lexer.Uint64()
						case "hash":
							blockHash = string(lexer.UnsafeString())
						default:
							lexer.Skip()
						}
						lexer.WantComma()
					}
					lexer.Delim('}')
				case "logs":
					block = rb.NextSlot(blockNum, blockHash)
					lexer.Delim('[')
					for !lexer.IsDelim(']') {
						lexer.Delim('{')
						var address string
						var dataHex string
						var topicCount int

						for !lexer.IsDelim('}') {
							lkey := lexer.UnsafeFieldName(false)
							lexer.WantColon()
							switch lkey {
							case "address":
								address = string(lexer.UnsafeString())
							case "data":
								dataHex = string(lexer.UnsafeString())
							case "topics":
								lexer.Delim('[')
								topicCount = 0
								for !lexer.IsDelim(']') {
									if topicCount < 4 {
										topics[topicCount] = string(lexer.UnsafeString())
									} else {
										lexer.Skip()
									}
									topicCount++
									lexer.WantComma()
								}
								lexer.Delim(']')
							default:
								lexer.Skip()
							}
							lexer.WantComma()
						}
						lexer.Delim('}')

						// Filter and direct decode ExchangeOrderFilled
						if address == targetAddr && topicCount >= 4 && topics[0] == targetTopic0 {
							// Append directly to block slices
							block.ExchangeOrderFilleds = append(block.ExchangeOrderFilleds, generated.ExchangeOrderFilled{})
							ev := &block.ExchangeOrderFilleds[len(block.ExchangeOrderFilleds)-1]

							// Set metadata
							ev.BlockNumber = blockNum
							ev.TransactionIndex = 0 // In e2e test parsed log it gets log Entry tx index. 
							// Here we skip it or mock for simple speed test.

							// Direct hex decode indexed topics into Go structs
							hexDecode20((*[20]byte)(&ev.Maker), topics[2])
							hexDecode20((*[20]byte)(&ev.Taker), topics[3])

							// Direct hex decode data words into uint256.Int fields
							if len(dataHex) >= 2+320 { // 5 words = 320 hex chars + "0x"
								var word [32]byte
								hexDecode32(&word, dataHex, 2)
								ev.MakerAssetID.SetBytes32(word[:])

								hexDecode32(&word, dataHex, 2+64)
								ev.TakerAssetID.SetBytes32(word[:])

								hexDecode32(&word, dataHex, 2+128)
								ev.MakerAmountFilled.SetBytes32(word[:])

								hexDecode32(&word, dataHex, 2+192)
								ev.TakerAmountFilled.SetBytes32(word[:])

								hexDecode32(&word, dataHex, 2+256)
								ev.Fee.SetBytes32(word[:])
							}

							block.Sequence = append(block.Sequence, uint8(generated.EventTypeExchangeOrderFilled))
						}
						lexer.WantComma()
					}
					lexer.Delim(']')
				default:
					lexer.Skip()
				}
				lexer.WantComma()
			}
			lexer.Delim('}')
		}
	}
}
