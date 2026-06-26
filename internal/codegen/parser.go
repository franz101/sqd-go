package codegen

import (
	"fmt"
	"go/format"
	"strings"

	"github.com/franz101/sqd-go/internal/template"
)

func hasNonIndexedAddressOrHash(events []eventSpec) bool {
	for _, ev := range events {
		for _, arg := range ev.Args {
			if arg.Indexed {
				continue
			}
			t := arg.SolidityType
			if t == "address" || t == "bytes32" {
				return true
			}
			if elem, _, ok := fixedArrayElement(t); ok {
				if elem == "address" || elem == "bytes32" {
					return true
				}
			}
		}
	}
	return false
}

type parserTemplateData struct {
	Events                     []eventSpec
	HasNonIndexedAddressOrHash bool
	TopicGroups                []parserTopicGroup
}

type parserTopicGroup struct {
	Topic0Lower      string
	HasAddressChecks bool
	Events           []parserEventGroup
}

type parserEventGroup struct {
	GoTypeName        string
	HasAddressCheck   bool
	AddressCheckExpr  string
	HasNonIndexed     bool
	IndexedDecodes    []string
	NonIndexedDecodes []string
}

func generateParserGo(events []eventSpec) ([]byte, error) {
	groups := make(map[string][]eventSpec)
	var orderedTopics []string
	for _, ev := range events {
		t0Lower := strings.ToLower(ev.Topic0)
		if _, ok := groups[t0Lower]; !ok {
			orderedTopics = append(orderedTopics, t0Lower)
		}
		groups[t0Lower] = append(groups[t0Lower], ev)
	}

	var topicGroups []parserTopicGroup
	for _, t0Lower := range orderedTopics {
		evs := groups[t0Lower]
		var group parserTopicGroup
		group.Topic0Lower = t0Lower
		for _, ev := range evs {
			if len(ev.ContractAddress) > 0 {
				group.HasAddressChecks = true
				break
			}
		}

		for _, ev := range evs {
			evGroup := parserEventGroup{
				GoTypeName: ev.GoTypeName,
			}
			if len(ev.ContractAddress) > 0 {
				evGroup.HasAddressCheck = true
				var addrExprs []string
				for _, addr := range ev.ContractAddress {
					addrExprs = append(addrExprs, fmt.Sprintf("addressLower == %q", strings.ToLower(addr)))
				}
				evGroup.AddressCheckExpr = strings.Join(addrExprs, " || ")
			}

			decodeArgs := ev.DecodeArgs
			if len(decodeArgs) == 0 {
				decodeArgs = ev.Args
			}

			tIdx := 1
			for _, arg := range decodeArgs {
				if arg.Indexed {
					if !arg.Omitted {
						evGroup.IndexedDecodes = append(evGroup.IndexedDecodes, renderParserIndexedDecode(arg, tIdx))
					}
					tIdx++
				} else {
					evGroup.HasNonIndexed = true
				}
			}

			if evGroup.HasNonIndexed {
				wordIdx := 0
				for _, arg := range decodeArgs {
					if !arg.Indexed {
						if !arg.Omitted {
							evGroup.NonIndexedDecodes = append(evGroup.NonIndexedDecodes, renderParserDataDecode(arg, wordIdx))
						}
						wordIdx += abiHeadWords(arg.SolidityType)
					}
				}
			}

			group.Events = append(group.Events, evGroup)
		}
		topicGroups = append(topicGroups, group)
	}

	tmplData := parserTemplateData{
		Events:                     events,
		HasNonIndexedAddressOrHash: hasNonIndexedAddressOrHash(events),
		TopicGroups:                topicGroups,
	}

	src := template.MustExecute("code/parserGo", tmplData)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		fmt.Println("--- FAILED SOURCE ---")
		fmt.Println(src)
		fmt.Println("---------------------")
		return []byte(src), fmt.Errorf("format parser source: %w", err)
	}
	return formatted, nil
}

func renderParserIndexedDecode(arg eventArg, topicIdx int) string {
	topic := fmt.Sprintf("topics[%d]", topicIdx)
	switch {
	case arg.SolidityType == "address":
		return fmt.Sprintf("ev.%s = abiunpack.DecodeAddressFromTopic(%s)", arg.GoFieldName, topic)
	case arg.SolidityType == "bool":
		return fmt.Sprintf("ev.%s = abiunpack.TopicBool(%s)", arg.GoFieldName, topic)
	case arg.SolidityType == "bytes32":
		return fmt.Sprintf("ev.%s = abiunpack.DecodeTopicHash(%s)", arg.GoFieldName, topic)
	case isBytesN(arg.SolidityType):
		return fmt.Sprintf("abiunpack.DecodeTopicFixedBytes(%s, ev.%s[:], %d)", topic, arg.GoFieldName, bytesNSize(arg.SolidityType))
	case strings.HasPrefix(arg.SolidityType, "uint"), strings.HasPrefix(arg.SolidityType, "int"):
		return fmt.Sprintf("abiunpack.DecodeTopicUint256(%s, &ev.%s)", topic, arg.GoFieldName)
	default:
		return fmt.Sprintf("// %s has unsupported indexed ABI type %q.", arg.GoFieldName, arg.SolidityType)
	}
}

func renderParserDataDecode(arg eventArg, wordIdx int) string {
	label := arg.GoFieldName
	solType := arg.SolidityType
	if elem, ok := dynamicArrayElement(solType); ok {
		return renderParserDynamicArrayDecode(label, arg, elem, wordIdx)
	}
	if elem, length, ok := fixedArrayElement(solType); ok {
		return renderParserFixedArrayDecode(label, arg, elem, length, wordIdx)
	}
	switch {
	case solType == "bytes":
		return fmt.Sprintf("ev.%s, _ = abiunpack.Bytes(dataBytes, %d)", arg.GoFieldName, wordIdx)
	case solType == "string":
		return fmt.Sprintf("if rawVal, ok := abiunpack.Bytes(dataBytes, %d); ok { ev.%s = string(rawVal) }", wordIdx, arg.GoFieldName)
	case solType == "address":
		return fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok {\n\tev.%s = common.BytesToAddress(word[12:32])\n}", wordIdx, arg.GoFieldName)
	case solType == "bool":
		return fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok {\n\tev.%s, _ = abiunpack.Bool(word)\n}", wordIdx, arg.GoFieldName)
	case solType == "bytes32":
		return fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok {\n\tev.%s = common.BytesToHash(word)\n}", wordIdx, arg.GoFieldName)
	case isBytesN(solType):
		return fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok {\n\tcopy(ev.%s[:], word[:%d])\n}", wordIdx, arg.GoFieldName, bytesNSize(solType))
	case strings.HasPrefix(solType, "uint"), strings.HasPrefix(solType, "int"):
		return fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok {\n\tev.%s.SetBytes32(word)\n}", wordIdx, arg.GoFieldName)
	default:
		return fmt.Sprintf("// %s has unsupported ABI type %q.", arg.GoFieldName, solType)
	}
}

func renderParserDynamicArrayDecode(label string, arg eventArg, elem string, wordIdx int) string {
	switch {
	case strings.HasPrefix(elem, "uint"), strings.HasPrefix(elem, "int"):
		return fmt.Sprintf("ev.%s, _ = abiunpack.Uint256Array(dataBytes, %d)", arg.GoFieldName, wordIdx)
	case elem == "address":
		return fmt.Sprintf("ev.%s, _ = abiunpack.AddressArray(dataBytes, %d)", arg.GoFieldName, wordIdx)
	case elem == "bytes32":
		return fmt.Sprintf("ev.%s, _ = abiunpack.HashArray(dataBytes, %d)", arg.GoFieldName, wordIdx)
	case elem == "bool":
		return fmt.Sprintf("ev.%s, _ = abiunpack.BoolArray(dataBytes, %d)", arg.GoFieldName, wordIdx)
	default:
		return fmt.Sprintf("// %s has unsupported dynamic array ABI type %q.", arg.GoFieldName, arg.SolidityType)
	}
}

func renderParserFixedArrayDecode(label string, arg eventArg, elem string, length int, wordIdx int) string {
	var b strings.Builder
	for i := 0; i < length; i++ {
		target := fmt.Sprintf("ev.%s[%d]", arg.GoFieldName, i)
		switch {
		case strings.HasPrefix(elem, "uint"), strings.HasPrefix(elem, "int"):
			b.WriteString(fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok { %s.SetBytes32(word) }\n", wordIdx+i, target))
		case elem == "address":
			b.WriteString(fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok { %s = common.BytesToAddress(word[12:32]) }\n", wordIdx+i, target))
		case elem == "bytes32":
			b.WriteString(fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok { %s = common.BytesToHash(word) }\n", wordIdx+i, target))
		case elem == "bool":
			b.WriteString(fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok { %s, _ = abiunpack.Bool(word) }\n", wordIdx+i, target))
		case isBytesN(elem):
			b.WriteString(fmt.Sprintf("if word, ok = abiunpack.Word(dataBytes, %d); ok { copy(%s[:], word[:%d]) }\n", wordIdx+i, target, bytesNSize(elem)))
		default:
			b.WriteString(fmt.Sprintf("// %s has unsupported fixed array ABI type %q.\n", arg.GoFieldName, arg.SolidityType))
			return b.String()
		}
	}
	return b.String()
}
