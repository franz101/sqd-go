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
store_raw_logs: true
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
	assertNotContains(t, schema, "CREATE TABLE IF NOT EXISTS `case_1_lbtc_event_only`.blocks")
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
	assertContains(t, goCode, "From")
	assertContains(t, goCode, "common.Address")
	assertContains(t, goCode, "`json:\"from\"`")
	assertContains(t, goCode, "Value")
	assertContains(t, goCode, "uint256.Int")
	assertContains(t, goCode, "`json:\"value\"`")
	assertContains(t, goCode, "func UnpackLog(address string, topics []string, data []byte) (*DecodedLog, error)")
	assertContains(t, goCode, "var _addr0_0 = common.HexToAddress(\"0x8236a87084f8B84306f72007F36F2618A5634494\")")
	assertContains(t, goCode, "logAddress := common.HexToAddress(address)")
	assertContains(t, goCode, "topic0 == _topic0 && (logAddress == _addr0_0)")
	assertContains(t, goCode, "func UnpackLBTCTransferLog(topics []string, data []byte) (*LBTCTransfer, error)")

	inserter := readText(t, filepath.Join(root, "generated", "inserter.go"))
	assertContains(t, inserter, "type LBTCTransferBatch struct")
	assertContains(t, inserter, "func (b *LBTCTransferBatch) TableName() string { return \"lbtc_transfer_events\" }")
	assertContains(t, inserter, "colValue proto.ColUInt256")

	schemaGo := readText(t, filepath.Join(root, "generated", "schema.go"))
	assertContains(t, schemaGo, "type Entities struct")
	assertContains(t, schemaGo, "LBTCTransfer []LBTCTransfer")
	assertContains(t, schemaGo, "type Block struct")
	assertContains(t, schemaGo, "type Log struct")
	assertContains(t, schemaGo, "type SyncState struct")

	processor := readText(t, filepath.Join(root, "generated", "custom_processor.go"))
	assertNotContains(t, processor, "type Entities struct")
	assertContains(t, processor, "type AddressPosition struct")
	assertContains(t, processor, "func CustomProcessing(ctx context.Context, store Store, entities *Entities) error")
	assertContains(t, processor, "func (s *MemoryState) SyncToClickHouse")
}

func TestGenerateStoreBlocksOptionIncludesBlocksTable(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: with_blocks
store_blocks: true
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

	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	schema := readText(t, filepath.Join(root, ".sqd", "generated", "schema.sql"))
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `with_blocks`.blocks")
	assertContains(t, schema, "ORDER BY (chain_id, block_number)")
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
	configYAML := []byte(`name: market_fixture
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
	customTypes := []byte(`package market_fixture

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

// pk: Account, AssetID
type MemoryHoldingSchema struct {
	Account        common.Address
	AssetID        common.Hash
	Amount         decimal.Decimal
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}

// pk: ID
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
	assertContains(t, schema, "-- Custom tables generated from custom schema definitions.")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `market_fixture`.`memory_holdings`")
	assertContains(t, schema, "`amount` Decimal(38, 18)")
	assertContains(t, schema, "`updated_at` DateTime64(3, 'UTC')")
	assertContains(t, schema, "`block_number` UInt64")
	assertContains(t, schema, "`transaction_index` UInt64")
	assertContains(t, schema, "`log_index` UInt64")
	assertContains(t, schema, "PRIMARY KEY (`account`, `asset_id`)")
	assertContains(t, schema, "ORDER BY (`account`, `asset_id`, `block_number`, `transaction_index`, `log_index`);")
	assertContains(t, schema, "CREATE TABLE IF NOT EXISTS `market_fixture`.`memory_markets`")
	assertContains(t, schema, "`question_ids` Array(FixedString(32))")
	assertContains(t, schema, "PRIMARY KEY (`id`)")
	assertContains(t, schema, "ORDER BY (`id`, `block_number`, `transaction_index`, `log_index`);")

	hotState := readText(t, filepath.Join(root, "generated", "hotstate.go"))
	assertContains(t, hotState, "type MemoryHolding struct")
	assertContains(t, hotState, "TxIndex")
	assertContains(t, hotState, "uint64")
	assertContains(t, hotState, "type HoldingsClockEntry struct")
	assertContains(t, hotState, "type HoldingsClockCache struct")
	assertContains(t, hotState, "ring     []HoldingsClockEntry")
	assertNotContains(t, hotState, "inner *clock.Cache")
	assertContains(t, hotState, "func NewHoldingsClockCache(capacity uint64) *HoldingsClockCache")
	assertContains(t, hotState, "type MarketsClockCache struct")
	assertContains(t, hotState, "type MemoryHoldingBatch struct")
	assertContains(t, hotState, "func (b *MemoryHoldingBatch) Insert(ctx context.Context, conn *ch.Client, db string) error")
	assertContains(t, hotState, "func NewHotState(capacity uint64) *HotState")
}

func TestGenerateDefaultForkUsesCollapsingMergeTreeAndManifest(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: hot_lbtc
store_raw_logs: true
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

func TestGenerateStateHandlesFromCustomSchemaAndConfig(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: generic_state
state:
  - name: Position
    source_table: memory_holdings
    key:
      - Account
      - AssetID
    mode: hotstate
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: Ledger
        events:
          - event: HoldingTouched(address indexed account, bytes32 indexed assetId, uint256 value)
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	customSchema := []byte(`package generic_state

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

// pk: Account, AssetID
type MemoryHoldingSchema struct {
	Account        common.Address
	AssetID        common.Hash
	Amount         decimal.Decimal
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}
`)
	if err := os.WriteFile(filepath.Join(root, "custom_schema.go"), customSchema, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	stateGo := readText(t, filepath.Join(root, "generated", "state.go"))
	assertContains(t, stateGo, "type Position = MemoryHolding")
	assertContains(t, stateGo, "Position       PositionState")
	assertContains(t, stateGo, "func (h PositionState) Get(account common.Address, assetID common.Hash) (*Position, bool)")
	assertContains(t, stateGo, "func (h PositionState) Save(value *Position, meta EventMeta)")
	assertContains(t, stateGo, "h.state.HotState.UpdateMemoryHolding(*value)")
	assertNotContains(t, stateGo, "Internal"+"PositionState")
	assertNotContains(t, stateGo, "User"+"PositionKey")

	processorGo := readText(t, filepath.Join(root, "generated", "custom_processor.go"))
	assertContains(t, processorGo, "prefetchBlocksState(ctx, store, state, []*ParsedBlock{block})")
	assertContains(t, processorGo, "prefetchProtoBlocksState(ctx, store, state, []*ProtoEventBlock{block})")
	assertContains(t, processorGo, "Resolver.Queue(")
	assertNotContains(t, processorGo, "set"+"EventMeta")
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

func TestGenerateCustomSchemaAndProcessorPaths(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config_folder")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configYAML := []byte(`name: test_custom_paths
chains:
  - id: 1
    start_block: 0
    contracts:
      - name: Dummy
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
`)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	// Write custom_schema.go in the config folder
	customSchemaGo := []byte(`package test_custom_paths

import (
	"time"
	"github.com/ethereum/go-ethereum/common"
)

type DummyStateSchema struct {
	User           common.Address
	Balance        uint64
	UpdatedAtBlock uint64
	UpdatedAt      time.Time
}
`)
	if err := os.WriteFile(filepath.Join(configDir, "custom_schema.go"), customSchemaGo, 0o644); err != nil {
		t.Fatal(err)
	}

	// Write a user custom processor next to the config to verify it's detected and not overwritten/modified
	userProcessor := []byte(`package test_custom_paths
// My custom logic
`)
	if err := os.WriteFile(filepath.Join(configDir, "custom_processor.go"), userProcessor, 0o644); err != nil {
		t.Fatal(err)
	}

	// Run codegen using the path to the config file directory
	if _, err := Generate(configDir); err != nil {
		t.Fatal(err)
	}

	// Verify custom_schema.sql is generated in BOTH .sqd/generated and generated/ under project root (configDir)
	schemaSQL1 := readText(t, filepath.Join(configDir, ".sqd", "generated", "custom_schema.sql"))
	schemaSQL2 := readText(t, filepath.Join(configDir, "generated", "custom_schema.sql"))
	assertContains(t, schemaSQL1, "CREATE TABLE IF NOT EXISTS `test_custom_paths`.`dummy_states`")
	assertContains(t, schemaSQL2, "CREATE TABLE IF NOT EXISTS `test_custom_paths`.`dummy_states`")

	// Verify that the custom_processor.go next to the config is not modified
	procContent, err := os.ReadFile(filepath.Join(configDir, "custom_processor.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(procContent), "// My custom logic") {
		t.Fatalf("expected custom processor next to config to remain untouched, got:\n%s", string(procContent))
	}

	// Now delete custom_processor.go and run codegen again to verify it creates an empty one under generated/
	if err := os.Remove(filepath.Join(configDir, "custom_processor.go")); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(configDir); err != nil {
		t.Fatal(err)
	}

	emptyProc := readText(t, filepath.Join(configDir, "generated", "custom_processor.go"))
	assertContains(t, emptyProc, "Code generated by sqd-go codegen; DO NOT EDIT.")
	assertContains(t, emptyProc, "var CustomProcessFn func(state *State, block *ParsedBlock) error")
	assertContains(t, emptyProc, "func CustomProcessing(ctx context.Context, store Store, state *State, block *ParsedBlock) error")
	assertContains(t, emptyProc, "func NewProcessor(protoMode bool) (*Processor, error)")
	assertContains(t, emptyProc, "const defaultRingBufferSize uint32 = 8192")
	assertContains(t, emptyProc, "var stateStore Store")
	assertContains(t, emptyProc, "p.State.Store = stateStore")
	assertContains(t, emptyProc, "processGroup := func(group blockGroup) error")
	assertNotContains(t, emptyProc, "prefetchBlockBatchSize")
	assertNotContains(t, emptyProc, "blocks := make([]*ParsedBlock")
	assertNotContains(t, emptyProc, "var protoBlocks []*ProtoEventBlock")
	assertNotContains(t, emptyProc, "protoBlocks := make([]*ProtoEventBlock")
	assertNotContains(t, emptyProc, "set"+"EventMeta")
}

func TestGenerateOmitAndMetadataOptions(t *testing.T) {
	root := t.TempDir()
	configYAML := []byte(`name: test_omit_meta
store_raw_logs: false
include_metadata:
  - chain_id
  - block_hash
chains:
  - id: 137
    start_block: 0
    contracts:
      - name: Exchange
        events:
          - event: OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint256 makerAssetId, uint256 takerAmountFilled)
            omit:
              - orderHash
`)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), configYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(root); err != nil {
		t.Fatal(err)
	}

	// 1. Verify schema.sql
	schema := readText(t, filepath.Join(root, ".sqd", "generated", "schema.sql"))
	// Raw logs should be omitted (so no CREATE TABLE ... logs)
	assertNotContains(t, schema, "CREATE TABLE IF NOT EXISTS `test_omit_meta`.logs")
	// Included metadata columns: chain_id, block_hash. Omitted: contract_address, transaction_hash
	assertContains(t, schema, "chain_id UInt64")
	assertContains(t, schema, "block_hash FixedString(32)")
	assertNotContains(t, schema, "contract_address FixedString(20)")
	assertNotContains(t, schema, "transaction_hash FixedString(32)")
	// Omitted field orderHash should not be in the table columns
	assertNotContains(t, schema, "`orderHash` FixedString(32)")
	assertContains(t, schema, "`maker` FixedString(20)")
	assertContains(t, schema, "`taker` FixedString(20)")
	assertContains(t, schema, "`makerAssetId` UInt256")
	assertContains(t, schema, "`takerAmountFilled` UInt256")

	// 2. Verify views.sql is comment only since raw logs are omitted
	views := readText(t, filepath.Join(root, ".sqd", "generated", "views.sql"))
	assertContains(t, views, "Raw logs omitted by config")

	// 3. Verify events.go has the correct EventMeta struct and no OrderHash field
	eventsGo := readText(t, filepath.Join(root, "generated", "events.go"))
	assertContains(t, eventsGo, "ChainID")
	assertContains(t, eventsGo, "BlockHash")
	assertNotContains(t, eventsGo, "OrderHash")
	assertContains(t, eventsGo, "ContractAddress")
	assertContains(t, eventsGo, "TransactionHash")
	assertContains(t, eventsGo, "Maker")
	assertContains(t, eventsGo, "Taker")
	assertContains(t, eventsGo, "if len(topics) < 4")
	assertContains(t, eventsGo, "ev.Maker = common.HexToAddress(topics[2])")
	assertContains(t, eventsGo, "ev.Taker = common.HexToAddress(topics[3])")
	assertContains(t, eventsGo, "ev.MakerAssetID.SetBytes32(word)")

	// 4. Verify inserter.go has the correct ClickHouseCommonColumnNames
	inserterGo := readText(t, filepath.Join(root, "generated", "inserter.go"))
	assertContains(t, inserterGo, `"chain_id"`)
	assertContains(t, inserterGo, `"block_hash"`)
	assertNotContains(t, inserterGo, `"contract_address"`)
	assertNotContains(t, inserterGo, `"transaction_hash"`)
}
