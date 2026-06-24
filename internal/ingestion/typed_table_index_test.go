package ingestion

import (
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

// TestBuildTypedTableIndexHonorsNameOverride is a regression test for the
// reindex crash where the runtime table resolver ignored an event's `name:`
// override and derived a table name that schema.sql never created
// (UNKNOWN_TABLE on INSERT into exchange_v2_order_filled_events). codegen's
// buildEventSpecs honors the override -> exchange_v2_order_filled_v2_events;
// buildTypedTableIndex must agree.
func TestBuildTypedTableIndexHonorsNameOverride(t *testing.T) {
	override := "OrderFilledV2"
	const addr = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	chain := &config.Chain{
		ID:         137,
		StartBlock: 1,
		Contracts: []config.ChainContractConfig{{
			Name:    "ExchangeV2",
			Address: config.Address{addr},
			Events: []config.EventConfig{{
				Event: "OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint8 side, uint256 tokenId, uint256 makerAmountFilled, uint256 takerAmountFilled, uint256 fee, bytes32 builder, bytes32 metadata)",
				Name:  &override,
			}},
		}},
	}

	idx, err := buildTypedTableIndex(chain)
	if err != nil {
		t.Fatalf("buildTypedTableIndex: %v", err)
	}

	// Decoded logs carry the BASE event name ("OrderFilled"); lookup must still
	// resolve via the base name...
	table, ok := idx.lookup(addr, "OrderFilled")
	if !ok {
		t.Fatal("lookup by base event name failed")
	}
	// ...but the table name must be the override-derived one, matching schema.sql.
	const want = "exchange_v2_order_filled_v2_events"
	if table.Name != want {
		t.Fatalf("table name = %q, want %q (override must be honored, else schema/runtime mismatch)", table.Name, want)
	}
}

// TestBuildTypedTableIndexBaseNameWithoutOverride ensures a non-overridden event
// still resolves to the base-name table (the behavior that already worked for v1).
func TestBuildTypedTableIndexBaseNameWithoutOverride(t *testing.T) {
	const addr = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	chain := &config.Chain{
		ID:         137,
		StartBlock: 1,
		Contracts: []config.ChainContractConfig{{
			Name:    "Exchange",
			Address: config.Address{addr},
			Events: []config.EventConfig{{
				Event: "OrderFilled(bytes32 indexed orderHash, address indexed maker, address indexed taker, uint256 makerAssetId, uint256 takerAssetId, uint256 makerAmountFilled, uint256 takerAmountFilled, uint256 fee)",
			}},
		}},
	}

	idx, err := buildTypedTableIndex(chain)
	if err != nil {
		t.Fatalf("buildTypedTableIndex: %v", err)
	}
	table, ok := idx.lookup(addr, "OrderFilled")
	if !ok {
		t.Fatal("lookup failed")
	}
	const want = "exchange_order_filled_events"
	if table.Name != want {
		t.Fatalf("table name = %q, want %q", table.Name, want)
	}
}
