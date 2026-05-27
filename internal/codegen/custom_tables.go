package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/franz101/sqd-go/internal/config"
)

type customTableSpec struct {
	Name          string
	Engine        string
	PrimaryKey    []string
	OrderBy       []string
	GoTypeName    string
	Fields        []customFieldSpec
	Columns       []customColumnSpec
	PrimaryKeySet bool
}

type customColumnSpec struct {
	Name    string
	Type    string
	Default string
}

type customFieldSpec struct {
	Name       string
	Type       string
	ColumnName string
	ColumnType string
	Default    string
}

func loadCustomTableSpecs(root string) ([]customTableSpec, error) {
	path := filepath.Join(root, "custom_types.go")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Size() == 0 {
		return nil, nil
	}

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse custom types: %w", err)
	}

	var tables []customTableSpec
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			table, err := customTableFromType(gen, typeSpec)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", typeSpec.Name.Name, err)
			}
			for _, field := range structType.Fields.List {
				customFields, err := parseCustomFields(fileSet, field)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", typeSpec.Name.Name, err)
				}
				for _, customField := range customFields {
					table.Fields = append(table.Fields, customField)
					table.Columns = append(table.Columns, customColumnSpec{
						Name:    customField.ColumnName,
						Type:    customField.ColumnType,
						Default: customField.Default,
					})
				}
			}
			table.addRequiredBlockFields()
			if len(table.Columns) == 0 {
				return nil, fmt.Errorf("%s: custom table requires at least one field", typeSpec.Name.Name)
			}
			table.resolvePrimaryKey(typeSpec.Name.Name)
			table.OrderBy = finalCustomOrderBy(table.PrimaryKey)
			for _, column := range table.PrimaryKey {
				if _, ok := table.fieldByColumn(column); !ok {
					return nil, fmt.Errorf("%s: custom table pk column %q does not match a field", typeSpec.Name.Name, column)
				}
			}
			tables = append(tables, table)
		}
	}
	return tables, nil
}

func customTableFromType(gen *ast.GenDecl, typeSpec *ast.TypeSpec) (customTableSpec, error) {
	return customTableSpec{
		Name:          customTableName(typeSpec.Name.Name),
		Engine:        "ReplacingMergeTree(block_number)",
		GoTypeName:    customTableEntityName(typeSpec.Name.Name),
		PrimaryKeySet: false,
	}, nil
}

func parseCustomFields(fileSet *token.FileSet, field *ast.Field) ([]customFieldSpec, error) {
	typeName := exprString(fileSet, field.Type)
	if typeName == "" {
		return nil, nil
	}

	var names []string
	for _, name := range field.Names {
		names = append(names, name.Name)
	}
	if len(names) == 0 {
		names = append(names, exportIdent(typeName))
	}

	fields := make([]customFieldSpec, 0, len(names))
	for _, name := range names {
		columnName := toSnake(name)
		columnType := clickHouseTypeFromGo(typeName)
		var chDefault string
		if typeName == "time.Time" {
			chDefault = "now64(3)"
		}
		fields = append(fields, customFieldSpec{
			Name:       name,
			Type:       typeName,
			ColumnName: columnName,
			ColumnType: columnType,
			Default:    chDefault,
		})
	}
	return fields, nil
}

func generateCustomSchemaSQL(cfg *config.Config, tables []customTableSpec) string {
	var b strings.Builder
	db := quoteSQLIdent(cfg.Name)
	b.WriteString("\n")
	b.WriteString("-- Custom tables generated from custom_types.go.\n")
	for _, table := range tables {
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s.%s (\n", db, quoteSQLIdent(table.Name)))
		for i, column := range table.Columns {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString("  ")
			b.WriteString(quoteSQLIdent(column.Name))
			b.WriteString(" ")
			b.WriteString(column.Type)
			if column.Default != "" {
				b.WriteString(" DEFAULT ")
				b.WriteString(column.Default)
			}
		}
		b.WriteString("\n) ENGINE = ")
		b.WriteString(table.Engine)
		b.WriteString("\nPRIMARY KEY (")
		for i, column := range table.PrimaryKey {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteSQLIdent(column))
		}
		b.WriteString(")")
		b.WriteString("\nORDER BY (")
		for i, column := range table.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteSQLIdent(column))
		}
		b.WriteString(");\n")
	}
	return b.String()
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func customTableEntityName(typeName string) string {
	typeName = strings.TrimSuffix(typeName, "Schema")
	typeName = strings.TrimSuffix(typeName, "Table")
	if typeName == "" {
		return "CustomEntity"
	}
	return typeName
}

func exprString(fileSet *token.FileSet, expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprString(fileSet, t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(fileSet, t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + exprString(fileSet, t.Elt)
		}
		return "[" + exprString(fileSet, t.Len) + "]" + exprString(fileSet, t.Elt)
	case *ast.MapType:
		return fmt.Sprintf("map[%s]%s", exprString(fileSet, t.Key), exprString(fileSet, t.Value))
	case *ast.InterfaceType:
		return "any"
	case *ast.BasicLit:
		return t.Value
	case *ast.IndexExpr:
		return exprString(fileSet, t.X) + "[" + exprString(fileSet, t.Index) + "]"
	case *ast.IndexListExpr:
		var parts []string
		for _, index := range t.Indices {
			parts = append(parts, exprString(fileSet, index))
		}
		return exprString(fileSet, t.X) + "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func (t customTableSpec) fieldByColumn(column string) (customFieldSpec, bool) {
	for _, field := range t.Fields {
		if field.ColumnName == column {
			return field, true
		}
	}
	return customFieldSpec{}, false
}

func (t customTableSpec) keyFields() []customFieldSpec {
	fields := make([]customFieldSpec, 0, len(t.PrimaryKey))
	for _, column := range t.PrimaryKey {
		if field, ok := t.fieldByColumn(column); ok {
			fields = append(fields, field)
		}
	}
	return fields
}

func (t *customTableSpec) addRequiredBlockFields() {
	for _, field := range []customFieldSpec{
		{Name: "BlockNumber", Type: "uint64", ColumnName: "block_number", ColumnType: "UInt64"},
		{Name: "TxIndex", Type: "uint64", ColumnName: "transaction_index", ColumnType: "UInt64"},
		{Name: "LogIndex", Type: "uint64", ColumnName: "log_index", ColumnType: "UInt64"},
	} {
		if _, ok := t.fieldByColumn(field.ColumnName); ok {
			continue
		}
		t.Fields = append(t.Fields, field)
		t.Columns = append(t.Columns, customColumnSpec{Name: field.ColumnName, Type: field.ColumnType})
	}
}

func customTableName(typeName string) string {
	return toSnake(pluralizeGoName(customTableEntityName(typeName)))
}

func (t *customTableSpec) resolvePrimaryKey(typeName string) {
	if t.PrimaryKeySet {
		t.PrimaryKey = t.normalizePrimaryKey(t.PrimaryKey)
		return
	}
	t.PrimaryKey = inferCustomPrimaryKey(t.Fields)
	fmt.Fprintf(os.Stderr, "warning: %s has no pk comment; inferred primary key %s\n", typeName, strings.Join(t.PrimaryKey, ", "))
}

func (t customTableSpec) normalizePrimaryKey(primaryKey []string) []string {
	out := make([]string, 0, len(primaryKey))
	for _, item := range primaryKey {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if field, ok := customFieldByName(t.Fields, item); ok {
			out = append(out, field.ColumnName)
			continue
		}
		out = append(out, toSnake(item))
	}
	return out
}

func finalCustomOrderBy(primaryKey []string) []string {
	out := make([]string, 0, len(primaryKey)+3)
	out = append(out, primaryKey...)
	out = append(out, "block_number", "transaction_index", "log_index")
	return out
}

func inferCustomPrimaryKey(fields []customFieldSpec) []string {
	if hasCustomField(fields, "User") && hasCustomField(fields, "TokenID") {
		return []string{toSnake("User"), toSnake("TokenID")}
	}
	for _, name := range []string{"ID", "Address", "User", "MarketID", "ConditionID"} {
		if field, ok := customFieldByName(fields, name); ok {
			return []string{field.ColumnName}
		}
	}
	for _, field := range fields {
		if strings.HasSuffix(field.Name, "ID") && !isRequiredBlockColumn(field.ColumnName) {
			return []string{field.ColumnName}
		}
	}
	for _, field := range fields {
		if !isRequiredBlockColumn(field.ColumnName) {
			return []string{field.ColumnName}
		}
	}
	return []string{"block_number", "transaction_index", "log_index"}
}

func hasCustomField(fields []customFieldSpec, name string) bool {
	_, ok := customFieldByName(fields, name)
	return ok
}

func customFieldByName(fields []customFieldSpec, name string) (customFieldSpec, bool) {
	for _, field := range fields {
		if field.Name == name {
			return field, true
		}
	}
	return customFieldSpec{}, false
}

func isRequiredBlockColumn(column string) bool {
	switch column {
	case "block_number", "transaction_index", "log_index":
		return true
	default:
		return false
	}
}

func clickHouseTypeFromGo(goType string) string {
	switch goType {
	case "bool":
		return "UInt8"
	case "uint8":
		return "UInt8"
	case "uint16":
		return "UInt16"
	case "uint32":
		return "UInt32"
	case "uint", "uint64":
		return "UInt64"
	case "int8":
		return "Int8"
	case "int16":
		return "Int16"
	case "int32":
		return "Int32"
	case "int", "int64":
		return "Int64"
	case "float32":
		return "Float32"
	case "float64":
		return "Float64"
	case "string":
		return "String"
	case "time.Time":
		return "DateTime64(3, 'UTC')"
	case "common.Address":
		return "FixedString(20)"
	case "common.Hash":
		return "FixedString(32)"
	case "uint256.Int":
		return "UInt256"
	case "decimal.Decimal":
		return "Decimal(38, 18)"
	case "[]byte":
		return "String"
	}
	if strings.HasPrefix(goType, "[]") {
		return "Array(" + clickHouseTypeFromGo(strings.TrimPrefix(goType, "[]")) + ")"
	}
	if strings.HasPrefix(goType, "*") {
		return clickHouseTypeFromGo(strings.TrimPrefix(goType, "*"))
	}
	return "String"
}
