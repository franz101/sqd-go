package codegen

import (
	"fmt"
	"go/format"
	"strconv"
	"strings"

	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/template"
)

type uniqueEvent struct {
	GoTypeName string
	Cases      []string
}

func generateInserterGo(cfg *config.Config, events []eventSpec) ([]byte, error) {
	usedCases := make(map[string]struct{})
	var uniqueEvents []uniqueEvent

	for _, ev := range events {
		var cases []string
		for _, name := range []string{ev.EventName, ev.GoTypeName} {
			if _, ok := usedCases[name]; ok {
				continue
			}
			usedCases[name] = struct{}{}
			cases = append(cases, strconv.Quote(name))
		}
		if len(cases) > 0 {
			uniqueEvents = append(uniqueEvents, uniqueEvent{
				GoTypeName: ev.GoTypeName,
				Cases:      cases,
			})
		}
	}

	tmplData := struct {
		Events                 []eventSpec
		UniqueEvents           []uniqueEvent
		IncludeChainID         bool
		IncludeBlockHash       bool
		IncludeContractAddress bool
		IncludeTransactionHash bool
	}{
		Events:                 events,
		UniqueEvents:           uniqueEvents,
		IncludeChainID:         cfg.MetadataIncluded("chain_id"),
		IncludeBlockHash:       cfg.MetadataIncluded("block_hash"),
		IncludeContractAddress: cfg.MetadataIncluded("contract_address"),
		IncludeTransactionHash: cfg.MetadataIncluded("transaction_hash"),
	}

	src := template.MustExecute("code/inserterGo", tmplData)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		fmt.Println("--- FAILED SOURCE ---")
		fmt.Println(src)
		fmt.Println("---------------------")
		return []byte(src), fmt.Errorf("format inserter source: %w", err)
	}
	return formatted, nil
}

func (arg eventArg) ProtoColumnType() string {
	chType := arg.ClickHouseType
	switch {
	case chType == "UInt8":
		return "proto.ColUInt8"
	case chType == "UInt256":
		return "proto.ColUInt256"
	case chType == "FixedString(20)", chType == "FixedString(32)", strings.HasPrefix(chType, "FixedString("):
		return "proto.ColFixedStr"
	default:
		return "proto.ColStr"
	}
}

func (arg eventArg) ProtoColumnInit() string {
	if strings.HasPrefix(arg.ClickHouseType, "FixedString(") {
		return "proto.ColFixedStr{Size: " + strconv.Itoa(fixedStringSize(arg.ClickHouseType)) + "}"
	}
	return ""
}

func (arg eventArg) AppendExpr() string {
	switch {
	case arg.ClickHouseType == "UInt8":
		return "clickHouseBool(ev." + arg.GoFieldName + ")"
	case arg.ClickHouseType == "UInt256":
		return "clickHouseUInt256(ev." + arg.GoFieldName + ")"
	case arg.ClickHouseType == "FixedString(20)", arg.ClickHouseType == "FixedString(32)":
		return "ev." + arg.GoFieldName + ".Bytes()"
	case strings.HasPrefix(arg.ClickHouseType, "FixedString("):
		return "ev." + arg.GoFieldName + "[:]"
	case arg.SolidityType == "bytes":
		return "clickHouseBytes(ev." + arg.GoFieldName + ")"
	case arg.GoType == "[]uint256.Int":
		return "clickHouseUint256Array(ev." + arg.GoFieldName + ")"
	case arg.GoType == "[]common.Hash":
		return "clickHouseHashArray(ev." + arg.GoFieldName + ")"
	case arg.GoType == "[]common.Address":
		return "clickHouseAddressArray(ev." + arg.GoFieldName + ")"
	case arg.GoType == "[]bool":
		return "clickHouseBoolArray(ev." + arg.GoFieldName + ")"
	default:
		return "fmt.Sprint(ev." + arg.GoFieldName + ")"
	}
}

func fixedStringSize(chType string) int {
	open := strings.IndexByte(chType, '(')
	close := strings.IndexByte(chType, ')')
	if open < 0 || close <= open+1 {
		return 0
	}
	n, _ := strconv.Atoi(chType[open+1 : close])
	return n
}
