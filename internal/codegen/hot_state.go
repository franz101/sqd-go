package codegen

import (
	"bytes"
	"fmt"
	"go/format"
	"sort"
	"strconv"
	"strings"

	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/template"
)

type hotStateSpec struct {
	table     customTableSpec
	baseName  string
	keyType   string
	entryType string
	cacheType string
	batchType string
}

func generateHotStateGo(tables []customTableSpec, events []eventSpec) ([]byte, error) {
	var b bytes.Buffer
	specs := hotStateSpecs(tables)

	tmplData := struct {
		Tables   []customTableSpec
		Events   []eventSpec
		Specs    []hotStateSpec
		HasSpecs bool
	}{
		Tables:   tables,
		Events:   events,
		Specs:    specs,
		HasSpecs: len(specs) > 0,
	}

	b.WriteString(template.MustExecute("code/hotStatePreludeGo", tmplData))
	renderHotStateImports(&b, tables, events)
	b.WriteString(template.MustExecute("code/hotStateRuntimeGo", tmplData))
	for _, spec := range specs {
		if !spec.table.IsEvent {
			renderHotStateEntity(&b, spec.table)
		}
		renderClockKey(&b, spec)
		renderClockCache(&b, spec)
		if !spec.table.IsEvent {
			renderCustomBatch(&b, spec)
			renderClockRecover(&b, spec)
		}
		renderBatchResolver(&b, spec)
	}
	renderHotStateType(&b, specs)
	renderHotStateHelpers(&b, tables, events)

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format source: %w", err)
	}
	return formatted, nil
}

func renderHotStateImports(b *bytes.Buffer, tables []customTableSpec, events []eventSpec) {
	imports := map[string]string{
		`"context"`:                              "",
		`"encoding/binary"`:                      "",
		`"fmt"`:                                  "",
		`"strings"`:                              "",
		`"sync"`:                                 "",
		`"sync/atomic"`:                          "",
		`"github.com/ClickHouse/ch-go"`:          "",
		`"github.com/ClickHouse/ch-go/proto"`:    "",
		`"github.com/franz101/sqd-go/coldcache"`: "",
	}
	if customTablesUseDecimal(tables) {
		imports[`"encoding/binary"`] = ""
		imports[`"math/big"`] = ""
	}
	if len(tables) > 0 {
		imports[`"strconv"`] = ""
		imports[`"time"`] = ""
	}
	if len(hotStateSpecs(tables)) > 0 {
		// The cold-recovery runtime reads ClickHouse settings and the recovery
		// floor from the environment (generated code must not import internal
		// packages), so it needs os + strconv.
		imports[`"os"`] = ""
		imports[`"strconv"`] = ""
	}
	if customTablesUseColdCache(tables) {
		// Cold tier (Pebble): pointer-free values are stored as raw bytes via
		// zero-transformation unsafe memcpy; pointer-bearing (but serializable)
		// values go through a generated marshal/unmarshal codec. filepath builds
		// the per-cache dir; unsafe keys the cold lookups (keys are always flat).
		imports[`"path/filepath"`] = ""
		imports[`"unsafe"`] = ""
	}
	for _, table := range tables {
		for _, field := range table.Fields {
			switch {
			case strings.Contains(field.Type, "time."):
				imports[`"time"`] = ""
			case strings.Contains(field.Type, "common."):
				imports[`"github.com/ethereum/go-ethereum/common"`] = ""
			case strings.Contains(field.Type, "uint256."):
				imports[`"github.com/holiman/uint256"`] = ""
			case strings.Contains(field.Type, "decimal."):
				imports[`"github.com/shopspring/decimal"`] = ""
			case strings.Contains(field.Type, "protomath."):
				imports[`"github.com/franz101/sqd-go/protomath"`] = ""
			}
		}
	}
	// Event views (ProtoEventBlock) reference the uint256 hot-state helpers for any
	// event with a uint256/uint256[] arg, even when no custom/hot-state table uses
	// uint256 — so the import must follow event usage too.
	if eventsUseUint256(events) || eventsUseUint256Slice(events) {
		imports[`"github.com/holiman/uint256"`] = ""
	}
	b.WriteString("import (\n")
	for _, imp := range sortedImportKeys(imports) {
		b.WriteString("\t")
		b.WriteString(imp)
		b.WriteString("\n")
	}
	b.WriteString(")\n\n")
}

type hotEntityFieldTmpl struct {
	Name    string
	MemType string
	ChTag   string
}

func renderHotStateEntity(b *bytes.Buffer, table customTableSpec) {
	data := struct {
		GoTypeName string
		Fields     []hotEntityFieldTmpl
	}{GoTypeName: table.GoTypeName}
	for _, field := range table.Fields {
		data.Fields = append(data.Fields, hotEntityFieldTmpl{
			Name:    field.Name,
			MemType: memoryGoType(field, table.IsEvent),
			ChTag:   strconv.Quote("name=" + field.ColumnName + ";type=" + field.ColumnType),
		})
	}
	b.WriteString(template.MustExecute("code/hotStateEntity", data))
}

type hotKeyFieldTmpl struct {
	Name string
	Type string
}

func renderClockKey(b *bytes.Buffer, spec hotStateSpec) {
	data := struct {
		KeyType    string
		GoTypeName string
		KeyFields  []hotKeyFieldTmpl
	}{KeyType: spec.keyType, GoTypeName: spec.table.GoTypeName}
	for _, field := range spec.table.keyFields() {
		data.KeyFields = append(data.KeyFields, hotKeyFieldTmpl{Name: field.Name, Type: field.Type})
	}
	b.WriteString(template.MustExecute("code/hotStateClockKey", data))
}

func renderClockCache(b *bytes.Buffer, spec hotStateSpec) {
	valueType := spec.table.GoTypeName
	keyFields := spec.table.keyFields()

	b.WriteString("type ")
	b.WriteString(spec.entryType)
	b.WriteString(" struct {\n\tkey ")
	b.WriteString(spec.keyType)
	b.WriteString("\n\tvalue ")
	b.WriteString(valueType)
	b.WriteString("\n\treferenced uint32\n\tinUse uint32\n}\n\n")

	// Cache struct. The index is a pointer-free chained hash over an arena (A3):
	// buckets[h] = ring index of a chain head (-1 = empty); next[ringIdx] = next
	// ring index in the same bucket (-1 = end). Replaces a sync.Map[key]idx whose
	// boxed key+idx interfaces were ~3 GC-scanned objects per live entry. The
	// runtime uses one processor owner after startup. Atomics on slot flags do not
	// make the full cache concurrent-safe because the index and values are plain;
	// they remain because the measured atomic-free candidate was not a consistent
	// full-workload improvement.
	fmt.Fprintf(b, `type %[1]s struct {
	// buckets, next, ring form a fixed-size clock replacement cache.
	// SINGLE-OWNER CONTRACT: This cache is NOT thread-safe. All operations
	// must occur from a single goroutine (the processor). Concurrent access
	// will corrupt the linked-list structures and cause data races.
	// The atomics used for entry flags (inUse, referenced) protect only
	// the per-slot state, NOT the cache index structures.
	buckets  []int32
	next     []int32
	ring     []%[2]s
	mask     uint32
	capacity uint64
	hand     uint64
	size     uint64
	// cold is an optional Pebble-backed tier holding evicted entries (raw bytes for
	// pointer-free entities, or a marshaled payload for serializable ones). nil
	// unless attached via HotState.EnableColdCache.
	cold    *coldcache.Store
	metrics *BatchResolverMetrics
}

func New%[1]s(capacity uint64) *%[1]s {
	if capacity == 0 {
		capacity = DefaultClockCacheCapacity
	}
	bucketCount := uint64(8)
	for bucketCount < capacity*2 {
		bucketCount <<= 1
	}
	c := &%[1]s{
		ring:     make([]%[2]s, capacity),
		buckets:  make([]int32, bucketCount),
		next:     make([]int32, capacity),
		mask:     uint32(bucketCount - 1),
		capacity: capacity,
	}
	for i := range c.buckets {
		c.buckets[i] = -1
	}
	for i := range c.next {
		c.next[i] = -1
	}
	return c
}

`, spec.cacheType, spec.entryType)

	// keyHash folds fixed-width keys by machine words. Hash and address keys are
	// already uniformly distributed, so byte-at-a-time FNV adds work without
	// improving the cache index distribution.
	fmt.Fprintf(b, "func (c *%s) keyHash(key %s) uint32 {\n\th := uint64(1469598103934665603)\n", spec.cacheType, spec.keyType)
	for _, field := range keyFields {
		switch field.Type {
		case "common.Address":
			fmt.Fprintf(b, "\th = clockHash20(h, key.%s[:])\n", field.Name)
		case "common.Hash":
			fmt.Fprintf(b, "\th = clockHash32(h, key.%s[:])\n", field.Name)
		default:
			fmt.Fprintf(b, "\th = clockHashBytes(h, key.%s[:])\n", field.Name)
		}
	}
	b.WriteString("\treturn uint32(h^(h>>32)) & c.mask\n}\n\n")

	fmt.Fprintf(b, `func (c *%[1]s) idxLookup(key %[2]s) (uint64, bool) {
	for i := c.buckets[c.keyHash(key)]; i >= 0; i = c.next[i] {
		if c.ring[i].key == key {
			return uint64(i), true
		}
	}
	return 0, false
}

func (c *%[1]s) idxInsert(key %[2]s, ringIdx uint32) {
	h := c.keyHash(key)
	c.next[ringIdx] = c.buckets[h]
	c.buckets[h] = int32(ringIdx)
}

func (c *%[1]s) idxUnlink(key %[2]s) {
	h := c.keyHash(key)
	prev := int32(-1)
	for i := c.buckets[h]; i >= 0; i = c.next[i] {
		if c.ring[i].key == key {
			if prev < 0 {
				c.buckets[h] = c.next[i]
			} else {
				c.next[prev] = c.next[i]
			}
			c.next[i] = -1
			return
		}
		prev = i
	}
}

`, spec.cacheType, spec.keyType)

	coldOn := entityUsesCold(spec.table)
	coldFree := coldOn && isPointerFreeEntity(spec.table) // raw-bytes unsafe memcpy
	coldSer := coldOn && !coldFree                        // binary marshal/unmarshal codec
	// spill: on CLOCK eviction, write the victim (value or tombstone) to the cold
	// tier BEFORE overwriting it, so a later re-reference is served from disk and a
	// dirty-but-evicted entry isn't lost before Commit reads it.
	spill := ""
	if coldFree {
		spill = "\t\t\tif c.cold != nil {\n" +
			"\t\t\t\tc.cold.Put(unsafe.Slice((*byte)(unsafe.Pointer(&e.key)), unsafe.Sizeof(e.key)), unsafe.Slice((*byte)(unsafe.Pointer(&e.value)), unsafe.Sizeof(e.value)))\n" +
			"\t\t\t}\n"
	} else if coldSer {
		spill = "\t\t\tif c.cold != nil {\n" +
			"\t\t\t\tc.cold.Put(unsafe.Slice((*byte)(unsafe.Pointer(&e.key)), unsafe.Sizeof(e.key)), marshalCold" + valueType + "(e.value))\n" +
			"\t\t\t}\n"
	}
	fmt.Fprintf(b, `func (c *%[1]s) Set(value %[3]s) {
	c.SetByKey(New%[2]s(value), value)
}

func (c *%[1]s) SetByKey(key %[2]s, value %[3]s) {
	if idx, ok := c.idxLookup(key); ok {
		e := &c.ring[idx]
		if atomic.LoadUint32(&e.inUse) == 1 && e.key == key {
			e.value = value
			atomic.StoreUint32(&e.referenced, 1)
			return
		}
	}
	for {
		hand := atomic.AddUint64(&c.hand, 1)
		idx := (hand - 1) %% c.capacity
		e := &c.ring[idx]
		if atomic.CompareAndSwapUint32(&e.inUse, 1, 0) {
			if atomic.LoadUint32(&e.referenced) == 1 {
				atomic.StoreUint32(&e.referenced, 0)
				atomic.StoreUint32(&e.inUse, 1)
				continue
			}
%[4]s			c.idxUnlink(e.key)
			e.key = key
			e.value = value
			atomic.StoreUint32(&e.referenced, 0)
			c.idxInsert(key, uint32(idx))
			atomic.StoreUint32(&e.inUse, 1)
			return
		}
		if atomic.LoadUint32(&e.inUse) == 0 {
			if atomic.CompareAndSwapUint32(&e.inUse, 0, 2) {
				e.key = key
				e.value = value
				atomic.StoreUint32(&e.referenced, 0)
				c.idxInsert(key, uint32(idx))
				atomic.AddUint64(&c.size, 1)
				atomic.StoreUint32(&e.inUse, 1)
				return
			}
		}
	}
}

`, spec.cacheType, spec.keyType, valueType, spill)

	if coldFree {
		// Cold-consult on hot miss: an evicted entry (spilled on eviction) is served
		// from Pebble (~8µs) instead of a ClickHouse round-trip (~1.9ms), then
		// promoted back into the hot ring. A cold tombstone round-trips as a value
		// with Tombstone=true, which the state-level Get treats as "absent".
		fmt.Fprintf(b, `func (c *%[1]s) Get(key %[2]s) (%[3]s, bool) {
	if idx, ok := c.idxLookup(key); ok {
		e := &c.ring[idx]
		if atomic.LoadUint32(&e.inUse) == 1 && e.key == key {
			atomic.StoreUint32(&e.referenced, 1)
			if c.metrics != nil {
				c.metrics.HotHits++
			}
			return e.value, true
		}
	}
	if c.cold != nil {
		// GetInto copies the stored bytes straight into v (a fixed-size value
		// whose layout matches the bytes written on eviction), avoiding the
		// per-hit []byte allocation that the value-returning cold.Get does. key is
		// copied to a local first: taking &key directly forces the key parameter
		// to the heap on EVERY Get (escape analysis is flow-insensitive), including
		// hot hits that never reach this cold branch. &k keeps the escape contained.
		k := key
		var v %[3]s
		if found, _ := c.cold.GetInto(unsafe.Slice((*byte)(unsafe.Pointer(&v)), unsafe.Sizeof(v)), unsafe.Slice((*byte)(unsafe.Pointer(&k)), unsafe.Sizeof(k))); found {
			c.SetByKey(key, v)
			if c.metrics != nil {
				c.metrics.ColdHits++
			}
			return v, true
		}
	}
	return %[3]s{}, false
}

`, spec.cacheType, spec.keyType, valueType)
	} else if coldSer {
		// Serialized cold-consult: pointer-bearing values can't use the zero-copy
		// GetInto memcpy, so a hot miss fetches the marshaled bytes from Pebble and
		// decodes them (one alloc per cold hit — still far cheaper than a ClickHouse
		// round-trip), then promotes the entry back into the hot ring.
		fmt.Fprintf(b, `func (c *%[1]s) Get(key %[2]s) (%[3]s, bool) {
	if idx, ok := c.idxLookup(key); ok {
		e := &c.ring[idx]
		if atomic.LoadUint32(&e.inUse) == 1 && e.key == key {
			atomic.StoreUint32(&e.referenced, 1)
			if c.metrics != nil {
				c.metrics.HotHits++
			}
			return e.value, true
		}
	}
	if c.cold != nil {
		k := key
		if data, found, _ := c.cold.Get(unsafe.Slice((*byte)(unsafe.Pointer(&k)), unsafe.Sizeof(k))); found {
			if v, ok := unmarshalCold%[3]s(data); ok {
				c.SetByKey(key, v)
				if c.metrics != nil {
					c.metrics.ColdHits++
				}
				return v, true
			}
		}
	}
	return %[3]s{}, false
}

`, spec.cacheType, spec.keyType, valueType)
	} else {
		fmt.Fprintf(b, `func (c *%[1]s) Get(key %[2]s) (%[3]s, bool) {
	idx, ok := c.idxLookup(key)
	if !ok {
		return %[3]s{}, false
	}
	e := &c.ring[idx]
	if atomic.LoadUint32(&e.inUse) == 1 && e.key == key {
		atomic.StoreUint32(&e.referenced, 1)
		if c.metrics != nil {
			c.metrics.HotHits++
		}
		return e.value, true
	}
	return %[3]s{}, false
}

`, spec.cacheType, spec.keyType, valueType)
	}

	if len(keyFields) > 0 {
		b.WriteString("func (c *")
		b.WriteString(spec.cacheType)
		b.WriteString(") GetByFields(")
		renderClockKeyParams(b, keyFields)
		b.WriteString(") (")
		b.WriteString(valueType)
		b.WriteString(", bool) {\n\treturn c.Get(")
		b.WriteString(spec.keyType)
		b.WriteString("{")
		for i, field := range keyFields {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(field.Name)
			b.WriteString(": ")
			b.WriteString(lowerFirst(field.Name))
		}
		b.WriteString("})\n}\n\n")

		if coldOn {
			// ColdMightContain reports whether the key may have ever been written to
			// the cold tier (negative-filter probe; no false negatives). A hot+cold
			// miss with ColdMightContain==false is provably new, so the authoritative
			// read path may skip ClickHouse. A miss with ==true may be an evicted
			// entry (the bounded flat backend evicts), so ClickHouse must be checked.
			b.WriteString("func (c *")
			b.WriteString(spec.cacheType)
			b.WriteString(") ColdMightContain(")
			renderClockKeyParams(b, keyFields)
			b.WriteString(") bool {\n\tif c == nil || c.cold == nil {\n\t\treturn false\n\t}\n\tk := ")
			b.WriteString(spec.keyType)
			b.WriteString("{")
			for i, field := range keyFields {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(field.Name)
				b.WriteString(": ")
				b.WriteString(lowerFirst(field.Name))
			}
			b.WriteString("}\n\treturn c.cold.MightContain(unsafe.Slice((*byte)(unsafe.Pointer(&k)), unsafe.Sizeof(k)))\n}\n\n")
		}
	}

	// coldDel mirrors a hard delete into the cold tier so a rolled-back key cannot
	// resurrect from disk on a later miss.
	coldDel := ""
	if coldOn {
		coldDel = "\t\t\tif c.cold != nil {\n" +
			"\t\t\t\tc.cold.Delete(unsafe.Slice((*byte)(unsafe.Pointer(&key)), unsafe.Sizeof(key)))\n" +
			"\t\t\t}\n"
	}
	fmt.Fprintf(b, `func (c *%[1]s) Delete(key %[2]s) bool {
	idx, ok := c.idxLookup(key)
	if !ok {
		return false
	}
	e := &c.ring[idx]
	if atomic.CompareAndSwapUint32(&e.inUse, 1, 0) {
		if e.key == key {
			c.idxUnlink(key)
%[4]s			e.key = %[2]s{}
			e.value = %[3]s{}
			atomic.StoreUint32(&e.referenced, 0)
			atomic.AddUint64(&c.size, ^uint64(0))
			return true
		}
		atomic.StoreUint32(&e.inUse, 1)
	}
	return false
}

func (c *%[1]s) Range(fn func(%[2]s, %[3]s) bool) {
	if fn == nil {
		return
	}
	limit := atomic.LoadUint64(&c.hand)
	if limit > c.capacity {
		limit = c.capacity
	}
	for i := uint64(0); i < limit; i++ {
		e := &c.ring[i]
		if atomic.LoadUint32(&e.inUse) == 1 {
			if !fn(e.key, e.value) {
				return
			}
		}
	}
}

func (c *%[1]s) AppendValues(dst []%[3]s) []%[3]s {
	if c == nil {
		return dst
	}
	if atomic.LoadUint64(&c.size) == 0 {
		return dst
	}
	limit := atomic.LoadUint64(&c.hand)
	if limit > c.capacity {
		limit = c.capacity
	}
	for i := 0; i < int(limit); i++ {
		e := &c.ring[i]
		if atomic.LoadUint32(&e.inUse) == 1 {
			dst = append(dst, e.value)
		}
	}
	return dst
}

func (c *%[1]s) Len() uint64 {
	return atomic.LoadUint64(&c.size)
}

`, spec.cacheType, spec.keyType, valueType, coldDel)
	if coldSer {
		renderColdCodec(b, spec)
	}
}

// batchFieldTmpl is the per-column plumbing the batch template needs. Each value
// is precomputed by the existing batch* helpers so the template only interpolates.
type batchFieldTmpl struct {
	VarName          string   // proto column field, e.g. "colBalance"
	ColType          string   // its proto.Col* type
	ScratchName      string   // optional scratch field ("" if none)
	ScratchType      string   // scratch field type (only meaningful with ScratchName)
	InitLines        []string // constructor setup statements
	ColumnNameQuoted string   // strconv.Quote'd ClickHouse column name
	InputData        string   // expression bound into proto.Input{Data: ...}
	AppendLines      []string // statements appending one item's column values
}

// renderCustomBatch renders the columnar insert batch type + methods for one
// entity. The INSERT body string folds in the column list and is pre-quoted; the
// asyncInsertSettings() call selects wait/no-wait at runtime (see that helper).
func renderCustomBatch(b *bytes.Buffer, spec hotStateSpec) {
	data := struct {
		BatchType        string
		GoTypeName       string
		TableNameQuoted  string
		Fields           []batchFieldTmpl
		RowsExpr         string
		InsertBodyQuoted string
	}{
		BatchType:        spec.batchType,
		GoTypeName:       spec.table.GoTypeName,
		TableNameQuoted:  strconv.Quote(spec.table.Name),
		RowsExpr:         batchColumnRowsExpr(spec.table.Fields[0]),
		InsertBodyQuoted: strconv.Quote("INSERT INTO %s.%s " + customInsertColumnList(spec.table) + " VALUES"),
	}
	for _, field := range spec.table.Fields {
		data.Fields = append(data.Fields, batchFieldTmpl{
			VarName:          batchColumnField(field),
			ColType:          batchColumnType(field),
			ScratchName:      batchScratchField(field),
			ScratchType:      batchScratchType(field),
			InitLines:        batchInitLines(field),
			ColumnNameQuoted: strconv.Quote(field.ColumnName),
			InputData:        batchInputData(field),
			AppendLines:      batchAppendLines(field, spec.table.IsEvent),
		})
	}
	b.WriteString(template.MustExecute("code/hotStateBatch", data))
}

// recoverFieldTmpl is the per-column decode plumbing the recover template needs.
// Every value is precomputed here by the existing helper functions so the
// template only has to interpolate strings.
type recoverFieldTmpl struct {
	VarName          string   // local proto column var, e.g. "colAccount"
	ColType          string   // its decoder type, e.g. "proto.ColFixedStr"
	InitLines        []string // setup statements run before the query (e.g. SetSize)
	ColumnNameQuoted string   // strconv.Quote'd ClickHouse column name
	ResultData       string   // expression placed in proto.Results{Data: ...}
	Name             string   // Go struct field name
	ValueExpr        string   // expression that reads row i back into the field
	IsMeta           bool     // true for EventMeta-embedded columns (resolver event split only)
}

// recoverKeyFieldTmpl names one key column, used by the resolver template to
// write key fields back into a not-found tombstone (tombstone.X = key.X).
type recoverKeyFieldTmpl struct {
	Name string
}

// isEventMetaField reports whether a column belongs to the embedded EventMeta
// struct (so the resolver builds it under EventMeta{...} for event entities).
func isEventMetaField(name string) bool {
	switch name {
	case "BlockNumber", "BlockTimestamp", "TransactionIndex", "LogIndex":
		return true
	}
	return false
}

// resolverResultFields builds the shared per-column result plumbing used by both
// the recover and resolver templates.
func resolverResultFields(spec hotStateSpec) []recoverFieldTmpl {
	var fields []recoverFieldTmpl
	for _, field := range spec.table.Fields {
		fields = append(fields, recoverFieldTmpl{
			VarName:          batchColumnField(field),
			ColType:          resultColumnType(field),
			InitLines:        resultInitLines(field),
			ColumnNameQuoted: strconv.Quote(field.ColumnName),
			ResultData:       resultData(field),
			Name:             field.Name,
			ValueExpr:        resultValueExpr(field, spec.table.IsEvent),
			IsMeta:           isEventMetaField(field.Name),
		})
	}
	return fields
}

// renderClockRecover renders <Cache>.Recover. ColdKind selects the cold-tier
// write-back callback ("pointerfree" = raw unsafe bytes, "serializable" = codec),
// or "" for the hot-only path. The cold-tier filter-keys pass is pre-rendered
// here (it writes into its own buffer) and handed to the template verbatim.
func renderClockRecover(b *bytes.Buffer, spec hotStateSpec) {
	data := struct {
		CacheType       string
		GoTypeName      string
		KeyType         string
		Fields          []recoverFieldTmpl
		QueryQuoted     string
		TableNameQuoted string
		WhereExpr       string
		ColdKind        string
		FilterKeysPass  string
	}{
		CacheType:       spec.cacheType,
		GoTypeName:      spec.table.GoTypeName,
		KeyType:         spec.keyType,
		QueryQuoted:     strconv.Quote(recoverQuery(spec.table)),
		TableNameQuoted: strconv.Quote(spec.table.Name),
		WhereExpr:       recoverWhereExpr(spec.table),
	}
	for _, field := range spec.table.Fields {
		data.Fields = append(data.Fields, recoverFieldTmpl{
			VarName:          batchColumnField(field),
			ColType:          resultColumnType(field),
			InitLines:        resultInitLines(field),
			ColumnNameQuoted: strconv.Quote(field.ColumnName),
			ResultData:       resultData(field),
			Name:             field.Name,
			ValueExpr:        resultValueExpr(field, spec.table.IsEvent),
		})
	}

	// Classify the cold tier and pre-render the filter-keys pass (a no-op for
	// entities without an updated_at_block column).
	switch {
	case isPointerFreeEntity(spec.table):
		data.ColdKind = "pointerfree"
	case isColdSerializableEntity(spec.table):
		data.ColdKind = "serializable"
	}
	if data.ColdKind != "" {
		var fk bytes.Buffer
		renderClockFilterKeysPass(&fk, spec)
		data.FilterKeysPass = fk.String()
	}

	b.WriteString(template.MustExecute("code/hotStateRecover", data))
}

type hotTypeSpecTmpl struct {
	BaseName     string
	CacheType    string
	KeyType      string
	GoTypeName   string
	BatchType    string
	ResolverType string
	IsEvent      bool
}

func renderHotStateType(b *bytes.Buffer, specs []hotStateSpec) {
	view := func(spec hotStateSpec) hotTypeSpecTmpl {
		return hotTypeSpecTmpl{
			BaseName:     spec.baseName,
			CacheType:    spec.cacheType,
			KeyType:      spec.keyType,
			GoTypeName:   spec.table.GoTypeName,
			BatchType:    spec.batchType,
			ResolverType: spec.table.GoTypeName + "BatchResolver",
			IsEvent:      spec.table.IsEvent,
		}
	}
	data := struct {
		Specs     []hotTypeSpecTmpl
		ColdSpecs []hotTypeSpecTmpl
		HasCold   bool
	}{}
	for _, spec := range specs {
		data.Specs = append(data.Specs, view(spec))
		if entityUsesCold(spec.table) {
			data.ColdSpecs = append(data.ColdSpecs, view(spec))
		}
	}
	data.HasCold = len(data.ColdSpecs) > 0
	b.WriteString(template.MustExecute("code/hotStateType", data))
}

func renderHotStateHelpers(b *bytes.Buffer, tables []customTableSpec, events []eventSpec) {
	// Shared FNV-1a byte fold used by every clock cache's flat index (A3). The
	// index is a pointer-free open-chained hash over an arena (buckets []int32 head
	// + next []int32 chain), so the GC scans two flat integer slices instead of the
	// millions of boxed interface objects a sync.Map[key]idx allocated.
	b.WriteString(`
// BatchResolverMetrics are cumulative until SnapshotAndResetMetrics is called.
// Resolver access follows the processor's single-owner state contract.
type BatchResolverMetrics struct {
	HotHits      uint64
	ColdHits     uint64
	DBFallbacks  uint64
	QueuedMisses uint64
	UniqueMisses uint64
	RoundTrips   uint64
	ResolveNanos uint64
	Skipped      uint64
}

func clockHashWord(seed, word uint64) uint64 {
	return (seed ^ word) * 1099511628211
}

func clockHash20(seed uint64, b []byte) uint64 {
	h := clockHashWord(seed, binary.LittleEndian.Uint64(b[0:8]))
	h = clockHashWord(h, binary.LittleEndian.Uint64(b[8:16]))
	return clockHashWord(h, uint64(binary.LittleEndian.Uint32(b[16:20])))
}

func clockHash32(seed uint64, b []byte) uint64 {
	h := clockHashWord(seed, binary.LittleEndian.Uint64(b[0:8]))
	h = clockHashWord(h, binary.LittleEndian.Uint64(b[8:16]))
	h = clockHashWord(h, binary.LittleEndian.Uint64(b[16:24]))
	return clockHashWord(h, binary.LittleEndian.Uint64(b[24:32]))
}

func clockHashBytes(seed uint64, b []byte) uint64 {
	h := seed
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	return h
}
`)
	if customTablesUseBool(tables) {
		b.WriteString("\nfunc hotStateBool(v bool) uint8 {\n\tif v {\n\t\treturn 1\n\t}\n\treturn 0\n}\n")
		b.WriteString("\nfunc hotStateBoolSQL(v bool) string {\n\tif v {\n\t\treturn \"1\"\n\t}\n\treturn \"0\"\n}\n")
	}
	if customTablesUseUint256(tables) || customTablesUseUint256Slice(tables) ||
		eventsUseUint256(events) || eventsUseUint256Slice(events) {
		b.WriteString(`
func hotStateUInt256(v uint256.Int) proto.UInt256 {
	return proto.UInt256{
		Low:  proto.UInt128{Low: v[0], High: v[1]},
		High: proto.UInt128{Low: v[2], High: v[3]},
	}
}

func hotStateUint256(v proto.UInt256) uint256.Int {
	return uint256.Int{v.Low.Low, v.Low.High, v.High.Low, v.High.High}
}
`)
	}
	if customTablesUseDecimal(tables) {
		b.WriteString(`
func hotStateDecimalToDecimal256(d decimal.Decimal) protomath.Decimal256 {
	out, _ := protomath.FromDecimal256ScaledBigInt(d.Shift(18).BigInt())
	return out
}

func hotStateDecimalFromDecimal256(v protomath.Decimal256) decimal.Decimal {
	return decimal.NewFromBigInt(v.ScaledBig(), -18)
}
`)
	}
	if customTablesUseType(tables, "protomath.Decimal256") {
		b.WriteString(`
type decimal256Col struct {
	proto.ColDecimal256
}

func (c *decimal256Col) Type() proto.ColumnType {
	return proto.ColumnType("Decimal(76, 18)")
}
`)
	}
	if customTablesUseHashSlice(tables) {
		b.WriteString(`
func hotStateHashSliceBytes(values []common.Hash, scratch [][]byte) [][]byte {
	scratch = scratch[:0]
	for _, value := range values {
		scratch = append(scratch, value.Bytes())
	}
	return scratch
}

func hotStateHashSlice(values [][]byte) []common.Hash {
	out := make([]common.Hash, 0, len(values))
	for _, value := range values {
		out = append(out, common.BytesToHash(value))
	}
	return out
}
`)
	}
	if customTablesUseUint256Slice(tables) || eventsUseUint256Slice(events) {
		b.WriteString(`
func hotStateUInt256Slice(values []uint256.Int, scratch []proto.UInt256) []proto.UInt256 {
	scratch = scratch[:0]
	for _, value := range values {
		scratch = append(scratch, hotStateUInt256(value))
	}
	return scratch
}

func hotStateUint256Slice(values []proto.UInt256) []uint256.Int {
	out := make([]uint256.Int, 0, len(values))
	for _, value := range values {
		out = append(out, hotStateUint256(value))
	}
	return out
}
`)
	}
}

// renderClockKeyParams writes the key columns as a Go parameter list
// ("name type, name type, ...") for the generated cache accessor signatures.
func renderClockKeyParams(b *bytes.Buffer, fields []customFieldSpec) {
	params := make([]string, len(fields))
	for i, field := range fields {
		params[i] = lowerFirst(field.Name) + " " + field.Type
	}
	b.WriteString(strings.Join(params, ", "))
}

func hotStateSpecs(tables []customTableSpec) []hotStateSpec {
	used := make(map[string]int)
	specs := make([]hotStateSpec, 0, len(tables))
	for _, table := range tables {
		var base string
		if table.BaseName != "" {
			base = uniqueExported(used, table.BaseName)
		} else {
			base = uniqueExported(used, customClockBaseName(table.GoTypeName))
		}
		specs = append(specs, hotStateSpec{
			table:     table,
			baseName:  base,
			keyType:   base + "ClockKey",
			entryType: base + "ClockEntry",
			cacheType: base + "ClockCache",
			batchType: table.GoTypeName + "Batch",
		})
	}
	return specs
}

func customClockBaseName(typeName string) string {
	name := strings.TrimPrefix(customTableEntityName(typeName), "Memory")
	return pluralizeGoName(name)
}

func pluralizeGoName(name string) string {
	switch {
	case name == "":
		return "Entities"
	case strings.HasSuffix(name, "s"), strings.HasSuffix(name, "S"):
		return name
	case strings.HasSuffix(name, "y"):
		return strings.TrimSuffix(name, "y") + "ies"
	case strings.HasSuffix(name, "Y"):
		return strings.TrimSuffix(name, "Y") + "ies"
	default:
		return name + "s"
	}
}

func lowerFirst(name string) string {
	if name == "" {
		return "value"
	}
	if name == strings.ToUpper(name) {
		return strings.ToLower(name)
	}
	return strings.ToLower(name[:1]) + name[1:]
}

func sortedImportKeys(imports map[string]string) []string {
	keys := make([]string, 0, len(imports))
	for key := range imports {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func batchColumnField(field customFieldSpec) string {
	return "col" + field.Name
}

func batchScratchField(field customFieldSpec) string {
	switch field.Type {
	case "[]common.Hash", "[]uint256.Int":
		return "scratch" + field.Name
	default:
		return ""
	}
}

func batchScratchType(field customFieldSpec) string {
	switch field.Type {
	case "[]common.Hash":
		return "[][]byte"
	case "[]uint256.Int":
		return "[]proto.UInt256"
	default:
		return ""
	}
}

// memoryGoType returns the in-memory Go type for a hot-state value-struct field,
// which may differ from the schema/source type to keep the rings pointer-free
// (A2). Timestamps are stored as int64 unix-millis in memory — the ClickHouse
// column stays DateTime64(3, 'UTC') — so MemoryUserPosition et al. carry no
// *time.Location pointer and the GC skips the whole ring backing array on every
// mark, instead of scanning all N entries. Conversions happen at Append/Recover
// (see batchAppendLines / resultValueExpr) and at assignment (state.go Save).
func memoryGoType(field customFieldSpec, isEvent bool) string {
	// Event value-structs embed EventMeta (shared with the ingestion/handler layer,
	// where BlockTimestamp is a time.Time), so they keep time.Time. Only the derived
	// entity rings (UserPositions et al.) — the ones that grow to capacity — are
	// converted to int64 to make their backing arrays pointer-free.
	if isEvent {
		return field.Type
	}
	switch field.Type {
	case "time.Time":
		return "int64"
	default:
		return field.Type
	}
}

func batchColumnType(field customFieldSpec) string {
	switch field.Type {
	case "bool", "uint8":
		return "proto.ColUInt8"
	case "uint16":
		return "proto.ColUInt16"
	case "uint32":
		return "proto.ColUInt32"
	case "uint", "uint64":
		return "proto.ColUInt64"
	case "int8":
		return "proto.ColInt8"
	case "int16":
		return "proto.ColInt16"
	case "int32":
		return "proto.ColInt32"
	case "int", "int64":
		return "proto.ColInt64"
	case "float32":
		return "proto.ColFloat32"
	case "float64":
		return "proto.ColFloat64"
	case "string", "[]byte":
		return "proto.ColStr"
	case "time.Time":
		return "proto.ColDateTime64"
	case "common.Address", "common.Hash":
		return "proto.ColFixedStr"
	case "uint256.Int":
		return "proto.ColUInt256"
	case "decimal.Decimal":
		return "decimal256Col"
	case "protomath.Decimal256":
		return "decimal256Col"
	case "[]common.Hash":
		return "*proto.ColArr[[]byte]"
	case "[]uint256.Int":
		return "*proto.ColArr[proto.UInt256]"
	case "[]string":
		return "*proto.ColArr[string]"
	default:
		return "proto.ColStr"
	}
}

func resultColumnType(field customFieldSpec) string {
	return batchColumnType(field)
}

func batchInitLines(field customFieldSpec) []string {
	return initLines("b.", field)
}

func resultInitLines(field customFieldSpec) []string {
	if field.Type == "decimal.Decimal" {
		return nil
	}
	return initLines("", field)
}

func initLines(prefix string, field customFieldSpec) []string {
	col := prefix + batchColumnField(field)
	switch field.Type {
	case "common.Address":
		return []string{col + ".SetSize(20)"}
	case "common.Hash":
		return []string{col + ".SetSize(32)"}
	case "time.Time":
		return []string{col + ".WithPrecision(proto.Precision(3))", col + ".WithLocation(time.UTC)"}
	case "[]common.Hash":
		return []string{col + " = (&proto.ColFixedStr{Size: 32}).Array()"}
	case "[]uint256.Int":
		return []string{col + " = new(proto.ColUInt256).Array()"}
	case "[]string":
		return []string{col + " = new(proto.ColStr).Array()"}
	default:
		return nil
	}
}

func batchInputData(field customFieldSpec) string {
	col := "b." + batchColumnField(field)
	switch field.Type {
	case "decimal.Decimal", "protomath.Decimal256":
		return "&" + col
	case "[]common.Hash", "[]uint256.Int", "[]string":
		return col
	default:
		return "&" + col
	}
}

func resultData(field customFieldSpec) string {
	col := batchColumnField(field)
	switch field.Type {
	case "decimal.Decimal", "protomath.Decimal256":
		return "&" + col
	case "[]common.Hash", "[]uint256.Int", "[]string":
		return col
	default:
		return "&" + col
	}
}

func batchColumnRowsExpr(field customFieldSpec) string {
	col := "b." + batchColumnField(field)
	if strings.HasPrefix(field.Type, "[]") {
		return col + ".Rows()"
	}
	return col + ".Rows()"
}

func batchAppendLines(field customFieldSpec, isEvent bool) []string {
	col := "b." + batchColumnField(field)
	value := "item." + field.Name
	if !isEvent && field.Type == "time.Time" {
		// In-memory value is int64 unix-millis (A2); the DateTime64(3) column's raw
		// units are also milliseconds, so append the raw value directly — no
		// time.Time is constructed on the hot insert path.
		return []string{col + ".AppendRaw(proto.DateTime64(" + value + "))"}
	}
	switch field.Type {
	case "common.Address", "common.Hash":
		return []string{col + ".Append(" + value + ".Bytes())"}
	case "bool":
		return []string{col + ".Append(hotStateBool(" + value + "))"}
	case "[]byte":
		return []string{col + ".AppendBytes(" + value + ")"}
	case "decimal.Decimal":
		return []string{col + ".Append(hotStateDecimalToDecimal256(" + value + ").Proto())"}
	case "protomath.Decimal256":
		return []string{col + ".Append(" + value + ".Proto())"}
	case "uint256.Int":
		return []string{col + ".Append(hotStateUInt256(" + value + "))"}
	case "[]common.Hash":
		return []string{col + ".Append(hotStateHashSliceBytes(" + value + ", b." + batchScratchField(field) + "))"}
	case "[]uint256.Int":
		return []string{col + ".Append(hotStateUInt256Slice(" + value + ", b." + batchScratchField(field) + "))"}
	default:
		return []string{col + ".Append(" + value + ")"}
	}
}

func resultValueExpr(field customFieldSpec, isEvent bool) string {
	col := batchColumnField(field)
	if !isEvent && field.Type == "time.Time" {
		// Recover path (cold): read the DateTime64 instant back as int64 unix-millis
		// to match the in-memory representation (A2). UnixMilli is timezone-
		// independent, so it round-trips the millisecond raw value exactly.
		return col + ".Row(i).UnixMilli()"
	}
	switch field.Type {
	case "common.Address":
		return "common.BytesToAddress(" + col + ".Row(i))"
	case "common.Hash":
		return "common.BytesToHash(" + col + ".Row(i))"
	case "bool":
		return col + ".Row(i) != 0"
	case "[]byte":
		return "append([]byte(nil), " + col + ".RowBytes(i)...)"
	case "decimal.Decimal":
		return "hotStateDecimalFromDecimal256(protomath.Decimal256(" + col + ".Row(i)))"
	case "protomath.Decimal256":
		return "protomath.Decimal256(" + col + ".Row(i))"
	case "uint256.Int":
		return "hotStateUint256(" + col + ".Row(i))"
	case "[]common.Hash":
		return "hotStateHashSlice(" + col + ".Row(i))"
	case "[]uint256.Int":
		return "hotStateUint256Slice(" + col + ".Row(i))"
	default:
		return col + ".Row(i)"
	}
}

func customInsertColumnList(table customTableSpec) string {
	columns := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		columns = append(columns, quoteSQLIdent(column.Name))
	}
	return "(" + strings.Join(columns, ", ") + ")"
}

func recoverQuery(table customTableSpec) string {
	q := func(col string) string { return "`t`." + quoteSQLIdent(col) }
	selects := make([]string, 0, len(table.Fields))
	for _, field := range table.Fields {
		selects = append(selects, q(field.ColumnName)+" AS "+quoteSQLIdent(field.ColumnName))
	}
	orderBy := make([]string, 0, len(table.PrimaryKey)+3)
	for _, k := range table.PrimaryKey {
		orderBy = append(orderBy, q(k)+" DESC")
	}
	orderBy = append(orderBy, q("block_number")+" DESC", q("transaction_index")+" DESC", q("log_index")+" DESC")
	limitBy := make([]string, 0, len(table.PrimaryKey))
	for _, k := range table.PrimaryKey {
		limitBy = append(limitBy, q(k))
	}
	return "SELECT " + strings.Join(selects, ", ") + " FROM %s.%s AS `t` WHERE %s ORDER BY " + strings.Join(orderBy, ", ") + " LIMIT 1 BY " + strings.Join(limitBy, ", ") + " SETTINGS optimize_read_in_order = 1"
}

func recoverWhereExpr(table customTableSpec) string {
	base := recoverBucketWhereExpr(table)
	for _, field := range table.Fields {
		if field.ColumnName == "updated_at_block" {
			return base + " + recoveryRecencyClause()"
		}
	}
	return base
}

func recoverBucketWhereExpr(table customTableSpec) string {
	if len(table.PrimaryKey) == 0 {
		return strconv.Quote("1")
	}
	firstKey := table.PrimaryKey[0]
	for _, field := range table.Fields {
		if field.ColumnName != firstKey {
			continue
		}
		if size, ok := fixedStringColumnSize(field.ColumnType); ok {
			return fmt.Sprintf("recoveryFixedStringRange(%s, bucket, %d)", strconv.Quote("`t`."+quoteSQLIdent(firstKey)), size)
		}
		break
	}
	q := func(col string) string { return "`t`." + quoteSQLIdent(col) }
	hashCols := make([]string, 0, len(table.PrimaryKey))
	for _, k := range table.PrimaryKey {
		hashCols = append(hashCols, q(k))
	}
	return "fmt.Sprintf(" + strconv.Quote("cityHash64("+strings.Join(hashCols, ", ")+") %% %d = %d") + ", recoveryBucketCount, bucket)"
}

func fixedStringColumnSize(chType string) (int, bool) {
	const prefix = "FixedString("
	if !strings.HasPrefix(chType, prefix) || !strings.HasSuffix(chType, ")") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(chType, prefix), ")"))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func quotedColumns(columns []string) []string {
	out := make([]string, 0, len(columns))
	for _, column := range columns {
		out = append(out, quoteSQLIdent(column))
	}
	return out
}

func customTablesUseDecimal(tables []customTableSpec) bool {
	return customTablesUseType(tables, "decimal.Decimal")
}

func customTablesUseBool(tables []customTableSpec) bool {
	return customTablesUseType(tables, "bool")
}

func customTablesUseUint256(tables []customTableSpec) bool {
	return customTablesUseType(tables, "uint256.Int")
}

func customTablesUseHashSlice(tables []customTableSpec) bool {
	return customTablesUseType(tables, "[]common.Hash")
}

func customTablesUseUint256Slice(tables []customTableSpec) bool {
	return customTablesUseType(tables, "[]uint256.Int")
}

func eventsUseUint256(events []eventSpec) bool {
	return eventsUseType(events, "uint256.Int")
}

func eventsUseUint256Slice(events []eventSpec) bool {
	return eventsUseType(events, "[]uint256.Int")
}

func eventsUseType(events []eventSpec, typ string) bool {
	for _, ev := range events {
		for _, arg := range ev.Args {
			if arg.GoType == typ {
				return true
			}
		}
	}
	return false
}

func customTablesUseType(tables []customTableSpec, typ string) bool {
	for _, table := range tables {
		for _, field := range table.Fields {
			if field.Type == typ {
				return true
			}
		}
	}
	return false
}

// notFlatType reports whether a Go type string carries a pointer/slice/map/string
// (or time.Time), disqualifying it from the zero-transformation unsafe memcpy used
// by the pointer-free cold path, and (for keys) from the cold tier entirely.
func notFlatType(t string) bool {
	return strings.HasPrefix(t, "[]") || strings.HasPrefix(t, "*") ||
		strings.HasPrefix(t, "map[") || strings.Contains(t, "string") ||
		t == "time.Time"
}

// isPointerFreeEntity reports whether the entity's in-memory value AND key are
// free of pointers/slices/strings, so they can be stored in the Pebble cold tier
// as raw bytes via unsafe memcpy (zero-transformation). Slice-bearing entities
// (e.g. Array(UInt256) payouts, Array(FixedString(32)) question_ids) are excluded
// and keep the ClickHouse-fallback lazy-load path. Note: memoryGoType maps
// time.Time -> int64 for non-event entities, so timestamps don't disqualify them.
func isPointerFreeEntity(table customTableSpec) bool {
	for _, field := range table.Fields {
		if notFlatType(memoryGoType(field, table.IsEvent)) {
			return false
		}
	}
	for _, field := range table.keyFields() {
		if notFlatType(field.Type) {
			return false
		}
	}
	return true
}

// coldFieldEncodable reports whether a field's (memory-resolved) Go type can be
// encoded by the generated binary cold codec: fixed scalars, fixed-size byte
// arrays (common.Hash/common.Address/uint256.Int), and slices whose elements are
// fixed-size byte arrays. Strings, decimals, maps and structs are NOT encodable —
// an entity with such a field stays on the ClickHouse-fallback lazy-load path.
func coldFieldEncodable(field customFieldSpec, isEvent bool) bool {
	switch memoryGoType(field, isEvent) {
	case "bool", "uint8", "int8", "uint16", "int16", "uint32", "int32",
		"uint", "int", "uint64", "int64":
		return true
	case "common.Hash", "common.Address", "uint256.Int":
		return true
	case "[]common.Hash", "[]uint256.Int", "[]common.Address":
		return true
	}
	return false
}

// isColdSerializableEntity reports whether a pointer-BEARING entity can still use
// the Pebble cold tier via a generated marshal/unmarshal codec (its only pointers
// are slices of fixed-size elements). The key must be pointer-free because cold
// keys are always encoded via unsafe memcpy; values are encoded field-by-field.
// This lets slice-bearing entities (Condition's []uint256.Int payouts, Market's
// []common.Hash question_ids) avoid the per-miss ClickHouse SELECT storm.
func isColdSerializableEntity(table customTableSpec) bool {
	for _, field := range table.keyFields() {
		if notFlatType(field.Type) {
			return false
		}
	}
	for _, field := range table.Fields {
		if !coldFieldEncodable(field, table.IsEvent) {
			return false
		}
	}
	return true
}

// entityUsesCold reports whether a (non-event) entity participates in the Pebble
// cold tier — either as a pointer-free value (raw-bytes memcpy) or a serializable
// one (binary codec). Events are append-only (no lazy Get-by-key), so they never
// need it.
func entityUsesCold(table customTableSpec) bool {
	return !table.IsEvent && (isPointerFreeEntity(table) || isColdSerializableEntity(table))
}

// customTablesUseColdCache reports whether any entity can use the cold tier;
// drives the "unsafe" + "path/filepath" imports in the generated hot state.
func customTablesUseColdCache(tables []customTableSpec) bool {
	for _, table := range tables {
		if entityUsesCold(table) {
			return true
		}
	}
	return false
}

// coldFixedWidth returns the on-wire byte width of a fixed-size resolved Go type
// (scalar or [N]byte array), or 0 for slices (variable length). Used to size
// decode bounds checks and the slice-element sanity cap.
func coldFixedWidth(goType string) int {
	switch goType {
	case "bool", "uint8", "int8":
		return 1
	case "uint16", "int16":
		return 2
	case "uint32", "int32":
		return 4
	case "uint", "int", "uint64", "int64":
		return 8
	case "common.Hash":
		return 32
	case "common.Address":
		return 20
	case "uint256.Int":
		return 32
	}
	return 0
}

// coldSliceElem maps a slice element type to its resolved Go element type for the
// types the cold codec supports ([]common.Hash, []uint256.Int, []common.Address).
// Returns ("", false) for unsupported element types.
func coldSliceElem(goType string) (string, bool) {
	switch goType {
	case "[]common.Hash":
		return "common.Hash", true
	case "[]uint256.Int":
		return "uint256.Int", true
	case "[]common.Address":
		return "common.Address", true
	}
	return "", false
}

// coldEncodeExpr returns a Go statement that appends the encoding of expression
// `e` (of resolved Go type `t`) to the []byte `b`. Only valid for fixed types.
func coldEncodeExpr(e, t string) string {
	switch t {
	case "bool":
		return "if " + e + " { b = append(b, 1) } else { b = append(b, 0) }"
	case "uint8", "int8":
		return "b = append(b, byte(" + e + "))"
	case "uint16", "int16":
		return "b = binary.LittleEndian.AppendUint16(b, uint16(" + e + "))"
	case "uint32", "int32":
		return "b = binary.LittleEndian.AppendUint32(b, uint32(" + e + "))"
	case "uint", "int", "uint64", "int64":
		return "b = binary.LittleEndian.AppendUint64(b, uint64(" + e + "))"
	case "common.Hash", "common.Address":
		return "b = append(b, " + e + "[:]...)"
	case "uint256.Int":
		return "{ _x := " + e + ".Bytes32(); b = append(b, _x[:]...) }"
	}
	return ""
}

// coldDecodeAssign returns Go statements that read one value of resolved Go type
// `t` from data[off:] into the lvalue `dst`, advancing off, with a bounds guard
// that returns (zero, false) on truncation. Only valid for fixed types.
func coldDecodeAssign(dst, t string) []string {
	w := coldFixedWidth(t)
	switch t {
	case "bool":
		return []string{"if off+1 > len(data) { return v, false }", dst + " = data[off] != 0", "off++"}
	case "uint8":
		return []string{"if off+1 > len(data) { return v, false }", dst + " = uint8(data[off])", "off++"}
	case "int8":
		return []string{"if off+1 > len(data) { return v, false }", dst + " = int8(data[off])", "off++"}
	case "uint16":
		return []string{"if off+2 > len(data) { return v, false }", dst + " = binary.LittleEndian.Uint16(data[off:])", "off += 2"}
	case "int16":
		return []string{"if off+2 > len(data) { return v, false }", dst + " = int16(binary.LittleEndian.Uint16(data[off:]))", "off += 2"}
	case "uint32":
		return []string{"if off+4 > len(data) { return v, false }", dst + " = binary.LittleEndian.Uint32(data[off:])", "off += 4"}
	case "int32":
		return []string{"if off+4 > len(data) { return v, false }", dst + " = int32(binary.LittleEndian.Uint32(data[off:]))", "off += 4"}
	case "uint", "uint64":
		return []string{"if off+8 > len(data) { return v, false }", dst + " = binary.LittleEndian.Uint64(data[off:])", "off += 8"}
	case "int", "int64":
		return []string{"if off+8 > len(data) { return v, false }", dst + " = int64(binary.LittleEndian.Uint64(data[off:]))", "off += 8"}
	case "common.Hash", "common.Address":
		return []string{
			fmt.Sprintf("if off+%d > len(data) { return v, false }", w),
			fmt.Sprintf("copy(%s[:], data[off:off+%d])", dst, w),
			fmt.Sprintf("off += %d", w),
		}
	case "uint256.Int":
		return []string{"if off+32 > len(data) { return v, false }", dst + ".SetBytes32(data[off:off+32])", "off += 32"}
	}
	return nil
}

// renderColdCodec emits marshalCold<T>/unmarshalCold<T> for a serializable
// (pointer-bearing) entity: a compact, deterministic binary encoding (LE
// scalars, raw bytes for Hash/Address/uint256, uvarint-length-prefixed slices,
// Tombstone trailer). Used by the spill/Get/Recover cold paths in place of the
// unsafe memcpy used by pointer-free entities.
// coldFieldTmpl is the per-field encode/decode plumbing for the cold codec.
// IsSlice selects the slice shape (length-prefixed, bounded element loop); the
// scalar shape uses EncodeExpr/DecodeLines directly on the whole field.
type coldFieldTmpl struct {
	Name        string
	IsSlice     bool
	Elem        string   // slice element type (slice only)
	FixedWidth  int      // on-wire element width, for the decode bounds check (slice only)
	EncodeExpr  string   // marshal expression (element when slice, whole field when scalar)
	DecodeLines []string // unmarshal statements (element when slice, whole field when scalar)
}

// renderColdCodec emits marshalCold<T>/unmarshalCold<T> for a serializable entity.
// The slice-vs-scalar decision (coldSliceElem) is resolved here so the template
// stays a straight interpolation of precomputed expressions.
func renderColdCodec(b *bytes.Buffer, spec hotStateSpec) {
	data := struct {
		GoTypeName string
		Fields     []coldFieldTmpl
	}{GoTypeName: spec.table.GoTypeName}

	for _, field := range spec.table.Fields {
		gt := memoryGoType(field, spec.table.IsEvent)
		if elem, ok := coldSliceElem(gt); ok {
			data.Fields = append(data.Fields, coldFieldTmpl{
				Name:        field.Name,
				IsSlice:     true,
				Elem:        elem,
				FixedWidth:  coldFixedWidth(elem),
				EncodeExpr:  coldEncodeExpr("v."+field.Name+"[i]", elem),
				DecodeLines: coldDecodeAssign("v."+field.Name+"[i]", elem),
			})
			continue
		}
		data.Fields = append(data.Fields, coldFieldTmpl{
			Name:        field.Name,
			EncodeExpr:  coldEncodeExpr("v."+field.Name, gt),
			DecodeLines: coldDecodeAssign("v."+field.Name, gt),
		})
	}

	b.WriteString(template.MustExecute("code/hotStateColdCodec", data))
}

// renderBatchResolver renders <T>BatchResolver. The IN-list builder and the
// SELECT query differ by key arity (multi-column tuples vs a single column vs no
// key), so those two pieces are precomputed here; everything else is static text
// or the shared per-column result plumbing.
func renderBatchResolver(b *bytes.Buffer, spec hotStateSpec) {
	keyFields := spec.table.keyFields()

	data := struct {
		ResolverType        string
		CacheType           string
		KeyType             string
		ValueType           string
		IsEvent             bool
		KeyFields           []recoverKeyFieldTmpl
		KeyValuesAppendExpr string
		QueryStmt           string
		Fields              []recoverFieldTmpl
	}{
		ResolverType: spec.table.GoTypeName + "BatchResolver",
		CacheType:    spec.cacheType,
		KeyType:      spec.keyType,
		ValueType:    spec.table.GoTypeName,
		IsEvent:      spec.table.IsEvent,
		Fields:       resolverResultFields(spec),
	}
	for _, field := range keyFields {
		data.KeyFields = append(data.KeyFields, recoverKeyFieldTmpl{Name: field.Name})
	}

	// Build the per-key value expression and the bucketed SELECT. Multi-column
	// keys emit a "(a, b)" tuple; a single column emits the bare value; a keyless
	// entity has no IN-list at all (queryStr stays empty).
	if len(keyFields) > 1 {
		expr := `"("`
		for i, field := range keyFields {
			if i > 0 {
				expr += ` + ", "`
			}
			expr += " + " + keySQLValueExpr("key."+field.Name, field)
		}
		expr += ` + ")"`
		data.KeyValuesAppendExpr = expr

		queryBody := fmt.Sprintf("SELECT %s FROM %%s.%s WHERE %s IN (%%s) ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY %s",
			customSelectColumnList(spec.table),
			spec.table.Name,
			customInsertColumnList(customTableSpec{Columns: keyColumns(keyFields)}),
			keyColumnExpressionList(keyFields),
		)
		data.QueryStmt = fmt.Sprintf("queryStr := fmt.Sprintf(%s, quoteIdent(db), strings.Join(values, \", \"))", strconv.Quote(queryBody))
	} else if len(keyFields) == 1 {
		keyField := keyFields[0]
		data.KeyValuesAppendExpr = keySQLValueExpr("key."+keyField.Name, keyField)

		queryBody := fmt.Sprintf("SELECT %s FROM %%s.%s WHERE %s IN (%%s) ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY %s",
			customSelectColumnList(spec.table),
			spec.table.Name,
			quoteSQLIdent(keyField.ColumnName),
			keyColumnExpressionList(keyFields),
		)
		data.QueryStmt = fmt.Sprintf("queryStr := fmt.Sprintf(%s, quoteIdent(db), strings.Join(values, \", \"))", strconv.Quote(queryBody))
	}

	b.WriteString(template.MustExecute("code/hotStateBatchResolver", data))
}

func keySQLValueExpr(expr string, field customFieldSpec) string {
	switch field.Type {
	case "common.Address", "common.Hash":
		return fmt.Sprintf("fmt.Sprintf(\"unhex('%%s')\", strings.TrimPrefix(%s.Hex(), \"0x\"))", expr)
	case "string":
		return fmt.Sprintf("fmt.Sprintf(\"'%%s'\", strings.ReplaceAll(%s, \"'\", \"''\"))", expr)
	case "bool":
		return fmt.Sprintf("hotStateBoolSQL(%s)", expr)
	case "uint256.Int":
		return expr + ".Dec()"
	case "decimal.Decimal":
		return fmt.Sprintf("fmt.Sprintf(\"'%%s'\", %s.String())", expr)
	case "protomath.Decimal256":
		return fmt.Sprintf("fmt.Sprintf(\"'%%s'\", %s.String(protomath.Decimal256Scale18))", expr)
	case "float32", "float64":
		return fmt.Sprintf("fmt.Sprintf(\"%%v\", %s)", expr)
	case "time.Time":
		return fmt.Sprintf("fmt.Sprintf(\"'%%s'\", %s.UTC().Format(time.RFC3339Nano))", expr)
	default:
		return fmt.Sprintf("fmt.Sprintf(\"%%d\", %s)", expr)
	}
}

func customSelectColumnList(table customTableSpec) string {
	selects := make([]string, 0, len(table.Fields))
	for _, field := range table.Fields {
		selects = append(selects, quoteSQLIdent(field.ColumnName))
	}
	return strings.Join(selects, ", ")
}

func keyColumns(fields []customFieldSpec) []customColumnSpec {
	out := make([]customColumnSpec, 0, len(fields))
	for _, f := range fields {
		out = append(out, customColumnSpec{Name: f.ColumnName})
	}
	return out
}

func keyColumnExpressionList(fields []customFieldSpec) string {
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		out = append(out, quoteSQLIdent(field.ColumnName))
	}
	return strings.Join(out, ", ")
}

func findStateEventSpec(events []eventSpec, state config.StateConfig) (*eventSpec, error) {
	if state.SourceTable != "" {
		for _, ev := range events {
			if ev.TableName == state.SourceTable {
				return &ev, nil
			}
		}
	}
	// Try to resolve from state.Name
	for _, ev := range events {
		if strings.EqualFold(ev.EventName, state.Name) || strings.EqualFold(ev.GoTypeName, state.Name) {
			return &ev, nil
		}
	}
	// Fallback: try to see if any event table contains the state name
	for _, ev := range events {
		if strings.Contains(strings.ToLower(ev.TableName), strings.ToLower(state.Name)) {
			return &ev, nil
		}
	}
	return nil, fmt.Errorf("source table or event matching %q not found in event tables", state.Name)
}
