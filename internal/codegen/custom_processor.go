package codegen

import (
	"fmt"
	"go/format"
	"strconv"
	"strings"

	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/template"
)

func generateEmptyCustomProcessorGo(cfg *config.Config, events []eventSpec, hotStateTables []customTableSpec) ([]byte, error) {
	plans := buildStatePrefetchPlans(cfg, events, hotStateSpecs(hotStateTables))
	tmplData := struct {
		Tables                  []customTableSpec
		PrefetchBlocksBody      string
		PrefetchProtoBlocksBody string
	}{
		Tables:                  hotStateTables,
		PrefetchBlocksBody:      renderPrefetchBlocksBody(plans),
		PrefetchProtoBlocksBody: renderPrefetchProtoBlocksBody(plans),
	}

	src := template.MustExecute("code/customProcessorGo", tmplData)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return []byte(src), fmt.Errorf("format custom processor source: %w", err)
	}
	return formatted, nil
}

type statePrefetchPlan struct {
	stateName string
	baseName  string
	keyType   string
	keyFields []customFieldSpec
	sources   []statePrefetchSource
}

type statePrefetchSource struct {
	slotField string
	eventType string
	fields    []statePrefetchField
}

type statePrefetchField struct {
	keyField   string
	eventField string
}

func renderPrefetchBlocksBody(plans []statePrefetchPlan) string {
	var b strings.Builder
	if len(plans) == 0 {
		b.WriteString(`func prefetchBlocksState(ctx context.Context, store Store, state *State, blocks []*ParsedBlock) error {
	return nil
}
`)
		return b.String()
	}

	b.WriteString(`func prefetchBlocksState(ctx context.Context, store Store, state *State, blocks []*ParsedBlock) error {
	if state == nil || state.HotState == nil || len(blocks) == 0 {
		return nil
	}
	// Safe nil check: store may be a non-nil interface wrapping a nil *sqd.Store.
	// store.Conn() panics on nil receiver, so check the concrete value first.
	if store == nil {
		return nil
	}
	if store.Conn() == nil {
		return nil
	}
	hot := state.HotState

	for _, block := range blocks {
		if block == nil {
			continue
		}
`)
	for _, plan := range plans {
		for _, source := range plan.sources {
			b.WriteString("\t\tfor _, ev := range block.")
			b.WriteString(source.slotField)
			b.WriteString(" {\n")
			b.WriteString("\t\t\thot.")
			b.WriteString(plan.baseName)
			b.WriteString("Resolver.Queue(")
			b.WriteString(plan.keyType)
			b.WriteString("{")
			for i, field := range source.fields {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(field.keyField)
				b.WriteString(": ev.")
				b.WriteString(field.eventField)
			}
			b.WriteString("})\n\t\t}\n")
		}
	}
	b.WriteString("\t}\n\n")

	for _, plan := range plans {
		b.WriteString("\tif err := hot.")
		b.WriteString(plan.baseName)
		b.WriteString("Resolver.Resolve(ctx, store.Conn(), store.DB()); err != nil {\n")
		b.WriteString("\t\treturn fmt.Errorf(")
		b.WriteString(strconv.Quote("prefetch " + plan.stateName + ": %w"))
		b.WriteString(", err)\n\t}\n")
	}
	b.WriteString("\treturn nil\n}\n")
	return b.String()
}

func renderPrefetchProtoBlocksBody(plans []statePrefetchPlan) string {
	var b strings.Builder
	if len(plans) == 0 {
		b.WriteString(`func prefetchProtoBlocksState(ctx context.Context, store Store, state *State, blocks []*ProtoEventBlock) error {
	return nil
}
`)
		return b.String()
	}

	b.WriteString(`func prefetchProtoBlocksState(ctx context.Context, store Store, state *State, blocks []*ProtoEventBlock) error {
	if state == nil || state.HotState == nil || len(blocks) == 0 {
		return nil
	}
	if store == nil {
		return nil
	}
	if store.Conn() == nil {
		return nil
	}
	hot := state.HotState

	for _, block := range blocks {
		if block == nil {
			continue
		}
`)
	for _, plan := range plans {
		for _, source := range plan.sources {
			b.WriteString("\t\tblock.Query")
			b.WriteString(source.eventType)
			b.WriteString("().Map(func(ev ")
			b.WriteString(source.eventType)
			b.WriteString("ProtoView) {\n")
			b.WriteString("\t\t\thot.")
			b.WriteString(plan.baseName)
			b.WriteString("Resolver.Queue(")
			b.WriteString(plan.keyType)
			b.WriteString("{")
			for i, field := range source.fields {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(field.keyField)
				b.WriteString(": ev.")
				b.WriteString(field.eventField)
				b.WriteString("()")
			}
			b.WriteString("})\n\t\t})\n")
		}
	}
	b.WriteString("\t}\n\n")

	for _, plan := range plans {
		b.WriteString("\tif err := hot.")
		b.WriteString(plan.baseName)
		b.WriteString("Resolver.Resolve(ctx, store.Conn(), store.DB()); err != nil {\n")
		b.WriteString("\t\treturn fmt.Errorf(")
		b.WriteString(strconv.Quote("prefetch " + plan.stateName + ": %w"))
		b.WriteString(", err)\n\t}\n")
	}
	b.WriteString("\treturn nil\n}\n")
	return b.String()
}

func buildStatePrefetchPlans(cfg *config.Config, events []eventSpec, specs []hotStateSpec) []statePrefetchPlan {
	if cfg == nil || len(cfg.State) == 0 {
		return nil
	}
	var plans []statePrefetchPlan
	for _, state := range cfg.State {
		spec, ok := statePrefetchSpec(state, events, specs)
		if !ok {
			continue
		}
		keyFields := spec.table.keyFields()
		if len(keyFields) == 0 {
			continue
		}
		plan := statePrefetchPlan{
			stateName: state.Name,
			baseName:  spec.baseName,
			keyType:   spec.keyType,
			keyFields: keyFields,
		}
		for _, source := range events {
			fields, ok := statePrefetchFields(source, keyFields)
			if !ok {
				continue
			}
			plan.sources = append(plan.sources, statePrefetchSource{
				slotField: source.GoTypeName + "s",
				eventType: source.GoTypeName,
				fields:    fields,
			})
		}
		if len(plan.sources) > 0 {
			plans = append(plans, plan)
		}
	}
	return plans
}

func statePrefetchSpec(state config.StateConfig, events []eventSpec, specs []hotStateSpec) (hotStateSpec, bool) {
	if table, ok := findStateCustomTableSpec(hotStateTables(specs), state); ok {
		return findHotStateSpec(specs, table)
	}
	target, err := findStateEventSpec(events, state)
	if err != nil {
		return hotStateSpec{}, false
	}
	table := stateEventToCustomTableSpec(target, state)
	return findHotStateSpec(specs, table)
}

func statePrefetchFields(source eventSpec, keyFields []customFieldSpec) ([]statePrefetchField, bool) {
	fields := make([]statePrefetchField, 0, len(keyFields))
	for _, keyField := range keyFields {
		arg, ok := matchingStatePrefetchArg(source, keyField)
		if !ok {
			return nil, false
		}
		fields = append(fields, statePrefetchField{keyField: keyField.Name, eventField: arg.GoFieldName})
	}
	return fields, true
}

func matchingStatePrefetchArg(source eventSpec, keyField customFieldSpec) (eventArg, bool) {
	for _, arg := range source.Args {
		if arg.ColumnName == keyField.ColumnName || arg.Name == keyField.ColumnName || arg.GoFieldName == keyField.Name {
			return arg, true
		}
	}
	return eventArg{}, false
}
