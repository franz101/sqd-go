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
	assertContains(t, schema, "last_hash String")
	assertContains(t, schema, "rollback_chain String")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.`lbtc_transfer_events`")
	assertContains(t, schema, "`from` FixedString(20)")
	assertContains(t, schema, "`value` UInt256")
	assertContains(t, schema, "PRIMARY KEY (`from`, `to`, block_number, transaction_index, log_index)")
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

func TestGenerateIncludesCustomTypesSQL(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: polymarket
chains:
  - id: 137
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint256 makerAssetId, uint256 takerAssetId, uint256 makerAmountFilled, uint256 takerAmountFilled, uint256 fee)
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	customTypes := []byte(`package polymarket

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

type MemoryUserPositionSchema struct {
	User           common.Address
	TokenID        common.Hash
	Amount         decimal.Decimal
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}

type MemoryMarketSchema struct {
	ID             common.Hash
	QuestionCount  uint32
	QuestionIDs    []common.Hash
	UpdatedAtBlock uint64
}
`)
	if err := os.WriteFile(filepath.Join(root, "custom_types.go"), customTypes, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	schema := readText(t, filepath.Join(root, ".sqd", "generated", "schema.sql"))
	assertContains(t, schema, "-- Custom tables generated from custom_types.go.")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `polymarket`.`memory_user_positions`")
	assertContains(t, schema, "`amount` Decimal(38, 18)")
	assertContains(t, schema, "`updated_at` DateTime64(3, 'UTC')")
	assertContains(t, schema, "`block_number` UInt64")
	assertContains(t, schema, "`transaction_index` UInt64")
	assertContains(t, schema, "`log_index` UInt64")
	assertContains(t, schema, "PRIMARY KEY (`user`, `token_id`)")
	assertContains(t, schema, "ORDER BY (`user`, `token_id`, `block_number`, `transaction_index`, `log_index`);")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `polymarket`.`memory_markets`")
	assertContains(t, schema, "`question_ids` Array(FixedString(32))")
	assertContains(t, schema, "PRIMARY KEY (`id`)")
	assertContains(t, schema, "ORDER BY (`id`, `block_number`, `transaction_index`, `log_index`);")

	hotState := readText(t, filepath.Join(root, "generated", "hotstate.go"))
	assertContains(t, hotState, "type MemoryUserPosition struct")
	assertContains(t, hotState, "TxIndex")
	assertContains(t, hotState, "uint64")
	assertContains(t, hotState, "type PositionsClockEntry struct")
	assertContains(t, hotState, "type PositionsClockCache struct")
	assertContains(t, hotState, "ring     []PositionsClockEntry")
	assertNotContains(t, hotState, "inner *clock.Cache")
	assertContains(t, hotState, "func NewPositionsClockCache(capacity uint64) *PositionsClockCache")
	assertContains(t, hotState, "type MarketsClockCache struct")
	assertContains(t, hotState, "type MemoryUserPositionBatch struct")
	assertContains(t, hotState, "func (b *MemoryUserPositionBatch) Insert(ctx context.Context, conn *ch.Client, db string) error")
	assertContains(t, hotState, "func NewHotState(capacity uint64) *HotState")
}

func TestGenerateDefaultForkUsesCollapsingMergeTreeAndManifest(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: hot_lbtc
fork: default
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
	var manifest Manifest
	readJSON(t, manifestPath, &manifest)
	if manifest.Fork != "default" {
		t.Fatalf("manifest fork = %q, want default", manifest.Fork)
	}

	schema := readText(t, filepath.Join(root, ".sqd", "generated", "schema.sql"))
	assertContains(t, schema, "sign Int8 DEFAULT 1")
	assertContains(t, schema, ") ENGINE = CollapsingMergeTree(sign)")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `hot_lbtc`.`lbtc_transfer_events`")
	assertContains(t, schema, "PRIMARY KEY (`from`, `to`, block_number, transaction_index, log_index)")

	views := readText(t, filepath.Join(root, ".sqd", "generated", "views.sql"))
	assertContains(t, views, "FROM `hot_lbtc`.logs FINAL")
	assertContains(t, views, "AND sign = 1")
}

func TestGenerateImplicitDefaultForkUsesCollapsingMergeTree(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: ring_lbtc
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: LBTC
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	schema := readText(t, filepath.Join(root, ".sqd", "generated", "schema.sql"))
	assertContains(t, schema, ") ENGINE = CollapsingMergeTree(sign)")
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

func TestGenerateRemovesStaleHotstateFile(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: test_stale
chains:
  - id: 1
    start_block: 0
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a fake hotstate.go file in generated dir
	goOutDir := filepath.Join(root, "generated")
	if err := os.MkdirAll(goOutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(goOutDir, "hotstate.go")
	if err := os.WriteFile(stalePath, []byte("package generated"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run Generate with no custom tables
	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	// Verify that the stale hotstate.go has been deleted
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("expected stale hotstate.go to be deleted, stat error: %v", err)
	}
}
