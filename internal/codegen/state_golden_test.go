package codegen

import (
	"bytes"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

// stateGoldenInput builds a representative set of inputs that exercises every
// branch of generateStateGo:
//   - a pointer-free entity that participates in the cold tier (usesCold == true)
//   - a string-bearing entity that does NOT (usesCold == false)
//   - a cfg.State alias whose handle name differs from the value type (type-alias branch)
func stateGoldenInput() ([]customTableSpec, *config.Config, []eventSpec) {
	userPositions := customTableSpec{
		Name:       "user_positions",
		GoTypeName: "MemoryUserPosition",
		IsEvent:    false,
		Fields: []customFieldSpec{
			{Name: "Account", Type: "common.Address", ColumnName: "account", ColumnType: "FixedString(20)"},
			{Name: "Balance", Type: "uint256.Int", ColumnName: "balance", ColumnType: "UInt256"},
			{Name: "TransferCount", Type: "uint64", ColumnName: "transfer_count", ColumnType: "UInt64"},
			{Name: "UpdatedAtBlock", Type: "uint64", ColumnName: "updated_at_block", ColumnType: "UInt64"},
			{Name: "UpdatedAt", Type: "time.Time", ColumnName: "updated_at", ColumnType: "DateTime64(3, 'UTC')"},
		},
		PrimaryKey: []string{"account"},
	}
	tokenMeta := customTableSpec{
		Name:       "token_meta",
		GoTypeName: "MemoryTokenMeta",
		IsEvent:    false,
		Fields: []customFieldSpec{
			{Name: "Token", Type: "common.Address", ColumnName: "token", ColumnType: "FixedString(20)"},
			{Name: "Symbol", Type: "string", ColumnName: "symbol", ColumnType: "String"},
			{Name: "UpdatedAtBlock", Type: "uint64", ColumnName: "updated_at_block", ColumnType: "UInt64"},
		},
		PrimaryKey: []string{"token"},
	}
	tables := []customTableSpec{userPositions, tokenMeta}
	cfg := &config.Config{
		Name:  "test_project",
		State: []config.StateConfig{{Name: "Account", SourceTable: "user_positions"}},
	}
	return tables, cfg, nil
}

// TestStateGoldenOutput captures (or compares against) the golden state.go output
// and runs a linter pass over the generated source.
func TestStateGoldenOutput(t *testing.T) {
	tables, cfg, events := stateGoldenInput()

	out, err := generateStateGo(tables, cfg, events)
	if err != nil {
		t.Fatalf("generateStateGo: %v", err)
	}

	const goldenPath = "/tmp/goldens/state.go"
	if golden, rerr := os.ReadFile(goldenPath); rerr == nil {
		if !bytes.Equal(out, golden) {
			t.Errorf("output differs from golden:\n--- golden\n%s\n--- got\n%s", golden, out)
		}
	} else {
		// First run: capture the golden from the current implementation.
		_ = os.MkdirAll("/tmp/goldens", 0o755)
		if werr := os.WriteFile(goldenPath, out, 0o644); werr != nil {
			t.Fatalf("write golden: %v", werr)
		}
		t.Logf("captured golden (%d bytes) at %s", len(out), goldenPath)
	}

	// Linter check 1: the generated source must parse as a Go file.
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "state.go", out, parser.AllErrors); perr != nil {
		t.Errorf("generated output does not parse: %v", perr)
	}

	// Linter check 2: gofmt-clean (format.Source is idempotent on the output).
	reformatted, ferr := format.Source(out)
	if ferr != nil {
		t.Fatalf("format.Source failed: %v", ferr)
	}
	if !bytes.Equal(out, reformatted) {
		t.Errorf("generated output is not gofmt-stable")
	}

	// Linter check 3: structural needles that must survive any refactor.
	s := string(out)
	for _, needle := range []string{
		"package generated",
		"type State struct {",
		"func NewState() *State {",
		"func (s *State) Commit(ctx context.Context, store Store) error {",
		"func (s *State) RestoreToBlock(blockNumber uint64) (uint64, error) {",
		"type AccountState struct{ state *State }",
		"type UserPositionState struct{ state *State }",
		"type TokenMetaState struct{ state *State }",
		"coldAuthoritative", // cold-tier branch present (usesCold == true entity)
		"func (h TokenMetaState) Save(",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("generated output missing expected string: %q", needle)
		}
	}
}
