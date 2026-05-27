package templating

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/franz101/sqd-go/internal/config"
	"gopkg.in/yaml.v3"
)

type TemplateOptions struct {
	Name     string
	Template string
	APIToken string
}

type ContractImportOptions struct {
	Name           string
	ABIFile        string
	ContractName   string
	Blockchain     string
	RPCURL         string
	StartBlock     uint64
	SingleContract bool
	AllEvents      bool
	Address        string
	APIToken       string
}

func InitTemplate(opts TemplateOptions) (string, error) {
	if opts.Name == "" {
		opts.Name = "sqd_indexer"
	}
	if opts.Template == "" {
		opts.Template = "erc20"
	}
	if opts.Template != "erc20" {
		return "", fmt.Errorf("unsupported template %q", opts.Template)
	}
	name := projectName(opts.Name)

	cfg := config.Config{
		Name:      name,
		Ecosystem: stringPtr("evm"),
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: 0,
			Contracts: []config.ChainContractConfig{{
				Name:    "ERC20",
				Address: config.Address{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"},
				Events: []config.EventConfig{{
					Event: "Transfer(address indexed from, address indexed to, uint256 value)",
				}},
			}},
		}},
	}
	return writeProject(opts.Name, &cfg, opts.APIToken)
}

func InitContractImport(opts ContractImportOptions) (string, error) {
	if opts.Name == "" {
		opts.Name = "sqd_indexer"
	}
	if opts.ABIFile == "" {
		return "", fmt.Errorf("--abi-file is required")
	}
	if opts.ContractName == "" {
		return "", fmt.Errorf("--contract-name is required")
	}
	name := projectName(opts.Name)
	chainID, err := chainIDFromName(opts.Blockchain)
	if err != nil {
		return "", err
	}
	events, err := eventsFromABIFile(opts.ABIFile)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", fmt.Errorf("no events found in %s", opts.ABIFile)
	}

	cfg := config.Config{
		Name:      name,
		Ecosystem: stringPtr("evm"),
		Chains: []config.Chain{{
			ID:         chainID,
			StartBlock: opts.StartBlock,
			Contracts: []config.ChainContractConfig{{
				Name:    opts.ContractName,
				Address: config.Address{opts.Address},
				Events:  events,
			}},
		}},
	}
	projectRoot, err := writeProject(opts.Name, &cfg, opts.APIToken)
	if err != nil {
		return "", err
	}

	abiDir := filepath.Join(projectRoot, "abis")
	if err := os.MkdirAll(abiDir, 0o755); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(opts.ABIFile)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(abiDir, filepath.Base(opts.ABIFile)), raw, 0o644); err != nil {
		return "", err
	}
	return projectRoot, nil
}

func writeProject(root string, cfg *config.Config, apiToken string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	configPath := filepath.Join(root, "config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return "", fmt.Errorf("%s already exists", configPath)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "schema.graphql"), []byte("type Event @entity {\n  id: ID!\n}\n"), 0o644); err != nil {
		return "", err
	}
	env := "CLICKHOUSE_HTTP_PORT=8123\nCLICKHOUSE_NATIVE_PORT=9000\nCLICKHOUSE_USER=default\nCLICKHOUSE_PASSWORD=sqd-clickhouse\n"
	if apiToken != "" {
		env += "SQD_API_TOKEN=" + apiToken + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(env), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

func eventsFromABIFile(path string) ([]config.EventConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse abi json: %w", err)
	}

	parsed, err := abi.JSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("parse abi: %w", err)
	}

	var events []config.EventConfig
	for _, entry := range entries {
		if entry.Type != "event" || entry.Name == "" {
			continue
		}
		ev, ok := parsed.Events[entry.Name]
		if !ok {
			continue
		}
		var args []string
		for _, input := range ev.Inputs {
			part := input.Type.String()
			if input.Indexed {
				part += " indexed"
			}
			if input.Name != "" {
				part += " " + input.Name
			}
			args = append(args, part)
		}
		events = append(events, config.EventConfig{
			Event: fmt.Sprintf("%s(%s)", ev.Name, strings.Join(args, ", ")),
		})
	}
	return events, nil
}

func chainIDFromName(name string) (uint64, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "ethereum", "ethereum-mainnet", "mainnet", "1":
		return 1, nil
	case "polygon", "polygon-mainnet", "matic", "137":
		return 137, nil
	default:
		id, err := strconv.ParseUint(name, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("unsupported blockchain %q", name)
		}
		return id, nil
	}
}

func stringPtr(v string) *string {
	return &v
}

func projectName(root string) string {
	name := filepath.Base(filepath.Clean(root))
	if name == "." || name == string(filepath.Separator) {
		return "sqd_indexer"
	}
	return name
}
