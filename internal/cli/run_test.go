package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/franz101/sqd-go-v2/internal/config"
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
	if got := contract.Address; got != "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48" {
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

	if code := runInitContractImport(&parsedArgs{initSource: "local", initABI: abiPath, initName: "USDC", project: projectDir}); code != 1 {
		t.Fatalf("second run exit = %d, want 1 for existing config", code)
	}
}
