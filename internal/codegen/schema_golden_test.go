package codegen

import (
	"bytes"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestSchemaGoGoldenOutput freezes generateSchemaGo output (with and without
// events) so the WriteString->template migration is provably byte-identical.
func TestSchemaGoGoldenOutput(t *testing.T) {
	cases := []struct {
		name   string
		events []eventSpec
		golden string
	}{
		{name: "no events", events: nil, golden: "/tmp/goldens/schema_empty.go"},
		{
			name: "with events",
			events: []eventSpec{
				{GoTypeName: "Transfer"},
				{GoTypeName: "Approval"},
			},
			golden: "/tmp/goldens/schema_events.go",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := generateSchemaGo(tc.events)
			if err != nil {
				t.Fatalf("generateSchemaGo: %v", err)
			}

			if golden, rerr := os.ReadFile(tc.golden); rerr == nil {
				if !bytes.Equal(out, golden) {
					t.Errorf("output differs from golden:\n--- golden\n%s\n--- got\n%s", golden, out)
				}
			} else {
				_ = os.MkdirAll("/tmp/goldens", 0o755)
				if werr := os.WriteFile(tc.golden, out, 0o644); werr != nil {
					t.Fatalf("write golden: %v", werr)
				}
				t.Logf("captured golden (%d bytes) at %s", len(out), tc.golden)
			}

			fset := token.NewFileSet()
			if _, perr := parser.ParseFile(fset, "schema.go", out, parser.AllErrors); perr != nil {
				t.Errorf("generated output does not parse: %v", perr)
			}
			reformatted, ferr := format.Source(out)
			if ferr != nil {
				t.Fatalf("format.Source failed: %v", ferr)
			}
			if !bytes.Equal(out, reformatted) {
				t.Errorf("generated output is not gofmt-stable")
			}
			s := string(out)
			for _, needle := range []string{
				"type Store interface {",
				"type Block struct {",
				"type Entities struct {",
				"func AppendDecodedLog(",
			} {
				if !strings.Contains(s, needle) {
					t.Errorf("missing expected string: %q", needle)
				}
			}
		})
	}
}
