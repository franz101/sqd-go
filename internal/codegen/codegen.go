package codegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/franz101/sqd-go-v2/internal/config"
)

type Manifest struct {
	Name       string          `json:"name"`
	Ecosystem  string          `json:"ecosystem,omitempty"`
	ConfigPath string          `json:"config_path"`
	Chains     []ManifestChain `json:"chains"`
}

type ManifestChain struct {
	ID         uint64             `json:"id"`
	StartBlock uint64             `json:"start_block"`
	EndBlock   *uint64            `json:"end_block,omitempty"`
	Contracts  []ManifestContract `json:"contracts"`
}

type ManifestContract struct {
	Name    string   `json:"name"`
	Events  []string `json:"events"`
	Address any      `json:"address,omitempty"`
}

func GenerateProject(project *config.Project) (string, error) {
	if err := config.Validate(project.Config); err != nil {
		return "", err
	}
	outDir := filepath.Join(project.Root, ".sqd", "generated")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	goOutDir := filepath.Join(project.Root, "generated")
	if err := os.MkdirAll(goOutDir, 0o755); err != nil {
		return "", err
	}
	outPath := filepath.Join(outDir, "manifest.json")

	manifest := buildManifest(project)
	if err := writeJSON(outPath, manifest); err != nil {
		return "", err
	}

	events, err := buildEventSpecs(project.Config)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(outDir, "schema.sql"), []byte(generateSchemaSQL(project.Config.Name, events)), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(outDir, "views.sql"), []byte(generateViewsSQL(project.Config.Name, events)), 0o644); err != nil {
		return "", err
	}
	goCode, err := generateGoCode(project.Config.Name, events)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(goOutDir, "events.go"), goCode, 0o644); err != nil {
		return "", err
	}
	inserterCode, err := generateInserterGo(events)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(goOutDir, "inserter.go"), inserterCode, 0o644); err != nil {
		return "", err
	}
	customProcessorCode, err := generateCustomProcessorGo(events)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(goOutDir, "custom_processor.go"), customProcessorCode, 0o644); err != nil {
		return "", err
	}
	return outPath, nil
}

func buildManifest(project *config.Project) Manifest {
	cfg := project.Config
	manifest := Manifest{
		Name:       cfg.Name,
		ConfigPath: project.ConfigPath,
	}
	if cfg.Ecosystem != nil {
		manifest.Ecosystem = *cfg.Ecosystem
	}
	for _, chain := range cfg.Chains {
		mc := ManifestChain{
			ID:         chain.ID,
			StartBlock: chain.StartBlock,
			EndBlock:   chain.EndBlock,
		}
		for _, contract := range chain.Contracts {
			out := ManifestContract{
				Name:    contract.Name,
				Address: contract.Address,
			}
			for _, event := range contract.Events {
				out.Events = append(out.Events, event.Event)
			}
			mc.Contracts = append(mc.Contracts, out)
		}
		manifest.Chains = append(manifest.Chains, mc)
	}
	return manifest
}

func Generate(path string) (string, error) {
	project, err := config.LoadProject(path)
	if err != nil {
		return "", fmt.Errorf("load project: %w", err)
	}
	return GenerateProject(project)
}

func writeJSON(path string, v any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type eventSpec struct {
	ChainID         uint64
	ContractName    string
	ContractIdent   string
	EventName       string
	EventIdent      string
	EventSignature  string
	CanonicalSig    string
	Topic0          string
	ViewName        string
	TableName       string
	GoTypeName      string
	GoConstPrefix   string
	ContractAddress []string
	Args            []eventArg
}

type eventArg struct {
	Name           string
	ColumnName     string
	SolidityType   string
	ClickHouseType string
	Indexed        bool
	GoFieldName    string
	GoType         string
}

type parsedEvent struct {
	Name         string
	CanonicalSig string
	Args         []eventArg
}

func buildEventSpecs(cfg *config.Config) ([]eventSpec, error) {
	var specs []eventSpec
	usedViews := make(map[string]int)
	usedTypes := make(map[string]int)
	usedConsts := make(map[string]int)

	for _, chain := range cfg.Chains {
		for _, contract := range chain.Contracts {
			for _, ev := range contract.Events {
				parsed, err := parseEvent(ev.Event)
				if err != nil {
					return nil, fmt.Errorf("%s.%s: %w", contract.Name, ev.Event, err)
				}
				eventName := parsed.Name
				if ev.Name != nil && strings.TrimSpace(*ev.Name) != "" {
					eventName = strings.TrimSpace(*ev.Name)
				}
				contractIdent := exportIdent(contract.Name)
				eventIdent := exportIdent(eventName)
				viewName := uniqueLower(usedViews, toSnake(contract.Name+"_"+eventName))
				typeName := uniqueExported(usedTypes, contractIdent+eventIdent)
				constPrefix := uniqueExported(usedConsts, contractIdent+eventIdent)

				specs = append(specs, eventSpec{
					ChainID:         chain.ID,
					ContractName:    contract.Name,
					ContractIdent:   contractIdent,
					EventName:       parsed.Name,
					EventIdent:      eventIdent,
					EventSignature:  strings.TrimSpace(ev.Event),
					CanonicalSig:    parsed.CanonicalSig,
					Topic0:          crypto.Keccak256Hash([]byte(parsed.CanonicalSig)).Hex(),
					ViewName:        viewName,
					TableName:       viewName + "_events",
					GoTypeName:      typeName,
					GoConstPrefix:   constPrefix,
					ContractAddress: addresses(contract.Address),
					Args:            parsed.Args,
				})
			}
		}
	}
	return specs, nil
}

func parseEvent(sig string) (*parsedEvent, error) {
	sig = strings.TrimSpace(sig)
	sig = strings.TrimPrefix(sig, "event ")
	open := strings.IndexByte(sig, '(')
	close := strings.LastIndexByte(sig, ')')
	if open <= 0 || close <= open {
		return nil, fmt.Errorf("invalid event signature")
	}
	name := strings.TrimSpace(sig[:open])
	if name == "" {
		return nil, fmt.Errorf("event name is required")
	}
	args := splitArgs(sig[open+1 : close])
	abiJSON := eventABIJSON(name, args)
	parsed, err := abi.JSON(strings.NewReader(abiJSON))
	if err != nil {
		return nil, err
	}
	abiEvent, ok := parsed.Events[name]
	if !ok {
		return nil, fmt.Errorf("event not found after parsing")
	}
	usedFields := make(map[string]int)
	out := &parsedEvent{Name: name, CanonicalSig: abiEvent.Sig}
	for i, input := range abiEvent.Inputs {
		argName := input.Name
		if strings.TrimSpace(argName) == "" {
			argName = fmt.Sprintf("p%d", i)
		}
		fieldName := uniqueExported(usedFields, exportIdent(argName))
		out.Args = append(out.Args, eventArg{
			Name:           argName,
			ColumnName:     argName,
			SolidityType:   input.Type.String(),
			ClickHouseType: clickHouseType(input.Type.String()),
			Indexed:        input.Indexed,
			GoFieldName:    fieldName,
			GoType:         goType(input.Type.String()),
		})
	}
	return out, nil
}

func eventABIJSON(name string, args []string) string {
	inputs := make([]string, 0, len(args))
	for i, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		parts := strings.Fields(arg)
		if len(parts) == 0 {
			continue
		}
		typ := normalizeSolidityType(parts[0])
		indexed := false
		paramName := fmt.Sprintf("p%d", i)
		for _, part := range parts[1:] {
			if part == "indexed" {
				indexed = true
				continue
			}
			paramName = part
		}
		inputs = append(inputs, fmt.Sprintf(
			`{"indexed":%t,"name":%s,"type":%s}`,
			indexed, strconv.Quote(paramName), strconv.Quote(typ),
		))
	}
	return fmt.Sprintf(`[{"anonymous":false,"inputs":[%s],"name":%s,"type":"event"}]`,
		strings.Join(inputs, ","), strconv.Quote(name))
}

func splitArgs(args string) []string {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}
	var out []string
	start := 0
	depth := 0
	for i, r := range args {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(args[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(args[start:]))
	return out
}

func generateSchemaSQL(dbName string, events []eventSpec) string {
	db := quoteSQLIdent(dbName)
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`-- Code generated by sqd-go codegen; DO NOT EDIT.

CREATE DATABASE IF NOT EXISTS %[1]s;

CREATE TABLE IF NOT EXISTS %[1]s.blocks (
  chain_id UInt64,
  block_number UInt64,
  block_timestamp DateTime64(3, 'UTC'),
  block_hash String,
  inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = MergeTree()
ORDER BY (chain_id, block_number);

CREATE TABLE IF NOT EXISTS %[1]s.logs (
  chain_id UInt64,
  block_number UInt64,
  block_timestamp DateTime64(3, 'UTC'),
  block_hash String,
  transaction_hash FixedString(32),
  transaction_index UInt64,
  log_index UInt64,
  address FixedString(20),
  event_name LowCardinality(String),
  topic0 FixedString(32),
  params String,
  inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = MergeTree()
ORDER BY (chain_id, block_number, transaction_index, log_index);

CREATE TABLE IF NOT EXISTS %[1]s.sync_state (
  chain_id UInt64,
  last_block UInt64,
  updated_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = MergeTree()
ORDER BY (chain_id, updated_at);
`, db))
	for _, ev := range events {
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s.%s (\n", db, quoteSQLIdent(ev.TableName)))
		b.WriteString("  chain_id UInt64,\n")
		b.WriteString("  block_number UInt64,\n")
		b.WriteString("  block_timestamp DateTime64(3, 'UTC'),\n")
		b.WriteString("  block_hash FixedString(32),\n")
		b.WriteString("  contract_address FixedString(20),\n")
		b.WriteString("  transaction_hash FixedString(32),\n")
		b.WriteString("  transaction_index UInt64,\n")
		b.WriteString("  log_index UInt64")
		for _, arg := range ev.Args {
			b.WriteString(",\n")
			b.WriteString("  ")
			b.WriteString(quoteSQLIdent(arg.ColumnName))
			b.WriteString(" ")
			b.WriteString(arg.ClickHouseType)
		}
		b.WriteString("\n) ENGINE = MergeTree()\n")
		b.WriteString("ORDER BY (chain_id, block_number, transaction_index, log_index);\n")
	}
	if hasERC20Transfer(events) {
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %[1]s.erc20_address_positions (
  address FixedString(20),
  balance_raw String,
  total_in_raw String,
  total_out_raw String,
  net_flow_raw String,
  realized_pnl_raw String,
  transfer_count UInt64,
  updated_at_block UInt64,
  updated_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(updated_at_block)
ORDER BY address;
`, db))
	}
	return b.String()
}

func generateViewsSQL(dbName string, events []eventSpec) string {
	var b strings.Builder
	db := quoteSQLIdent(dbName)
	b.WriteString("-- Code generated by sqd-go codegen; DO NOT EDIT.\n\n")
	for _, ev := range events {
		view := db + "." + quoteSQLIdent(ev.ViewName)
		b.WriteString(fmt.Sprintf("DROP VIEW IF EXISTS %s;\n\n", view))
		b.WriteString(fmt.Sprintf("CREATE VIEW %s AS\n", view))
		b.WriteString("SELECT\n")
		b.WriteString("  chain_id,\n")
		b.WriteString("  block_number,\n")
		b.WriteString("  block_timestamp,\n")
		b.WriteString("  block_hash,\n")
		b.WriteString("  concat('0x', lower(hex(address))) AS contract_address,\n")
		b.WriteString("  concat('0x', lower(hex(transaction_hash))) AS transaction_hash,\n")
		b.WriteString("  transaction_index,\n")
		b.WriteString("  log_index")
		for _, arg := range ev.Args {
			b.WriteString(",\n")
			b.WriteString("  ")
			b.WriteString(jsonExtractExpr(arg))
			b.WriteString(" AS ")
			b.WriteString(quoteSQLIdent(arg.ColumnName))
		}
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("FROM %s.logs\n", db))
		b.WriteString(fmt.Sprintf("WHERE chain_id = %d\n", ev.ChainID))
		b.WriteString(fmt.Sprintf("  AND event_name = %s\n", quoteSQLString(ev.EventName)))
		b.WriteString(fmt.Sprintf("  AND topic0 = unhex(%s)", quoteSQLString(strip0x(ev.Topic0))))
		if len(ev.ContractAddress) > 0 {
			b.WriteString("\n  AND address IN (")
			for i, addr := range ev.ContractAddress {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString("unhex(")
				b.WriteString(quoteSQLString(strip0x(addr)))
				b.WriteString(")")
			}
			b.WriteString(")")
		}
		b.WriteString(";\n\n")
	}
	return b.String()
}

func generateGoCode(projectName string, events []eventSpec) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by sqd-go codegen; DO NOT EDIT.\n\n")
	b.WriteString("package generated\n\n")

	// Build import block
	var imports []string
	if len(events) > 0 {
		imports = append(imports, `"fmt"`)
		imports = append(imports, `"github.com/ethereum/go-ethereum/common"`)
		imports = append(imports, `"github.com/franz101/sqd-go-v2/internal/parser/abiunpack"`)
	}
	if hasUint256Type(events) {
		imports = append(imports, `"github.com/holiman/uint256"`)
	}
	imports = append(imports, `"time"`)
	if len(imports) == 1 {
		b.WriteString("import " + imports[0] + "\n\n")
	} else if len(imports) > 1 {
		b.WriteString("import (\n")
		for _, imp := range imports {
			b.WriteString("\t" + imp + "\n")
		}
		b.WriteString(")\n\n")
	}

	b.WriteString("const ProjectName = ")
	b.WriteString(strconv.Quote(projectName))
	b.WriteString("\n\n")

	b.WriteString(`type EventMeta struct {
	ChainID         uint64 ` + "`json:\"chain_id\"`" + `
	BlockNumber     uint64 ` + "`json:\"block_number\"`" + `
	BlockTimestamp  time.Time ` + "`json:\"block_timestamp\"`" + `
	BlockHash       string ` + "`json:\"block_hash\"`" + `
	ContractAddress string ` + "`json:\"contract_address\"`" + `
	TransactionHash string ` + "`json:\"transaction_hash\"`" + `
	TransactionIndex uint64 ` + "`json:\"transaction_index\"`" + `
	LogIndex         uint64 ` + "`json:\"log_index\"`" + `
}

`)
	for _, ev := range events {
		b.WriteString("const ")
		b.WriteString(ev.GoConstPrefix)
		b.WriteString("Signature = ")
		b.WriteString(strconv.Quote(ev.EventSignature))
		b.WriteString("\n")
		b.WriteString("const ")
		b.WriteString(ev.GoConstPrefix)
		b.WriteString("CanonicalSignature = ")
		b.WriteString(strconv.Quote(ev.CanonicalSig))
		b.WriteString("\n")
		b.WriteString("const ")
		b.WriteString(ev.GoConstPrefix)
		b.WriteString("Topic0 = ")
		b.WriteString(strconv.Quote(ev.Topic0))
		b.WriteString("\n")
		b.WriteString("const ")
		b.WriteString(ev.GoConstPrefix)
		b.WriteString("View = ")
		b.WriteString(strconv.Quote(ev.ViewName))
		b.WriteString("\n")
		if len(ev.ContractAddress) == 1 {
			b.WriteString("const ")
			b.WriteString(ev.GoConstPrefix)
			b.WriteString("Address = ")
			b.WriteString(strconv.Quote(ev.ContractAddress[0]))
			b.WriteString("\n")
		} else if len(ev.ContractAddress) > 1 {
			b.WriteString("var ")
			b.WriteString(ev.GoConstPrefix)
			b.WriteString("Addresses = []string{")
			for i, addr := range ev.ContractAddress {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(strconv.Quote(addr))
			}
			b.WriteString("}\n")
		}
		b.WriteString("\n")

		b.WriteString("type ")
		b.WriteString(ev.GoTypeName)
		b.WriteString(" struct {\n")
		b.WriteString("\tEventMeta\n")
		for _, arg := range ev.Args {
			b.WriteString("\t")
			b.WriteString(arg.GoFieldName)
			b.WriteString(" ")
			b.WriteString(arg.GoType)
			b.WriteString(" `json:")
			b.WriteString(strconv.Quote(arg.Name))
			b.WriteString("`")
			if arg.Indexed {
				b.WriteString(" // indexed")
			}
			b.WriteString("\n")
		}
		b.WriteString("}\n\n")
	}
	renderUnpackDispatcher(&b, events)
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return nil, err
	}
	return formatted, nil
}

func renderUnpackDispatcher(b *bytes.Buffer, events []eventSpec) {
	if len(events) == 0 {
		return
	}
	for i, ev := range events {
		b.WriteString("var _topic")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(" = common.HexToHash(")
		b.WriteString(strconv.Quote(ev.Topic0))
		b.WriteString(")\n")
		for j, addr := range ev.ContractAddress {
			b.WriteString("var _addr")
			b.WriteString(strconv.Itoa(i))
			b.WriteString("_")
			b.WriteString(strconv.Itoa(j))
			b.WriteString(" = common.HexToAddress(")
			b.WriteString(strconv.Quote(addr))
			b.WriteString(")\n")
		}
	}
	b.WriteString(`
type DecodedLog struct {
	EventName string
	Topic0   string
	Value    any
}

func UnpackLog(address string, topics []string, data []byte) (*DecodedLog, error) {
	if len(topics) == 0 {
		return nil, nil
	}
	logAddress := common.HexToAddress(address)
	topic0 := common.HexToHash(topics[0])
`)
	for i, ev := range events {
		b.WriteString("\tif topic0 == _topic")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(" && ")
		b.WriteString(addressMatchExpr(i, len(ev.ContractAddress)))
		b.WriteString(" {\n")
		b.WriteString("\t\tev, err := Unpack")
		b.WriteString(ev.GoTypeName)
		b.WriteString("Log(topics, data)\n")
		b.WriteString("\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
		b.WriteString("\t\tif ev == nil {\n\t\t\treturn nil, nil\n\t\t}\n")
		b.WriteString("\t\treturn &DecodedLog{EventName: ")
		b.WriteString(strconv.Quote(ev.EventName))
		b.WriteString(", Topic0: ")
		b.WriteString(strconv.Quote(ev.Topic0))
		b.WriteString(", Value: ev}, nil\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("\treturn nil, nil\n}\n\n")
	for _, ev := range events {
		renderEventUnpackFunction(b, ev)
	}
}

func addressMatchExpr(eventIdx, addrCount int) string {
	if addrCount == 0 {
		return "true"
	}
	var parts []string
	for i := 0; i < addrCount; i++ {
		parts = append(parts, fmt.Sprintf("logAddress == _addr%d_%d", eventIdx, i))
	}
	return "(" + strings.Join(parts, " || ") + ")"
}

func renderEventUnpackFunction(b *bytes.Buffer, ev eventSpec) {
	requiredTopics := 1
	for _, arg := range ev.Args {
		if arg.Indexed {
			requiredTopics++
		}
	}
	b.WriteString("func Unpack")
	b.WriteString(ev.GoTypeName)
	b.WriteString("Log(topics []string, data []byte) (*")
	b.WriteString(ev.GoTypeName)
	b.WriteString(", error) {\n")
	b.WriteString(fmt.Sprintf("\tif len(topics) < %d {\n\t\treturn nil, nil\n\t}\n", requiredTopics))
	b.WriteString("\tvar ev ")
	b.WriteString(ev.GoTypeName)
	b.WriteString("\n\tvar ok bool\n\tvar word []byte\n\t_ = ok\n\t_ = word\n")
	topicIdx := 1
	dataWord := 0
	for _, arg := range ev.Args {
		if arg.Indexed {
			renderIndexedDecode(b, arg, topicIdx)
			topicIdx++
			continue
		}
		renderDataDecode(b, ev.GoTypeName, arg, dataWord)
		dataWord += abiHeadWords(arg.SolidityType)
	}
	b.WriteString("\treturn &ev, nil\n}\n\n")
}

func renderIndexedDecode(b *bytes.Buffer, arg eventArg, topicIdx int) {
	topic := fmt.Sprintf("topics[%d]", topicIdx)
	switch {
	case arg.SolidityType == "address":
		b.WriteString(fmt.Sprintf("\tev.%s = common.HexToAddress(%s)\n", arg.GoFieldName, topic))
	case arg.SolidityType == "bool":
		b.WriteString(fmt.Sprintf("\tev.%s = abiunpack.TopicBool(%s)\n", arg.GoFieldName, topic))
	case arg.SolidityType == "bytes32":
		b.WriteString(fmt.Sprintf("\tev.%s = common.HexToHash(%s)\n", arg.GoFieldName, topic))
	case isBytesN(arg.SolidityType):
		b.WriteString(fmt.Sprintf("\tabiunpack.DecodeTopicFixedBytes(%s, ev.%s[:], %d)\n", topic, arg.GoFieldName, bytesNSize(arg.SolidityType)))
	case strings.HasPrefix(arg.SolidityType, "uint"), strings.HasPrefix(arg.SolidityType, "int"):
		b.WriteString(fmt.Sprintf("\tabiunpack.DecodeTopicUint256(%s, &ev.%s)\n", topic, arg.GoFieldName))
	default:
		b.WriteString(fmt.Sprintf("\t// %s has unsupported indexed ABI type %q.\n", arg.GoFieldName, arg.SolidityType))
	}
}

func renderDataDecode(b *bytes.Buffer, eventType string, arg eventArg, wordIdx int) {
	label := eventType + "." + arg.GoFieldName
	solType := arg.SolidityType
	if elem, ok := dynamicArrayElement(solType); ok {
		renderDynamicArrayDecode(b, label, arg, elem, wordIdx)
		return
	}
	if elem, length, ok := fixedArrayElement(solType); ok {
		renderFixedArrayDecode(b, label, arg, elem, length, wordIdx)
		return
	}
	switch {
	case solType == "bytes":
		b.WriteString(fmt.Sprintf("\tif ev.%s, ok = abiunpack.Bytes(data, %d); !ok {\n", arg.GoFieldName, wordIdx))
		b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid bytes\", %q)\n\t}\n", label))
	case solType == "string":
		b.WriteString("\tvar raw" + arg.GoFieldName + " []byte\n")
		b.WriteString(fmt.Sprintf("\tif raw%s, ok = abiunpack.Bytes(data, %d); !ok {\n", arg.GoFieldName, wordIdx))
		b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid string\", %q)\n\t}\n", label))
		b.WriteString(fmt.Sprintf("\tev.%s = string(raw%s)\n", arg.GoFieldName, arg.GoFieldName))
	case solType == "address":
		renderWordGuard(b, label, wordIdx)
		b.WriteString(fmt.Sprintf("\tev.%s = common.BytesToAddress(word[12:32])\n", arg.GoFieldName))
	case solType == "bool":
		renderWordGuard(b, label, wordIdx)
		b.WriteString(fmt.Sprintf("\tif ev.%s, ok = abiunpack.Bool(word); !ok {\n", arg.GoFieldName))
		b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid bool word\", %q)\n\t}\n", label))
	case solType == "bytes32":
		renderWordGuard(b, label, wordIdx)
		b.WriteString(fmt.Sprintf("\tev.%s = common.BytesToHash(word)\n", arg.GoFieldName))
	case isBytesN(solType):
		renderWordGuard(b, label, wordIdx)
		b.WriteString(fmt.Sprintf("\tcopy(ev.%s[:], word[:%d])\n", arg.GoFieldName, bytesNSize(solType)))
	case strings.HasPrefix(solType, "uint"), strings.HasPrefix(solType, "int"):
		renderWordGuard(b, label, wordIdx)
		b.WriteString(fmt.Sprintf("\tev.%s.SetBytes32(word)\n", arg.GoFieldName))
	default:
		b.WriteString(fmt.Sprintf("\t// %s has unsupported ABI type %q.\n", arg.GoFieldName, solType))
	}
}

func renderDynamicArrayDecode(b *bytes.Buffer, label string, arg eventArg, elem string, wordIdx int) {
	switch {
	case strings.HasPrefix(elem, "uint"), strings.HasPrefix(elem, "int"):
		b.WriteString(fmt.Sprintf("\tif ev.%s, ok = abiunpack.Uint256Array(data, %d); !ok {\n", arg.GoFieldName, wordIdx))
		b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid uint256 array\", %q)\n\t}\n", label))
	case elem == "address":
		b.WriteString(fmt.Sprintf("\tif ev.%s, ok = abiunpack.AddressArray(data, %d); !ok {\n", arg.GoFieldName, wordIdx))
		b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid address array\", %q)\n\t}\n", label))
	case elem == "bytes32":
		b.WriteString(fmt.Sprintf("\tif ev.%s, ok = abiunpack.HashArray(data, %d); !ok {\n", arg.GoFieldName, wordIdx))
		b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid hash array\", %q)\n\t}\n", label))
	case elem == "bool":
		b.WriteString(fmt.Sprintf("\tif ev.%s, ok = abiunpack.BoolArray(data, %d); !ok {\n", arg.GoFieldName, wordIdx))
		b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid bool array\", %q)\n\t}\n", label))
	default:
		b.WriteString(fmt.Sprintf("\t// %s has unsupported dynamic array ABI type %q.\n", arg.GoFieldName, arg.SolidityType))
	}
}

func renderFixedArrayDecode(b *bytes.Buffer, label string, arg eventArg, elem string, length int, wordIdx int) {
	for i := 0; i < length; i++ {
		target := fmt.Sprintf("ev.%s[%d]", arg.GoFieldName, i)
		currentLabel := fmt.Sprintf("%s[%d]", label, i)
		switch {
		case strings.HasPrefix(elem, "uint"), strings.HasPrefix(elem, "int"):
			renderWordGuard(b, currentLabel, wordIdx+i)
			b.WriteString(fmt.Sprintf("\t%s.SetBytes32(word)\n", target))
		case elem == "address":
			renderWordGuard(b, currentLabel, wordIdx+i)
			b.WriteString(fmt.Sprintf("\t%s = common.BytesToAddress(word[12:32])\n", target))
		case elem == "bytes32":
			renderWordGuard(b, currentLabel, wordIdx+i)
			b.WriteString(fmt.Sprintf("\t%s = common.BytesToHash(word)\n", target))
		case elem == "bool":
			renderWordGuard(b, currentLabel, wordIdx+i)
			b.WriteString(fmt.Sprintf("\tif %s, ok = abiunpack.Bool(word); !ok {\n", target))
			b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: invalid bool word\", %q)\n\t}\n", currentLabel))
		case isBytesN(elem):
			renderWordGuard(b, currentLabel, wordIdx+i)
			b.WriteString(fmt.Sprintf("\tcopy(%s[:], word[:%d])\n", target, bytesNSize(elem)))
		default:
			b.WriteString(fmt.Sprintf("\t// %s has unsupported fixed array ABI type %q.\n", arg.GoFieldName, arg.SolidityType))
			return
		}
	}
}

func renderWordGuard(b *bytes.Buffer, label string, wordIdx int) {
	b.WriteString(fmt.Sprintf("\tif word, ok = abiunpack.Word(data, %d); !ok {\n", wordIdx))
	b.WriteString(fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"decode %%s: missing ABI word %d (data len %%d)\", %q, len(data))\n", wordIdx, label))
	b.WriteString("\t}\n")
}

func abiHeadWords(solType string) int {
	if _, ok := dynamicArrayElement(solType); ok {
		return 1
	}
	if _, _, ok := fixedArrayElement(solType); ok {
		_, length, _ := fixedArrayElement(solType)
		return length
	}
	return 1
}

func jsonExtractExpr(arg eventArg) string {
	if strings.Contains(arg.SolidityType, "[") || strings.HasPrefix(arg.SolidityType, "(") || strings.HasPrefix(arg.SolidityType, "tuple") {
		return fmt.Sprintf("JSONExtractRaw(params, %s)", quoteSQLString(arg.Name))
	}
	if arg.SolidityType == "bool" {
		return fmt.Sprintf("JSONExtractBool(params, %s)", quoteSQLString(arg.Name))
	}
	return fmt.Sprintf("JSONExtractString(params, %s)", quoteSQLString(arg.Name))
}

func goType(solType string) string {
	if elem, ok := dynamicArrayElement(solType); ok {
		return "[]" + goType(elem)
	}
	if elem, length, ok := fixedArrayElement(solType); ok {
		return fmt.Sprintf("[%d]%s", length, goType(elem))
	}
	switch {
	case solType == "bool":
		return "bool"
	case solType == "address":
		return "common.Address"
	case solType == "bytes32":
		return "common.Hash"
	case isBytesN(solType):
		return fmt.Sprintf("[%d]byte", bytesNSize(solType))
	case solType == "bytes":
		return "[]byte"
	case solType == "string":
		return "string"
	case strings.HasPrefix(solType, "uint"), strings.HasPrefix(solType, "int"):
		return "uint256.Int"
	default:
		return "string"
	}
}

func clickHouseType(solType string) string {
	if _, ok := dynamicArrayElement(solType); ok {
		return "String"
	}
	if _, _, ok := fixedArrayElement(solType); ok {
		return "String"
	}
	switch {
	case solType == "bool":
		return "UInt8"
	case solType == "address":
		return "FixedString(20)"
	case solType == "bytes32":
		return "FixedString(32)"
	case isBytesN(solType):
		return fmt.Sprintf("FixedString(%d)", bytesNSize(solType))
	case solType == "bytes", solType == "string":
		return "String"
	case strings.HasPrefix(solType, "uint"):
		return "UInt256"
	case strings.HasPrefix(solType, "int"):
		return "Int256"
	default:
		return "String"
	}
}

func normalizeSolidityType(typ string) string {
	switch typ {
	case "uint":
		return "uint256"
	case "int":
		return "int256"
	default:
		return typ
	}
}

func hasUint256Type(events []eventSpec) bool {
	for _, ev := range events {
		for _, arg := range ev.Args {
			if strings.Contains(arg.GoType, "uint256.Int") {
				return true
			}
		}
	}
	return false
}

func hasCommonType(events []eventSpec) bool {
	for _, ev := range events {
		for _, arg := range ev.Args {
			if strings.Contains(arg.GoType, "common.") {
				return true
			}
		}
	}
	return len(events) > 0
}

func hasERC20Transfer(events []eventSpec) bool {
	for _, ev := range events {
		if isERC20Transfer(ev) {
			return true
		}
	}
	return false
}

func isERC20Transfer(ev eventSpec) bool {
	return ev.CanonicalSig == "Transfer(address,address,uint256)" &&
		len(ev.Args) == 3 &&
		ev.Args[0].SolidityType == "address" &&
		ev.Args[1].SolidityType == "address" &&
		ev.Args[2].SolidityType == "uint256"
}

func dynamicArrayElement(solType string) (string, bool) {
	if strings.HasSuffix(solType, "[]") {
		return solType[:len(solType)-2], true
	}
	return "", false
}

func fixedArrayElement(solType string) (string, int, bool) {
	close := strings.LastIndex(solType, "]")
	open := strings.LastIndex(solType, "[")
	if open < 0 || close != len(solType)-1 || close <= open+1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(solType[open+1 : close])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return solType[:open], n, true
}

func isBytesN(solType string) bool {
	if !strings.HasPrefix(solType, "bytes") || solType == "bytes" {
		return false
	}
	n := bytesNSize(solType)
	return n > 0 && n <= 32
}

func bytesNSize(solType string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(solType, "bytes"))
	return n
}

func addresses(addr any) []string {
	switch v := addr.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func uniqueLower(used map[string]int, base string) string {
	if base == "" {
		base = "event"
	}
	count := used[base]
	used[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}

func uniqueExported(used map[string]int, base string) string {
	if base == "" {
		base = "Generated"
	}
	count := used[base]
	used[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s%d", base, count+1)
}

func exportIdent(s string) string {
	parts := identifierParts(s)
	if len(parts) == 0 {
		return "Generated"
	}
	var b strings.Builder
	for _, part := range parts {
		runes := []rune(part)
		if len(runes) == 0 {
			continue
		}
		b.WriteRune(unicode.ToUpper(runes[0]))
		for _, r := range runes[1:] {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "Generated"
	}
	if r := []rune(out)[0]; unicode.IsDigit(r) {
		return "N" + out
	}
	return out
}

func toSnake(s string) string {
	parts := identifierParts(s)
	if len(parts) == 0 {
		return "event"
	}
	for i := range parts {
		parts[i] = strings.ToLower(parts[i])
	}
	return strings.Join(parts, "_")
}

func identifierParts(s string) []string {
	var parts []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			parts = append(parts, b.String())
			b.Reset()
		}
	}
	var prevLower bool
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if unicode.IsUpper(r) && prevLower {
				flush()
			}
			b.WriteRune(r)
			prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
			continue
		}
		flush()
		prevLower = false
	}
	flush()
	return parts
}

func quoteSQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func quoteSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func strip0x(v string) string {
	return strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X")
}
