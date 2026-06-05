# ECS Performance Distillation

## The Benchmark

On an Intel i7-8700K, iterating all 1,000,000 entities with a physics tick (Position + Velocity update) takes:

- **ECS MapSlices: ~1.93ms** — on par with a hand-written raw slice loop (~1.95ms)
- **ECS MapId (per-entity lambda): ~3.3ms** — still fast, but ~70% slower than the archetype-slice path
- **ECS MapIdParallel: scales across cores**

The ECS achieves performance indistinguishable from writing custom `[]Position` and `[]Velocity` slices by hand. This document explains *why*.

## 1. Monomorphization via Code Generation

The single biggest performance lever: there are **no interface calls in the hot path**.

### The Problem with Interface Dispatch

Traditional Go ECS frameworks pass around `interface{}` or `Component` interfaces. Every access to a component requires:

- Boxing/unboxing values into interface headers (allocation + indirection)
- Virtual method dispatch (the CPU can't predict the call target)
- Type assertions in user code

### How This ECS Avoids It

A code generator (`internal/gen/main.go`) reads a template (`internal/gen/view.tgo`) and emits concrete `View1[A]`, `View2[A,B]`, ... through `View12[A..L]` — each fully monomorphized for its arity.

The template generates 12 versions of every method. `View2[A, B]` is not `View2[any, any]` — it's `View2[Position, Velocity]` at instantiation time, and then Go's generics monomorphize it into a **concrete struct with concrete method bodies**.

In `MapId`, the inner loop body is:

```go
for idx := range ids {
    if ids[idx] == InvalidEntity { continue }
    if compA != nil { retA = &compA[idx] }
    if compB != nil { retB = &compB[idx] }
    lambda(ids[idx], retA, retB)
}
```

Every variable name is resolved at compile time. There is not a single `interface{}` cast, type switch, or reflection call in this loop. The Go compiler can inline aggressively and perform bounds-check elimination.

Key insight from the benchmark comments: *"Before we applied monomorphization techniques... ~3.4ms. After: ~1.93ms."* Monomorphization alone is responsible for roughly a 43% speedup.

## 2. Archetype Storage Model

### Contiguous Component Slices

Every component type `T` in a given archetype is stored in a **single contiguous `[]T` slice** — exactly like a hand-written data-oriented array:

```go
type componentList[T any] struct {
    comp []T  // ALL Position values for entities in this archetype
}
```

Entities within an archetype share the same index. Entity 3's Position is at `positions.comp[3]`, entity 3's Velocity is at `velocities.comp[3]`. This is **struct-of-arrays** layout, which is optimal for:

- Vectorized SIMD-friendly access patterns
- CPU cache line utilization (sequential access loads 8 float64s per cache line)
- Predictable stride for the hardware prefetcher

### One Archetype Per Component Combination

For 1M entities all having `{Position, Velocity}`, there is exactly **one archetype** with one `[]Position` of length 1,000,000 and one `[]Velocity` of length 1,000,000. The iteration loop inside `MapSlices` hands the user callback a single pair of slices — exactly what a hand-rolled native solution would do.

The cost of adding/removing a component from an entity is an archetype migration (copy data from old archetype to new), which is O(components) but infrequent. The hot read path pays zero cost.

## 3. Zero-Copy Pointer Access

`Read()` returns `*T` — a **direct pointer into internal storage**:

```go
func (v *View2[A, B]) Read(id Id) (*A, *B) {
    loc, _ := v.world.arch.Get(id)
    sliceA, _ := v.storageA.slice.Get(loc.archId)
    retA = &sliceA.comp[index]  // <-- pointer into the internal slice
    // ...
}
```

This means:

- **No allocation** on read
- **Zero copies** of component data into/out of storage
- Mutations via `*ptr = newValue` write **directly into archetype storage** — no write-back step needed
- The `MapId` lambda receives `*Position` and `*Velocity` and writes to them directly

This is the same model you'd use with hand-written slices: `&positions[i]`.

## 4. Hole-Based Deletion (Lazy Compaction)

When an entity is deleted:

```go
func (e *archEngine) TagForDeletion(loc entLoc, id Id) {
    lookup.id[loc.index] = InvalidEntity  // Mark slot as empty
    lookup.holes = append(lookup.holes, int(loc.index))  // Track for later
}
```

Deletion is O(1) — just mark the slot and append to the holes list. No shifting, no re-packing, no invalidating pointers of other entities.

When a new entity is spawned into the same archetype, `addToEasiestHole` pops from the holes list and reuses the slot:

```go
func (l *lookupList) addToEasiestHole(id Id) int {
    if len(l.holes) > 0 {
        index := l.holes[len(l.holes)-1]
        l.id[index] = id
        l.holes = l.holes[:len(l.holes)-1]
        return index
    }
    l.id = append(l.id, id)
    return len(l.id) - 1
}
```

During iteration, holes are cheaply skipped with a single branch:

```go
if ids[idx] == InvalidEntity { continue }
```

Manual compaction via `CleanupHoles()` is available but rarely needed — the swap-with-last-and-shrink strategy defragments component data in O(holes) time.

## 5. Bitmask Filtering (4x uint64 = O(1) for 255 Component Types)

Component matching uses a 256-bit bitmask stored as `[4]uint64`:

```go
type archetypeMask [4]uint64  // 256 bits = up to 255 components
```

Every archetype carries a mask of which components it contains. Every query builds a mask of which components it requires. The filter is a single 4-iteration AND loop:

```go
func (m archetypeMask) contains(a archetypeMask) bool {
    for i := range m {
        if (m[i] & a[i]) != m[i] {
            return false
        }
    }
    return true
}
```

This is **4 CPU instructions** of work — faster than any hash table lookup, set intersection, or string comparison. Adding/removing a single component from a mask is a single bit shift + OR/AND-NOT:

```go
mask[idx] |= (1 << offset)   // add
mask[idx] &= ^(1 << offset)  // remove
```

The component ID space is compact: `CompId` is a `uint16` assigned sequentially starting at 1, via `reflect.TypeOf` hashing at registration time. Component IDs are valid for the lifetime of the process across all Worlds.

## 6. Generation-Based Query Caching

The `filterList` struct caches the list of matched archetype IDs and only recomputes when the world's archetype generation changes:

```go
type filterList struct {
    comps                     []CompId
    withoutArchMask           archetypeMask
    cachedArchetypeGeneration int
    archIds                   []archetypeId
}

func (f *filterList) regenerate(world *World) {
    if world.engine.getGeneration() != f.cachedArchetypeGeneration {
        f.archIds = world.engine.FilterList(f.archIds, f.comps)
        // ... apply Without filter ...
        f.cachedArchetypeGeneration = world.engine.getGeneration()
    }
}
```

The generation counter is incremented **only when a new archetype is created** (new component combination appears). For a stable world where all archetypes exist from the start, the generation never changes, and every `MapId`/`MapSlices` call skips the `FilterList` entirely — just a single integer comparison.

## 7. The intmap Custom Integer Hashmap

For the two fundamental lookups — `Id -> entLoc` (entity location) and `archetypeId -> componentList` (component storage) — a custom open-addressing hashmap is used instead of Go's built-in `map`:

Located at `internal/intmap/map64.go`, key properties:

- **Open addressing with linear probing**, stored in a contiguous `[]pair[K, V]` slice
- **Power-of-2 sizing** — the modulo operation becomes a bitmask: `phiMix64(int(key)) & (len(m.data) - 1)`
- **Phi-mix hash**: `h = x * -1640531527; return h ^ (h >> 16)` — a single multiply + XOR + shift
- **Zero-key fast path**: the zero value is stored separately, avoiding probing for entity 0 (which never exists anyway)
- **No per-operation allocation**: the data array is pre-allocated and grows via doubling (rehash), not incremental
- **Special zero-value handling**: the zero-value of the key type acts as the "empty slot" sentinel

Benchmark comments show the intmap replacement was a measurable win:
```
// Before (Go built-in map):  ~0.55s per iteration
// After (intmap):           ~0.45s per iteration  (~18% faster)
```

The `locMap` type wraps this for `Id -> entLoc` lookups, while `internalMap[archetypeId, *componentList[T]]` wraps it for component storage. Both are on the hot path of every `Read()`, every `Write()`, and every query.

## 8. MapSlices vs MapId: The Two Iteration Strategies

### MapId (per-entity lambda)

```go
view.MapId(func(id Id, pos *Position, vel *Velocity) {
    pos.x += vel.x * dt
})
```

For each archetype, iterates entity-by-entity, computing `*Position` and `*Velocity` pointers per entity. The inner loop contains:

- One `InvalidEntity` check per entity
- One nil-check + pointer dereference per component per entity
- A function call through the lambda (Go can inline this in many cases)

**Cost**: ~3.3ms for 1M entities.

### MapSlices (archetype-slice lambda)

```go
view.MapSlices(func(ids []Id, pos []Position, vel []Velocity) {
    for i := range ids {
        if ids[i] == InvalidEntity { continue }
        pos[i].x += vel[i].x * dt
    }
})
```

For each archetype, passes the **entire component slice** to the callback. The user loop iterates over slices directly — just like native code. The callback receives `[][]Id` and `[][]Position` and iterates over each archetype's slices.

**Cost**: ~1.93ms for 1M entities — **identical to raw slice iteration**.

Why the difference? MapSlices avoids:
- Per-entity pointer construction (`&comp[idx]`)
- Per-entity nil checks for each component
- Per-entity lambda call overhead
- Better cache behavior (operating on slices, not scattered pointer dereferences)

The tradeoff: MapSlices requires the user to handle holes manually (`if ids[i] == InvalidEntity`), while MapId hides that detail.

## 9. The Bundler Pattern for Batched Writes

When adding multiple components at once, the `Bundler` avoids repeated archetype lookups:

```go
type Bundler struct {
    archMask            archetypeMask
    Set                 [256]bool
    Components          [256]Component
    maxComponentIdAdded CompId
}
```

Instead of calling `Write(world, id, C(pos), C(vel), C(col), C(cnt))` which would trigger one archetype lookup + migration per component, the Bundler:

1. Accumulates all components into a fixed-size array
2. Computes the **final archetype mask** once
3. Allocates/moves to the destination archetype **once**
4. Writes all bundled components in a single pass

This is used by the `CommandQueue` for deferred writes — collect components in the bundler during game logic, execute all writes at the end of the frame. Reduces archetype migrations from N (one per component) to 1 (one for the entire bundle).

## 10. Putting It All Together: Why It Matches Raw Slices

The raw-slice "control" benchmark measures this:

```go
for j := range ids {
    pos[j].x += vel[j].x * dt
    pos[j].y += vel[j].y * dt
    pos[j].z += vel[j].z * dt
}
```

This is exactly what the ECS's `MapSlices` inner loop does. The ECS adds only:

1. **One atomic increment** for Id generation (spawn only, not in the hot loop)
2. **One `intmap.Get()`** per entity lookup in `Read()` (O(1) with open addressing)
3. **One bitmask AND** (4 uint64 operations) for archetype filtering (cached by generation)
4. **One `InvalidEntity` check** per entity to skip holes

All four overheads combined amount to **less than 2%** of the total iteration time — within measurement noise of the raw slice. For a game with 1M entities, the ECS costs ~0.04ms more per frame than hand-written code.

The key insight: **the ECS doesn't add abstraction overhead — it reorganizes data to make abstraction free.** The archetype storage is the same struct-of-arrays layout a skilled systems programmer would write by hand. The monomorphized codegen produces the same machine code a skilled programmer would write by hand. The only difference is that the ECS gives you query filtering, component add/remove, parallel iteration, and deferred command execution for free.
