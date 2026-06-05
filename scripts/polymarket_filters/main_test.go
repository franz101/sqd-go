package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

func TestPolymarketNetworkBoundaryAtNegRiskAdapterStart(t *testing.T) {
	project, err := config.LoadProject(repoPath("examples/polymarket"))
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	if len(project.Config.Chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(project.Config.Chains))
	}
	deployments, err := loadDeployments(repoPath("examples/polymarket/networks.yaml"))
	if err != nil {
		t.Fatalf("load networks: %v", err)
	}
	abiTopics := loadABITopics(repoPath("examples/polymarket/abis"))

	const block = uint64(50505403)
	rows := buildRows(project.Config.Chains[0].Contracts, deployments, abiTopics, block, block)

	for _, event := range []string{
		"MarketPrepared",
		"QuestionPrepared",
		"PositionSplit",
		"PositionsMerge",
		"PositionsConverted",
		"PayoutRedemption",
	} {
		row := requireRow(t, rows, "NegRiskAdapter", event)
		if row.StartBlock != block {
			t.Fatalf("NegRiskAdapter %s start block = %d, want %d", event, row.StartBlock, block)
		}
		if !row.Active {
			t.Fatalf("NegRiskAdapter %s should be active at block %d", event, block)
		}
		if row.ABIStatus != "ok" {
			t.Fatalf("NegRiskAdapter %s ABI status = %s, want ok", event, row.ABIStatus)
		}
		if !strings.EqualFold(row.ConfigTop, row.ABITopic) {
			t.Fatalf("NegRiskAdapter %s topic mismatch: config=%s abi=%s", event, row.ConfigTop, row.ABITopic)
		}
	}

	negRiskExchange := requireRow(t, rows, "NegRiskExchange", "OrderFilled")
	if negRiskExchange.StartBlock != 50505492 {
		t.Fatalf("NegRiskExchange start block = %d, want 50505492", negRiskExchange.StartBlock)
	}
	if negRiskExchange.Active {
		t.Fatalf("NegRiskExchange should be inactive at block %d", block)
	}
	if !strings.EqualFold(negRiskExchange.ConfigTop, negRiskExchange.ABITopic) {
		t.Fatalf("NegRiskExchange topic mismatch: config=%s abi=%s", negRiskExchange.ConfigTop, negRiskExchange.ABITopic)
	}
}

func requireRow(t *testing.T, rows []reportRow, contract, event string) reportRow {
	t.Helper()
	for _, row := range rows {
		if row.Contract == contract && row.Event == event {
			return row
		}
	}
	t.Fatalf("missing row for %s.%s", contract, event)
	return reportRow{}
}

func repoPath(parts ...string) string {
	all := append([]string{"..", ".."}, parts...)
	return filepath.Join(all...)
}
