package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config represents the root schema for the indexer YAML config.
type Config struct {
	Description     *string                `yaml:"description,omitempty" json:"description,omitempty"`
	Name            string                 `yaml:"name" json:"name"`
	Ecosystem       *string                `yaml:"ecosystem,omitempty" json:"ecosystem,omitempty"`
	Fork            ForkMode               `yaml:"fork,omitempty" json:"fork,omitempty"`
	Contracts       []GlobalContractConfig `yaml:"contracts,omitempty" json:"contracts,omitempty"`
	Chains          []Chain                `yaml:"chains" json:"chains"`
	RollbackOnReorg *bool                  `yaml:"rollback_on_reorg,omitempty" json:"rollback_on_reorg,omitempty"`
	RawEvents       *bool                  `yaml:"raw_events,omitempty" json:"raw_events,omitempty"`
	StoreBlocks     *bool                  `yaml:"store_blocks,omitempty" json:"store_blocks,omitempty"`
	StoreRawLogs    *bool                  `yaml:"store_raw_logs,omitempty" json:"store_raw_logs,omitempty"`
	ProtoMode       *bool                  `yaml:"proto_mode,omitempty" json:"proto_mode,omitempty"`
	ColdCache       *bool                  `yaml:"cold_cache,omitempty" json:"cold_cache,omitempty"`
	Compression     string                 `yaml:"compression,omitempty" json:"compression,omitempty"`
	IncludeMetadata []string               `yaml:"include_metadata,omitempty" json:"include_metadata,omitempty"`
	ExcludeMetadata []map[string]string    `yaml:"exclude_metadata,omitempty" json:"exclude_metadata,omitempty"`
	State           []StateConfig          `yaml:"state,omitempty" json:"state,omitempty"`
}

// ForkMode selects the fork-recovery strategy for the ingestion pipeline.
type ForkMode string

const (
	ForkModeDefault    ForkMode = "default"
	ForkModeSqd        ForkMode = "sqd"
	ForkModeRingBuffer ForkMode = "ringbuffer"
)

func (cfg *Config) ForkMode() ForkMode {
	if cfg == nil {
		return ForkModeDefault
	}
	mode := ForkMode(strings.TrimSpace(strings.ToLower(string(cfg.Fork))))
	if mode == "" {
		return ForkModeDefault
	}
	return mode
}

// UsesCollapsingMergeTree reports whether event tables should use
// CollapsingMergeTree(sign). It always returns false: reingestion prunes
// block_number > lastBlock before re-inserting, so a given
// (block_number, log_index) is written exactly once per run and duplicates are
// impossible. Plain MergeTree is therefore sufficient, which also lets rollback
// be N lightweight DELETEs (no sign-flip INSERT, no OPTIMIZE TABLE FINAL).
func (m ForkMode) UsesCollapsingMergeTree() bool {
	return false
}

func (m ForkMode) Valid() bool {
	switch m {
	case "", ForkModeDefault, ForkModeSqd, ForkModeRingBuffer:
		return true
	default:
		return false
	}
}

// GlobalContractConfig defines a contract and its events at the top level,
// shared across all chains.
type GlobalContractConfig struct {
	Name        string        `yaml:"name" json:"name"`
	ABIFilePath *string       `yaml:"abi_file_path,omitempty" json:"abi_file_path,omitempty"`
	Events      []EventConfig `yaml:"events" json:"events"`
}

// EventConfig is one ABI event to index, with optional field omissions.
type EventConfig struct {
	Event string   `yaml:"event" json:"event"`
	Name  *string  `yaml:"name,omitempty" json:"name,omitempty"`
	Omit  []string `yaml:"omit,omitempty" json:"omit,omitempty"`
}

// StateConfig declares a hot-state entity backed by a ClickHouse table.
type StateConfig struct {
	Name        string   `yaml:"name" json:"name"`
	SourceTable string   `yaml:"source_table" json:"source_table"`
	Key         []string `yaml:"key,omitempty" json:"key,omitempty"`
	Mode        string   `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// Chain is one EVM chain to index, with a start block and per-chain contracts.
type Chain struct {
	ID         uint64                `yaml:"id" json:"id"`
	StartBlock uint64                `yaml:"start_block" json:"start_block"`
	EndBlock   *uint64               `yaml:"end_block,omitempty" json:"end_block,omitempty"`
	Contracts  []ChainContractConfig `yaml:"contracts,omitempty" json:"contracts,omitempty"`
}

// ChainContractConfig overrides a global contract for a specific chain.
type ChainContractConfig struct {
	Name    string        `yaml:"name" json:"name"`
	Address Address       `yaml:"address,omitempty" json:"address,omitempty"`
	Events  []EventConfig `yaml:"events,omitempty" json:"events,omitempty"`
}

// Address is one or more hex contract addresses (YAML accepts a single string or a list).
type Address []string

func (a *Address) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.DocumentNode && len(value.Content) == 1 {
		value = value.Content[0]
	}
	if value.Kind == yaml.ScalarNode && value.Tag == "!!str" {
		var s string
		if err := value.Decode(&s); err == nil {
			*a = []string{s}
			return nil
		}
	}
	if value.Kind == yaml.SequenceNode {
		var slice []string
		if err := value.Decode(&slice); err == nil {
			*a = slice
			return nil
		}
	}
	return fmt.Errorf("line %d: address must be a string or a list of strings", value.Line)
}

// Project is a loaded project: the parsed config plus the resolved root directory.
type Project struct {
	Root       string
	ConfigPath string
	Config     *Config
}

// LoadProject loads, parses, and validates a project from the given path
// (directory or config file).
func LoadProject(path string) (*Project, error) {
	root, configPath, err := ResolveProjectPath(path)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadFile(configPath)
	if err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return &Project{Root: root, ConfigPath: configPath, Config: cfg}, nil
}

// ResolveProjectPath returns (root dir, config file path) from a user-supplied
// path that may be a directory or a direct config file reference.
func ResolveProjectPath(path string) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", fmt.Errorf("project path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		for _, name := range []string{"config.yaml", "config.yml"} {
			configPath := filepath.Join(path, name)
			if _, err := os.Stat(configPath); err == nil {
				return path, configPath, nil
			}
		}
		return "", "", fmt.Errorf("find config.yaml or config.yml in %s", path)
	}
	return filepath.Dir(path), path, nil
}

// LoadFile reads and parses a YAML config file with strict field validation.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// strict validation
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.ApplyExcludeMetadata()
	return &cfg, nil
}

func (cfg *Config) ApplyExcludeMetadata() {
	if cfg == nil || len(cfg.ExcludeMetadata) == 0 {
		return
	}
	omittedFields := make(map[string][]string)
	for _, m := range cfg.ExcludeMetadata {
		for eventName, fieldName := range m {
			omittedFields[strings.ToLower(eventName)] = append(omittedFields[strings.ToLower(eventName)], fieldName)
		}
	}
	for i := range cfg.Chains {
		for j := range cfg.Chains[i].Contracts {
			for k := range cfg.Chains[i].Contracts[j].Events {
				ev := &cfg.Chains[i].Contracts[j].Events[k]
				declName := parseEventName(ev.Event)
				if fields, ok := omittedFields[strings.ToLower(declName)]; ok {
					for _, f := range fields {
						found := false
						for _, existing := range ev.Omit {
							if strings.EqualFold(existing, f) {
								found = true
								break
							}
						}
						if !found {
							ev.Omit = append(ev.Omit, f)
						}
					}
				}
			}
		}
	}
}

func parseEventName(sig string) string {
	sig = strings.TrimSpace(sig)
	sig = strings.TrimPrefix(sig, "event ")
	open := strings.IndexByte(sig, '(')
	if open <= 0 {
		return sig
	}
	return strings.TrimSpace(sig[:open])
}

// Validate checks that the config has a name, valid fork mode, and at least one chain.
func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if mode := cfg.ForkMode(); !mode.Valid() {
		return fmt.Errorf("fork must be default, sqd, or ringbuffer")
	}
	if len(cfg.Chains) == 0 {
		return fmt.Errorf("at least one chain is required")
	}
	switch strings.TrimSpace(strings.ToLower(cfg.Compression)) {
	case "", "lz4", "zstd":
	default:
		return fmt.Errorf("compression must be lz4 or zstd")
	}
	for i, chain := range cfg.Chains {
		if chain.ID == 0 {
			return fmt.Errorf("chains[%d].id is required", i)
		}
		if chain.EndBlock != nil && *chain.EndBlock < chain.StartBlock {
			return fmt.Errorf("chains[%d].end_block must be >= start_block", i)
		}
		for j, contract := range chain.Contracts {
			if strings.TrimSpace(contract.Name) == "" {
				return fmt.Errorf("chains[%d].contracts[%d].name is required", i, j)
			}
			if len(contract.Events) == 0 {
				return fmt.Errorf("chains[%d].contracts[%d].events is required", i, j)
			}
			for k, event := range contract.Events {
				if strings.TrimSpace(event.Event) == "" {
					return fmt.Errorf("chains[%d].contracts[%d].events[%d].event is required", i, j, k)
				}
			}
		}
	}
	for i, state := range cfg.State {
		if strings.TrimSpace(state.Name) == "" {
			return fmt.Errorf("state[%d].name is required", i)
		}
		mode := strings.TrimSpace(strings.ToLower(state.Mode))
		switch mode {
		case "", "db_prefetch", "hotstate":
		default:
			return fmt.Errorf("state[%d].mode must be db_prefetch or hotstate", i)
		}
	}
	return nil
}

// ColumnCodec returns the ClickHouse column codec implied by the config's
// `compression` option, or "" for the server default (LZ4). ZSTD(1) measured
// 20-33% smaller on-disk than LZ4 on real event/state tables; higher levels
// and per-type codecs added <=2% on top, so a single codec is deliberate.
func (cfg *Config) ColumnCodec() string {
	if cfg == nil {
		return ""
	}
	switch strings.TrimSpace(strings.ToLower(cfg.Compression)) {
	case "zstd":
		return "ZSTD(1)"
	default:
		return ""
	}
}

func (cfg *Config) ShouldStoreRawLogs() bool {
	if cfg == nil || cfg.StoreRawLogs == nil {
		return false
	}
	return *cfg.StoreRawLogs
}

func (cfg *Config) ShouldStoreBlocks() bool {
	if cfg == nil || cfg.StoreBlocks == nil {
		return false
	}
	return *cfg.StoreBlocks
}

func (cfg *Config) MetadataIncluded(field string) bool {
	if cfg == nil {
		return false
	}
	for _, f := range cfg.IncludeMetadata {
		if strings.EqualFold(f, field) || strings.EqualFold(strings.ReplaceAll(f, "_", ""), strings.ReplaceAll(field, "_", "")) {
			return true
		}
	}
	return false
}
