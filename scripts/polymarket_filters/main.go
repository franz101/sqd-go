package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/franz101/sqd-go/internal/client"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/parser"
	"gopkg.in/yaml.v3"
)

type networksFile struct {
	Matic network `yaml:"matic"`
}

type network struct {
	Contracts map[string]deployment `yaml:"contracts"`
}

type deployment struct {
	Address    string `yaml:"address"`
	StartBlock uint64 `yaml:"startBlock"`
}

type reportRow struct {
	Contract   string
	Event      string
	Address    []string
	StartBlock uint64
	Active     bool
	ConfigTop  string
	ABITopic   string
	ABIStatus  string
}

func main() {
	projectPath := flag.String("project", "examples/polymarket", "project directory or config file")
	networksPath := flag.String("networks", "examples/polymarket/networks.yaml", "networks.yaml path")
	abisDir := flag.String("abis", "examples/polymarket/abis", "ABI directory")
	from := flag.Uint64("from", 0, "optional first block for active/inactive report")
	to := flag.Uint64("to", 0, "optional last block for active/inactive report")
	flag.Parse()

	project, err := config.LoadProject(*projectPath)
	if err != nil {
		log.Fatalf("load project: %v", err)
	}
	if len(project.Config.Chains) == 0 {
		log.Fatal("project has no chains")
	}
	chain := project.Config.Chains[0]

	deployments, err := loadDeployments(*networksPath)
	if err != nil {
		log.Fatalf("load networks: %v", err)
	}
	abiTopics := loadABITopics(*abisDir)

	_, filters, err := parser.BuildEventDecoder(chain.Contracts)
	if err != nil {
		log.Fatalf("build filters: %v", err)
	}

	fmt.Printf("project=%s chain=%d filters=%d", project.Config.Name, chain.ID, len(filters))
	if *from > 0 || *to > 0 {
		fmt.Printf(" range=%d-%d", *from, *to)
	}
	fmt.Println()
	fmt.Println()

	rows := buildRows(chain.Contracts, deployments, abiTopics, *from, *to)
	printRows(rows)
	fmt.Println()
	printPortalFilters(filters)
}

func loadDeployments(path string) (map[string]deployment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var parsed networksFile
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	return parsed.Matic.Contracts, nil
}

func loadABITopics(dir string) map[string]map[string]string {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		log.Fatalf("glob ABIs: %v", err)
	}
	out := make(map[string]map[string]string)
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("read ABI %s: %v", path, err)
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		abiJSON := extractABIJSON(path, raw)
		parsed, err := abi.JSON(strings.NewReader(string(abiJSON)))
		if err != nil {
			log.Fatalf("parse ABI %s: %v", path, err)
		}
		out[name] = make(map[string]string)
		for eventName, event := range parsed.Events {
			out[name][eventName] = event.ID.Hex()
		}

		// Keep a cheap validity check so corrupt ABI JSON fails loudly even when
		// geth accepts an empty event set.
		var rawJSON any
		if err := json.Unmarshal(raw, &rawJSON); err != nil {
			log.Fatalf("validate ABI JSON %s: %v", path, err)
		}
	}
	return out
}

func extractABIJSON(path string, raw []byte) []byte {
	var artifact struct {
		ABI json.RawMessage `json:"abi"`
	}
	if err := json.Unmarshal(raw, &artifact); err == nil && len(artifact.ABI) > 0 {
		return artifact.ABI
	}
	var rawJSON any
	if err := json.Unmarshal(raw, &rawJSON); err != nil {
		log.Fatalf("validate ABI JSON %s: %v", path, err)
	}
	return raw
}

func buildRows(contracts []config.ChainContractConfig, deployments map[string]deployment, abiTopics map[string]map[string]string, from, to uint64) []reportRow {
	var rows []reportRow
	for _, contract := range contracts {
		dep := deploymentFor(contract.Name, deployments)
		abiName := abiNameFor(contract.Name, abiTopics)
		for _, event := range contract.Events {
			eventName := eventName(event.Event)
			configTopic := configTopic(contract, event)
			abiTopic, abiStatus := topicFromABI(abiName, eventName, abiTopics)
			rows = append(rows, reportRow{
				Contract:   contract.Name,
				Event:      eventName,
				Address:    []string(contract.Address),
				StartBlock: dep.StartBlock,
				Active:     activeInRange(dep.StartBlock, from, to),
				ConfigTop:  configTopic,
				ABITopic:   abiTopic,
				ABIStatus:  abiStatus,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].StartBlock != rows[j].StartBlock {
			return rows[i].StartBlock < rows[j].StartBlock
		}
		if rows[i].Contract != rows[j].Contract {
			return rows[i].Contract < rows[j].Contract
		}
		return rows[i].Event < rows[j].Event
	})
	return rows
}

func deploymentFor(contract string, deployments map[string]deployment) deployment {
	if dep, ok := deployments[contract]; ok {
		return dep
	}
	if contract == "FixedProductMarketMaker" {
		return deployments["FixedProductMarketMakerFactory"]
	}
	return deployment{}
}

func abiNameFor(contract string, abiTopics map[string]map[string]string) string {
	if _, ok := abiTopics[contract]; ok {
		return contract
	}
	if contract == "NegRiskExchange" {
		return "Exchange"
	}
	return contract
}

func topicFromABI(abiName, eventName string, abiTopics map[string]map[string]string) (string, string) {
	events, ok := abiTopics[abiName]
	if !ok {
		return "", "missing ABI"
	}
	topic, ok := events[eventName]
	if !ok {
		return "", "missing event"
	}
	return topic, "ok"
}

func configTopic(contract config.ChainContractConfig, event config.EventConfig) string {
	_, filters, err := parser.BuildEventDecoder([]config.ChainContractConfig{{
		Name:    contract.Name,
		Address: contract.Address,
		Events:  []config.EventConfig{event},
	}})
	if err != nil {
		return "error: " + err.Error()
	}
	for _, filter := range filters {
		if len(filter.Topic0) > 0 {
			return filter.Topic0[0]
		}
	}
	return ""
}

func activeInRange(startBlock, from, to uint64) bool {
	if from == 0 && to == 0 {
		return true
	}
	if to == 0 {
		to = from
	}
	if startBlock == 0 {
		return true
	}
	return to >= startBlock
}

func eventName(sig string) string {
	if idx := strings.IndexByte(sig, '('); idx >= 0 {
		return sig[:idx]
	}
	return sig
}

func printRows(rows []reportRow) {
	fmt.Println("events:")
	for _, row := range rows {
		status := "active"
		if !row.Active {
			status = "inactive"
		}
		address := strings.Join(row.Address, ",")
		if address == "" {
			address = "*"
		}
		topicStatus := "match"
		if row.ABIStatus != "ok" {
			topicStatus = row.ABIStatus
		} else if row.ConfigTop != "" && !strings.EqualFold(row.ConfigTop, row.ABITopic) {
			topicStatus = "mismatch"
		}
		fmt.Printf("- %-31s %-38s start=%-8d %-8s address=%s abi=%s topic=%s\n",
			row.Contract, row.Event, row.StartBlock, status, address, topicStatus, nonEmpty(row.ABITopic, row.ConfigTop))
	}
}

func printPortalFilters(filters []client.LogFilter) {
	fmt.Println("portal filters:")
	for i, filter := range filters {
		address := strings.Join(filter.Address, ",")
		if address == "" {
			address = "*"
		}
		fmt.Printf("- %d address=%s topic0=%s\n", i, address, strings.Join(filter.Topic0, ","))
	}
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
