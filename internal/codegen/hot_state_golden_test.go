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

// hotStateGoldenTables builds a representative table set exercising every
// cold-tier classification plus an event entity:
//   - MemoryUserPosition: pointer-free -> raw-bytes cold tier
//   - MemoryCondition:    slice-bearing serializable -> codec cold tier
//   - MemoryTokenMeta:    string field -> no cold tier
//   - TransferEvent:      IsEvent -> append-only (no dirty/Update/Restore)
func hotStateGoldenTables() []customTableSpec {
	return []customTableSpec{
		{
			Name:       "user_positions",
			GoTypeName: "MemoryUserPosition",
			IsEvent:    false,
			Fields: []customFieldSpec{
				{Name: "Account", Type: "common.Address", ColumnName: "account", ColumnType: "FixedString(20)"},
				{Name: "Balance", Type: "uint256.Int", ColumnName: "balance", ColumnType: "UInt256"},
				{Name: "TransferCount", Type: "uint64", ColumnName: "transfer_count", ColumnType: "UInt64"},
				// updated_at_block triggers renderClockFilterKeysPass (the cold
				// recovery filter-keys pass), exercising that splice in Recover.
				{Name: "UpdatedAtBlock", Type: "uint64", ColumnName: "updated_at_block", ColumnType: "UInt64"},
			},
			PrimaryKey: []string{"account"},
		},
		{
			Name:       "conditions",
			GoTypeName: "MemoryCondition",
			IsEvent:    false,
			Fields: []customFieldSpec{
				{Name: "Id", Type: "common.Hash", ColumnName: "id", ColumnType: "FixedString(32)"},
				{Name: "Payouts", Type: "[]uint256.Int", ColumnName: "payouts", ColumnType: "Array(UInt256)"},
			},
			PrimaryKey: []string{"id"},
		},
		{
			Name:       "token_meta",
			GoTypeName: "MemoryTokenMeta",
			IsEvent:    false,
			Fields: []customFieldSpec{
				{Name: "Token", Type: "common.Address", ColumnName: "token", ColumnType: "FixedString(20)"},
				{Name: "Symbol", Type: "string", ColumnName: "symbol", ColumnType: "String"},
			},
			PrimaryKey: []string{"token"},
		},
		{
			Name:       "transfer_events",
			GoTypeName: "TransferEvent",
			IsEvent:    true,
			Fields: []customFieldSpec{
				{Name: "From", Type: "common.Address", ColumnName: "from_addr", ColumnType: "FixedString(20)"},
				{Name: "Value", Type: "uint256.Int", ColumnName: "value", ColumnType: "UInt256"},
			},
			PrimaryKey: []string{"from_addr"},
		},
	}
}

// TestHotStateGoldenOutput is the parity safety net: it freezes the full
// generateHotStateGo output so render-function migrations to templates can be
// proven byte-identical. It also lints the generated source.
func TestHotStateGoldenOutput(t *testing.T) {
	tables := hotStateGoldenTables()

	out, err := generateHotStateGo(tables, nil)
	if err != nil {
		t.Fatalf("generateHotStateGo: %v", err)
	}

	const goldenPath = "/tmp/goldens/hot_state.go"
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

	// Linter check 1: parse.
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "hot_state.go", out, parser.AllErrors); perr != nil {
		t.Errorf("generated output does not parse: %v", perr)
	}

	// Linter check 2: gofmt-stable.
	reformatted, ferr := format.Source(out)
	if ferr != nil {
		t.Fatalf("format.Source failed: %v", ferr)
	}
	if !bytes.Equal(out, reformatted) {
		t.Errorf("generated output is not gofmt-stable")
	}

	// Linter check 3: structural needles across branches.
	s := string(out)
	for _, needle := range []string{
		"type HotState struct {",
		"func NewHotState(capacity uint64) *HotState {",
		"type MemoryUserPosition struct {",
		"type UserPositionsClockKey struct {",
		"func (s *HotState) UpdateMemoryUserPosition(",
		"func (s *HotState) RestoreMemoryCondition(",
		"coldcache.Open",                      // cold tier present (pointer-free + serializable)
		"func NewTransferEventBatchResolver(", // event resolver
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("generated output missing expected string: %q", needle)
		}
	}
}
