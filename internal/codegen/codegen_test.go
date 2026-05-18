package codegen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateWritesSQLViewsAndGoCode(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: case_1_lbtc_event_only
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: LBTC
        address: "0x8236a87084f8B84306f72007F36F2618A5634494"
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	manifestPath, err := Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".sqd", "generated", "manifest.json"); manifestPath != want {
		t.Fatalf("manifest path = %q, want %q", manifestPath, want)
	}

	var manifest Manifest
	readJSON(t, manifestPath, &manifest)
	if manifest.Name != "case_1_lbtc_event_only" {
		t.Fatalf("manifest name = %q", manifest.Name)
	}

	schema := readText(t, filepath.Join(root, ".sqd", "generated", "schema.sql"))
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.blocks")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.logs")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.sync_state")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.`lbtc_transfer_events`")
	assertContains(t, schema, "`from` FixedString(20)")
	assertContains(t, schema, "`value` UInt256")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.erc20_address_positions")
	assertContains(t, schema, "realized_pnl_raw String")

	views := readText(t, filepath.Join(root, ".sqd", "generated", "views.sql"))
	assertContains(t, views, "CREATE VIEW `case_1_lbtc_event_only`.`lbtc_transfer` AS")
	assertContains(t, views, "JSONExtractString(params, 'from') AS `from`")
	assertContains(t, views, "JSONExtractString(params, 'value') AS `value`")
	assertContains(t, views, "topic0 = unhex('ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef')")

	goCode := readText(t, filepath.Join(root, "generated", "events.go"))
	assertContains(t, goCode, "package generated")
	assertContains(t, goCode, "const LBTCTransferTopic0 = \"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef\"")
	assertContains(t, goCode, "type LBTCTransfer struct")
	assertContains(t, goCode, "From  common.Address `json:\"from\"`")
	assertContains(t, goCode, "Value uint256.Int    `json:\"value\"`")
	assertContains(t, goCode, "func UnpackLog(address string, topics []string, data []byte) (*DecodedLog, error)")
	assertContains(t, goCode, "var _addr0_0 = common.HexToAddress(\"0x8236a87084f8B84306f72007F36F2618A5634494\")")
	assertContains(t, goCode, "logAddress := common.HexToAddress(address)")
	assertContains(t, goCode, "topic0 == _topic0 && (logAddress == _addr0_0)")
	assertContains(t, goCode, "func UnpackLBTCTransferLog(topics []string, data []byte) (*LBTCTransfer, error)")

	inserter := readText(t, filepath.Join(root, "generated", "inserter.go"))
	assertContains(t, inserter, "type LBTCTransferBatch struct")
	assertContains(t, inserter, "func (b *LBTCTransferBatch) TableName() string { return \"lbtc_transfer_events\" }")
	assertContains(t, inserter, "colValue proto.ColUInt256")

	processor := readText(t, filepath.Join(root, "generated", "custom_processor.go"))
	assertContains(t, processor, "type Entities struct")
	assertContains(t, processor, "LBTCTransfer []LBTCTransfer")
	assertContains(t, processor, "type AddressPosition struct")
	assertContains(t, processor, "func CustomProcessing(ctx context.Context, store Store, entities *Entities) error")
	assertContains(t, processor, "func (s *MemoryState) SyncToClickHouse")
}

func TestGenerateOmitsLogAddressWhenNoContractAddressFilter(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: no_address_filter
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: AnyTransfer
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	goCode := readText(t, filepath.Join(root, "generated", "events.go"))

	assertContains(t, goCode, "_ = address")
	assertContains(t, goCode, "topic0 == _topic0 && true")
	assertNotContains(t, goCode, "logAddress := common.HexToAddress(address)")
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected output to contain %q\n\n%s", want, got)
	}
}

func assertNotContains(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Fatalf("expected output not to contain %q\n\n%s", want, got)
	}
}
