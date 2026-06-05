# ECS Code Generation Guide

This guide explains the internal code generation system that produces `view_gen.go`.
All source files referenced live at `../ecs/`.

---

## 1. Overview: What Gets Generated

The ECS library uses Go's `go generate` to produce `view_gen.go` — a ~6500-line file containing:

- **`ViewN` structs** (N=1 through 12): Generic query types parameterized by 1-12 component types.
- **`QueryN` constructors**: Create queries for specific component combinations.
- **`Read` methods**: Direct access to an entity's components by ID.
- **`Count` methods**: Count entities matching the query.
- **`MapId` methods**: Sequential iteration over matching entities.
- **`MapIdParallel` methods**: Multi-threaded iteration via goroutine pool.
- **`MapSlices` methods**: Batch iteration (deprecated, tentative).

These types enable the user-facing API:

```go
query := ecs.Query2[Position, Velocity](world)
query.MapId(func(id ecs.Id, pos *Position, vel *Velocity) { ... })
```

The code generator creates concrete methods for each generic arity (1 component, 2 components, ..., 12 components). Without code generation, Go generics would require significantly more complex runtime reflection.

---

## 2. The Code Generation Pipeline

### 2.1 Trigger: `go:generate`

File: `ecs/ecs.go` (line 2):
```go
//go:generate go run ./internal/gen >> view_gen.go
```

This directive tells `go generate` to:
1. Compile and run `./internal/gen/main.go`
2. Append its stdout to `view_gen.go`

**Important**: The `>>` means it APPENDS. This is unusual — most codegen uses `>`. The design expects you to clear `view_gen.go` before regeneration (or the generator itself manages it by writing to a temp file). In practice, the generator writes directly via `os.WriteFile`, so the redirect is a safety net.

Run it with:
```bash
cd ecs && go generate ./...
```

### 2.2 The Generator: `internal/gen/main.go`

This is a small standalone Go program:

```go
package main

import (
    "bytes"
    _ "embed"
    "go/format"
    "io/fs"
    "os"
    "strings"
    "text/template"
)

//go:embed view.tgo
var viewTemplate string

type viewData struct {
    Views [][]string
}

func main() {
    data := viewData{
        Views: [][]string{
            []string{"A"},
            []string{"A", "B"},
            []string{"A", "B", "C"},
            []string{"A", "B", "C", "D"},
            []string{"A", "B", "C", "D", "E"},
            []string{"A", "B", "C", "D", "E", "F"},
            []string{"A", "B", "C", "D", "E", "F", "G"},
            []string{"A", "B", "C", "D", "E", "F", "G", "H"},
            []string{"A", "B", "C", "D", "E", "F", "G", "H", "I"},
            []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"},
            []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K"},
            []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L"},
        },
    }
    // ... template setup, execution, formatting ...
}
```

Key points:
- Uses Go's `embed` to bundle `view.tgo` at compile time.
- The `Views` slice controls how many component arities are generated. Each entry is a list of type parameter names (A through L).
- 12 entries produces View1 through View12 (A alone, A+B, A+B+C, ..., A+B+C+...+L).
- The output is run through `go/format.Source()` for proper formatting.
- Falls back to unformatted output if formatting fails (with a panic for visibility).

### 2.3 The Template: `internal/gen/view.tgo`

This is a Go `text/template` file. It uses Go's template syntax with custom functions.

The template iterates over `{{range $i, $element := .Views}}` to produce one `ViewN` block per arity.

Key template patterns:

```go
// Type definition with N type parameters
type View{{len $element}}[{{join $element ","}} any] struct { ... }

// Component storage fields (one per type parameter)
{{range $ii, $arg := $element}}
    storage{{$arg}} *componentStorage[{{$arg}}]
{{end}}

// CompId registration
{{range $ii, $arg := $element}}
    name({{$arg}}{{$arg}}),
{{end}}

// Pointer return types
return {{with len $element}}{{nils .}}{{end}}
// Produces: return nil, nil (for 2 components)
// or: return nil, nil, nil (for 3 components)
```

---

## 3. Custom Template Functions

Defined in `main.go` lines 38-84:

| Function | Purpose | Example (for ["A","B"]) |
|----------|---------|--------------------------|
| `join` | Join with separator | `join $element ","` → `"A,B"` |
| `lower` | Lowercase first | `lower "ABC"` → `"aBC"` |
| `nils` | N comma-separated `nil` | `nils 3` → `"nil, nil, nil"` |
| `retlist` | Prefix with "ret" | `retlist $element` → `"retA, retB"` |
| `lambdaArgs` | `lower + " *" + upper` | `lambdaArgs $element` → `"a *A, b *B"` |
| `sliceLambdaArgs` | `lower + " []" + upper` | `sliceLambdaArgs $element` → `"a []A, b []B"` |
| `parallelLambdaStructArgs` | `lower + " []" + upper` with `"; "` | `"a []A; b []B"` |
| `parallelLambdaArgsFromStruct` | `"param" + upper` | `"paramA, paramB"` |

These functions generate the type-specific signatures for each `ViewN` method. The template uses them to build:
- `MapId` lambda signature: `func(id Id, a *A, b *B)`
- `MapSlices` lambda signature: `func(id []Id, a []A, b []B)`
- `MapIdParallel` lambda signature: same as MapId (per-entity pointers)

---

## 4. How the Template Maps Generics to Concrete Types

### Type Parameter Substitution

The template uses placeholder letters (A, B, C, ...) as stand-ins for actual component types. When the user writes:

```go
ecs.Query2[Position, Velocity](world)
```

Go's generics substitute `A=Position, B=Velocity` into the generated `View2[A, B]` struct. The generated struct:

```go
type View2[A, B any] struct {
    world  *World
    filter filterList
    storageA *componentStorage[A]
    storageB *componentStorage[B]
}
```

becomes at compile time:

```go
type View2[Position, Velocity any] struct {
    world  *World
    filter filterList
    storageA *componentStorage[Position]
    storageB *componentStorage[Velocity]
}
```

### Component ID Resolution

Each generated `QueryN` function calls `name()` on zero-value instances to get `CompId`:

```go
func Query2[A, B any](world *World, filters ...Filter) *View2[A, B] {
    storageA := getStorage[A](world.engine)
    storageB := getStorage[B](world.engine)

    var AA A
    var BB B

    comps := []CompId{
        name(AA),  // reflect.TypeOf -> registered CompId
        name(BB),
    }
    // ...
}
```

`name()` (in `name.go`) uses `reflect.TypeOf(t)` to look up (or register) a unique `CompId` for each type. This is how the ECS knows that `Position` is component #3 and `Velocity` is component #4, etc.

---

## 5. The Component ID System

### `CompId` Type

```go
// ecs/component.go
type CompId uint16
```

`uint16` supports up to 65535 components, but the archetype mask limits this further (see below).

### Registration via `name()` and `nameTyped()`

```go
// ecs/name.go
func name(t any) CompId {
    componentIdMutex.Lock()
    defer componentIdMutex.Unlock()

    typeof := reflect.TypeOf(t)
    compId, ok := registeredComponents[typeof]
    if !ok {
        compId = componentRegistryCounter
        if compId > maxComponentId {
            panic(fmt.Sprintf("ecs: maximum number of components exceeded: %d", maxComponentId))
        }
        registeredComponents[typeof] = compId
        componentRegistryCounter++
    }
    return compId
}
```

Key behaviors:
- Uses `reflect.TypeOf` to get a unique key per Go type.
- Global registry (shared across all `World` instances).
- Thread-safe via mutex.
- Panics if `maxComponentId` is exceeded.
- `nameTyped()` additionally calls `registerComponentStorage[T]()` to create the `storageBuilder` for that type.

### `CompId` in Archetype Masks

The archetype system uses bitmasks to track which components an entity has. Each `CompId` is a bit position:

```go
// ecs/mask.go
const numMaskBlocks = 4           // 4 uint64 blocks
const maxComponentId = (numMaskBlocks * 64) - 1  // = 255

type archetypeMask [numMaskBlocks]uint64

func (m *archetypeMask) addComponent(compId CompId) {
    idx := compId / 64
    offset := compId - (64 * idx)
    m[idx] |= (1 << offset)
}
```

So `CompId` 1 sets bit 1 of block 0, `CompId` 65 sets bit 1 of block 1, etc.

---

## 6. Extending Beyond 12 Components

### Step 1: Add more entries to `Views`

In `internal/gen/main.go`, add more letter sequences:

```go
data := viewData{
    Views: [][]string{
        // ... existing 12 entries ...
        []string{"A","B","C","D","E","F","G","H","I","J","K","L","M"},      // 13
        []string{"A","B","C","D","E","F","G","H","I","J","K","L","M","N"},  // 14
        []string{"A","B","C","D","E","F","G","H","I","J","K","L","M","N","O"}, // 15
        []string{"A","B","C","D","E","F","G","H","I","J","K","L","M","N","O","P"}, // 16
    },
}
```

The template handles arbitrary N — it iterates over `$element` regardless of length.

### Step 2: Regenerate

```bash
cd ecs
rm view_gen.go         # clear old generated code
go generate ./...      # regenerate
```

### Step 3: Verify compilation

```bash
go build ./...
```

The template generates everything needed: struct fields, storage references, query methods, MapId/MapIdParallel/MapSlices — all driven by the `$element` list.

**Important**: There are diminishing returns. The generated file grows ~500 lines per arity. At 12 components it's already ~6500 lines. At 20 it would be ~12000+ lines. The Go compiler handles generic monomorphization, so compile times increase with each arity.

### Maximum component types

`maxComponentId` is defined in `mask.go` as `(numMaskBlocks * 64) - 1 = 255`. To support more:

```go
// mask.go
const numMaskBlocks = 8  // was 4
const maxComponentId = (numMaskBlocks * 64) - 1  // now 511
```

This doubles the archetype mask size (from 32 bytes to 64 bytes per mask). Update `blankArchMask` accordingly (it auto-sizes from the const).

---

## 7. Generating Custom Query Types

The template can be extended with new query methods or entirely new view types.

### Adding a new method to all generated views

Edit `view.tgo`. For example, to add a `FilteredMap` method that takes a predicate:

```go
// Inside the {{range .Views}} block, after MapSlices:

// Maps only entities where the predicate returns true
func (v *View{{len $element}}[{{join $element ","}}]) FilteredMap(
    pred func(id Id, {{lambdaArgs $element}}) bool,
    lambda func(id Id, {{lambdaArgs $element}}),
) {
    v.MapId(func(id Id, {{retlist $element}}) {
        if pred(id, {{retlist $element}}) {
            lambda(id, {{retlist $element}})
        }
    })
}
```

Then regenerate. The method appears on every View1-12.

### Adding a new view type for specialized queries

If you need query types that don't fit the `ViewN` pattern, you can write a separate template. For example, a `MarketQuery` that pre-caches market-specific indexes:

Create `internal/gen/market.tgo` and add generation logic in `main.go`. But for most cases, it's simpler to just use `Optional` filters on existing views:

```go
query := ecs.Query3[Order, Market, Price](world,
    ecs.Optional(Price{}),
)
```

### Specialized market queries (example)

For Polymarket, you might want a query that efficiently groups by market:

```go
// Manual query (no codegen needed, just build on existing primitives)
func MarketOrdersQuery(world *ecs.World, marketID ecs.Id) *ecs.View2[Order, Token] {
    // Query all Order+Token, filter in MapId by Order.OutcomeID -> Market
    return ecs.Query2[Order, Token](world)
}

// Usage in system:
func MatchMarket(dt time.Duration, query *ecs.View2[Order, Token], target ecs.Id) {
    query.MapId(func(id ecs.Id, o *Order, t *Token) {
        if o.OutcomeID != target && o.OutcomeID != target+1 { // binary market
            return
        }
        // match...
    })
}
```

Pre-filtering inside `MapId` is idiomatic ECS — the archetype filter handles component-level filtering, and your lambda handles value-level filtering.

---

## 8. Codegen for Custom Archetype Operations

The existing archetype operations (`moveArchetype`, `TagForDeletion`, etc.) work generically on `storage` interface + `CompId`. They don't need codegen — they operate on the abstract storage layer.

If you need an operation that requires knowing component types at compile time, the pattern is:

1. Add the operation signature to the template (e.g., `DumpComponents` that returns all component values)
2. Use template functions to generate type-safe access

Example: Add a `Dump` method to `ViewN`:

```go
// In view.tgo, inside the View{{len $element}} block:

// Dump returns all component values for the entity.
func (v *View{{len $element}}[{{join $element ","}}]) Dump(id Id) ({{join $element ","}}, bool) {
    ptrs := v.Read(id)
    if ptrs == ({{nils $element | len}}) {
        var zero struct {
            {{range $ii, $arg := $element}}
            {{$arg}} {{$arg}}
            {{end}}
        }
        return zero, false
    }
    return {{range $ii, $arg := $element}}*ptrs.{{$arg}}, {{end}}true
}
```

This demonstrates the template pattern: iterate over `$element` to generate per-type fields.

---

## 9. Relationship Between view_gen.go and view.tgo

```
view.tgo (template, 295 lines)
    |
    | go run ./internal/gen
    v
view_gen.go (generated, ~6464 lines)
```

The template is the **source of truth**. `view_gen.go` is a build artifact — never edit it manually.

### What the template controls

| Template section | Generated output |
|-----------------|------------------|
| `type View{{len $element}}` | View1 through View12 structs |
| `func Query{{len $element}}` | Query1 through Query12 constructors |
| `func (v *ViewN) Read` | Read methods for 1-12 components |
| `func (v *ViewN) Count` | Count methods for 1-12 components |
| `func (v *ViewN) MapId` | MapId for 1-12 components |
| `func (v *ViewN) MapIdParallel` | MapIdParallel for 1-12 components |
| `func (v *ViewN) MapSlices` | MapSlices for 1-12 components |

The `{{range $i, $element := .Views}}` outer loop creates 12 copies of the entire block, each with different arities.

### Template variable substitution

For a View with components [A, B, C]:
- `{{len $element}}` → `3`
- `{{join $element ","}}` → `"A,B,C"`
- `{{lower (index $element 0)}}` → `"a"`
- `{{nils (len $element)}}` → `"nil, nil, nil"`
- `{{retlist $element}}` → `"retA, retB, retC"`
- `{{lambdaArgs $element}}` → `"a *A, b *B, c *C"`

---

## 10. Extending inject.go (SystemN types)

The `inject.go` file is NOT generated — it's hand-written with generics up to `System3`. The pattern is:

```go
type System1[A any] struct { lambda func(dt time.Duration, a A) }
type System2[A, B any] struct { lambda func(dt time.Duration, a A, b B) }
type System3[A, B, C any] struct { lambda func(dt time.Duration, a A, b B, c C) }
```

Each has a `Build(world *World) System` method that calls `GetInjectable` for each type parameter and wraps the lambda.

To add System4-12, follow the same pattern:

```go
type System4[A, B, C, D any] struct {
    lambda func(dt time.Duration, a A, b B, c C, d D)
}

func (s System4[A, B, C, D]) Build(world *World) System {
    aRes := GetInjectable[A](world)
    bRes := GetInjectable[B](world)
    cRes := GetInjectable[C](world)
    dRes := GetInjectable[D](world)

    systemName := runtime.FuncForPC(reflect.ValueOf(any(s.lambda)).Pointer()).Name()
    return System{
        Name: systemName,
        Func: func(dt time.Duration) {
            s.lambda(dt, aRes, bRes, cRes, dRes)
        },
    }
}

func NewSystem4[A, B, C, D any](lambda func(dt time.Duration, a A, b B, c C, d D)) System4[A, B, C, D] {
    return System4[A, B, C, D]{lambda: lambda}
}
```

This could also be code-generated from a template — the pattern is mechanical enough. A commented-out `NewSystem4` already exists in `inject.go` line 113-127.

---

## 11. Complete Regeneration Workflow

```bash
# 1. Make changes to view.tgo (or main.go Views list)
vim ecs/internal/gen/view.tgo

# 2. Clear old generated code
rm ecs/view_gen.go

# 3. Run code generation
cd ecs && go generate ./...

# 4. Verify compilation
go build ./...
go test ./...

# 5. If adding new query arities, also extend inject.go if needed
```

---

## 12. Debugging Generated Code

If the generated code doesn't compile:

1. **Check formatting**: The generator runs `go/format.Source()`. If formatting fails, it panics. You'll see the raw unformatted output written to `view_gen.go` before the panic. Read the raw output to find syntax errors.

2. **Template syntax errors**: `template.Must` panics at generator startup if the template has syntax errors. Run `go run ./internal/gen` directly to see the error.

3. **Missing template functions**: If you reference a function not in `funcs`, template execution fails. All used functions are in `main.go` lines 38-84.

4. **Component storage mismatch**: If `getStorage[T]` returns the wrong type, it's because `name()` registered a different type than expected. The `getStorageByCompId` function panics on type assertion failure.

---

## 13. Architecture Diagram

```
ecs.go (go:generate directive)
    |
    v
internal/gen/main.go
    |-- embed: view.tgo
    |-- Views: [][]string (12 entries: A, AB, ABC, ...)
    |-- funcMap: join, lower, nils, retlist, lambdaArgs, ...
    |-- template.Execute()
    |-- go/format.Source()
    |
    v
view_gen.go (~6500 lines)
    |-- View1[A] ... View12[A..L]
    |-- Query1[A] ... Query12[A..L]
    |-- Read, Count, MapId, MapIdParallel, MapSlices for each

Used by:
    inject.go (System1-3, GetInjectable)
    component.go (ecs.C, ecs.Comp, W, Writer, Component)
    name.go (name, nameTyped, registration)
    storage.go (componentStorage[T], archEngine)
    world.go (World, Spawn, Write, Delete)
```

---

## 14. Quick Reference: Template Variables

| Variable | Scope | Meaning |
|----------|-------|---------|
| `.Views` | Top-level data | `[][]string` of component letter lists |
| `$element` | Outer range | Current component list, e.g., `["A","B"]` |
| `$i` | Outer range index | 0-based index into Views |
| `$arg` | Inner range | Individual component letter, e.g., `"A"` |
| `$ii` | Inner range index | 0-based index into $element |

Common expressions:
```
{{len $element}}           -> 2
{{join $element ","}}     -> "A,B"
{{join $element ",*"}}    -> "A,*B"
{{nils (len $element)}}   -> "nil, nil"
{{retlist $element}}      -> "retA, retB"
{{lambdaArgs $element}}   -> "a *A, b *B"
{{lower $arg}}             -> "a" (when $arg is "A")
```
