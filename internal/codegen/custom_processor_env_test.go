package codegen

import (
	"strings"
	"testing"
)

func TestGenerateEmptyCustomProcessorGoEnvLookup(t *testing.T) {
	out, err := generateEmptyCustomProcessorGo(nil, nil, []customTableSpec{{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	src := string(out)

	if !strings.Contains(src, "SQD_STATE_CACHE_CAPACITY") {
		t.Errorf("expected SQD_STATE_CACHE_CAPACITY in generated code")
	}
	if !strings.Contains(src, "func newState() *State {") {
		t.Errorf("expected newState() helper in generated code")
	}
}
