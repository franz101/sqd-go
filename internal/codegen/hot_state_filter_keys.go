package codegen

import (
	"bytes"
	"strconv"
	"strings"
)

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

	b.WriteString("// Negative-filter completeness pass: keys whose latest update is below the\n")
	b.WriteString("// recovery floor are excluded from the value load above and would otherwise be\n")
	b.WriteString("// absent from the filter, making the authoritative skip-CH gate reset a real\n")
	b.WriteString("// pre-existing position to zero. Add their keys only (values not needed).\n")
	b.WriteString("if preClause, active := recoveryPreFloorClause(); active {\n")
	b.WriteString("if err := recoverFilterKeysParallel(ctx, conn, recoveryBucketCount, c.cold, func(ctx context.Context, conn *ch.Client, bucket int, emit func(" + spec.keyType + ")) error {\n")
	b.WriteString("var (\n")
	for _, f := range pk {
		b.WriteString(batchColumnField(f) + " " + resultColumnType(f) + "\n")
	}
	b.WriteString(")\n")
	for _, f := range pk {
		for _, line := range resultInitLines(f) {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("results := proto.Results{\n")
	for _, f := range pk {
		b.WriteString("{Name: " + strconv.Quote(f.ColumnName) + ", Data: " + resultData(f) + "},\n")
	}
	b.WriteString("}\n")
	b.WriteString("return conn.Do(ctx, ch.Query{Body: fmt.Sprintf(" + strconv.Quote(sql) + ", quoteIdent(db), quoteIdent(" + strconv.Quote(spec.table.Name) + "), " + recoverBucketWhereExpr(spec.table) + "+preClause), Result: results, OnResult: func(ctx context.Context, block proto.Block) error {\n")
	b.WriteString("for i := 0; i < block.Rows; i++ {\n")
	b.WriteString("emit(" + spec.keyType + "{\n")
	for _, f := range pk {
		b.WriteString(f.Name + ": " + resultValueExpr(f, false) + ",\n")
	}
	b.WriteString("})\n")
	b.WriteString("}\n")
	b.WriteString("return nil\n")
	b.WriteString("}})\n")
	b.WriteString("}); err != nil {\n")
	b.WriteString("return err\n")
	b.WriteString("}\n")
	b.WriteString("}\n")
}
