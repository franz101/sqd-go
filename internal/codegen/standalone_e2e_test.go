package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// statefulConfigYAML is an ERC20-style config that, paired with the
// custom_schema.go below, drives the full stateful codegen path: a hot-state
// entity (→ coldcache), a custom Processor (→ sqd.Store / sqd.CustomLog) and
// topic/data decoding (→ abiunpack). It therefore exercises every sqd-go
// package the generated code imports.
const statefulConfigYAML = `name: standaloneproj
ecosystem: evm
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: ERC20
        address: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
          - event: Approval(address indexed owner, address indexed spender, uint256 value)
`

const statefulSchemaGo = `package standaloneproj

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// pk: Address
type UserPositionSchema struct {
	Address        common.Address
	Balance        uint256.Int
	TransferCount  uint64
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}
`

// statefulProcessorGo is the hand-written root package: exactly what a notebook
// user has on disk (alongside custom_schema.go) with no go.mod. It imports the
// public sqd facade and the project's own generated package.
const statefulProcessorGo = `package standaloneproj

import (
	generated "standaloneproj/generated"

	"github.com/franz101/sqd-go/sqd"
)

func Process(state *generated.State, block *generated.ParsedBlock) error {
	for ev := range block.EventsIter() {
		if e, ok := ev.(*generated.ERC20Transfer); ok {
			pos, found := state.UserPosition.Get(e.To)
			if !found {
				pos = &generated.UserPosition{Address: e.To}
			}
			pos.Balance.Add(&pos.Balance, &e.Value)
			pos.TransferCount++
			pos.UpdatedAtBlock = e.EventMeta.BlockNumber
			state.UserPosition.Save(pos, e.EventMeta)
		}
	}
	return nil
}

func init() {
	generated.CustomProcessFn = Process
	sqd.RegisterProcessor(generated.ProjectName, func() (sqd.Processor, error) {
		return generated.NewProcessor(sqd.GetProtoMode())
	})
}
`

// repoRoot returns the sqd-go module root (this file is at
// <root>/internal/codegen/standalone_e2e_test.go).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !strings.Contains(string(data), "module github.com/franz101/sqd-go") {
		t.Fatalf("could not locate sqd-go module root at %s", root)
	}
	return root
}

// TestGeneratedCodeImportsNoInternalPackages is the fast regression guard for
// the notebook "--state: no go.mod found" / "use of internal package not
// allowed" failure: a project compiled as its own module (outside a sqd-go
// checkout) can only import non-internal packages. Before the public-API change
// the generated code imported internal/parser/abiunpack, internal/coldcache,
// internal/database and internal/ingestion, so it could never build standalone.
func TestGeneratedCodeImportsNoInternalPackages(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(statefulConfigYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "custom_schema.go"), []byte(statefulSchemaGo), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(root); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	genDir := filepath.Join(root, "generated")
	entries, err := os.ReadDir(genDir)
	if err != nil {
		t.Fatal(err)
	}
	const internalPrefix = "github.com/franz101/sqd-go/internal/"
	var sawSQDImport bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src := readText(t, filepath.Join(genDir, e.Name()))
		if strings.Contains(src, internalPrefix) {
			t.Errorf("generated %s imports an internal package (%s...) — cannot build as a standalone module", e.Name(), internalPrefix)
		}
		if strings.Contains(src, "github.com/franz101/sqd-go/sqd") {
			sawSQDImport = true
		}
	}
	if !sawSQDImport {
		t.Error("expected at least one generated file to import the public github.com/franz101/sqd-go/sqd package")
	}
}

// TestGeneratedProjectBuildsAsStandaloneModule reproduces the full notebook
// scenario end to end: a directory holding only config.yaml + custom_schema.go +
// custom_processor.go (no go.mod, not inside a sqd-go checkout) is turned into a
// self-contained module that requires sqd-go, then codegen + go build are run
// over it. This is the "double compilation" that previously died at the internal
// import wall. Gated behind -short because it shells out to the Go toolchain.
func TestGeneratedProjectBuildsAsStandaloneModule(t *testing.T) {
	if testing.Short() {
		t.Skip("standalone build shells out to the go toolchain; skipped under -short")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	root := repoRoot(t)

	// codegen's easyjson step shells out to `go run` inside the project module;
	// -mod=mod lets it (and the later build) auto-resolve go.sum entries from the
	// warm module cache. Set on the process env so the in-process Generate call's
	// easyjson subprocess inherits it. GOWORK=off ignores any ambient workspace.
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	proj := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// This mirrors the exact ordering --state must perform in a clean env, where
	// codegen's easyjson step shells out to `go run` inside the project module and
	// therefore needs the dependency closure resolved before it runs:
	//
	//  1. write config.yaml + custom_schema.go + a go.mod requiring sqd-go,
	//  2. seed the module graph so easyjson's bootstrap can build (go get + -mod=mod),
	//  3. codegen (now easyjson succeeds),
	//  4. drop in custom_processor.go + a runner main that registers the processor,
	//  5. go mod tidy + go build.
	//
	// The sqd-go require is replaced with this checkout so the test validates the
	// working tree rather than a published version.
	write("config.yaml", statefulConfigYAML)
	write("custom_schema.go", statefulSchemaGo)
	goMod := "module standaloneproj\n\ngo 1.25\n\nrequire github.com/franz101/sqd-go v0.0.0\n\nreplace github.com/franz101/sqd-go => " + filepath.ToSlash(root) + "\n"
	write("go.mod", goMod)

	env := os.Environ()

	if out, err := runGo(goBin, proj, env, "get", "github.com/franz101/sqd-go@v0.0.0"); err != nil {
		t.Fatalf("seeding sqd-go dependency closure failed: %v\n%s", err, out)
	}

	if _, err := Generate(proj); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	write("custom_processor.go", statefulProcessorGo)
	write("sqdrun/main.go", "package main\n\nimport (\n\t\"os\"\n\n\t_ \"standaloneproj\"\n\n\t\"github.com/franz101/sqd-go/sqd\"\n)\n\nfunc main() { os.Exit(sqd.Run(os.Args[1:])) }\n")

	if out, err := runGo(goBin, proj, env, "mod", "tidy"); err != nil {
		t.Fatalf("go mod tidy failed (standalone module): %v\n%s", err, out)
	}
	// The real assertion: the standalone module compiles, including the runner
	// main that blank-imports the project package.
	if out, err := runGo(goBin, proj, env, "build", "./..."); err != nil {
		t.Fatalf("standalone module failed to build (this is the notebook --state bug): %v\n%s", err, out)
	}
}

func runGo(goBin, dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command(goBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
