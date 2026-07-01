package codegen

import (
	"strings"
	"testing"
)

// TestRecoveryColumnFor pins the auto-detection of the recency column recovery
// uses to bound its scan: "updated_at_block" (a user-declared ReplacingMergeTree
// version column) wins when present, "block_number" (codegen-injected on every
// non-event hot-state table via addRequiredBlockFields) is the fallback, and a
// table with neither degrades to "" (no recency filter, matching pre-fix
// behavior) rather than referencing an undeclared column.
func TestRecoveryColumnFor(t *testing.T) {
	tests := []struct {
		name   string
		fields []customFieldSpec
		want   string
	}{
		{
			name: "updated_at_block present wins over block_number",
			fields: []customFieldSpec{
				{Name: "ID", ColumnName: "id"},
				{Name: "BlockNumber", ColumnName: "block_number"},
				{Name: "UpdatedAtBlock", ColumnName: "updated_at_block"},
			},
			want: "updated_at_block",
		},
		{
			name: "block_number fallback when no updated_at_block (the common case: every non-event table gets block_number via addRequiredBlockFields, but not every project declares updated_at_block)",
			fields: []customFieldSpec{
				{Name: "ID", ColumnName: "id"},
				{Name: "BlockNumber", ColumnName: "block_number"},
				{Name: "TxIndex", ColumnName: "transaction_index"},
				{Name: "LogIndex", ColumnName: "log_index"},
			},
			want: "block_number",
		},
		{
			name: "neither column present degrades to no recency filter",
			fields: []customFieldSpec{
				{Name: "ID", ColumnName: "id"},
				{Name: "Symbol", ColumnName: "symbol"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table := customTableSpec{Fields: tt.fields}
			if got := recoveryColumnFor(table); got != tt.want {
				t.Errorf("recoveryColumnFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRecoverWhereExprUsesAutoDetectedColumn confirms recoverWhereExpr embeds
// the auto-detected column (not a hardcoded "updated_at_block") into the
// generated recoveryRecencyClauseFor call.
func TestRecoverWhereExprUsesAutoDetectedColumn(t *testing.T) {
	withUpdatedAt := customTableSpec{
		PrimaryKey: []string{"id"},
		Fields: []customFieldSpec{
			{Name: "ID", ColumnName: "id", ColumnType: "UInt64"},
			{Name: "BlockNumber", ColumnName: "block_number"},
			{Name: "UpdatedAtBlock", ColumnName: "updated_at_block"},
		},
	}
	if got := recoverWhereExpr(withUpdatedAt); got == "" {
		t.Fatal("recoverWhereExpr returned empty for a table with a primary key")
	} else if want := `recoveryRecencyClauseFor(floor, "updated_at_block")`; !strings.Contains(got, want) {
		t.Errorf("recoverWhereExpr() = %q, want it to contain %q", got, want)
	}

	blockNumberOnly := customTableSpec{
		PrimaryKey: []string{"id"},
		Fields: []customFieldSpec{
			{Name: "ID", ColumnName: "id", ColumnType: "UInt64"},
			{Name: "BlockNumber", ColumnName: "block_number"},
		},
	}
	if got := recoverWhereExpr(blockNumberOnly); !strings.Contains(got, `recoveryRecencyClauseFor(floor, "block_number")`) {
		t.Errorf("recoverWhereExpr() = %q, want it to fall back to block_number", got)
	}
}
