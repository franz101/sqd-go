package polymarket

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

func TestDevPolymarketSplitMergePrefetchesConditionStateFromEvents(t *testing.T) {
	ctx := context.Background()
	store := newPolymarketIntegrationStore(t, ctx)

	state := generated.NewState()
	user := common.HexToAddress("0x0000000000000000000000000000000000000123")
	taker := common.HexToAddress("0x0000000000000000000000000000000000000456")
	collateral := testSubgraphUSDC
	conditionID := common.HexToHash("0xdb4ab1dbbedd6aeec17aa6e3f66262cff0e3b04742dd3acdf99652575e5422f8")
	questionID := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	yesTokenID := ctfPositionID(state, collateral, common.Hash{}, conditionID, 0)
	noTokenID := ctfPositionID(state, collateral, common.Hash{}, conditionID, 1)

	seedConditionPreparationEvent(t, ctx, store, generated.ConditionalTokensConditionPreparation{
		EventMeta: generated.EventMeta{
			BlockNumber:      10,
			BlockTimestamp:   time.Unix(10, 0).UTC(),
			TransactionIndex: 1,
			LogIndex:         1,
		},
		ConditionID:      conditionID,
		Oracle:           common.HexToAddress("0x0000000000000000000000000000000000000abc"),
		QuestionID:       questionID,
		OutcomeSlotCount: *uint256.NewInt(2),
	})

	block := &generated.ParsedBlock{
		BlockNumber: 100,
		Sequence: []uint8{
			uint8(generated.EventTypeExchangeOrderFilled),
			uint8(generated.EventTypeConditionalTokensPositionSplit),
			uint8(generated.EventTypeConditionalTokensPositionsMerge),
		},
		ExchangeOrderFilleds: []generated.ExchangeOrderFilled{
			{
				EventMeta: generated.EventMeta{
					BlockNumber:      100,
					BlockTimestamp:   time.Unix(100, 0).UTC(),
					TransactionIndex: 1,
					LogIndex:         1,
				},
				Maker:             user,
				Taker:             taker,
				MakerAssetID:      *uint256.NewInt(0),
				TakerAssetID:      yesTokenID,
				MakerAmountFilled: *uint256.NewInt(5_000_000),
				TakerAmountFilled: *uint256.NewInt(10_000_000),
			},
		},
		ConditionalTokensPositionSplits: []generated.ConditionalTokensPositionSplit{
			{
				EventMeta: generated.EventMeta{
					BlockNumber:      100,
					BlockTimestamp:   time.Unix(100, 0).UTC(),
					TransactionIndex: 1,
					LogIndex:         2,
				},
				Stakeholder:        user,
				CollateralToken:    collateral,
				ParentCollectionID: common.Hash{},
				ConditionID:        conditionID,
				Partition:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
				Amount:             *uint256.NewInt(2_000_000),
			},
		},
		ConditionalTokensPositionsMerges: []generated.ConditionalTokensPositionsMerge{
			{
				EventMeta: generated.EventMeta{
					BlockNumber:      100,
					BlockTimestamp:   time.Unix(100, 0).UTC(),
					TransactionIndex: 1,
					LogIndex:         3,
				},
				Stakeholder:        user,
				CollateralToken:    collateral,
				ParentCollectionID: common.Hash{},
				ConditionID:        conditionID,
				Partition:          []uint256.Int{*uint256.NewInt(1), *uint256.NewInt(2)},
				Amount:             *uint256.NewInt(500_000),
			},
		},
	}

	if err := CustomProcessing(ctx, store, state, block); err != nil {
		t.Fatalf("CustomProcessing failed: %v", err)
	}

	if _, ok := state.GetCondition(conditionID); !ok {
		t.Fatalf("expected condition %s to be loaded from condition preparation event state", conditionID)
	}

	yes := state.GetUserPosition(user, yesTokenID)
	if yes == nil {
		t.Fatal("expected YES position to include order fill and split/merge updates")
	}
	assertDecimalEqual(t, "YES amount", yes.Amount, decimal.NewFromInt(11_500_000))
	assertDecimalEqual(t, "YES total_bought", yes.TotalBought, decimal.NewFromInt(12_000_000))

	no := state.GetUserPosition(user, noTokenID)
	if no == nil {
		t.Fatal("expected NO position from split/merge updates")
	}
	assertDecimalEqual(t, "NO amount", no.Amount, decimal.NewFromInt(1_500_000))
	assertDecimalEqual(t, "NO total_bought", no.TotalBought, decimal.NewFromInt(2_000_000))
}

func newPolymarketIntegrationStore(t *testing.T, ctx context.Context) *database.Store {
	t.Helper()

	host := envOr("CLICKHOUSE_HOST", "127.0.0.1")
	port := envIntOr("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOr("CLICKHOUSE_USER", "default")
	password := envOr("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("polymarket_it_%d", time.Now().UnixNano())

	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse integration test requires ClickHouse at %s:%d: %v", host, port, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = database.DropClickHouseDatabase(context.Background(), host, port, user, password, db)
	})

	for _, name := range []string{"schema.sql", "custom_schema.sql"} {
		path := filepath.Join("generated", name)
		if err := store.ApplySQLFileWithDatabase(ctx, path, "polymarket"); err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
	}
	return store
}

func seedConditionPreparationEvent(t *testing.T, ctx context.Context, store *database.Store, ev generated.ConditionalTokensConditionPreparation) {
	t.Helper()
	batch := generated.NewConditionalTokensConditionPreparationBatch()
	batch.Append(ev.EventMeta, &ev)
	if err := generated.InsertEventBatch(ctx, store.Conn(), store.DB(), batch); err != nil {
		t.Fatalf("insert condition preparation event: %v", err)
	}
}

func assertDecimalEqual(t *testing.T, label string, got, want decimal.Decimal) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s = %s, want %s", label, got, want)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envIntOr(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}
