package codegen

import (
	"bytes"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// unpackGoldenEvents exercises both dispatcher branches: an event with multiple
// contract addresses (address-match expression) and one with none (the
// "_ = address" path), plus indexed + non-indexed args for the unpack functions.
func unpackGoldenEvents() []eventSpec {
	return []eventSpec{
		{
			EventName:       "Transfer",
			GoTypeName:      "Transfer",
			Topic0:          "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
			ContractAddress: []string{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "0xdAC17F958D2ee523a2206206994597C13D831ec7"},
			Args: []eventArg{
				{Name: "from", SolidityType: "address", Indexed: true, GoFieldName: "From", GoType: "common.Address"},
				{Name: "to", SolidityType: "address", Indexed: true, GoFieldName: "To", GoType: "common.Address"},
				{Name: "value", SolidityType: "uint256", Indexed: false, GoFieldName: "Value", GoType: "uint256.Int"},
			},
		},
		{
			EventName:       "Approval",
			GoTypeName:      "Approval",
			Topic0:          "0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925",
			ContractAddress: nil,
			Args: []eventArg{
				{Name: "owner", SolidityType: "address", Indexed: true, GoFieldName: "Owner", GoType: "common.Address"},
				{Name: "value", SolidityType: "uint256", Indexed: false, GoFieldName: "Value", GoType: "uint256.Int"},
			},
		},
	}
}

// TestUnpackDispatcherGoldenOutput freezes renderUnpackDispatcher output so the
// dispatcher-skeleton migration to a template stays byte-identical. The output is
// wrapped in a minimal package and linted.
func TestUnpackDispatcherGoldenOutput(t *testing.T) {
	var b bytes.Buffer
	renderUnpackDispatcher(&b, unpackGoldenEvents())
	out := b.Bytes()

	const goldenPath = "/tmp/goldens/unpack_dispatcher.go"
	if golden, rerr := os.ReadFile(goldenPath); rerr == nil {
		if !bytes.Equal(out, golden) {
			t.Errorf("output differs from golden:\n--- golden\n%s\n--- got\n%s", golden, out)
		}
	} else {
		_ = os.MkdirAll("/tmp/goldens", 0o755)
		if werr := os.WriteFile(goldenPath, out, 0o644); werr != nil {
			t.Fatalf("write golden: %v", werr)
		}
		t.Logf("captured golden (%d bytes) at %s", len(out), goldenPath)
	}

	// Lint: wrap the fragment in a package with the imports it references.
	wrapped := "package p\n\nimport (\n\t\"github.com/ethereum/go-ethereum/common\"\n\t\"github.com/franz101/sqd-go/abiunpack\"\n\t\"github.com/holiman/uint256\"\n)\n\nvar _ = abiunpack.TopicBool\nvar _ = uint256.Int{}\nvar _ = common.Address{}\n\ntype EventMeta struct{}\ntype Transfer struct{ EventMeta EventMeta; From, To common.Address; Value uint256.Int }\ntype Approval struct{ EventMeta EventMeta; Owner common.Address; Value uint256.Int }\n\n" + string(out)
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "unpack.go", wrapped, parser.AllErrors); perr != nil {
		t.Errorf("generated output does not parse: %v", perr)
	}
	if _, ferr := format.Source([]byte(wrapped)); ferr != nil {
		t.Errorf("go/format failed on generated output: %v", ferr)
	}
	s := string(out)
	for _, needle := range []string{
		"var _topic0 = common.HexToHash(",
		"var _addr0_0 = common.HexToAddress(",
		"func UnpackLogWithMeta(",
		"if topic0 == _topic0 && (logAddress == _addr0_0 || logAddress == _addr0_1) {",
		"if topic0 == _topic1 && true {",
		"func UnpackTransferLogWithMeta(",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("missing expected string: %q", needle)
		}
	}
}
