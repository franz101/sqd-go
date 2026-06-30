package codegen

import (
	"strings"
	"testing"

	"github.com/franz101/sqd-go/internal/config"
)

// TestPrefetchStateGeneric verifies the generated state-prefetch functions are
// driven purely by config (state + matching event args) and contain no
// project-specific scaffolding. Regression guard for the previously hardcoded
// Polymarket block (ExchangeOrderFilled / UserPositions / computed TokenID).
func TestPrefetchStateGeneric(t *testing.T) {
	hotStateTables := []customTableSpec{
		{
			Name:       "holder_balances",
			GoTypeName: "MemoryHolderBalance",
			IsEvent:    false,
			Fields: []customFieldSpec{
				{Name: "Holder", Type: "common.Address", ColumnName: "holder", ColumnType: "FixedString(20)"},
				{Name: "Balance", Type: "uint256.Int", ColumnName: "balance", ColumnType: "UInt256"},
			},
			PrimaryKey: []string{"holder"},
		},
	}
	events := []eventSpec{
		{
			EventName:  "Transfer",
			GoTypeName: "Transfer",
			Topic0:     "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
			Args: []eventArg{
				{Name: "holder", SolidityType: "address", Indexed: true, GoFieldName: "Holder", GoType: "common.Address", ColumnName: "holder"},
				{Name: "amount", SolidityType: "uint256", Indexed: false, GoFieldName: "Amount", GoType: "uint256.Int", ColumnName: "amount"},
			},
		},
	}
	cfg := &config.Config{
		Name:  "test_project",
		State: []config.StateConfig{{Name: "HolderBalance", SourceTable: "holder_balances"}},
	}

	out, err := generateEmptyCustomProcessorGo(cfg, events, hotStateTables)
	if err != nil {
		// A non-nil error means the generated source failed to parse/format.
		t.Fatalf("generateEmptyCustomProcessorGo: %v", err)
	}
	s := string(out)

	// Generic, config-driven prefetch must be present.
	for _, needle := range []string{
		"func prefetchBlocksState(ctx context.Context, store Store, state *State, blocks []*ParsedBlock) error {",
		"for _, ev := range block.Transfers {",
		"hot.HolderBalancesResolver.Queue(HolderBalancesClockKey{Holder: ev.Holder})",
		"hot.HolderBalancesResolver.Resolve(ctx, store.Conn(), store.DB())",
		`return fmt.Errorf("prefetch HolderBalance: %w", err)`,
		// proto variant
		"func prefetchProtoBlocksState(ctx context.Context, store Store, state *State, blocks []*ProtoEventBlock) error {",
		"block.QueryTransfer().Map(func(ev TransferProtoView) {",
		"hot.HolderBalancesResolver.Queue(HolderBalancesClockKey{Holder: ev.Holder()})",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("generated prefetch missing generic code: %q", needle)
		}
	}

	// No project-specific scaffolding may leak into the generic generator.
	for _, banned := range []string{
		"ExchangeOrderFilled",
		"NegRiskExchangeOrderFilled",
		"UserPositionsResolver",
		"UserPositionsClockKey",
		"MakerAssetID",
		"TakerAssetID",
		"prefetch Position",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("generated prefetch leaked project-specific scaffolding: %q", banned)
		}
	}
}
