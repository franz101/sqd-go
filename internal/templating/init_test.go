package templating

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

func TestStringPtr(t *testing.T) {
	input := "test_string"
	result := stringPtr(input)

	if result == nil {
		t.Fatal("stringPtr returned nil")
	}
	if *result != input {
		t.Errorf("stringPtr(%q) = %q, want %q", input, *result, input)
	}
}

func TestProjectName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple path", "/path/to/my_project", "my_project"},
		{"current directory", ".", "sqd_indexer"},
		{"root", string(filepath.Separator), "sqd_indexer"},
		{"nested path", "/home/user/sqd/projects/my_indexer", "my_indexer"},
		{"with trailing slash", "/path/to/project/", "project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := projectName(tt.input)
			if result != tt.expected {
				t.Errorf("projectName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestChainIDFromName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected uint64
		hasError bool
	}{
		{"ethereum mainnet", "ethereum", 1, false},
		{"mainnet", "mainnet", 1, false},
		{"ethereum-mainnet", "ethereum-mainnet", 1, false},
		{"numeric 1", "1", 1, false},
		{"polygon", "polygon", 137, false},
		{"polygon-mainnet", "polygon-mainnet", 137, false},
		{"matic", "matic", 137, false},
		{"numeric 137", "137", 137, false},
		{"numeric chain", "56", 56, false},
		{"invalid", "invalid-chain", 0, true},
		{"empty string", "", 1, false}, // defaults to ethereum
		{"whitespace", "  ", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := chainIDFromName(tt.input)
			if tt.hasError {
				if err == nil {
					t.Errorf("chainIDFromName(%q) expected error, got nil", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("chainIDFromName(%q) unexpected error: %v", tt.input, err)
				}
				if result != tt.expected {
					t.Errorf("chainIDFromName(%q) = %d, want %d", tt.input, result, tt.expected)
				}
			}
		})
	}
}

func TestTemplateOptionsDefaults(t *testing.T) {
	tests := []struct {
		name              string
		input             TemplateOptions
		expectedName      string
		expectedTemplate  string
		expectedHasError bool
	}{
		{
			name: "all fields provided",
			input: TemplateOptions{
				Name:     "my_indexer",
				Template: "erc20",
				APIToken: "token123",
			},
			expectedName:      "my_indexer",
			expectedTemplate:  "erc20",
			expectedHasError: false,
		},
		{
			name: "default name",
			input: TemplateOptions{
				Template: "erc20",
				APIToken: "token123",
			},
			expectedName:      "sqd_indexer",
			expectedTemplate:  "erc20",
			expectedHasError: false,
		},
		{
			name: "default template",
			input: TemplateOptions{
				Name:     "my_indexer",
				APIToken: "token123",
			},
			expectedName:      "my_indexer",
			expectedTemplate:  "erc20",
			expectedHasError: false,
		},
		{
			name: "unsupported template",
			input: TemplateOptions{
				Name:     "my_indexer",
				Template: "unsupported",
				APIToken: "token123",
			},
			expectedHasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp directory for testing
			tmpDir := t.TempDir()
			oldInput := tt.input
			tt.input.Name = filepath.Join(tmpDir, tt.input.Name)

			result, err := InitTemplate(tt.input)

			if tt.expectedHasError {
				if err == nil {
					t.Errorf("InitTemplate(%+v) expected error, got nil", oldInput)
				}
			} else {
				if err != nil {
					t.Errorf("InitTemplate(%+v) unexpected error: %v", oldInput, err)
				}
				if result == "" {
					t.Error("InitTemplate returned empty string")
				}
			}
		})
	}
}

func TestContractImportOptionsValidation(t *testing.T) {
	tests := []struct {
		name              string
		input             ContractImportOptions
		expectedHasError bool
		errorContains     string
	}{
		{
			name: "missing ABI file",
			input: ContractImportOptions{
				Name:         "my_indexer",
				ContractName: "MyContract",
				Blockchain:   "ethereum",
			},
			expectedHasError: true,
			errorContains:     "--abi-file is required",
		},
		{
			name: "missing contract name",
			input: ContractImportOptions{
				Name:     "my_indexer",
				ABIFile:  "abi.json",
				Blockchain: "ethereum",
			},
			expectedHasError: true,
			errorContains:     "--contract-name is required",
		},
		{
			name: "valid options",
			input: ContractImportOptions{
				Name:         "my_indexer",
				ABIFile:      "abi.json",
				ContractName: "MyContract",
				Blockchain:   "ethereum",
				StartBlock:   0,
			},
			expectedHasError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp directory and a fake ABI file
			tmpDir := t.TempDir()
			abiPath := filepath.Join(tmpDir, "abi.json")

			if tt.input.ABIFile != "" {
				// Create a minimal ABI file
				abiContent := `[{"type":"event","name":"Transfer","inputs":[]}]`
				if err := os.WriteFile(abiPath, []byte(abiContent), 0644); err != nil {
					t.Fatalf("failed to create test ABI file: %v", err)
				}
				tt.input.ABIFile = abiPath
				tt.input.Name = filepath.Join(tmpDir, tt.input.Name)
			}

			_, err := InitContractImport(tt.input)

			if tt.expectedHasError {
				if err == nil {
					t.Error("InitContractImport expected error, got nil")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("error should contain %q, got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("InitContractImport unexpected error: %v", err)
				}
			}
		})
	}
}

func TestEventsFromABIFile(t *testing.T) {
	tests := []struct {
		name              string
		abiContent        string
		expectedCount     int
		expectedHasError bool
		errorContains     string
	}{
		{
			name: "valid ABI with events",
			abiContent: `[{
				"type": "event",
				"name": "Transfer",
				"inputs": [
					{"name": "from", "type": "address", "indexed": true},
					{"name": "to", "type": "address", "indexed": true},
					{"name": "value", "type": "uint256", "indexed": false}
				]
			}]`,
			expectedCount:     1,
			expectedHasError: false,
		},
		{
			name: "ABI with multiple events",
			abiContent: `[{
				"type": "event",
				"name": "Transfer",
				"inputs": [{"name": "from", "type": "address", "indexed": true}]
			}, {
				"type": "event",
				"name": "Approval",
				"inputs": [{"name": "owner", "type": "address", "indexed": true}]
			}]`,
			expectedCount:     2,
			expectedHasError: false,
		},
		{
			name:              "ABI with no events",
			abiContent:        `[{"type": "function", "name": "transfer"}]`,
			expectedCount:     0,
			expectedHasError: false,
		},
		{
			name:              "invalid JSON",
			abiContent:        `not valid json`,
			expectedHasError: true,
			errorContains:     "parse abi json",
		},
		{
			name:              "non-existent file",
			abiContent:        "",
			expectedHasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.abiContent != "" {
				tmpDir := t.TempDir()
				path = filepath.Join(tmpDir, "test.abi")
				if err := os.WriteFile(path, []byte(tt.abiContent), 0644); err != nil {
					t.Fatalf("failed to create test ABI file: %v", err)
				}
			} else {
				path = "/nonexistent/path/file.abi"
			}

			events, err := eventsFromABIFile(path)

			if tt.expectedHasError {
				if err == nil {
					t.Error("eventsFromABIFile expected error, got nil")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("error should contain %q, got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("eventsFromABIFile unexpected error: %v", err)
				}
				if len(events) != tt.expectedCount {
					t.Errorf("got %d events, want %d", len(events), tt.expectedCount)
				}
			}
		})
	}
}

func TestWriteProject(t *testing.T) {
	tmpDir := t.TempDir()
	projectRoot := filepath.Join(tmpDir, "test_project")

	cfg := &config.Config{
		Name:      "test_project",
		Ecosystem: stringPtr("evm"),
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: 0,
			Contracts: []config.ChainContractConfig{{
				Name: "TestContract",
			}},
		}},
	}

	result, err := writeProject(projectRoot, cfg, "test_token")
	if err != nil {
		t.Fatalf("writeProject unexpected error: %v", err)
	}

	if result != projectRoot {
		t.Errorf("writeProject returned %q, want %q", result, projectRoot)
	}

	// Check config.yaml exists
	configPath := filepath.Join(projectRoot, "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("config.yaml was not created")
	}

	// Check schema.graphql exists
	schemaPath := filepath.Join(projectRoot, "schema.graphql")
	if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
		t.Error("schema.graphql was not created")
	}

	// Check .env exists
	envPath := filepath.Join(projectRoot, ".env")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		t.Error(".env was not created")
	}

	// Check .env contains API token
	envContent, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read .env file: %v", err)
	}
	if !strings.Contains(string(envContent), "SQD_API_TOKEN=test_token") {
		t.Error(".env does not contain SQD_API_TOKEN")
	}

	// Test that writing again fails
	_, err = writeProject(projectRoot, cfg, "test_token")
	if err == nil {
		t.Error("writeProject should fail when project already exists")
	}
}

func TestWriteProjectNoAPIToken(t *testing.T) {
	tmpDir := t.TempDir()
	projectRoot := filepath.Join(tmpDir, "test_project")

	cfg := &config.Config{
		Name:      "test_project",
		Ecosystem: stringPtr("evm"),
		Chains:    []config.Chain{{ID: 1, StartBlock: 0}},
	}

	result, err := writeProject(projectRoot, cfg, "")
	if err != nil {
		t.Fatalf("writeProject unexpected error: %v", err)
	}

	if result != projectRoot {
		t.Errorf("writeProject returned %q, want %q", result, projectRoot)
	}

	// Check .env does not contain API token
	envPath := filepath.Join(projectRoot, ".env")
	envContent, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read .env file: %v", err)
	}
	if strings.Contains(string(envContent), "SQD_API_TOKEN") {
		t.Error(".env should not contain SQD_API_TOKEN when no token provided")
	}
}

func TestWriteProjectError(t *testing.T) {
	tests := []struct {
		name        string
		setupError  func() (string, *config.Config, string)
		errorCheck  func(error) bool
		description string
	}{
		{
			name: "invalid directory path",
			setupError: func() (string, *config.Config, string) {
				// Use an invalid path that cannot be created
				return "/dev/null/invalid/test", &config.Config{}, "token"
			},
			errorCheck: func(err error) bool {
				return err != nil
			},
			description: "should fail with invalid directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectRoot, cfg, token := tt.setupError()
			_, err := writeProject(projectRoot, cfg, token)
			if !tt.errorCheck(err) {
				t.Errorf("%s: expected error, got %v", tt.description, err)
			}
		})
	}
}

func TestEventConfigGeneration(t *testing.T) {
	// Test that events are correctly formatted
	tmpDir := t.TempDir()
	abiPath := filepath.Join(tmpDir, "test.abi")

	abiContent := `[{
		"type": "event",
		"name": "Transfer",
		"inputs": [
			{"name": "from", "type": "address", "indexed": true},
			{"name": "to", "type": "address", "indexed": true},
			{"name": "value", "type": "uint256", "indexed": false}
		]
	}]`

	if err := os.WriteFile(abiPath, []byte(abiContent), 0644); err != nil {
		t.Fatalf("failed to create test ABI: %v", err)
	}

	events, err := eventsFromABIFile(abiPath)
	if err != nil {
		t.Fatalf("eventsFromABIFile error: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	event := events[0].Event
	if !strings.Contains(event, "Transfer") {
		t.Errorf("event should contain 'Transfer', got %q", event)
	}
	if !strings.Contains(event, "from") {
		t.Errorf("event should contain 'from', got %q", event)
	}
	if !strings.Contains(event, "to") {
		t.Errorf("event should contain 'to', got %q", event)
	}
	if !strings.Contains(event, "value") {
		t.Errorf("event should contain 'value', got %q", event)
	}
}

func TestInitContractImportFull(t *testing.T) {
	tmpDir := t.TempDir()
	abiPath := filepath.Join(tmpDir, "test.abi")

	// Create a valid ABI file
	abiContent := `[{
		"type": "event",
		"name": "MyEvent",
		"inputs": [{"name": "value", "type": "uint256", "indexed": false}]
	}]`

	if err := os.WriteFile(abiPath, []byte(abiContent), 0644); err != nil {
		t.Fatalf("failed to create test ABI: %v", err)
	}

	opts := ContractImportOptions{
		Name:         filepath.Join(tmpDir, "my_project"),
		ABIFile:      abiPath,
		ContractName: "MyContract",
		Blockchain:   "ethereum",
		StartBlock:   1000,
		Address:      "0x1234567890123456789012345678901234567890",
		APIToken:     "test_token",
	}

	result, err := InitContractImport(opts)
	if err != nil {
		t.Fatalf("InitContractImport error: %v", err)
	}

	if result == "" {
		t.Error("InitContractImport returned empty string")
	}

	// Check that project files exist
	configPath := filepath.Join(result, "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("config.yaml was not created")
	}

	// Check that ABI was copied
	abisDir := filepath.Join(result, "abis")
	copiedABI := filepath.Join(abisDir, filepath.Base(abiPath))
	if _, err := os.Stat(copiedABI); os.IsNotExist(err) {
		t.Error("ABI file was not copied to abis/ directory")
	}
}