package codegen

import (
	"bytes"
	"fmt"
	"go/format"
	"strings"
)

func generateParserGo(events []eventSpec) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString("type InsertBatches struct {\n")

	for _, ev := range events {
		buf.WriteString(fmt.Sprintf("\t%s *%sBatch\n", ev.GoTypeName, ev.GoTypeName))
	}

	buf.WriteString(`}

func NewInsertBatches() *InsertBatches {
	return &InsertBatches{
`)

	for _, ev := range events {
		buf.WriteString(fmt.Sprintf("\t\t%s: New%sBatch(),\n", ev.GoTypeName, ev.GoTypeName))
	}

	buf.WriteString(`	}
}

func (b *InsertBatches) Reset() {
`)

	for _, ev := range events {
		buf.WriteString(fmt.Sprintf("\tb.%s.Reset()\n", ev.GoTypeName))
	}

	buf.WriteString(`}

func (b *InsertBatches) Insert(ctx context.Context, conn *ch.Client, db string) error {
`)

	for _, ev := range events {
		buf.WriteString(fmt.Sprintf(`	if b.%s.Rows() > 0 {
		if err := InsertEventBatch(ctx, conn, db, b.%s); err != nil {
			return fmt.Errorf("insert %s: %%w", err)
		}
	}
`, ev.GoTypeName, ev.GoTypeName, ev.TableName))
	}

	buf.WriteString(`	return nil
}

func ParseJSONL(data []byte, batches *InsertBatches, ring *OrderedHistoricRingBuffer) (uint64, error) {
	return ParseJSONLV2(data, batches, ring, nil)
}

func ParseJSONLV2(data []byte, batches *InsertBatches, ring *OrderedHistoricRingBuffer, onBlock func(*ParsedBlock) error) (uint64, error) {
	return parseJSONL(data, batches, ring, nil, onBlock, nil, 0, nil)
}

func ParseJSONLProto(data []byte, batches *InsertBatches, ring *ProtoRingBuffer, onBlock func(*ProtoEventBlock) error) (uint64, error) {
	return parseJSONL(data, batches, nil, ring, nil, onBlock, 0, nil)
}

type parsedLineMeta struct {
	number    uint64
	timestamp uint64
	hash      string
	line      []byte
}

func ParseJSONLProtoStream(data []byte, batches *InsertBatches, ring *ProtoRingBuffer, endBlock uint64, onLine func(proto *ProtoEventBlock, number, timestamp uint64, hash string, line []byte) error) (uint64, error) {
	var callback func(*ParsedBlock, *ProtoEventBlock, parsedLineMeta) error
	if onLine != nil {
		callback = func(_ *ParsedBlock, proto *ProtoEventBlock, meta parsedLineMeta) error {
			return onLine(proto, meta.number, meta.timestamp, meta.hash, meta.line)
		}
	}
	return parseJSONL(data, batches, nil, ring, nil, nil, endBlock, callback)
}

func parseJSONL(data []byte, batches *InsertBatches, ring *OrderedHistoricRingBuffer, protoRing *ProtoRingBuffer, onBlock func(*ParsedBlock) error, onProtoBlock func(*ProtoEventBlock) error, endBlock uint64, onLine func(*ParsedBlock, *ProtoEventBlock, parsedLineMeta) error) (uint64, error) {
	var topics [4]string
	var dataHex string
	var dataBytes []byte
	var eventCount uint64

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

		l := &jlexer.Lexer{Data: line}
		var blockNum uint64
		var blockTimestamp uint64
		var blockHash string
		var blockTime time.Time // Pre-calculated once per block to avoid repeated time.Unix calls
		var slot *ParsedBlock
		var protoSlot *ProtoEventBlock
		var lineSkipped bool

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
					case "hash":
						blockHash = l.UnsafeString()
					default:
						l.Skip()
					}
					l.WantComma()
				}
				l.Delim('}')
				blockTime = time.Unix(int64(blockTimestamp), 0).UTC() // Calculate once per block
			case "logs":
				if endBlock > 0 && blockNum > endBlock {
					lineSkipped = true
					l.SkipRecursive()
					l.WantComma()
					continue
				}
				if ring != nil {
					slot = ring.NextSlot(blockNum, blockHash)
				}
				if protoRing != nil {
					protoSlot = protoRing.NextProtoSlot(blockNum, blockHash)
				}
				l.Delim('[')
				for !l.IsDelim(']') {
					topics = [4]string{} // Reset topics array to prevent stale data from previous log
					l.Delim('{')
					dataHex = ""
					var topicIdx int
					var txIndex, logIndex uint64
					var txHash string
					var address string

					for !l.IsDelim('}') {
						lkey := l.UnsafeFieldName(false)
						l.WantColon()
						switch lkey {
						case "address":
							address = l.UnsafeString()
						case "transactionHash":
							txHash = l.UnsafeString()
						case "transactionIndex":
							txIndex = l.Uint64()
						case "logIndex":
							logIndex = l.Uint64()
						case "data":
							dataHex = l.UnsafeString()
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

					// Event routing and decoding
					meta := EventMeta{
						BlockNumber:      blockNum,
						BlockTimestamp:   blockTime,
						BlockHash:        abiunpack.HashFromHex(blockHash),
						ContractAddress:  abiunpack.AddressFromHex(address),
						TransactionHash:  abiunpack.HashFromHex(txHash),
						TransactionIndex: txIndex,
						LogIndex:         logIndex,
					}
	`)
	// Group events by lowercased topic0
	groups := make(map[string][]eventSpec)
	var orderedTopics []string
	for _, ev := range events {
		t0Lower := strings.ToLower(ev.Topic0)
		if _, ok := groups[t0Lower]; !ok {
			orderedTopics = append(orderedTopics, t0Lower)
		}
		groups[t0Lower] = append(groups[t0Lower], ev)
	}

	writeCases := func(indent string) {
		for _, t0Lower := range orderedTopics {
			evs := groups[t0Lower]
			buf.WriteString(fmt.Sprintf("%scase %q:\n", indent, t0Lower))

			hasAddressChecks := false
			for _, ev := range evs {
				if len(ev.ContractAddress) > 0 {
					hasAddressChecks = true
					break
				}
			}
			if hasAddressChecks {
				buf.WriteString(fmt.Sprintf("%s\taddressLower := toLowerASCII(address)\n", indent))
			}

			for _, ev := range evs {
				if len(ev.ContractAddress) > 0 {
					var addrExprs []string
					for _, addr := range ev.ContractAddress {
						addrExprs = append(addrExprs, fmt.Sprintf("addressLower == %q", strings.ToLower(addr)))
					}
					buf.WriteString(fmt.Sprintf("%s\tif %s {\n", indent, strings.Join(addrExprs, " || ")))
				} else {
					buf.WriteString(fmt.Sprintf("%s{\n", indent))
				}

				buf.WriteString(fmt.Sprintf("%s\t\tvar ev %s\n", indent, ev.GoTypeName))
				buf.WriteString(fmt.Sprintf("%s\t\tev.EventMeta = meta\n", indent))
				buf.WriteString(fmt.Sprintf("%s\t\tvar ok bool\n%s\t\tvar word []byte\n%s\t\t_ = ok\n%s\t\t_ = word\n", indent, indent, indent, indent))

				decodeArgs := ev.DecodeArgs
				if len(decodeArgs) == 0 {
					decodeArgs = ev.Args
				}

				// Decode indexed fields
				tIdx := 1
				hasNonIndexed := false
				for _, arg := range decodeArgs {
					if arg.Indexed {
						if !arg.Omitted {
							renderParserIndexedDecode(&buf, arg, tIdx)
						}
						tIdx++
					} else {
						hasNonIndexed = true
					}
				}

				// Decode non-indexed fields
				if hasNonIndexed {
					buf.WriteString(fmt.Sprintf("%s\t\tdataBytes = abiunpack.AppendHexBytes(dataBytes[:0], dataHex)\n", indent))
					wordIdx := 0
					for _, arg := range decodeArgs {
						if !arg.Indexed {
							if !arg.Omitted {
								renderParserDataDecode(&buf, arg, wordIdx)
							}
							wordIdx += abiHeadWords(arg.SolidityType)
						}
					}
				}

				// Save/append
				buf.WriteString(fmt.Sprintf("%s\t\teventCount++\n", indent))
				buf.WriteString(fmt.Sprintf("%s\t\tif batches != nil {\n", indent))
				buf.WriteString(fmt.Sprintf("%s\t\t\tbatches.%s.Append(meta, &ev)\n", indent, ev.GoTypeName))
				buf.WriteString(fmt.Sprintf("%s\t\t}\n", indent))
				buf.WriteString(fmt.Sprintf("%s\t\tif slot != nil {\n", indent))
				buf.WriteString(fmt.Sprintf("%s\t\t\tslot.%ss = append(slot.%ss, ev)\n", indent, ev.GoTypeName, ev.GoTypeName))
				buf.WriteString(fmt.Sprintf("%s\t\t\tslot.Sequence = append(slot.Sequence, uint8(EventType%s))\n", indent, ev.GoTypeName))
				buf.WriteString(fmt.Sprintf("%s\t\t}\n", indent))
				buf.WriteString(fmt.Sprintf("%s\t\tif protoSlot != nil {\n", indent))
				buf.WriteString(fmt.Sprintf("%s\t\t\tprotoSlot.Append%s(meta, &ev)\n", indent, ev.GoTypeName))
				buf.WriteString(fmt.Sprintf("%s\t\t}\n", indent))

				// Close address condition block
				buf.WriteString(fmt.Sprintf("%s\t}\n", indent))
			}
		}
	}

	buf.WriteString(`
					topic0 := topics[0]
					_ = meta
					_ = dataBytes

					switch topic0 {
`)

	writeCases("\t\t\t\t\t")

	buf.WriteString(`					default:
						topic0Lower := toLowerASCII(topic0)
						if topic0Lower != topic0 {
							switch topic0Lower {
`)

	writeCases("\t\t\t\t\t\t\t")

	buf.WriteString(`							}
						}
					}
`)

	buf.WriteString(`
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
			return eventCount, l.Error()
		}
		if onLine != nil {
			if !lineSkipped {
				if err := onLine(slot, protoSlot, parsedLineMeta{
					number: blockNum, timestamp: blockTimestamp, hash: blockHash, line: line,
				}); err != nil {
					return eventCount, err
				}
			}
			continue
		}
		if onBlock != nil && slot != nil {
			if err := onBlock(slot); err != nil {
				return eventCount, err
			}
		}
		if onProtoBlock != nil && protoSlot != nil {
			if err := onProtoBlock(protoSlot); err != nil {
				return eventCount, err
			}
		}
	}
	return eventCount, nil
}

func toLowerASCII(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		} else {
			b[i] = c
		}
	}
	return string(b)
}
`)

	body := buf.Bytes()

	var out bytes.Buffer
	out.WriteString("// Code generated by sqd-go codegen; DO NOT EDIT.\n\npackage generated\n\nimport (\n")
	out.WriteString("\t\"context\"\n\t\"fmt\"\n\t\"time\"\n\n")
	out.WriteString("\t\"github.com/ClickHouse/ch-go\"\n")
	// common is only referenced when an event decodes an address/bytes32 from the
	// data section; indexed args go through abiunpack. Emit the import only when used.
	if bytes.Contains(body, []byte("common.")) {
		out.WriteString("\t\"github.com/ethereum/go-ethereum/common\"\n")
	}
	out.WriteString("\t\"github.com/mailru/easyjson/jlexer\"\n")
	out.WriteString("\t\"github.com/franz101/sqd-go/abiunpack\"\n")
	out.WriteString(")\n\n")
	out.Write(body)

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("format source: %w", err)
	}
	return formatted, nil
}

func renderParserIndexedDecode(b *bytes.Buffer, arg eventArg, topicIdx int) {
	topic := fmt.Sprintf("topics[%d]", topicIdx)
	switch {
	case arg.SolidityType == "address":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s = abiunpack.DecodeTopicAddress(%s)\n", arg.GoFieldName, topic))
	case arg.SolidityType == "bool":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s = abiunpack.TopicBool(%s)\n", arg.GoFieldName, topic))
	case arg.SolidityType == "bytes32":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s = abiunpack.DecodeTopicHash(%s)\n", arg.GoFieldName, topic))
	case isBytesN(arg.SolidityType):
		b.WriteString(fmt.Sprintf("\t\t\t\t\tabiunpack.DecodeTopicFixedBytes(%s, ev.%s[:], %d)\n", topic, arg.GoFieldName, bytesNSize(arg.SolidityType)))
	case strings.HasPrefix(arg.SolidityType, "uint"), strings.HasPrefix(arg.SolidityType, "int"):
		b.WriteString(fmt.Sprintf("\t\t\t\t\tabiunpack.DecodeTopicUint256(%s, &ev.%s)\n", topic, arg.GoFieldName))
	default:
		b.WriteString(fmt.Sprintf("\t\t\t\t\t// %s has unsupported indexed ABI type %q.\n", arg.GoFieldName, arg.SolidityType))
	}
}

func renderParserDataDecode(b *bytes.Buffer, arg eventArg, wordIdx int) {
	label := arg.GoFieldName
	solType := arg.SolidityType
	if elem, ok := dynamicArrayElement(solType); ok {
		renderParserDynamicArrayDecode(b, label, arg, elem, wordIdx)
		return
	}
	if elem, length, ok := fixedArrayElement(solType); ok {
		renderParserFixedArrayDecode(b, label, arg, elem, length, wordIdx)
		return
	}
	switch {
	case solType == "bytes":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s, _ = abiunpack.Bytes(dataBytes, %d)\n", arg.GoFieldName, wordIdx))
	case solType == "string":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tif rawVal, ok := abiunpack.Bytes(dataBytes, %d); ok { ev.%s = string(rawVal) }\n", wordIdx, arg.GoFieldName))
	case solType == "address":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok {\n", wordIdx))
		b.WriteString(fmt.Sprintf("\t\t\t\t\t\tev.%s = common.BytesToAddress(word[12:32])\n\t\t\t\t\t}\n", arg.GoFieldName))
	case solType == "bool":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok {\n", wordIdx))
		b.WriteString(fmt.Sprintf("\t\t\t\t\t\tev.%s, _ = abiunpack.Bool(word)\n\t\t\t\t\t}\n", arg.GoFieldName))
	case solType == "bytes32":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok {\n", wordIdx))
		b.WriteString(fmt.Sprintf("\t\t\t\t\t\tev.%s = common.BytesToHash(word)\n\t\t\t\t\t}\n", arg.GoFieldName))
	case isBytesN(solType):
		b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok {\n", wordIdx))
		b.WriteString(fmt.Sprintf("\t\t\t\t\t\tcopy(ev.%s[:], word[:%d])\n\t\t\t\t\t}\n", arg.GoFieldName, bytesNSize(solType)))
	case strings.HasPrefix(solType, "uint"), strings.HasPrefix(solType, "int"):
		b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok {\n", wordIdx))
		b.WriteString(fmt.Sprintf("\t\t\t\t\t\tev.%s.SetBytes32(word)\n\t\t\t\t\t}\n", arg.GoFieldName))
	default:
		b.WriteString(fmt.Sprintf("\t\t\t\t\t// %s has unsupported ABI type %q.\n", arg.GoFieldName, solType))
	}
}

func renderParserDynamicArrayDecode(b *bytes.Buffer, label string, arg eventArg, elem string, wordIdx int) {
	switch {
	case strings.HasPrefix(elem, "uint"), strings.HasPrefix(elem, "int"):
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s, _ = abiunpack.Uint256Array(dataBytes, %d)\n", arg.GoFieldName, wordIdx))
	case elem == "address":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s, _ = abiunpack.AddressArray(dataBytes, %d)\n", arg.GoFieldName, wordIdx))
	case elem == "bytes32":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s, _ = abiunpack.HashArray(dataBytes, %d)\n", arg.GoFieldName, wordIdx))
	case elem == "bool":
		b.WriteString(fmt.Sprintf("\t\t\t\t\tev.%s, _ = abiunpack.BoolArray(dataBytes, %d)\n", arg.GoFieldName, wordIdx))
	default:
		b.WriteString(fmt.Sprintf("\t\t\t\t\t// %s has unsupported dynamic array ABI type %q.\n", arg.GoFieldName, arg.SolidityType))
	}
}

func renderParserFixedArrayDecode(b *bytes.Buffer, label string, arg eventArg, elem string, length int, wordIdx int) {
	for i := 0; i < length; i++ {
		target := fmt.Sprintf("ev.%s[%d]", arg.GoFieldName, i)
		switch {
		case strings.HasPrefix(elem, "uint"), strings.HasPrefix(elem, "int"):
			b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok { %s.SetBytes32(word) }\n", wordIdx+i, target))
		case elem == "address":
			b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok { %s = common.BytesToAddress(word[12:32]) }\n", wordIdx+i, target))
		case elem == "bytes32":
			b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok { %s = common.BytesToHash(word) }\n", wordIdx+i, target))
		case elem == "bool":
			b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok { %s, _ = abiunpack.Bool(word) }\n", wordIdx+i, target))
		case isBytesN(elem):
			b.WriteString(fmt.Sprintf("\t\t\t\t\tif word, ok = abiunpack.Word(dataBytes, %d); ok { copy(%s[:], word[:%d]) }\n", wordIdx+i, target, bytesNSize(elem)))
		default:
			b.WriteString(fmt.Sprintf("\t\t\t\t\t// %s has unsupported fixed array ABI type %q.\n", arg.GoFieldName, arg.SolidityType))
			return
		}
	}
}
