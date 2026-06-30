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
		{"simple", "module github.com/franz101/sqd-go\n\ngo 1.26\n", "github.com/franz101/sqd-go"},
		{"leading comment", "// a comment\nmodule example.com/x\n", "example.com/x"},
		{"extra spaces", "module   example.com/y  \n", "example.com/y"},
		{"no module", "go 1.26\nrequire foo v1\n", ""},
		{"crlf", "module example.com/z\r\ngo 1.26\r\n", "example.com/z"},
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
	// public sqd package, and dispatch — otherwise the re-exec would not pick up
	// the processor. It imports sqd (not internal/cli) so the same runner builds
	// in a standalone module that only depends on sqd-go.
	for _, want := range []string{
		"package main",
		`_ "github.com/franz101/sqd-go/uniswap"`,
		`"github.com/franz101/sqd-go/sqd"`,
		"sqd.Run(os.Args[1:])",
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

func TestGeneratedImportBase(t *testing.T) {
	dir := t.TempDir()
	// custom_schema.go has no generated import (should be skipped over).
	if err := os.WriteFile(filepath.Join(dir, "custom_schema.go"),
		[]byte("package proj\n\nimport \"time\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A _test.go importing /generated must be ignored even though it matches.
	if err := os.WriteFile(filepath.Join(dir, "decoy_test.go"),
		[]byte("package proj\n\nimport _ \"decoy/generated\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// custom_processor.go carries the real generated import.
	if err := os.WriteFile(filepath.Join(dir, "custom_processor.go"),
		[]byte("package proj\n\nimport (\n\tgenerated \"myproj/generated\"\n\t\"github.com/franz101/sqd-go/sqd\"\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	base, err := generatedImportBase(dir)
	if err != nil {
		t.Fatalf("generatedImportBase: %v", err)
	}
	if base != "myproj" {
		t.Fatalf("generatedImportBase = %q, want %q", base, "myproj")
	}
}

func TestGeneratedImportBaseMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom_processor.go"),
		[]byte("package proj\n\nimport \"fmt\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := generatedImportBase(dir); err == nil {
		t.Fatal("expected an error when no */generated import is present")
	}
}

func TestWriteStandaloneGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := writeStandaloneGoMod(dir, "myproj", "v1.2.3", ""); err != nil {
		t.Fatal(err)
	}
	got := readFileString(t, filepath.Join(dir, "go.mod"))
	for _, want := range []string{
		"module myproj",
		"go " + sqdGoDirective,
		"require " + sqdModulePath + " v1.2.3",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("go.mod missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "replace") {
		t.Fatalf("did not expect a replace directive:\n%s", got)
	}

	// With a replace (dev/CI against a local checkout).
	dir2 := t.TempDir()
	if err := writeStandaloneGoMod(dir2, "p2", "v0.0.0", "/local/sqd"); err != nil {
		t.Fatal(err)
	}
	got2 := readFileString(t, filepath.Join(dir2, "go.mod"))
	if !strings.Contains(got2, "replace "+sqdModulePath+" => /local/sqd") {
		t.Fatalf("go.mod missing replace directive:\n%s", got2)
	}
}

func TestIsUsableVersion(t *testing.T) {
	cases := map[string]bool{
		"v1.2.3":               true,
		"v0.0.0-20240101-abcd": true,
		"":                     false,
		"(devel)":              false,
	}
	for v, want := range cases {
		if got := isUsableVersion(v); got != want {
			t.Fatalf("isUsableVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestSqdModuleVersionNonEmpty(t *testing.T) {
	// In a test binary the recorded version is usually empty → "latest"; either
	// way it must be a non-empty token usable in a require/`go get`.
	if v := sqdModuleVersion(); strings.TrimSpace(v) == "" {
		t.Fatal("sqdModuleVersion() returned empty")
	}
}

func TestSetenvTemp(t *testing.T) {
	const newVar = "SQD_TEST_SETENV_NEW"
	const existing = "SQD_TEST_SETENV_EXISTING"
	os.Unsetenv(newVar)
	t.Setenv(existing, "orig") // auto-restored by the test framework too

	restore := setenvTemp(map[string]string{newVar: "a", existing: "b"})
	if os.Getenv(newVar) != "a" || os.Getenv(existing) != "b" {
		t.Fatal("setenvTemp did not apply the overrides")
	}
	restore()
	if _, ok := os.LookupEnv(newVar); ok {
		t.Fatal("setenvTemp should have unset a previously-absent var on restore")
	}
	if got := os.Getenv(existing); got != "orig" {
		t.Fatalf("setenvTemp restored %q = %q, want %q", existing, got, "orig")
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestModuleInfoAndProjectChecks(t *testing.T) {
	// Build a fake module tree: <tmp>/go.mod + <tmp>/proj/{config bits}.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/fake\n\ngo 1.26\n"), 0o644); err != nil {
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
