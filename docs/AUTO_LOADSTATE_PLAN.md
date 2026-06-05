# Plan: Auto-Generate LoadStateFromClickHouseFn

## Goal
Eliminate 200+ lines of manual ClickHouse HTTP code by auto-generating `LoadStateFromClickHouseFn` based on schema.

## Current State (Manual)
```go
// custom_processor.go - ~200 lines of manual code
func init() {
    generated.LoadStateFromClickHouseFn = func(...) error {
        // 1. HTTP client setup
        // 2. 5 separate queries (Condition, Position, NegRiskEvent, Market, FPMM)
        // 3. JSON row structs
        // 4. Type conversions (string → Address, Hash, uint256, Decimal256)
        // 5. MarkDirty() calls
    }
}
```

## Target State (Auto-Generated)
```go
// generated/state.go - auto-generated
func LoadStateFromClickHouseImpl(state *State, ctx context.Context, httpPort int, blockNumber uint64) error {
    // Auto-generated per-table loaders
    loadCondition(state, ctx, httpPort, blockNumber)
    loadPosition(state, ctx, httpPort, blockNumber)
    loadNegRiskEvent(state, ctx, httpPort, blockNumber)
    // ...

    state.LastSyncBlock = blockNumber
    state.LastPruneBlock = blockNumber
    state.SaveSnapshot(blockNumber)
    return nil
}
```

## Implementation Plan

### Phase 1: Add ClickHouse Type Mapping

**File**: `internal/codegen/schema_parser.go` (new or extend)

**Map Go types to ClickHouse types**:
```go
var goToCHType = map[string]string{
    "common.Address":  "FixedString(20)",
    "common.Hash":     "FixedString(32)",
    "uint256.Int":     "UInt256",
    "protomath.Decimal256": "Decimal(18,0)", // or use string
    "uint8":           "UInt8",
    "uint32":          "UInt32",
    "uint64":          "UInt64",
    "int64":           "Int64",
    "bool":            "UInt8",
    "time.Time":       "DateTime",
}
```

**Map Go types to JSON parsing**:
```go
var goToJSONParser = map[string]jsonParser{
    "common.Address":  "common.HexToAddress(row.%s)",
    "common.Hash":     "common.HexToHash(row.%s)",
    "uint256.Int":     "uint256.MustFromDecimal(row.%s)",
    "protomath.Decimal256": "decimal256FromDecimal(decimal.MustFromString(row.%s))",
    "uint8":           "row.%s",
    "uint32":          "row.%s",
    "uint64":          "row.%s",
    "bool":            "row.%s != 0",
}
```

### Phase 2: Generate Query Builder

**File**: `internal/codegen/state.go` (add `renderLoadStateFromClickHouse()`)

**For each table, generate**:
```go
func load{TableName}(state *State, ctx context.Context, httpPort int, blockNumber uint64) error {
    query := build{TableName}Query(blockNumber)
    return queryClickHouse(ctx, httpPort, query, func(dec *json.Decoder) error {
        return parse{TableName}Row(state, dec)
    })
}

func build{TableName}Query(blockNumber uint64) string {
    if blockNumber > 0 {
        return fmt.Sprintf(`SELECT id, oracle, question_id, outcome_slot_count, resolved, payouts
                           FROM %s.memory_conditions
                           WHERE block_number <= %d
                           ORDER BY block_number DESC, transaction_index DESC, log_index DESC
                           LIMIT 1 BY id`, dbName, blockNumber)
    }
    return fmt.Sprintf(`SELECT id, oracle, question_id, outcome_slot_count, resolved, payouts
                       FROM %s.memory_conditions
                       ORDER BY block_number DESC, transaction_index DESC, log_index DESC
                       LIMIT 1 BY id`, dbName)
}
```

### Phase 3: Generate Row Parser

**For each table, generate**:
```go
func parse{TableName}Row(state *State, dec *json.Decoder) error {
    var row struct {
        ID               string   `json:"id"`
        Oracle           string   `json:"oracle"`
        QuestionID       string   `json:"question_id"`
        OutcomeSlotCount uint8    `json:"outcome_slot_count"`
        Resolved         uint8    `json:"resolved"`
        Payouts          []string `json:"payouts"`
    }
    if err := dec.Decode(&row); err != nil {
        return err
    }

    val := &generated.Condition{
        ID:               common.HexToHash(row.ID),
        Oracle:           common.HexToAddress(row.Oracle),
        QuestionID:       common.HexToHash(row.QuestionID),
        OutcomeSlotCount: row.OutcomeSlotCount,
        Resolved:         row.Resolved != 0,
    }

    // Handle array types
    for _, pStr := range row.Payouts {
        pInt, err := uint256.FromDecimal(pStr)
        if err != nil {
            return err
        }
        val.Payouts = append(val.Payouts, *pInt)
    }

    state.Condition.MarkDirty(val)
    return nil
}
```

### Phase 4: Special Type Handlers

**Array types** (`[]uint256.Int`, `[]common.Hash`):
```go
// Generate loop for array fields
for _, itemStr := range row.{Field} {
    item, err := parse{FieldType}(itemStr)
    if err != nil {
        return err
    }
    val.{Field} = append(val.{Field}, item)
}
```

**Decimal256 types** (need helper):
```go
// Helper function (generate once)
func decimal256FromDecimal(d decimal.Decimal) protomath.Decimal256 {
    i, _ := protomath.Decimal256FromDec(d)
    return i
}
```

### Phase 5: Wire Into State.go

**Add to `generateStateGo()`**:
```go
// After renderStateHandleTypes()
renderLoadStateFromClickHouse(&b, handles, cfg)

// This generates:
func LoadStateFromClickHouseImpl(state *State, ctx context.Context, httpPort int, blockNumber uint64) error {
    if err := loadCondition(state, ctx, httpPort, blockNumber); err != nil {
        log.Printf("LoadState: condition load failed: %v", err)
    }
    if err := loadPosition(state, ctx, httpPort, blockNumber); err != nil {
        log.Printf("LoadState: position load failed: %v", err)
    }
    // ... for each table

    state.LastSyncBlock = blockNumber
    state.LastPruneBlock = blockNumber
    state.SaveSnapshot(blockNumber)
    return nil
}
```

### Phase 6: Remove Manual Code

**File**: `examples/polymarket/custom_processor.go`

**Delete**:
- Lines 1055-1249 (the entire `init()` function with LoadStateFromClickHouseFn)

**Result**: User code drops from ~250 lines to ~50 lines!

## Type Conversion Table

| Go Type | ClickHouse Type | JSON Parse |
|---------|-----------------|------------|
| `common.Address` | `FixedString(20)` | `common.HexToAddress(row.X)` |
| `common.Hash` | `FixedString(32)` | `common.HexToHash(row.X)` |
| `uint256.Int` | `UInt256` | `uint256.FromDecimal(row.X)` |
| `protomath.Decimal256` | `Decimal(18,0)` | `decimal256FromDecimal(decimal.NewFromString(row.X))` |
| `uint8` | `UInt8` | `row.X` |
| `uint32` | `UInt32` | `row.X` |
| `uint64` | `UInt64` | `row.X` |
| `bool` | `UInt8` | `row.X != 0` |
| `time.Time` | `DateTime` | `time.Parse(..., row.X)` |
| `[]common.Hash` | `Array(FixedString(32))` | Loop + `common.HexToHash` |
| `[]uint256.Int` | `Array(UInt256)` | Loop + `uint256.FromDecimal` |

## Array Handling Pattern

For each array field, generate:
```go
// Field: QuestionIDs []common.Hash
for _, qidStr := range row.QuestionIDs {
    val.QuestionIDs = append(val.QuestionIDs, common.HexToHash(qidStr))
}

// Field: Payouts []uint256.Int
for _, pStr := range row.Payouts {
    pInt, err := uint256.FromDecimal(pStr)
    if err != nil {
        return err
    }
    val.Payouts = append(val.Payouts, *pInt)
}
```

## Implementation Steps

1. ✅ **Add type mapping constants** to `internal/codegen/codegen.go`
2. ✅ **Create `renderLoadStateFromClickHouse()`** function
3. ✅ **Create `renderTableLoader()`** per-table generator
4. ✅ **Create `renderRowParser()`** with type conversions
5. ✅ **Generate helper functions** (decimal256FromDecimal, etc.)
6. ✅ **Wire into generateStateGo()**
7. ✅ **Test with polymarket** (verify auto-gen matches manual)
8. ✅ **Remove manual code from custom_processor.go**

## Files to Modify

| File | Change |
|------|--------|
| `internal/codegen/codegen.go` | Add type mappings |
| `internal/codegen/state.go` | Add renderLoadStateFromClickHouse(), renderTableLoader(), renderRowParser() |
| `examples/polymarket/custom_processor.go` | Delete init() function (200+ lines) |
| `examples/polymarket/generated/state.go` | Auto-generates new LoadState code |

## Validation

After generation, verify:
1. All tables have loader functions
2. Type conversions are correct
3. Array fields loop properly
4. MarkDirty() called for each row
5. Error handling present

## Expected Result

**Before** (manual):
```go
// custom_processor.go: 200 lines
func init() {
    generated.LoadStateFromClickHouseFn = func(...) error {
        // HTTP client
        // 5 queries
        // 5 row structs
        // Type conversions
        // MarkDirty calls
    }
}
```

**After** (auto-generated):
```go
// custom_processor.go: 0 lines (nothing!)

// generated/state.go: auto-generated
func LoadStateFromClickHouseImpl(...) error {
    loadCondition(...)
    loadPosition(...)
    // ... per table
}
```

User code drops by **200 lines**. Zero manual ClickHouse code needed!
