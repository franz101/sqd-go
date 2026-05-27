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
}

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

func (m ForkMode) UsesCollapsingMergeTree() bool {
	return m == "" || m == ForkModeDefault
}

func (m ForkMode) Valid() bool {
	switch m {
	case "", ForkModeDefault, ForkModeSqd, ForkModeRingBuffer:
		return true
	default:
		return false
	}
}

type GlobalContractConfig struct {
	Name        string        `yaml:"name" json:"name"`
	ABIFilePath *string       `yaml:"abi_file_path,omitempty" json:"abi_file_path,omitempty"`
	Events      []EventConfig `yaml:"events" json:"events"`
}

type EventConfig struct {
	Event string  `yaml:"event" json:"event"`
	Name  *string `yaml:"name,omitempty" json:"name,omitempty"`
}

type Chain struct {
	ID         uint64                `yaml:"id" json:"id"`
	StartBlock uint64                `yaml:"start_block" json:"start_block"`
	EndBlock   *uint64               `yaml:"end_block,omitempty" json:"end_block,omitempty"`
	Contracts  []ChainContractConfig `yaml:"contracts,omitempty" json:"contracts,omitempty"`
}

type ChainContractConfig struct {
	Name    string        `yaml:"name" json:"name"`
	Address Address       `yaml:"address,omitempty" json:"address,omitempty"`
	Events  []EventConfig `yaml:"events,omitempty" json:"events,omitempty"`
}

type Address []string

func (a *Address) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		*a = []string{s}
		return nil
	}
	var slice []string
	if err := value.Decode(&slice); err == nil {
		*a = slice
		return nil
	}
	return fmt.Errorf("line %d: address must be a string or a list of strings", value.Line)
}

type Project struct {
	Root       string
	ConfigPath string
	Config     *Config
}

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
	return &cfg, nil
}

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
	return nil
}
