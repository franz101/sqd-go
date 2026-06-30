package codegen

import (
	"strings"
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

func TestRenderStateScaffoldDerivesERC20NamesFromConfig(t *testing.T) {
	cfg := &config.Config{
		Name: "coin_indexer",
		Chains: []config.Chain{{
			ID: 1,
			Contracts: []config.ChainContractConfig{{
				Name: "Coin",
				Events: []config.EventConfig{{
					Event: "Transfer(address indexed sender, address indexed recipient, uint256 amount)",
				}},
			}},
		}},
	}

	schema, processor, err := RenderStateScaffold(cfg, "coin_indexer", "coin_indexer/generated")
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(schema), "type UserPositionSchema struct")
	for _, want := range []string{
		`generated "coin_indexer/generated"`,
		"*generated.CoinTransfer",
		"transfer.Sender",
		"transfer.Recipient",
		"transfer.Amount",
		"generated.CustomProcessProtoFn = ProcessProto",
		"sqd.RegisterProcessor(generated.ProjectName",
	} {
		assertContains(t, string(processor), want)
	}
	assertNotContains(t, string(processor), "LBTC")
}

func TestRenderStateScaffoldUsesFirstConfiguredEventForGenericProject(t *testing.T) {
	cfg := &config.Config{
		Name: "vault_indexer",
		Chains: []config.Chain{{
			ID: 1,
			Contracts: []config.ChainContractConfig{{
				Name: "Vault",
				Events: []config.EventConfig{{
					Event: "Deposit(address indexed user, uint256 assets)",
				}},
			}},
		}},
	}

	schema, processor, err := RenderStateScaffold(cfg, "vault_indexer", "example/vault/generated")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), "type EntityStateSchema struct") {
		t.Fatalf("generic schema missing EntityStateSchema:\n%s", schema)
	}
	assertContains(t, string(processor), "*generated.VaultDeposit")
	assertNotContains(t, string(processor), "UserPosition")
}
