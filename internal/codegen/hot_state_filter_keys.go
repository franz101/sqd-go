package codegen

import (
	"bytes"
	"strings"

	"github.com/franz101/sqd-go/internal/template"
)

// clockFilterKeysData is the template data for clockFilterKeysGo.
type clockFilterKeysData struct {
	KeyType   string
	TableName string
	SQL       string
	WhereExpr string
	PKFields  []clockFilterFieldData
}

type clockFilterFieldData struct {
	ColVar     string
	ColType    string
	ColumnName string
	FieldName  string
	InitLines  []string
	ResultData string
	ValueExpr  string
}

// renderClockFilterKeysPass emits a recovery pass that adds the KEYS of positions
// last updated below SQD_RECOVERY_MIN_BLOCK to the cold tier's negative filter,
// without loading their values. The recency-bounded value load
// (recoverColdParallel) only adds keys for updated_at_block >= floor; the
// authoritative skip-CH gate (coldAuthoritative && !ColdMightContain) would then
// treat a real, pre-floor position as brand-new on a hot+cold miss, skip
// ClickHouse, and overwrite it with zero. This keys-only pass closes that gap so
// the filter is complete (every key that exists in ClickHouse is "maybe present")
// while the cold VALUE store stays bounded by the floor.
//
// No-op for entities without an updated_at_block column (the recency clause never
// applied, so the value load already covered every key).
func renderClockFilterKeysPass(b *bytes.Buffer, spec hotStateSpec) {
	hasUpdatedAt := false
	for _, f := range spec.table.Fields {
		if f.ColumnName == "updated_at_block" {
			hasUpdatedAt = true
			break
		}
	}
	if !hasUpdatedAt {
		return
	}

	var pk []customFieldSpec
	for _, col := range spec.table.PrimaryKey {
		for _, f := range spec.table.Fields {
			if f.ColumnName == col {
				pk = append(pk, f)
				break
			}
		}
	}
	if len(pk) == 0 {
		return
	}

	qcol := func(col string) string { return "`t`." + quoteSQLIdent(col) }
	selects := make([]string, 0, len(pk))
	groupBy := make([]string, 0, len(pk))
	for _, f := range pk {
		selects = append(selects, qcol(f.ColumnName)+" AS "+quoteSQLIdent(f.ColumnName))
		groupBy = append(groupBy, qcol(f.ColumnName))
	}
	sql := "SELECT " + strings.Join(selects, ", ") + " FROM %s.%s AS `t` WHERE %s GROUP BY " + strings.Join(groupBy, ", ")

	fields := make([]clockFilterFieldData, 0, len(pk))
	for _, f := range pk {
		fields = append(fields, clockFilterFieldData{
			ColVar:     batchColumnField(f),
			ColType:    resultColumnType(f),
			ColumnName: f.ColumnName,
			FieldName:  f.Name,
			InitLines:  resultInitLines(f),
			ResultData: resultData(f),
			ValueExpr:  resultValueExpr(f, false),
		})
	}

	data := clockFilterKeysData{
		KeyType:   spec.keyType,
		TableName: spec.table.Name,
		SQL:       sql,
		WhereExpr: recoverBucketWhereExpr(spec.table),
		PKFields:  fields,
	}

	b.WriteString(template.MustExecute("code/clockFilterKeysGo", data))
}
