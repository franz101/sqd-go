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
	Contracts       []GlobalContractConfig `yaml:"contracts,omitempty" json:"contracts,omitempty"`
	Chains          []Chain                `yaml:"chains" json:"chains"`
	RollbackOnReorg *bool                  `yaml:"rollback_on_reorg,omitempty" json:"rollback_on_reorg,omitempty"`
	RawEvents       *bool                  `yaml:"raw_events,omitempty" json:"raw_events,omitempty"`
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
	Address any           `yaml:"address,omitempty" json:"address,omitempty"`
	Events  []EventConfig `yaml:"events,omitempty" json:"events,omitempty"`
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
	if err := yaml.Unmarshal(data, &cfg); err != nil {
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
