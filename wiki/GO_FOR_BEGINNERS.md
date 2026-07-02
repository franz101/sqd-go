# Go for sqd-go Beginners

If you're new to Go but want to use sqd-go, this guide explains the Go concepts you'll encounter. No prior Go experience required!

## What is Go?

Go is a programming language created by Google. It's:
- **Compiled** - Go code is compiled to machine code before running
- **Statically typed** - Variable types are checked before the program runs
- **Module-based** - Go projects are organized as modules with dependencies

## Key Go Concepts for sqd-go

### 1. Packages (`package`)

Every Go file belongs to a **package**. Think of packages as folders that group related code together.

```go
package main       // The main package - where your program starts
package generated  // Generated code package
package myproject  // Your custom project package
```

**Important rule**: All `.go` files in the same directory must have the same package name!

**Why this matters**: In sqd-go, `custom_schema.go` and `custom_processor.go` live in the same directory, so they MUST share the same package name. If they don't, you'll get an error like `found packages X and Y in <dir>`.

### 2. Imports (`import`)

Go uses `import` to include code from other packages:

```go
import (
    "fmt"                    // Standard library package
    "os"                     // Standard library package
    "github.com/ethereum/go-ethereum/common"  // External package
)
```

**How import paths work**:
- Standard library packages (like `fmt`, `os`) are single names
- External packages use URLs like `github.com/username/project`
- Your own packages use your module path + directory name

**Example**: If your `go.mod` says `module myproject`, then:
- Your main package: `myproject`
- Your generated code: `myproject/generated`
- Import it as: `import "myproject/generated"`

### 3. Go Modules (`go.mod`)

A **module** is a collection of Go packages that are versioned together. Every Go project has a `go.mod` file at its root.

```go
// go.mod file
module myproject

go 1.26

require github.com/franz101/sqd-go v0.0.0
```

**What this means**:
- `module myproject` - Your project's name (used in imports)
- `go 1.26` - The Go version required
- `require` - Other modules your project depends on

### 4. The `init()` Function

Go has a special function called `init()` that runs automatically before the main program starts:

```go
func init() {
    // This runs automatically when the program starts
    sqd.RegisterProcessor("myproject", myProcessorFactory)
}
```

**Why sqd-go uses `init()`**: It registers your custom processor before the indexer starts running, without you needing to call it manually.

### 5. Types and Structs

Go uses **structs** to group related data together:

```go
type UserPosition struct {
    Address        common.Address  // Ethereum address
    Balance        uint256.Int     // Large number (for token amounts)
    TransferCount  uint64          // Regular number
}
```

**Common Go types in sqd-go**:
- `uint64` - Unsigned 64-bit integer (block numbers, counts)
- `uint256.Int` - Very large numbers (token amounts)
- `string` - Text
- `bool` - True/false
- `time.Time` - Date and time
- `[]T` - Slice (array) of type T

### 6. Pointers (`*T`)

Go uses pointers to reference memory locations:

```go
var pos *UserPosition              // Pointer to UserPosition
pos = &UserPosition{Address: addr}  // Get address of struct
pos.Balance.Add(...)                // Access fields through pointer
```

**When you'll see pointers**:
- Function return types: `func Get() (*UserPosition, bool)`
- State entities are always pointers
- Don't worry - you mostly use them as-is

### 7. Error Handling

Go uses explicit error values instead of exceptions:

```go
result, err := someFunction()
if err != nil {
    return err  // Return the error to caller
}
// Use result
```

**In sqd-go processors**:
```go
func Process(state *generated.State, block *generated.ParsedBlock) error {
    // Your processing logic
    if somethingBad {
        return fmt.Errorf("something went wrong: %v", problem)
    }
    return nil  // Success! Return nil for no error
}
```

### 8. Interfaces

An **interface** defines a set of methods that a type must implement:

```go
type Event interface {
    Meta() EventMeta  // Any type with this method satisfies the interface
}
```

**Why this matters**: Your custom processor works with any event type because they all implement the `Event` interface.

### 9. Methods and Receivers

Go can define functions on types (called methods):

```go
func (e EventMeta) Meta() EventMeta {
    return e  // Method on EventMeta type
}

func (s *entityStateHandle[T]) Get(key T) (*T, bool) {
    // Method on entityStateHandle with pointer receiver
    // ...
}
```

**How to read this**:
- `func (e EventMeta)` - Method that operates on EventMeta (value receiver)
- `func (s *entityStateHandle[T])` - Method that operates on a pointer (pointer receiver)
- You don't need to define these - they're generated for you

### 10. Type Assertions and Switches

Go can check the concrete type of an interface value:

```go
var event Event = &LBTCTransfer{...}

// Type assertion
transfer, ok := event.(*LBTCTransfer)
if ok {
    // Use transfer.From, transfer.To, transfer.Value
}

// Type switch (used in sqd-go processors)
switch e := ev.(type) {
case *generated.LBTCTransfer:
    // Handle Transfer event
case *generated.ERC20Approval:
    // Handle Approval event
default:
    // Unknown event type
}
```

### 11. The `context` Package

Go uses `context.Context` to manage deadlines and cancellation:

```go
import "context"

func Process(ctx context.Context, store Store, entities *Entities) error {
    // ctx carries deadlines and cancellation signals
    // Most of the time you don't need to use it directly
    return nil
}
```

### 12. Goroutines and Channels (Advanced)

Go has lightweight threads called goroutines and channels for communication:

```go
// Start a goroutine
go func() {
    // Runs concurrently
}()

// Send/receive on channels
ch := make(chan int)
ch <- 42        // Send
value := <-ch   // Receive
```

**Note**: You typically won't write goroutines in sqd-go processors, but they're used internally.

## Common Go Patterns in sqd-go

### Pattern 1: Multiple Return Values

Go functions can return multiple values:

```go
pos, ok := state.UserPositions.Get(address)
// pos = *UserPosition or nil
// ok = true if found, false if not
```

### Pattern 2: Deferred Cleanup

`defer` runs a function when the current function exits:

```go
func process() error {
    file, err := os.Open("data.txt")
    if err != nil {
        return err
    }
    defer file.Close()  // Will run when process() returns
    
    // Use file...
}
```

### Pattern 3: Blank Identifier (`_`)

Ignore values you don't need:

```go
import _ "myproject"  // Import for side effects (init() functions)

_, err := someFunction()  // Ignore first return value
```

## sqd-go Specific Gotchas

### 1. Package Name Mismatch

❌ **Wrong** - Different package names in same directory:
```go
// custom_schema.go
package myproject

// custom_processor.go  
package MyProject  // Different! This will fail
```

✅ **Correct** - Same package name:
```go
// Both files
package myproject
```

### 2. Import Path Mistakes

❌ **Wrong** - Importing internal packages:
```go
import "github.com/franz101/sqd-go/internal/database"  // Forbidden!
```

✅ **Correct** - Use public packages:
```go
import "github.com/franz101/sqd-go/sqd"  // Public API
```

### 3. Module Path Mismatch

If your `go.mod` says `module myproject`, then:
- ✅ `import "myproject/generated"`
- ❌ `import "generated"` (missing module prefix)
- ❌ `import "otherproject/generated"` (wrong module name)

### 4. Forgetting Error Returns

❌ **Wrong** - Ignoring errors:
```go
result, _ := someFunction()  // If this fails, you won't know!
```

✅ **Correct** - Always handle errors:
```go
result, err := someFunction()
if err != nil {
    return err  // Or log it, or handle it
}
```

## Debugging Tips for Go Beginners

### 1. Use fmt.Println for debugging
```go
import "fmt"

fmt.Printf("Processing event %+v\n", event)
fmt.Printf("State: %+v\n", pos)
```

### 2. Read compiler errors carefully
Go errors are detailed. They tell you exactly what's wrong:
- `found packages X and Y` → Package name mismatch
- `cannot use ... as type ...` → Type mismatch
- `undefined: ...` → Misspelled or missing import

### 3. Use `go vet` for additional checks
```bash
go vet ./...
```

### 4. Format your code with `go fmt`
```bash
go fmt ./...
```

## Next Steps

Now that you understand the Go basics, explore:
- **[GO_MODULES.md](GO_MODULES.md)** - How Go modules work in sqd-go
- **[EVENT_FIELDS.md](EVENT_FIELDS.md)** - Standard fields on every event
- **[METRICS.md](METRICS.md)** - Monitoring and tuning your indexer

## Common Error Messages Explained

| Error | Meaning | Fix |
|-------|---------|-----|
| `found packages X and Y` | Different package names in same directory | Use same package name in all files |
| `use of internal package not allowed` | Importing from `internal/` | Import public `sqd` package instead |
| `undefined: generated.Something` | Type doesn't exist in generated code | Check event name in `config.yaml` and re-run codegen |
| `cannot import "internal/..."` | Trying to import internal package | Use public API: `github.com/franz101/sqd-go/sqd` |
| `expected package ..., found ...` | Package name doesn't match directory | Rename package to match expectations |

Remember: Go is designed to be simple and explicit. Once you understand these basics, sqd-go development becomes much easier!