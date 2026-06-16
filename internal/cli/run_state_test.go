package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseModulePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "module github.com/franz101/sqd-go\n\ngo 1.25\n", "github.com/franz101/sqd-go"},
		{"leading comment", "// a comment\nmodule example.com/x\n", "example.com/x"},
		{"extra spaces", "module   example.com/y  \n", "example.com/y"},
		{"no module", "go 1.25\nrequire foo v1\n", ""},
		{"crlf", "module example.com/z\r\ngo 1.25\r\n", "example.com/z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseModulePath([]byte(c.in)); got != c.want {
				t.Fatalf("parseModulePath() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestProjectImportPath(t *testing.T) {
	const mod = "github.com/franz101/sqd-go"
	root := filepath.FromSlash("/home/dev/sqd-go")

	cases := []struct {
		name    string
		project string
		want    string
		wantErr bool
	}{
		{"subdir", filepath.FromSlash("/home/dev/sqd-go/uniswap"), mod + "/uniswap", false},
		{"nested", filepath.FromSlash("/home/dev/sqd-go/examples/polymarket"), mod + "/examples/polymarket", false},
		{"module root itself", root, mod, false},
		{"outside module", filepath.FromSlash("/home/dev/other/proj"), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := projectImportPath(mod, root, c.project)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("projectImportPath() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRunnerMainSourceCompilesShape(t *testing.T) {
	src := runnerMainSource("github.com/franz101/sqd-go/uniswap")
	// Must blank-import the project (runs its init/RegisterProcessor), import the
	// CLI, and dispatch — otherwise the re-exec would not pick up the processor.
	for _, want := range []string{
		"package main",
		`_ "github.com/franz101/sqd-go/uniswap"`,
		`"github.com/franz101/sqd-go/internal/cli"`,
		"cli.Run(os.Args[1:])",
		"DO NOT EDIT",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("runnerMainSource missing %q in:\n%s", want, src)
		}
	}
}

func TestFilterFlag(t *testing.T) {
	got := filterFlag([]string{"start", "uniswap/", "--state", "--restart"}, "--state")
	want := []string{"start", "uniswap/", "--restart"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("filterFlag() = %v, want %v", got, want)
	}
	// Idempotent when the flag is absent.
	got = filterFlag([]string{"start", "uniswap/"}, "--state")
	if strings.Join(got, " ") != "start uniswap/" {
		t.Fatalf("filterFlag(no flag) = %v", got)
	}
}

func TestStateFlagParsed(t *testing.T) {
	p, err := parseArgs([]string{"start", "uniswap/", "--state", "--restart"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !p.state {
		t.Fatal("--state not parsed into parsedArgs.state")
	}
	if p.command != "start" || p.project != "uniswap/" || !p.restart {
		t.Fatalf("unexpected parse: cmd=%q project=%q restart=%v", p.command, p.project, p.restart)
	}
}

func TestModuleInfoAndProjectChecks(t *testing.T) {
	// Build a fake module tree: <tmp>/go.mod + <tmp>/proj/{config bits}.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/fake\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}

	root, modPath, err := moduleInfo(proj)
	if err != nil {
		t.Fatalf("moduleInfo: %v", err)
	}
	if modPath != "example.com/fake" {
		t.Fatalf("module path = %q", modPath)
	}
	if got, _ := filepath.Abs(tmp); got != root {
		t.Fatalf("module root = %q, want %q", root, got)
	}

	imp, err := projectImportPath(modPath, root, proj)
	if err != nil {
		t.Fatalf("projectImportPath: %v", err)
	}
	if imp != "example.com/fake/proj" {
		t.Fatalf("import path = %q", imp)
	}

	// No .go at root yet → not an importable package.
	if projectHasRootGoPackage(proj) {
		t.Fatal("projectHasRootGoPackage = true with no root .go files")
	}
	// A custom_schema.go makes it a package; a _test.go alone does not.
	if err := os.WriteFile(filepath.Join(proj, "x_test.go"), []byte("package proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if projectHasRootGoPackage(proj) {
		t.Fatal("projectHasRootGoPackage counted a _test.go file")
	}
	if err := os.WriteFile(filepath.Join(proj, "custom_schema.go"), []byte("package proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !projectHasRootGoPackage(proj) {
		t.Fatal("projectHasRootGoPackage = false with a custom_schema.go present")
	}
}
