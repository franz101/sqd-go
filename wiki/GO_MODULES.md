# How Go modules work in sqd-go

This page is for people who know a little Go but find modules fuzzy. It explains
the one rule that, when misunderstood, silently broke the demo notebook: **your
indexer project is its own Go module, and it must import sqd-go through the
public `sqd` facade — never through `internal/`.**

## A 60-second refresher on Go modules

A **module** is a collection of Go packages versioned together. Each module has a
`go.mod` file at its root. The first line declares the module's **module path**:

```go
module github.com/franz101/sqd-go

go 1.26.4
```

The module path is the import prefix for every package inside that module. The
rule you need to remember:

> **import path = module path + the package's directory relative to the module root**

So in this repo, the directory `sqd/` (which holds `package sqd`) is imported as
`github.com/franz101/sqd-go/sqd`, and `abiunpack/` is
`github.com/franz101/sqd-go/abiunpack`. Nothing more magic than string
concatenation.

## sqd-go is exactly one module

There is a single `go.mod` at the repo root declaring
`github.com/franz101/sqd-go` (`go.mod:1`) with a `go 1.26.4` directive
(`go.mod:3`). There is **no `go.work`** file anywhere in the tree — sqd-go is not
a multi-module workspace. If you ever see module-resolution behavior that looks
like a workspace is in play, it isn't; check your own project's `go.mod` instead.

## The key idea: your project is its OWN module

When you build an indexer, your project directory is a **separate module** from
sqd-go. Your generated code and your hand-written `custom_processor.go` compile
against sqd-go's **public packages**:

- `github.com/franz101/sqd-go/sqd` — the registration/runtime facade
- `github.com/franz101/sqd-go/abiunpack` — zero-reflection ABI decode helpers
- `github.com/franz101/sqd-go/coldcache` — the cold-tier store

They must **never** import sqd-go's `internal/` packages (`internal/ingestion`,
`internal/database`, `internal/cli`, …). This is not a style preference — Go
*forbids* importing another module's `internal/` tree. The compiler will reject
it.

The `sqd` package documents exactly why it exists (`sqd/sqd.go:1-15`):

```go
// Package sqd is the public API surface that sqd-go projects compile against.
//
// A project's generated package and its hand-written custom_processor.go import
// this package instead of the module's internal/ packages. That indirection is
// what lets a project be built as its own standalone Go module — e.g. a notebook
// with only custom_schema.go + custom_processor.go and a scaffolded go.mod that
// requires github.com/franz101/sqd-go — without having to live inside a sqd-go
// checkout. Go forbids importing another module's internal/ packages, so the
// generated code could never compile out-of-module while it referenced
// internal/cli, internal/ingestion or internal/database directly.
package sqd
```

This indirection is the whole reason a notebook holding only `custom_schema.go` +
`custom_processor.go` can compile against sqd-go pulled straight from the module
proxy. The project never touches `internal/`, so it builds fine outside any
sqd-go checkout.

## The `generated` import is module-relative

Codegen writes your event structs and parser into a `generated/` subdirectory of
your project. Your `custom_processor.go` imports it like this:

```go
import (
    generated "myidx/generated"
)
```

Apply the import-path rule: if your project's module path is `myidx`, then the
`generated/` directory resolves to `myidx/generated`. In general:

> the generated import path = **`<your module path>` + `/generated`**

So if your `go.mod` says `module github.com/me/myidx`, the import becomes
`github.com/me/myidx/generated`. The string after `module` and the prefix of the
`generated` import **must agree**, or the build can't find the package (see the
pitfall about `generatedImportBase` below).

## What `sqd-go start <proj> --state` does mechanically

`--state` is the **only** code path that scaffolds a `go.mod` for you. Without it,
a prebuilt `sqd-go` binary has an empty processor registry (see pitfall 3), so a
stateful project runs as a no-op. `--state` collapses the old "generate twice"
workaround into a single command.

For a clean/standalone project (no enclosing sqd-go module), the steps in
`internal/cli/run_state.go` are:

1. **Preflight** — require the Go toolchain on `PATH` (`run_state.go:67`) and at
   least one `.go` file in the project root (`run_state.go:72`). Without a custom
   processor there's no stateful entity to run.
2. **Scaffold `go.mod`** (`writeStandaloneGoMod`, `run_state.go:290-298`):

   ```go
   module myidx

   go 1.25

   require github.com/franz101/sqd-go v0.0.0
   ```

   The module path is your project's import base; the `go` directive is `1.25`
   (`sqdGoDirective`, `run_state.go:31`); the required sqd-go version is the one
   the running binary was built from (`sqdModuleVersion`). If `SQD_GO_REPLACE` is
   set, a `replace` directive is appended (see below).
3. **Force module mode** — runs the toolchain with `GOWORK=off` and
   `GOFLAGS=-mod=mod` (`run_state.go:170,179`) so a stray workspace or vendoring
   can't interfere.
4. **`go get github.com/franz101/sqd-go@<ver>`** (`run_state.go:173`) to resolve
   the dependency.
5. **Run codegen** to (re)write the `generated/` package.
6. **`go mod tidy`** to settle the module graph.
7. **Build a tiny runner main** that blank-imports your project package — so its
   `init()` runs `sqd.RegisterProcessor` — and calls `sqd.Run`
   (`runnerMainSource`, `run_state.go:455-471`):

   ```go
   // Code generated by sqd-go `--state`; DO NOT EDIT.
   package main

   import (
       "os"

       _ "myidx"

       "github.com/franz101/sqd-go/sqd"
   )

   func main() { os.Exit(sqd.Run(os.Args[1:])) }
   ```

8. **`go build`** the runner, then **exec** it as a normal `start` run
   (`run_state.go:228,247`).

The blank import (`_ "myidx"`) is load-bearing: it's what pulls your `init()`
into the binary so the processor actually registers.

## The module path must match your `generated` import prefix

`generatedImportBase` (`run_state.go:327-353`) scans your root `.go` files for an
import ending in `/generated` and uses the prefix as the scaffolded module path.
If your **package name** and your **import base** disagree, the build fails with:

```
could not find the project's generated package import in <dir>
  a stateful project's custom_processor.go must import its generated package, e.g.
    generated "myproject/generated"
  with `myproject` matching the project's module/package name
```

Keep them consistent: package `myproject` → import `myproject/generated` →
module `myproject`.

## `SQD_GO_REPLACE` — build against a local sqd-go checkout

For development against an unreleased sqd-go, point the scaffolded `go.mod`'s
require at a local checkout (`run_state.go:142-147,294-296`):

```bash
export SQD_GO_REPLACE=/home/you/CODING/sqd-go
sqd-go start examples/myidx --state
```

This appends a `replace` directive to the generated `go.mod`:

```go
replace github.com/franz101/sqd-go => /home/you/CODING/sqd-go
```

Without it, `--state` requires the published version from `GOPROXY`; if that
can't be resolved you'll be told to set `SQD_GO_REPLACE` to a local checkout.

## Common module pitfalls

1. **Two package names in one directory.** `custom_schema.go` and
   `custom_processor.go` live in the same directory, so they **must share one
   package name**. Mismatched names produce:

   ```
   found packages X (custom_schema.go) and Y (custom_processor.go) in <dir>
   ```

2. **Importing `internal/` from your project.** Reaching into
   `github.com/franz101/sqd-go/internal/...` from another module is rejected by
   the compiler:

   ```
   use of internal package github.com/franz101/sqd-go/internal/... not allowed
   ```

   Use the public `sqd`, `abiunpack`, and `coldcache` packages instead.

3. **A prebuilt binary has an empty processor registry.** A `go install
   github.com/franz101/sqd-go@latest` binary contains **no** project package, so
   its processor registry is empty. Running plain `sqd-go start` on a *stateful*
   project then executes a no-op processor — it looks like it's running but does
   nothing. Fix: use `--state` (which rebuilds the binary with your processor
   compiled in), or build from a sqd-go checkout.

## The canonical proven layout

`internal/codegen/standalone_e2e_test.go` is the reference. It builds a directory
holding only `config.yaml` + `custom_schema.go` + `custom_processor.go` (no
`go.mod`, not inside a sqd-go checkout) into a self-contained module and compiles
it end to end. Notable details that match everything above:

- both custom files use `package standaloneproj`;
- `custom_processor.go` imports `generated "standaloneproj/generated"`;
- the scaffolded `go.mod` is `module standaloneproj` requiring sqd-go;
- `TestGeneratedCodeImportsNoInternalPackages` asserts the generated code never
  imports an `internal/` package — the exact regression that broke the notebook.

If your project mirrors that layout, `--state` will build and run it.
