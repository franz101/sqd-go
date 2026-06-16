package cli

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/franz101/sqd-go/internal/codegen"
	"github.com/franz101/sqd-go/internal/config"
)

// stateChildEnv marks the re-executed child so it runs the ingestion pipeline
// directly instead of rebuilding again (preventing an infinite regen loop).
const stateChildEnv = "SQD_STATE_CHILD"

// runStateRebuild implements `sqd-go start --state <project>`.
//
// The prebuilt `sqd-go` binary (go install …@latest) contains no project
// package, so its processor registry is empty: a custom_schema/state project's
// derived state and its PK-keyed cold tier never engage (processorForProject
// returns nil → a no-op ProcessorFunc). The historical workaround was to
// regenerate the project AND rebuild the binary by hand — the "generate twice".
//
// --state collapses that into one command: it (1) regenerates the project's Go
// package (the codegen Processor with EnableColdCache keyed on each entity PK),
// (2) writes a tiny runner `main` that blank-imports the project package so its
// init() → cli.RegisterProcessor is compiled in, (3) `go build`s that runner, and
// (4) execs it as a normal `start` run. The child's processorForProject now finds
// the registered processor, so custom state and the cold cache work.
//
// Pre-req: the project lives inside the sqd-go module (its scaffolded
// custom_processor.go imports internal/cli, which only resolves in-module) and
// has a Go package at its root (a custom_schema.go / custom_processor.go). An
// events-only contract-import project has neither a stateful entity nor a root
// package, so --state reports that and exits.
func runStateRebuild(args []string, projectPath string) int {
	project, err := config.LoadProject(projectPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	// project.Root may be relative (as typed on the CLI); module resolution and
	// filepath.Rel both need it absolute.
	projRoot, err := filepath.Abs(project.Root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: resolve project path: %v\n", err)
		return 1
	}

	if _, err := exec.LookPath("go"); err != nil {
		fmt.Fprintln(os.Stderr, "--state needs the Go toolchain on PATH to build the project into the binary; install Go or drop --state")
		return 1
	}

	if !projectHasRootGoPackage(projRoot) {
		fmt.Fprintf(os.Stderr, "--state: project %q has no custom processor (no .go files in %s).\n", project.Config.Name, projRoot)
		fmt.Fprintln(os.Stderr, "  --state runs a project's derived state + PK cold cache, which need a custom_schema.go/custom_processor.go.")
		fmt.Fprintln(os.Stderr, "  Scaffold one with `sqd-go init template <kind> <dir>` (or add a custom_schema), then re-run with --state.")
		fmt.Fprintln(os.Stderr, "  An events-only contract has no stateful entity to cache; run it without --state.")
		return 1
	}

	moduleRoot, modulePath, err := moduleInfo(projRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: %v\n", err)
		return 1
	}
	if modulePath != sqdModulePath {
		fmt.Fprintf(os.Stderr, "--state: project must live inside the sqd-go module (found module %q at %s).\n", modulePath, moduleRoot)
		fmt.Fprintln(os.Stderr, "  Its generated custom_processor.go imports internal/cli, which only resolves in-module. Place the project inside a sqd-go checkout.")
		return 1
	}
	importPath, err := projectImportPath(modulePath, moduleRoot, projRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: %v\n", err)
		return 1
	}

	// 1. Regenerate the project's Go package (Processor with EnableColdCache, etc.).
	log.Printf("--state: regenerating %s", project.Root)
	if _, err := codegen.GenerateProject(project); err != nil {
		fmt.Fprintf(os.Stderr, "codegen: %v\n", err)
		return 1
	}

	// 2. Write the runner main into a non-dotted temp dir inside the module (the go
	// tool ignores dirs starting with '.' or '_', so .sqd cannot host it).
	runnerDir, err := os.MkdirTemp(moduleRoot, "sqdrun")
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: create runner dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(runnerDir)
	if err := os.WriteFile(filepath.Join(runnerDir, "main.go"), []byte(runnerMainSource(importPath)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "--state: write runner: %v\n", err)
		return 1
	}

	// 3. Build the runner (imports the project package → init() registers the processor).
	bin, err := os.CreateTemp("", "sqd-state-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: create binary temp: %v\n", err)
		return 1
	}
	binPath := bin.Name()
	_ = bin.Close()
	defer os.Remove(binPath)

	log.Printf("--state: building %s (project %s)", project.Config.Name, importPath)
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = runnerDir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "--state: go build failed: %v\n", err)
		return 1
	}

	// 4. Exec the freshly built binary as a normal `start` run (without --state).
	childArgs := filterFlag(args, "--state")
	log.Printf("--state: launching processor-enabled run")
	child := exec.Command(binPath, childArgs...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = append(os.Environ(), stateChildEnv+"=1")
	if err := child.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "--state: run failed: %v\n", err)
		return 1
	}
	return 0
}

// sqdModulePath is the module the in-module project packages (and their internal/cli
// import) belong to. Kept as a const so projectImportPath/moduleInfo are checkable.
const sqdModulePath = "github.com/franz101/sqd-go"

// projectHasRootGoPackage reports whether dir contains at least one Go source file
// directly (a custom_schema.go / custom_processor.go), i.e. the project root is an
// importable package. Subdirectories (generated/) do not count.
func projectHasRootGoPackage(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			return true
		}
	}
	return false
}

// moduleInfo walks up from startDir to the nearest go.mod and returns its
// directory and declared module path.
func moduleInfo(startDir string) (root string, modulePath string, err error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", "", err
	}
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, rerr := os.ReadFile(gomod); rerr == nil {
			mp := parseModulePath(data)
			if mp == "" {
				return "", "", fmt.Errorf("go.mod at %s has no module path", dir)
			}
			return dir, mp, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("no go.mod found above %s", startDir)
		}
		dir = parent
	}
}

// parseModulePath extracts the module path from go.mod content (the `module …`
// directive), ignoring comments and blank lines.
func parseModulePath(gomod []byte) string {
	for _, line := range strings.Split(string(gomod), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "module"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// projectImportPath maps a project directory to its Go import path within the
// module. Returns an error if the project is outside the module tree.
func projectImportPath(modulePath, moduleRoot, projectRoot string) (string, error) {
	rel, err := filepath.Rel(moduleRoot, projectRoot)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return modulePath, nil
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("project %s is outside module %s (%s)", projectRoot, modulePath, moduleRoot)
	}
	return modulePath + "/" + rel, nil
}

// runnerMainSource is the generated runner: it blank-imports the project package
// (whose init() registers the processor) and dispatches the normal CLI.
func runnerMainSource(projectImportPath string) string {
	return fmt.Sprintf(`// Code generated by sqd-go `+"`--state`"+`; DO NOT EDIT.
package main

import (
	"os"

	_ %q

	"github.com/franz101/sqd-go/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
`, projectImportPath)
}

// filterFlag returns args with every occurrence of flag removed.
func filterFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == flag {
			continue
		}
		out = append(out, a)
	}
	return out
}
