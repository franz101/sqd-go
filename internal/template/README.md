# Template Usage Guide

## Overview

This package provides compile-time embedded templates for SQL DDL/DML generation and code scaffolding. Templates are parsed at package initialization and cached for efficient execution.

## Directory Structure

```
internal/template/
├── loader.go          # Template loader and execution functions
├── parity_test.go    # Parity tests comparing templates to old fmt.Sprintf
├── templates/
│   ├── sql/          # ClickHouse SQL templates
│   │   └── clickhouse.go.tmpl
│   └── code/         # Code scaffolding templates
│       ├── gomod.go.tmpl
│       ├── env.go.tmpl
│       └── graphql.go.tmpl
```

## Compile-Time Embedding

Templates are embedded using Go's `//go:embed` directive in `loader.go`:

```go
//go:embed templates/sql/*.tmpl templates/code/*.tmpl
var templateFS embed.FS
```

This means:
- **Templates are compiled into the binary** - no external files needed at runtime
- **Templates are parsed once at package init** - zero runtime overhead for template loading
- **No environment variables needed** - templates are part of the compiled code
- **Distribution is simple** - single binary contains everything

## Available Templates

### SQL Templates (`sql/` prefix)

- `createBlocksTable` - CREATE TABLE for blocks storage
- `createLogsTable` - CREATE TABLE for raw logs storage
- `createSyncStateTable` - CREATE TABLE for sync state tracking
- `insertBlocks` - INSERT statement for blocks
- `insertLogs` - INSERT statement for logs
- `insertSyncState` - INSERT statement for sync state
- `deleteWhereChain` - DELETE with chain_id filter
- `deleteWhereNoChain` - DELETE without chain_id filter
- `insertSelectFinal` - Complex INSERT with SELECT for CollapsingMergeTree sign flip
- `tablesWithBlockNumber` - System query to discover tables with block_number column
- `tableColumns` - System query to list table columns
- And more...

### Code Templates (`code/` prefix)

- `goMod` - go.mod file generation
- `envFile` - .env file generation
- `graphqlSchema` - GraphQL schema stub

## Usage Examples

### Basic Usage

```go
import "github.com/franz101/sqd-go/internal/template"

// Execute a template
sql := template.MustExecute("sql/insertBlocks", struct {
    DatabaseIdent string
}{
    DatabaseIdent: "`my_database`",  // Pre-quoted identifier
})

// Result:
// INSERT INTO `my_database`.blocks (chain_id, block_number, block_timestamp, block_hash) VALUES
```

### Conditional Fields

```go
// Create blocks table with optional sign column (CollapsingMergeTree)
sql := template.MustExecute("sql/createBlocksTable", struct {
    DatabaseIdent string
    Engine        string
    Collapsing    bool
}{
    DatabaseIdent: "`my_db`",
    Engine:        "CollapsingMergeTree(sign)",
    Collapsing:    true,  // Will include "sign Int8 DEFAULT 1," column
})
```

### Code Scaffolding

```go
// Generate go.mod file
goMod := template.MustExecute("code/goMod", struct {
    ModulePath    string
    GoVersion     string
    SQDModulePath string
    SQDVersion    string
    SQDReplace    string  // Empty = no replace directive
}{
    ModulePath:    "github.com/example/my-indexer",
    GoVersion:     "1.21",
    SQDModulePath: "github.com/franz101/sqd-go",
    SQDVersion:    "v1.2.3",
    SQDReplace:    "",  // or "/path/to/local/sqd-go"
})
```

### Error Handling

```go
// Use Execute() for error return instead of panic
result, err := template.Execute("sql/insertBlocks", data)
if err != nil {
    if template.IsNotFound(err) {
        // Handle missing template
    }
    // Handle other errors
}
```

## Data Structure Requirements

All identifiers must be **pre-quoted** before passing to templates:

```go
// GOOD - pre-quoted identifier
data := struct {
    DatabaseIdent string
}{
    DatabaseIdent: quoteIdent("my_db"),  // "`my_db`"
}

// BAD - raw identifier (SQL injection risk)
data := struct {
    DatabaseIdent string
}{
    DatabaseIdent: "my_db",  // Don't do this!
}
```

## Template Naming Convention

Templates are referenced by their category and name:
- Format: `{category}/{templateName}`
- SQL templates: `sql/{templateName}`
- Code templates: `code/{templateName}`

## List Available Templates

```go
names := template.List()
// Returns: ["sql/createBlocksTable", "sql/insertBlocks", "code/goMod", ...]
```

## Performance Considerations

1. **Templates are parsed once** at package initialization
2. **No runtime file I/O** - all templates are embedded
3. **Buffer reuse** - `strings.Builder` used internally
4. **Pre-join lists** - For simple column lists, pre-join in Go code before template execution

## Migration from fmt.Sprintf

### Before (fmt.Sprintf):
```go
signColumn := ""
if collapsing {
    signColumn = "sign Int8 DEFAULT 1,"
}
sql := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.blocks (
    chain_id UInt64, block_number UInt64,
    block_timestamp DateTime64(3, 'UTC'), block_hash String,
    %s
    inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = %s ORDER BY (chain_id, block_number)`, db, signColumn, engine)
```

### After (template):
```go
sql := template.MustExecute("sql/createBlocksTable", struct {
    DatabaseIdent string
    Engine        string
    Collapsing    bool
}{
    DatabaseIdent: db,
    Engine:        engine,
    Collapsing:    collapsing,
})
```

## Testing

Run parity tests to verify templates match old fmt.Sprintf output:

```bash
go test -v ./internal/template/...
```

These tests compare template output against the original fmt.Sprintf implementation to ensure identical behavior.
