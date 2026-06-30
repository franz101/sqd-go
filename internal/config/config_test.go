package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveProjectPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "sqd-go-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	t.Run("config.yaml", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "yaml")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		configPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(configPath, []byte("name: test"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		root, resolved, err := ResolveProjectPath(dir)
		if err != nil {
			t.Errorf("ResolveProjectPath() error = %v", err)
			return
		}
		if root != dir {
			t.Errorf("root = %v, want %v", root, dir)
		}
		if resolved != configPath {
			t.Errorf("resolved = %v, want %v", resolved, configPath)
		}
	})

	t.Run("config.yml", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "yml")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		configPath := filepath.Join(dir, "config.yml")
		if err := os.WriteFile(configPath, []byte("name: test"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		root, resolved, err := ResolveProjectPath(dir)
		if err != nil {
			t.Errorf("ResolveProjectPath() error = %v", err)
			return
		}
		if root != dir {
			t.Errorf("root = %v, want %v", root, dir)
		}
		if resolved != configPath {
			t.Errorf("resolved = %v, want %v", resolved, configPath)
		}
	})

	t.Run("both_prefers_yaml", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "both")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		yamlPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(yamlPath, []byte("name: yaml"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}
		ymlPath := filepath.Join(dir, "config.yml")
		if err := os.WriteFile(ymlPath, []byte("name: yml"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		_, resolved, err := ResolveProjectPath(dir)
		if err != nil {
			t.Errorf("ResolveProjectPath() error = %v", err)
			return
		}
		if resolved != yamlPath {
			t.Errorf("resolved = %v, want %v", resolved, yamlPath)
		}
	})
}

func TestForkModeDefaultsToDefault(t *testing.T) {
	cfg := &Config{Name: "test", Chains: []Chain{{ID: 1}}}

	if got := cfg.ForkMode(); got != ForkModeDefault {
		t.Fatalf("fork mode = %q, want %q", got, ForkModeDefault)
	}
	if cfg.ForkMode().UsesCollapsingMergeTree() {
		t.Fatal("default fork mode should use plain MergeTree (no duplicates are possible)")
	}
}

func TestForkModeAcceptsDefaultOnly(t *testing.T) {
	cfg := &Config{Name: "test", Fork: ForkMode(" DEFAULT "), Chains: []Chain{{ID: 1}}}

	if got := cfg.ForkMode(); got != ForkModeDefault {
		t.Fatalf("fork mode = %q, want %q", got, ForkModeDefault)
	}
}

func TestValidateRejectsUnknownForkMode(t *testing.T) {
	cfg := &Config{Name: "test", Fork: ForkMode("fastest"), Chains: []Chain{{ID: 1}}}

	err := Validate(cfg)

	if err == nil || !strings.Contains(err.Error(), "fork must be default") {
		t.Fatalf("Validate error = %v, want fork mode validation", err)
	}
}

func TestForkModeNeverUsesCollapsingMergeTree(t *testing.T) {
	// Reingestion prunes block_number > lastBlock before re-inserting, so no
	// duplicates are ever possible and plain MergeTree suffices for every mode.
	tests := []struct {
		mode ForkMode
	}{
		{ForkModeDefault},
		{ForkMode("")},
		{ForkModeSqd},
		{ForkModeRingBuffer},
	}

	for _, tt := range tests {
		if got := tt.mode.UsesCollapsingMergeTree(); got != false {
			t.Errorf("mode %q UsesCollapsingMergeTree() = %t, want false", tt.mode, got)
		}
	}
}

func TestStoreBlocksDefaultsToFalse(t *testing.T) {
	cfg := &Config{}

	if cfg.ShouldStoreBlocks() {
		t.Fatal("store_blocks should default to false")
	}
}

func TestStoreBlocksCanBeEnabled(t *testing.T) {
	enabled := true
	cfg := &Config{StoreBlocks: &enabled}

	if !cfg.ShouldStoreBlocks() {
		t.Fatal("store_blocks=true should enable block ledger storage")
	}
}

func TestConfigParsesRollbackOnReorgAndAliases(t *testing.T) {
	yamlConfig := []byte(`name: test_aliases
fork: sqd
rollback_on_reorg: true
chains:
  - id: 137
`)

	var cfg Config
	err := yaml.Unmarshal(yamlConfig, &cfg)
	if err != nil {
		t.Fatalf("failed to parse yaml with rollback_on_reorg and fork aliases: %v", err)
	}

	if cfg.Fork != ForkModeSqd {
		t.Fatalf("cfg.Fork = %q, want %q", cfg.Fork, ForkModeSqd)
	}

	if cfg.RollbackOnReorg == nil || !*cfg.RollbackOnReorg {
		t.Fatal("expected RollbackOnReorg to be true")
	}

	// Validation should pass since sqd is now a valid ForkMode
	err = Validate(&cfg)
	if err != nil {
		t.Fatalf("Validate failed for valid alias: %v", err)
	}
}

func TestConfigParsesStateTables(t *testing.T) {
	yamlConfig := []byte(`name: market_fixture
state:
  - name: condition_cache
    source_table: market_condition_preparation_events
    key:
      - conditionId
    mode: db_prefetch
chains:
  - id: 137
`)

	var cfg Config
	err := yaml.Unmarshal(yamlConfig, &cfg)
	if err != nil {
		t.Fatalf("failed to parse state config: %v", err)
	}

	if err := Validate(&cfg); err != nil {
		t.Fatalf("Validate failed for state config: %v", err)
	}
	if len(cfg.State) != 1 {
		t.Fatalf("state entries = %d, want 1", len(cfg.State))
	}
	state := cfg.State[0]
	if state.Name != "condition_cache" || state.SourceTable != "market_condition_preparation_events" || state.Mode != "db_prefetch" {
		t.Fatalf("unexpected state config: %#v", state)
	}
	if len(state.Key) != 1 || state.Key[0] != "conditionId" {
		t.Fatalf("unexpected state key: %#v", state.Key)
	}
}

func TestParseEventName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Transfer(address,address,uint256)", "Transfer"},
		{"  Transfer(address,address,uint256)", "Transfer"},
		{"event Transfer(address,uint256)", "Transfer"},
		{"  event Transfer(address,uint256)  ", "Transfer"},
		{"Transfer", "Transfer"},
		{"  Transfer  ", "Transfer"},
		{"NoParams", "NoParams"},
		{"SimpleEvent()", "SimpleEvent"},
		{"", ""},
		{"   ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseEventName(tt.input)
			if result != tt.expected {
				t.Errorf("parseEventName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestAddressUnmarshalYAML(t *testing.T) {
	tests := []struct {
		name         string
		yamlValue    string
		expectedLen  int
		expectedVals []string
		hasError     bool
	}{
		{
			name:         "single address",
			yamlValue:    "0x1234567890123456789012345678901234567890",
			expectedLen:  1,
			expectedVals: []string{"0x1234567890123456789012345678901234567890"},
		},
		{
			name:         "multiple addresses",
			yamlValue:    "- 0x1234567890123456789012345678901234567890\n- 0xabcdefabcdefabcdefabcdefabcdefabcdefabcd",
			expectedLen:  2,
			expectedVals: []string{"0x1234567890123456789012345678901234567890", "0xabcdefabcdefabcdefabcdefabcdefabcdefabcd"},
		},
		{
			name:      "invalid type",
			yamlValue: "123",
			hasError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var node yaml.Node
			err := yaml.Unmarshal([]byte(tt.yamlValue), &node)
			if err != nil && !tt.hasError {
				t.Fatalf("yaml.Unmarshal error: %v", err)
			}

			var addr Address
			err = addr.UnmarshalYAML(&node)

			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if len(addr) != tt.expectedLen {
					t.Errorf("address count = %d, want %d", len(addr), tt.expectedLen)
				}
				for i, val := range tt.expectedVals {
					if addr[i] != val {
						t.Errorf("address[%d] = %q, want %q", i, addr[i], val)
					}
				}
			}
		})
	}
}

func TestShouldStoreRawLogs(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *Config
		expected bool
	}{
		{"nil config", nil, false},
		{"nil pointer", &Config{StoreRawLogs: nil}, false},
		{"false", &Config{StoreRawLogs: boolPtr(false)}, false},
		{"true", &Config{StoreRawLogs: boolPtr(true)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.cfg.ShouldStoreRawLogs()
			if result != tt.expected {
				t.Errorf("ShouldStoreRawLogs() = %t, want %t", result, tt.expected)
			}
		})
	}
}

func TestMetadataIncluded(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *Config
		field    string
		expected bool
	}{
		{"nil config", nil, "block_number", false},
		{"empty list", &Config{IncludeMetadata: []string{}}, "block_number", false},
		{"exact match", &Config{IncludeMetadata: []string{"block_number"}}, "block_number", true},
		{"case insensitive", &Config{IncludeMetadata: []string{"BLOCK_NUMBER"}}, "block_number", true},
		{"underscore variations", &Config{IncludeMetadata: []string{"blocknumber"}}, "block_number", true},
		{"no match", &Config{IncludeMetadata: []string{"transaction_hash"}}, "block_number", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.cfg.MetadataIncluded(tt.field)
			if result != tt.expected {
				t.Errorf("MetadataIncluded(%q) = %t, want %t", tt.field, result, tt.expected)
			}
		})
	}
}

func TestValidateNilConfig(t *testing.T) {
	err := Validate(nil)
	if err == nil {
		t.Error("Validate(nil) should return error")
	}
	if !strings.Contains(err.Error(), "config is nil") {
		t.Errorf("error message should mention 'config is nil', got: %v", err)
	}
}

func TestValidateEmptyName(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *Config
		hasError bool
	}{
		{"empty string", &Config{Name: "", Chains: []Chain{{ID: 1}}}, true},
		{"whitespace only", &Config{Name: "   ", Chains: []Chain{{ID: 1}}}, true},
		{"valid name", &Config{Name: "my_indexer", Chains: []Chain{{ID: 1}}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.cfg)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), "name is required") {
					t.Errorf("error should mention 'name is required', got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateNoChains(t *testing.T) {
	cfg := &Config{Name: "test", Chains: []Chain{}}
	err := Validate(cfg)
	if err == nil {
		t.Error("Validate should return error for empty chains")
	}
	if !strings.Contains(err.Error(), "at least one chain is required") {
		t.Errorf("error should mention 'at least one chain is required', got: %v", err)
	}
}

func TestValidateChainID(t *testing.T) {
	tests := []struct {
		name     string
		chain    Chain
		hasError bool
	}{
		{"valid ID", Chain{ID: 1, Contracts: []ChainContractConfig{{Name: "test", Events: []EventConfig{{Event: "Test()"}}}}}, false},
		{"zero ID", Chain{ID: 0, Contracts: []ChainContractConfig{{Name: "test", Events: []EventConfig{{Event: "Test()"}}}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "test", Chains: []Chain{tt.chain}}
			err := Validate(cfg)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), "id is required") {
					t.Errorf("error should mention 'id is required', got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateEndBlock(t *testing.T) {
	tests := []struct {
		name     string
		chain    Chain
		hasError bool
	}{
		{"no end block", Chain{ID: 1, StartBlock: 0, Contracts: []ChainContractConfig{{Name: "test", Events: []EventConfig{{Event: "Test()"}}}}}, false},
		{"valid end block", Chain{ID: 1, StartBlock: 0, EndBlock: uint64Ptr(1000), Contracts: []ChainContractConfig{{Name: "test", Events: []EventConfig{{Event: "Test()"}}}}}, false},
		{"end block equals start", Chain{ID: 1, StartBlock: 1000, EndBlock: uint64Ptr(1000), Contracts: []ChainContractConfig{{Name: "test", Events: []EventConfig{{Event: "Test()"}}}}}, false},
		{"end block less than start", Chain{ID: 1, StartBlock: 1000, EndBlock: uint64Ptr(500), Contracts: []ChainContractConfig{{Name: "test", Events: []EventConfig{{Event: "Test()"}}}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "test", Chains: []Chain{tt.chain}}
			err := Validate(cfg)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), "end_block must be >= start_block") {
					t.Errorf("error should mention 'end_block must be >= start_block', got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateContractName(t *testing.T) {
	tests := []struct {
		name      string
		contract  ChainContractConfig
		hasError  bool
	}{
		{"valid name", ChainContractConfig{Name: "MyContract", Events: []EventConfig{{Event: "Test()"}}}, false},
		{"empty name", ChainContractConfig{Name: "", Events: []EventConfig{{Event: "Test()"}}}, true},
		{"whitespace name", ChainContractConfig{Name: "   ", Events: []EventConfig{{Event: "Test()"}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "test", Chains: []Chain{{ID: 1, Contracts: []ChainContractConfig{tt.contract}}}}
			err := Validate(cfg)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), "name is required") {
					t.Errorf("error should mention 'name is required', got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateContractEvents(t *testing.T) {
	tests := []struct {
		name     string
		contract ChainContractConfig
		hasError bool
	}{
		{"has events", ChainContractConfig{Name: "Test", Events: []EventConfig{{Event: "Test()"}}}, false},
		{"no events", ChainContractConfig{Name: "Test", Events: []EventConfig{}}, true},
		{"nil events", ChainContractConfig{Name: "Test", Events: nil}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "test", Chains: []Chain{{ID: 1, Contracts: []ChainContractConfig{tt.contract}}}}
			err := Validate(cfg)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), "events is required") {
					t.Errorf("error should mention 'events is required', got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateEventString(t *testing.T) {
	tests := []struct {
		name     string
		event    EventConfig
		hasError bool
	}{
		{"valid event", EventConfig{Event: "Transfer(address,uint256)"}, false},
		{"empty event", EventConfig{Event: ""}, true},
		{"whitespace event", EventConfig{Event: "   "}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Name: "test",
				Chains: []Chain{{ID: 1, Contracts: []ChainContractConfig{{
					Name: "Test",
					Events: []EventConfig{tt.event},
				}}}},
			}
			err := Validate(cfg)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if !strings.Contains(err.Error(), "event is required") {
					t.Errorf("error should mention 'event is required', got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateStateConfig(t *testing.T) {
	tests := []struct {
		name     string
		state    StateConfig
		hasError bool
	}{
		{"valid mode", StateConfig{Name: "test_state", SourceTable: "events", Mode: "db_prefetch"}, false},
		{"default mode", StateConfig{Name: "test_state", SourceTable: "events", Mode: ""}, false},
		{"hotstate mode", StateConfig{Name: "test_state", SourceTable: "events", Mode: "hotstate"}, false},
		{"invalid mode", StateConfig{Name: "test_state", SourceTable: "events", Mode: "invalid"}, true},
		{"empty name", StateConfig{Name: "", SourceTable: "events", Mode: "db_prefetch"}, true},
		{"whitespace name", StateConfig{Name: "   ", SourceTable: "events", Mode: "db_prefetch"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "test", Chains: []Chain{{ID: 1}}, State: []StateConfig{tt.state}}
			err := Validate(cfg)
			if tt.hasError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestApplyExcludeMetadata(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *Config
		expectedOmits  []string
		eventName      string
	}{
		{
			name: "no exclude metadata",
			cfg: &Config{
				Name: "test",
				Chains: []Chain{{ID: 1, Contracts: []ChainContractConfig{{
					Name: "Test",
					Events: []EventConfig{{Event: "Transfer(address,uint256)"}},
				}}}},
			},
			expectedOmits: []string{},
			eventName:     "Transfer",
		},
		{
			name: "with exclude metadata",
			cfg: &Config{
				Name: "test",
				ExcludeMetadata: []map[string]string{{"transfer": "from"}},
				Chains: []Chain{{ID: 1, Contracts: []ChainContractConfig{{
					Name: "Test",
					Events: []EventConfig{{Event: "Transfer(address,uint256)"}},
				}}}},
			},
			expectedOmits: []string{"from"},
			eventName:     "Transfer",
		},
		{
			name: "multiple excludes",
			cfg: &Config{
				Name: "test",
				ExcludeMetadata: []map[string]string{
					{"transfer": "from"},
					{"transfer": "to"},
				},
				Chains: []Chain{{ID: 1, Contracts: []ChainContractConfig{{
					Name: "Test",
					Events: []EventConfig{{Event: "Transfer(address,uint256)"}},
				}}}},
			},
			expectedOmits: []string{"from", "to"},
			eventName:     "Transfer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.ApplyExcludeMetadata()
			event := tt.cfg.Chains[0].Contracts[0].Events[0]
			if len(event.Omit) != len(tt.expectedOmits) {
				t.Errorf("Omit count = %d, want %d", len(event.Omit), len(tt.expectedOmits))
			}
			for i, expected := range tt.expectedOmits {
				if i >= len(event.Omit) || event.Omit[i] != expected {
					t.Errorf("Omit[%d] = %q, want %q", i, event.Omit[i], expected)
				}
			}
		})
	}
}

func TestLoadProject(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "sqd-go-load-project-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	t.Run("valid project", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "valid")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		configPath := filepath.Join(dir, "config.yaml")
		configContent := []byte("name: test_project\nchains:\n  - id: 1")
		if err := os.WriteFile(configPath, configContent, 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		project, err := LoadProject(dir)
		if err != nil {
			t.Errorf("LoadProject error: %v", err)
		}
		if project == nil {
			t.Fatal("LoadProject returned nil project")
		}
		if project.Root != dir {
			t.Errorf("project.Root = %q, want %q", project.Root, dir)
		}
		if project.ConfigPath != configPath {
			t.Errorf("project.ConfigPath = %q, want %q", project.ConfigPath, configPath)
		}
		if project.Config == nil {
			t.Error("project.Config is nil")
		}
	})

	t.Run("invalid path", func(t *testing.T) {
		_, err := LoadProject("/nonexistent/path")
		if err == nil {
			t.Error("LoadProject should return error for invalid path")
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "invalid")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		configPath := filepath.Join(dir, "config.yaml")
		// Invalid YAML
		if err := os.WriteFile(configPath, []byte("invalid: yaml: content:"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		_, err := LoadProject(dir)
		if err == nil {
			t.Error("LoadProject should return error for invalid config")
		}
	})
}

func TestResolveProjectPathErrors(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() (string, func())
		hasError    bool
		errorCheck  func(error) bool
	}{
		{
			name: "empty path",
			setup: func() (string, func()) {
				return "", func() {}
			},
			hasError: true,
			errorCheck: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "project path is required")
			},
		},
		{
			name: "non-existent path",
			setup: func() (string, func()) {
				return "/nonexistent/path/that/does/not/exist", func() {}
			},
			hasError: true,
			errorCheck: func(err error) bool {
				return err != nil
			},
		},
		{
			name: "directory without config",
			setup: func() (string, func()) {
				tmpDir, _ := os.MkdirTemp("", "sqd-go-no-config-*")
				return tmpDir, func() { os.RemoveAll(tmpDir) }
			},
			hasError: true,
			errorCheck: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "find config.yaml")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, cleanup := tt.setup()
			defer cleanup()

			_, _, err := ResolveProjectPath(path)
			if !tt.hasError {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Error("expected error, got nil")
				}
				if tt.errorCheck != nil && !tt.errorCheck(err) {
					t.Errorf("error check failed: %v", err)
				}
			}
		})
	}
}

func TestLoadFileErrors(t *testing.T) {
	tests := []struct {
		name       string
		setup      func() string
		hasError   bool
		errorCheck func(error) bool
	}{
		{
			name: "non-existent file",
			setup: func() string {
				return "/nonexistent/config.yaml"
			},
			hasError: true,
			errorCheck: func(err error) bool {
				return err != nil
			},
		},
		{
			name: "invalid YAML",
			setup: func() string {
				tmpDir, _ := os.MkdirTemp("", "sqd-go-invalid-yaml-*")
				path := filepath.Join(tmpDir, "config.yaml")
				os.WriteFile(path, []byte("invalid: yaml: [unclosed"), 0644)
				return path
			},
			hasError: true,
			errorCheck: func(err error) bool {
				return err != nil
			},
		},
		{
			name: "unknown field",
			setup: func() string {
				tmpDir, _ := os.MkdirTemp("", "sqd-go-unknown-field-*")
				path := filepath.Join(tmpDir, "config.yaml")
				os.WriteFile(path, []byte("name: test\nunknown_field: value\nchains:\n  - id: 1"), 0644)
				return path
			},
			hasError: true,
			errorCheck: func(err error) bool {
				return err != nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup()

			_, err := LoadFile(path)
			if !tt.hasError {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Error("expected error, got nil")
				}
				if tt.errorCheck != nil && !tt.errorCheck(err) {
					t.Errorf("error check failed: %v", err)
				}
			}
		})
	}
}

func TestForkModeValid(t *testing.T) {
	tests := []struct {
		mode     ForkMode
		expected bool
	}{
		{ForkModeDefault, true},
		{ForkModeSqd, true},
		{ForkModeRingBuffer, true},
		{ForkMode(""), true},
		{ForkMode("invalid"), false},
		{ForkMode("default"), true},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			result := tt.mode.Valid()
			if result != tt.expected {
				t.Errorf("ForkMode(%q).Valid() = %t, want %t", tt.mode, result, tt.expected)
			}
		})
	}
}

// Helper functions
func boolPtr(b bool) *bool {
	return &b
}

func uint64Ptr(u uint64) *uint64 {
	return &u
}
