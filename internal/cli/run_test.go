package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

const erc20ABI = `[
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true, "name": "from", "type": "address"},
      {"indexed": true, "name": "to", "type": "address"},
      {"indexed": false, "name": "value", "type": "uint256"}
    ],
    "name": "Transfer",
    "type": "event"
  },
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true, "name": "owner", "type": "address"},
      {"indexed": true, "name": "spender", "type": "address"},
      {"indexed": false, "name": "value", "type": "uint256"}
    ],
    "name": "Approval",
    "type": "event"
  }
]`

func TestParseArgsInitContractImportLocalSource(t *testing.T) {
	p, err := parseArgs([]string{
		"init", "contract-import", "local",
		"--abi", "erc20.json",
		"--name", "USDC",
		"--address", "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.command != "init:contract-import" {
		t.Fatalf("command = %q", p.command)
	}
	if p.initSource != "local" {
		t.Fatalf("initSource = %q", p.initSource)
	}
	if p.project != "" {
		t.Fatalf("project = %q, want empty default", p.project)
	}
}

func TestParseArgsStartNoResume(t *testing.T) {
	p, err := parseArgs([]string{"start", "examples/sample_project", "--no-resume", "--cpuprofile", "cpu.pprof"})
	if err != nil {
		t.Fatal(err)
	}
	if p.command != "start" || p.project != "examples/sample_project" {
		t.Fatalf("command/project = %q/%q", p.command, p.project)
	}
	if !p.noResume {
		t.Fatal("noResume = false, want true")
	}
	if p.cpuprofile != "cpu.pprof" {
		t.Fatalf("cpuprofile = %q, want cpu.pprof", p.cpuprofile)
	}
}

func TestExtractEventsFromABIFormattedJSON(t *testing.T) {
	got, err := extractEventsFromABI([]byte(erc20ABI))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Transfer(address indexed from, address indexed to, uint256 value)",
		"Approval(address indexed owner, address indexed spender, uint256 value)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestRunInitContractImportWritesConfig(t *testing.T) {
	dir := t.TempDir()
	abiPath := filepath.Join(dir, "erc20.json")
	if err := os.WriteFile(abiPath, []byte(erc20ABI), 0o644); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(dir, "usdc")
	code := runInitContractImport(&parsedArgs{
		initSource:   "local",
		initABI:      abiPath,
		initName:     "USDC",
		initAddress:  "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
		initChainID:  "polygon",
		initStartBlk: "123",
		project:      projectDir,
	})
	if code != 0 {
		t.Fatalf("runInitContractImport exit = %d", code)
	}

	project, err := config.LoadProject(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := project.Config
	if cfg.Name != "USDC" {
		t.Fatalf("name = %q", cfg.Name)
	}
	if cfg.Ecosystem == nil || *cfg.Ecosystem != "evm" {
		t.Fatalf("ecosystem = %#v", cfg.Ecosystem)
	}
	if got := cfg.Chains[0].ID; got != 137 {
		t.Fatalf("chain id = %d", got)
	}
	if got := cfg.Chains[0].StartBlock; got != 123 {
		t.Fatalf("start block = %d", got)
	}
	contract := cfg.Chains[0].Contracts[0]
	if got := contract.Address; len(got) != 1 || got[0] != "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48" {
		t.Fatalf("address = %#v", got)
	}
	gotEvents := make([]string, len(contract.Events))
	for i, event := range contract.Events {
		gotEvents[i] = event.Event
	}
	wantEvents := []string{
		"Transfer(address indexed from, address indexed to, uint256 value)",
		"Approval(address indexed owner, address indexed spender, uint256 value)",
	}
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("events = %#v, want %#v", gotEvents, wantEvents)
	}
	for _, path := range []string{
		filepath.Join(projectDir, ".env"),
		filepath.Join(projectDir, "compose.yml"),
		filepath.Join(projectDir, "abis", "erc20.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected scaffold file %s: %v", path, err)
		}
	}

	if code := runInitContractImport(&parsedArgs{initSource: "local", initABI: abiPath, initName: "USDC", project: projectDir}); code != 1 {
		t.Fatalf("second run exit = %d, want 1 for existing config", code)
	}
}

func TestPromptInteractiveInitTemplate(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "demo")
	input := strings.NewReader(projectDir + "\n2\n")
	var out bytes.Buffer

	req, err := promptInteractiveInit(input, &out)
	if err != nil {
		t.Fatal(err)
	}
	if req.ProjectDir != projectDir {
		t.Fatalf("project dir = %q", req.ProjectDir)
	}
	if req.ProjectName != "demo" {
		t.Fatalf("project name = %q", req.ProjectName)
	}
	if req.Option != initOptionERC20Template {
		t.Fatalf("option = %d, want template", req.Option)
	}
	if !strings.Contains(out.String(), "Blockchain ecosystem: EVM") {
		t.Fatalf("prompt output missing EVM ecosystem: %s", out.String())
	}
}

func TestRunInitTemplateWritesProject(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "erc20-demo")
	code := runInitTemplate(&parsedArgs{
		initSource:   "erc20",
		project:      projectDir,
		initChainID:  "polygon",
		initStartBlk: "123",
	})
	if code != 0 {
		t.Fatalf("runInitTemplate exit = %d", code)
	}

	project, err := config.LoadProject(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := project.Config
	if cfg.Name != "erc20-demo" {
		t.Fatalf("name = %q", cfg.Name)
	}
	if got := cfg.Chains[0].ID; got != 137 {
		t.Fatalf("chain id = %d", got)
	}
	if got := cfg.Chains[0].StartBlock; got != 123 {
		t.Fatalf("start block = %d", got)
	}
	if got := len(cfg.Chains[0].Contracts[0].Events); got != 2 {
		t.Fatalf("events = %d, want 2", got)
	}
	env, err := os.ReadFile(filepath.Join(projectDir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "CLICKHOUSE_NATIVE_PORT=9000") {
		t.Fatalf(".env missing native port: %s", string(env))
	}
	if _, err := os.Stat(filepath.Join(projectDir, "compose.yml")); err != nil {
		t.Fatalf("compose.yml missing: %v", err)
	}
}

func TestRunInitWithConfig(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "uniswap_pnl")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configYAML := `
name: case_1_lbtc_event_only
chains:
  - id: 1
    start_block: 0
    end_block: 22200000
    contracts:
      - name: LBTC
        address: "0x8236a87084f8B84306f72007F36F2618A5634494"
        events:
          - event: Transfer(address indexed from, address indexed to, uint256 value)
`
	configPath := filepath.Join(projectDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create dummy go.mod in dir so findGoModule works
	goModContent := "module github.com/franz101/sqd-go\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0o644); err != nil {
		t.Fatal(err)
	}

	code := runInitWithConfig(configPath)
	if code != 0 {
		t.Fatalf("runInitWithConfig exit = %d", code)
	}

	// Check files
	schemaPath := filepath.Join(projectDir, "custom_schema.go")
	if _, err := os.Stat(schemaPath); err != nil {
		t.Fatalf("custom_schema.go missing: %v", err)
	}
	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schemaBytes), "package uniswap_pnl") {
		t.Errorf("custom_schema.go package mismatch: %s", string(schemaBytes))
	}

	processorPath := filepath.Join(projectDir, "custom_processor.go")
	if _, err := os.Stat(processorPath); err != nil {
		t.Fatalf("custom_processor.go missing: %v", err)
	}
	processorBytes, err := os.ReadFile(processorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(processorBytes), "package uniswap_pnl") {
		t.Errorf("custom_processor.go package mismatch: %s", string(processorBytes))
	}
	expectedImport := `"github.com/franz101/sqd-go/uniswap_pnl/generated"`
	if !strings.Contains(string(processorBytes), expectedImport) {
		t.Errorf("custom_processor.go missing import %s: %s", expectedImport, string(processorBytes))
	}

	// Check generated files (events.go, schema.go, custom_processor.go)
	for _, f := range []string{"events.go", "schema.go", "custom_processor.go"} {
		path := filepath.Join(projectDir, "generated", f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("generated file missing: %s", path)
		}
	}
}

