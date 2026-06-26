package codegen

import (
	"os"
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

func TestGenerateGoldenOutput(t *testing.T) {
	cfg := &config.Config{Name: "test_project"}
	var events []eventSpec
	var hotStateTables []customTableSpec

	// Branch 1: no hot tables
	code1, err := generateEmptyCustomProcessorGo(cfg, events, hotStateTables)
	if err != nil {
		t.Fatalf("branch 1: %v", err)
	}
	os.WriteFile("/tmp/goldens/custom_processor_empty.go", code1, 0644)
	t.Logf("Branch 1 (no hot tables): %d bytes", len(code1))

	// Branch 2: with hot tables
	hotStateTables = []customTableSpec{
		{Name: "TokenBalance", IsEvent: false},
	}
	code2, err := generateEmptyCustomProcessorGo(cfg, events, hotStateTables)
	if err != nil {
		t.Fatalf("branch 2: %v", err)
	}
	os.WriteFile("/tmp/goldens/custom_processor_full.go", code2, 0644)
	t.Logf("Branch 2 (with hot tables): %d bytes", len(code2))
}
