package subgraph

import (
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

// Config holds the addresses for the contracts used by the subgraph.
type Config struct {
	NegRiskAdapter           common.Address
	NegRiskExchange          common.Address
	Exchange                 common.Address
	negRiskWrappedCollateral common.Address
}

type rawConfig struct {
	Contracts map[string]struct {
		Address string `yaml:"address"`
	} `yaml:"contracts"`
}

// LoadConfig loads the configuration from a YAML file.
func LoadConfig(networksPath, network string) (*Config, error) {
	data, err := os.ReadFile(networksPath)
	if err != nil {
		return nil, fmt.Errorf("read networks config file: %w", err)
	}

	var parsed map[string]rawConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse networks yaml: %w", err)
	}

	netConf, ok := parsed[network]
	if !ok {
		return nil, fmt.Errorf("network %q not found in config", network)
	}

	getAddr := func(name string) (common.Address, error) {
		contract, ok := netConf.Contracts[name]
		if !ok || contract.Address == "" {
			return common.Address{}, fmt.Errorf("contract %q not found for network %q", name, network)
		}
		return common.HexToAddress(contract.Address), nil
	}

	negRiskAdapter, err := getAddr("NegRiskAdapter")
	if err != nil {
		return nil, err
	}
	negRiskExchange, err := getAddr("NegRiskExchange")
	if err != nil {
		return nil, err
	}
	exchange, err := getAddr("Exchange")
	if err != nil {
		return nil, err
	}
	negRiskWrappedCollateral, err := getAddr("NegRiskWrappedCollateral")
	if err != nil {
		return nil, err
	}

	return &Config{
		NegRiskAdapter:           negRiskAdapter,
		NegRiskExchange:          negRiskExchange,
		Exchange:                 exchange,
		negRiskWrappedCollateral: negRiskWrappedCollateral,
	}, nil
}
