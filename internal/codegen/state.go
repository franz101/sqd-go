package codegen

import (
	"fmt"
	"go/format"
	"sort"
	"strings"

	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/template"
)

type stateHandleSpec struct {
	name      string
	stateType string
	valueName string
	valueType string
	spec      hotStateSpec
}

// Template-facing view types. generateStateGo precomputes everything the
// state.go.tmpl needs (imports, per-handle accessors, snapshot entities) so the
// template stays declarative and the Go side keeps the branching logic.

type stateKeyFieldTmpl struct {
	Name      string
	LowerName string
	Type      string
}

type stateSaveAssignTmpl struct {
	Field string
	Expr  string
}

type stateHandleTmpl struct {
	Name        string
	StateType   string
	ValueName   string
	ValueType   string
	IsEvent     bool
	UsesCold    bool
	NeedsAlias  bool
	BaseName    string
	KeyType     string
	KeyFields   []stateKeyFieldTmpl
	SaveAssigns []stateSaveAssignTmpl
}

type stateSnapshotTmpl struct {
	Field      string
	GoTypeName string
	BaseName   string
	KeyType    string
}

type stateTmplData struct {
	Imports   []string
	Handles   []stateHandleTmpl
	Snapshots []stateSnapshotTmpl
}

func generateStateGo(tables []customTableSpec, cfg *config.Config, events []eventSpec) ([]byte, error) {
	specs := hotStateSpecs(tables)
	handles := buildStateHandles(specs, cfg, events)

	data := buildStateTmplData(specs, handles)

	src := template.MustExecute("code/stateGo", data)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return []byte(src), fmt.Errorf("format source: %w", err)
	}
	return formatted, nil
}

func buildStateTmplData(specs []hotStateSpec, handles []stateHandleSpec) stateTmplData {
	data := stateTmplData{
		Imports: stateImports(handles),
	}
	for _, handle := range handles {
		data.Handles = append(data.Handles, buildStateHandleTmpl(handle))
	}
	for _, spec := range specs {
		if spec.table.IsEvent {
			continue
		}
		data.Snapshots = append(data.Snapshots, stateSnapshotTmpl{
			Field:      lowerFirst(spec.baseName),
			GoTypeName: spec.table.GoTypeName,
			BaseName:   spec.baseName,
			KeyType:    spec.keyType,
		})
	}
	return data
}

func buildStateHandleTmpl(handle stateHandleSpec) stateHandleTmpl {
	out := stateHandleTmpl{
		Name:       handle.name,
		StateType:  handle.stateType,
		ValueName:  handle.valueName,
		ValueType:  handle.valueType,
		IsEvent:    handle.spec.table.IsEvent,
		UsesCold:   entityUsesCold(handle.spec.table),
		NeedsAlias: handle.valueName != handle.valueType,
		BaseName:   handle.spec.baseName,
		KeyType:    handle.spec.keyType,
	}
	for _, field := range handle.spec.table.keyFields() {
		out.KeyFields = append(out.KeyFields, stateKeyFieldTmpl{
			Name:      field.Name,
			LowerName: lowerFirst(field.Name),
			Type:      field.Type,
		})
	}
	if !handle.spec.table.IsEvent {
		out.SaveAssigns = stateSaveAssigns(handle.spec.table)
	}
	return out
}

// stateSaveAssigns mirrors the metadata-field write-back the old renderStateHandleSave
// emitted: block/tx/log columns are stamped from EventMeta on every Save.
func stateSaveAssigns(table customTableSpec) []stateSaveAssignTmpl {
	var out []stateSaveAssignTmpl
	for _, field := range table.Fields {
		switch field.Name {
		case "BlockNumber", "UpdatedAtBlock":
			out = append(out, stateSaveAssignTmpl{Field: field.Name, Expr: "meta.BlockNumber"})
		case "BlockTimestamp", "UpdatedAt":
			if field.Type == "time.Time" {
				// A2: in-memory timestamp is int64 unix-millis to keep the ring
				// pointer-free; the DateTime64 column is restored at Append time.
				out = append(out, stateSaveAssignTmpl{Field: field.Name, Expr: "meta.BlockTimestamp.UnixMilli()"})
			} else {
				out = append(out, stateSaveAssignTmpl{Field: field.Name, Expr: "meta.BlockTimestamp"})
			}
		case "TxIndex", "TransactionIndex":
			out = append(out, stateSaveAssignTmpl{Field: field.Name, Expr: "meta.TransactionIndex"})
		case "LogIndex":
			out = append(out, stateSaveAssignTmpl{Field: field.Name, Expr: "meta.LogIndex"})
		}
	}
	return out
}

func buildStateHandles(specs []hotStateSpec, cfg *config.Config, events []eventSpec) []stateHandleSpec {
	used := make(map[string]struct{})
	var handles []stateHandleSpec
	add := func(name string, spec hotStateSpec) {
		name = exportIdent(name)
		if name == "" {
			return
		}
		if _, ok := used[name]; ok {
			return
		}
		used[name] = struct{}{}
		handles = append(handles, stateHandleSpec{
			name:      name,
			stateType: name + "State",
			valueName: name,
			valueType: spec.table.GoTypeName,
			spec:      spec,
		})
	}

	for _, spec := range specs {
		if spec.table.IsEvent {
			continue
		}
		name := strings.TrimPrefix(spec.table.GoTypeName, "Memory")
		add(name, spec)
	}

	if cfg != nil {
		for _, state := range cfg.State {
			if table, ok := findStateCustomTableSpec(hotStateTables(specs), state); ok {
				if spec, ok := findHotStateSpec(specs, table); ok {
					add(state.Name, spec)
				}
				continue
			}
			target, err := findStateEventSpec(events, state)
			if err != nil {
				continue
			}
			table := stateEventToCustomTableSpec(target, state)
			if spec, ok := findHotStateSpec(specs, table); ok {
				add(state.Name, spec)
			}
		}
	}
	return handles
}

func hotStateTables(specs []hotStateSpec) []customTableSpec {
	tables := make([]customTableSpec, 0, len(specs))
	for _, spec := range specs {
		tables = append(tables, spec.table)
	}
	return tables
}

func findHotStateSpec(specs []hotStateSpec, table customTableSpec) (hotStateSpec, bool) {
	for _, spec := range specs {
		if spec.table.GoTypeName == table.GoTypeName && spec.table.Name == table.Name {
			return spec, true
		}
	}
	return hotStateSpec{}, false
}

func stateImports(handles []stateHandleSpec) []string {
	imports := map[string]struct{}{
		`"context"`:                     {},
		`"fmt"`:                         {},
		`"os"`:                          {},
		`"strconv"`:                     {},
		`"sync"`:                        {},
		`"github.com/ClickHouse/ch-go"`: {},
	}
	for _, handle := range handles {
		for _, field := range handle.spec.table.keyFields() {
			stateAddImport(imports, field.Type)
		}
	}
	keys := make([]string, 0, len(imports))
	for imp := range imports {
		keys = append(keys, imp)
	}
	sort.Strings(keys)
	return keys
}

func stateAddImport(imports map[string]struct{}, typ string) {
	switch {
	case strings.Contains(typ, "common."):
		imports[`"github.com/ethereum/go-ethereum/common"`] = struct{}{}
	case strings.Contains(typ, "uint256."):
		imports[`"github.com/holiman/uint256"`] = struct{}{}
	case strings.Contains(typ, "decimal."):
		imports[`"github.com/shopspring/decimal"`] = struct{}{}
	case strings.Contains(typ, "time."):
		imports[`"time"`] = struct{}{}
	}
}
