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
