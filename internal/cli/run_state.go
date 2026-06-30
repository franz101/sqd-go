package cli

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/franz101/sqd-go/internal/codegen"
	"github.com/franz101/sqd-go/internal/config"
)

// stateChildEnv marks the re-executed child so it runs the ingestion pipeline
// directly instead of rebuilding again (preventing an infinite regen loop).
const stateChildEnv = "SQD_STATE_CHILD"

// sqdModulePath is the module the in-module project packages (and the public sqd
// facade they import) belong to.
const sqdModulePath = "github.com/franz101/sqd-go"

// sqdGoDirective is the language version the scaffolded standalone go.mod
// declares; it matches the sqd-go module's own go directive.
const sqdGoDirective = "1.25"

// runStateRebuild implements `sqd-go start --state <project>`.
//
// The prebuilt `sqd-go` binary (go install …@latest) contains no project
// package, so its processor registry is empty: a custom_schema/state project's
// derived state and its PK-keyed cold tier never engage (processorForProject
// returns nil → a no-op ProcessorFunc). --state collapses the historical
// "generate twice" workaround into one command: it regenerates the project's Go
// package, writes a tiny runner main that blank-imports it (so its init() →
// sqd.RegisterProcessor is compiled in), builds that runner, and execs it as a
// normal `start` run.
//
// Two layouts are supported:
//   - in-module: the project lives inside a sqd-go checkout. The runner is built
//     as an in-module package.
//   - standalone: a clean env (e.g. a notebook holding only custom_schema.go +
//     custom_processor.go, no go.mod). --state scaffolds a self-contained module
//     that requires sqd-go and builds the project against the public sqd /
//     abiunpack / coldcache packages. This is why generated code must not import
//     the module's internal/ packages — Go forbids importing another module's
//     internal/ tree.
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

	moduleRoot, modulePath, modErr := moduleInfo(projRoot)
	if modErr == nil && modulePath == sqdModulePath {
		// Project lives inside a sqd-go checkout.
		return runStateInModule(args, project, projRoot, moduleRoot, modulePath)
	}
	// Clean env: no enclosing sqd-go module (no go.mod, or a different module).
	return runStateStandalone(args, project, projRoot, moduleRoot, modulePath, modErr)
}

// runStateInModule builds the project as an in-module package of a sqd-go
// checkout and execs the resulting runner.
func runStateInModule(args []string, project *config.Project, projRoot, moduleRoot, modulePath string) int {
	importPath, err := projectImportPath(modulePath, moduleRoot, projRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: %v\n", err)
		return 1
	}

	log.Printf("--state: regenerating %s", project.Root)
	if _, err := codegen.GenerateProject(project); err != nil {
		fmt.Fprintf(os.Stderr, "codegen: %v\n", err)
		return 1
	}

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

	log.Printf("--state: building %s (project %s)", project.Config.Name, importPath)
	binPath, rc := buildRunner(runnerDir, nil)
	if rc != 0 {
		return rc
	}
	defer os.Remove(binPath)

	log.Printf("--state: launching processor-enabled run")
	return execStateChild(binPath, filterFlag(args, "--state"))
}

// runStateStandalone makes a clean-env project (notebook layout) buildable by
// turning it into a self-contained module that requires sqd-go, then runs the
// usual regenerate → runner → build → exec flow against the public packages.
//
// The dependency closure has to be resolved before codegen runs, because the
// easyjson step shells out to `go run` inside the project module; GOFLAGS=-mod=mod
// lets the toolchain populate go.sum from the warm module cache on the fly.
func runStateStandalone(args []string, project *config.Project, projRoot, moduleRoot, modulePath string, modErr error) int {
	// The project's generated package import is the contract the module path must
	// satisfy (so "<base>/generated" resolves to the codegen output).
	importBase, err := generatedImportBase(projRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: %v\n", err)
		return 1
	}

	// SQD_GO_REPLACE points the sqd-go require at a local checkout (dev/CI); a
	// real notebook fetches the installed version from the proxy instead.
	sqdReplace := strings.TrimSpace(os.Getenv("SQD_GO_REPLACE"))
	sqdVer := "v0.0.0"
	if sqdReplace == "" {
		sqdVer = sqdModuleVersion()
	}

	if modErr != nil {
		// No enclosing module: root the module at projRoot so importBase resolves
		// to ./generated.
		moduleRoot = projRoot
		modulePath = importBase
		if err := writeStandaloneGoMod(projRoot, importBase, sqdVer, sqdReplace); err != nil {
			fmt.Fprintf(os.Stderr, "--state: scaffold go.mod: %v\n", err)
			return 1
		}
		log.Printf("--state: scaffolded go.mod (module %s, require %s %s) for standalone build", importBase, sqdModulePath, sqdVer)
	} else {
		log.Printf("--state: %s is module %q (not the sqd-go module); building it standalone against %s %s", project.Config.Name, modulePath, sqdModulePath, sqdVer)
	}

	importPath, err := projectImportPath(modulePath, moduleRoot, projRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: %v\n", err)
		return 1
	}

	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")

	// Seed the module graph so codegen's easyjson bootstrap can compile.
	if rc := runGoCmd(moduleRoot, env, "get", sqdModulePath+"@"+sqdVer); rc != 0 {
		fmt.Fprintf(os.Stderr, "--state: could not resolve %s@%s; check network/GOPROXY, or set SQD_GO_REPLACE to a local sqd-go checkout\n", sqdModulePath, sqdVer)
		return rc
	}

	// codegen runs easyjson in-process; it inherits GOFLAGS/GOWORK from the env.
	restore := setenvTemp(map[string]string{"GOWORK": "off", "GOFLAGS": "-mod=mod"})
	log.Printf("--state: regenerating %s", project.Root)
	_, genErr := codegen.GenerateProject(project)
	restore()
	if genErr != nil {
		fmt.Fprintf(os.Stderr, "codegen: %v\n", genErr)
		return 1
	}

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

	// Resolve go.sum for the generated code's deps (clickhouse, pebble, easyjson)
	// now that every package — including the runner — is on disk.
	if rc := runGoCmd(moduleRoot, env, "mod", "tidy"); rc != 0 {
		return rc
	}

	log.Printf("--state: building %s (module %s, project %s)", project.Config.Name, modulePath, importPath)
	binPath, rc := buildRunner(runnerDir, env)
	if rc != 0 {
		return rc
	}
	defer os.Remove(binPath)

	log.Printf("--state: launching processor-enabled run")
	return execStateChild(binPath, filterFlag(args, "--state"))
}

// buildRunner compiles the runner package in runnerDir to a temp binary. env, if
// non-nil, overrides the build environment (used by the standalone path for
// GOFLAGS=-mod=mod). Returns the binary path and a CLI return code (0 on success).
func buildRunner(runnerDir string, env []string) (string, int) {
	bin, err := os.CreateTemp("", "sqd-state-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "--state: create binary temp: %v\n", err)
		return "", 1
	}
	binPath := bin.Name()
	_ = bin.Close()

	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = runnerDir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if env != nil {
		build.Env = env
	}
	if err := build.Run(); err != nil {
		_ = os.Remove(binPath)
		fmt.Fprintf(os.Stderr, "--state: go build failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "HINT: A build error here usually means:")
		fmt.Fprintln(os.Stderr, "  1. custom_processor.go references an event type not in config.yaml")
		fmt.Fprintln(os.Stderr, "     Fix: Run `go run . codegen <project>` after updating config.yaml")
		fmt.Fprintln(os.Stderr, "  2. Package name mismatch between custom_schema.go and custom_processor.go")
		fmt.Fprintln(os.Stderr, "     Fix: Ensure both files have the same package declaration (e.g., package uniswap)")
		fmt.Fprintln(os.Stderr, "  3. Generated code is out of sync with config")
		fmt.Fprintln(os.Stderr, "     Fix: Run `go run . codegen <project>` to regenerate")
		return "", 1
	}
	return binPath, 0
}

// execStateChild runs the freshly built binary as a normal `start` run and
// forwards termination signals so Ctrl-C / SIGTERM (and `timeout`) stop the
// indexer cleanly instead of orphaning it past this process's exit.
func execStateChild(binPath string, childArgs []string) int {
	child := exec.Command(binPath, childArgs...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = append(os.Environ(), stateChildEnv+"=1")
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "--state: start run failed: %v\n", err)
		return 1
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for s := range sigCh {
			_ = child.Process.Signal(s)
		}
	}()
	if err := child.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "--state: run failed: %v\n", err)
		return 1
	}
	return 0
}

// runGoCmd runs a `go` subcommand in dir with env, streaming output. Returns a
// CLI return code (0 on success).
func runGoCmd(dir string, env []string, args ...string) int {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "--state: `go %s` failed: %v\n", strings.Join(args, " "), err)
		return 1
	}
	return 0
}

// writeStandaloneGoMod scaffolds a module rooted at dir that requires sqd-go.
// sqdReplace, if set, adds a replace directive pointing at a local checkout.
func writeStandaloneGoMod(dir, modulePath, sqdVer, sqdReplace string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo %s\n\nrequire %s %s\n", modulePath, sqdGoDirective, sqdModulePath, sqdVer)
	if sqdReplace != "" {
		fmt.Fprintf(&b, "\nreplace %s => %s\n", sqdModulePath, filepath.ToSlash(sqdReplace))
	}
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte(b.String()), 0o644)
}

// sqdModuleVersion reports the sqd-go version the running binary was built from,
// so the scaffolded module requires a matching release. Falls back to "latest"
// for `go run`/devel builds with no recorded version.
func sqdModuleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "latest"
	}
	if bi.Main.Path == sqdModulePath && isUsableVersion(bi.Main.Version) {
		return bi.Main.Version
	}
	for _, dep := range bi.Deps {
		if dep.Path == sqdModulePath && isUsableVersion(dep.Version) {
			return dep.Version
		}
	}
	return "latest"
}

func isUsableVersion(v string) bool {
	return v != "" && v != "(devel)"
}

// generatedImportBase returns the import path prefix of the project's generated
// package (the "<base>" in `import "<base>/generated"`) by scanning the root Go
// files. The scaffolded module path must equal this so the import resolves.
func generatedImportBase(projRoot string) (string, error) {
	entries, err := os.ReadDir(projRoot)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(projRoot, name), nil, parser.ImportsOnly)
		if perr != nil {
			continue
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if base, ok := strings.CutSuffix(p, "/generated"); ok {
				return base, nil
			}
		}
	}
	return "", fmt.Errorf("could not find the project's generated package import in %s\n"+
		"  a stateful project's custom_processor.go must import its generated package, e.g.\n"+
		"    generated \"myproject/generated\"\n"+
		"  with `myproject` matching the project's module/package name", projRoot)
}

// setenvTemp sets the given environment variables and returns a function that
// restores their prior values.
func setenvTemp(kv map[string]string) func() {
	type prev struct {
		val string
		set bool
	}
	saved := make(map[string]prev, len(kv))
	for k, v := range kv {
		cur, ok := os.LookupEnv(k)
		saved[k] = prev{val: cur, set: ok}
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, p := range saved {
			if p.set {
				_ = os.Setenv(k, p.val)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}
}

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

	"github.com/franz101/sqd-go/sqd"
)

func main() { os.Exit(sqd.Run(os.Args[1:])) }
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
