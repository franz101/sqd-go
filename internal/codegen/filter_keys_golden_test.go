package codegen

import (
	"bytes"
	"go/format"
	"os"
	"strings"
	"testing"
)

func TestFilterKeysGoldenOutput(t *testing.T) {
	table := customTableSpec{
		Name:       "erc20_address_positions",
		GoTypeName: "Erc20AddressPosition",
		IsEvent:    false,
		Fields: []customFieldSpec{
			{Name: "Address", Type: "common.Address", ColumnName: "address", ColumnType: "FixedString(20)"},
			{Name: "Balance", Type: "big.Int", ColumnName: "balance_raw", ColumnType: "String"},
			{Name: "TotalIn", Type: "uint256.Int", ColumnName: "total_in_raw", ColumnType: "String"},
			{Name: "TotalOut", Type: "uint256.Int", ColumnName: "total_out_raw", ColumnType: "String"},
			{Name: "NetFlow", Type: "big.Int", ColumnName: "net_flow_raw", ColumnType: "String"},
			{Name: "RealizedPnL", Type: "big.Int", ColumnName: "realized_pnl_raw", ColumnType: "String"},
			{Name: "TransferCount", Type: "uint64", ColumnName: "transfer_count", ColumnType: "UInt64"},
			{Name: "UpdatedAtBlock", Type: "uint64", ColumnName: "updated_at_block", ColumnType: "UInt64"},
		},
		PrimaryKey: []string{"address"},
	}
	spec := hotStateSpec{
		table:   table,
		keyType: "AddressKey",
	}

	var b bytes.Buffer
	renderClockFilterKeysPass(&b, spec)
	out := b.Bytes()

	// 1:1 parity: compare against golden file
	golden, err := os.ReadFile("/tmp/goldens/filter_keys_pass.go")
	if err == nil {
		if !bytes.Equal(out, golden) {
			t.Errorf("output differs from golden:\n--- golden\n%s\n--- got\n%s", golden, out)
		}
	}

	// Linter check: wrap in a valid Go function and try to format
	wrapped := "package p\nfunc _() {\n" + string(out) + "\n}\n"
	formatted, fmtErr := format.Source([]byte(wrapped))
	if fmtErr != nil {
		t.Errorf("go/format failed on generated output: %v\nsource:\n%s", fmtErr, wrapped)
		return
	}

	// Verify formatted output still contains the key structures
	formattedStr := string(formatted)
	for _, needle := range []string{
		"recoveryPreFloorClause",
		"recoverFilterKeysParallel",
		"recoveryBucketCount",
		"proto.ColFixedStr",
		"proto.Results",
		"colAddress.SetSize",
		"colAddress.Row(i)",
		"recoveryFixedStringRange",
	} {
		if !strings.Contains(formattedStr, needle) {
			t.Errorf("formatted output missing expected string: %q", needle)
		}
	}

	t.Logf("Generated %d bytes, formatted OK", len(out))
}
