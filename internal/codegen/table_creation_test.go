package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGenerateCreatesEventAndCustomTables verifies that codegen emits DDL for
// BOTH kinds of tables from a single project:
//   - event tables (one per contract event) and the core sync_state, in schema.sql
//   - custom tables (one per custom schema type), in custom_schema.sql
//
// This is the generation-side ("unit") counterpart to the live-ClickHouse
// integration test in tests/tablecreate, which applies these artifacts and
// asserts the tables are physically created.
func TestGenerateCreatesEventAndCustomTables(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: table_fixture
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address indexed maker, uint256 makerAmountFilled)
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	customSchema := []byte(`package table_fixture

import "github.com/ethereum/go-ethereum/common"

// pk: ID
type MemoryMarketSchema struct {
	ID             common.Hash
	QuestionCount  uint32
	UpdatedAtBlock uint64
}
`)
	if err := os.WriteFile(filepath.Join(root, "custom_schema.go"), customSchema, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(root); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Event tables + the core sync_state table land in schema.sql.
	schema := readText(t, filepath.Join(root, ".sqd", "generated", "schema.sql"))
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `table_fixture`.sync_state")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `table_fixture`.`exchange_order_filled_events`")

	// Custom tables land in a dedicated custom_schema.sql.
	customSchemaSQL := readText(t, filepath.Join(root, ".sqd", "generated", "custom_schema.sql"))
	assertContains(t, customSchemaSQL, "CREATE TABLE IF NOT EXISTS `table_fixture`.`memory_markets`")
}
