package polymarket

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
	"github.com/klauspost/compress/zstd"
)

const v2FixturePath = "debugger/data/polymarket_v2_orderfilled/blocks_87200028_87200177.jsonl.zstd"

// zstdMagic identifies a zstd frame (RFC 8478 §3.1.1).
var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

func TestPolymarketV2RealWorldParity(t *testing.T) {
	data := loadV2Fixture(t)

	parsedState := generated.NewState()
	parsedState.SetSnapshotsEnabled(false)
	parsedRing, err := generated.NewOrderedHistoricRingBuffer(16)
	if err != nil {
		t.Fatal(err)
	}
	parsedEvents, err := generated.ParseJSONLV2(data, nil, parsedRing, func(block *generated.ParsedBlock) error {
		return Process(parsedState, block)
	})
	if err != nil {
		t.Fatalf("parsed-mode replay: %v", err)
	}

	protoState := generated.NewState()
	protoState.SetSnapshotsEnabled(false)
	protoRing, err := generated.NewProtoRingBuffer(16)
	if err != nil {
		t.Fatal(err)
	}
	protoEvents, err := generated.ParseJSONLProto(data, nil, protoRing, func(block *generated.ProtoEventBlock) error {
		return ProcessProto(protoState, block)
	})
	if err != nil {
		t.Fatalf("proto-mode replay: %v", err)
	}

	if parsedEvents != 4 || protoEvents != parsedEvents {
		t.Fatalf("event count mismatch: parsed=%d proto=%d want=4", parsedEvents, protoEvents)
	}
	parsedPositions := snapshotV2Positions(parsedState)
	protoPositions := snapshotV2Positions(protoState)
	if !reflect.DeepEqual(parsedPositions, protoPositions) {
		t.Fatalf("parsed/proto state mismatch:\nparsed=%#v\nproto=%#v", parsedPositions, protoPositions)
	}

	assertV2Position(t, protoPositions,
		common.HexToAddress("0xf1f0e9fb4823c0cff89c9cb3e82760c73370d2e6"),
		common.HexToHash("0x3f3a150ca34ddad4f33e8326d16c99be37d5f458ff300728aea2d70040be32c6"),
		"0.009228000000000000",
		"0.649999993499999413",
		"9.843200199940018056",
		"30.769228000000000000",
		87200064,
	)
	assertV2Position(t, protoPositions,
		common.HexToAddress("0xf3338c0f5c52e48fbe883d731226b7820e70ba41"),
		common.HexToHash("0x2a295a29a116c21f106de500e88acb37e5093a8ad69357d1f7b3c5248b639d97"),
		"0.000000000000000000",
		"0.760000000000000000",
		"-5.000000000000000000",
		"500.000000000000000000",
		87200177,
	)
}

func TestPolymarketV2RejectsInvalidSide(t *testing.T) {
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)

	handleOrderFilledV2Values(
		state,
		common.HexToAddress("0xf000000000000000000000000000000000000001"),
		*uint256.NewInt(2),
		*uint256.NewInt(1),
		*uint256.NewInt(1_000_000),
		*uint256.NewInt(1_000_000),
		generated.EventMeta{BlockNumber: 1},
	)

	if positions := snapshotV2Positions(state); len(positions) != 0 {
		t.Fatalf("invalid v2 side mutated positions: %#v", positions)
	}
}

func TestPolymarketV2ExchangeStakeholdersAreIgnored(t *testing.T) {
	systemAddresses := []common.Address{
		exchangeAddr,
		negRiskExchangeAddr,
		exchangeV2Addr,
		negRiskExchangeV2Addr,
		negRiskAdapterAddr,
	}
	systemAddresses = append(systemAddresses, ctfCollateralAdapters[:]...)
	systemAddresses = append(systemAddresses, negRiskCollateralAdapters[:]...)
	for _, address := range systemAddresses {
		if !isIgnoredStakeholder(address) {
			t.Fatalf("system stakeholder %s was not ignored", address)
		}
	}
	if isIgnoredStakeholder(common.HexToAddress("0xf000000000000000000000000000000000000001")) {
		t.Fatal("ordinary user address was ignored")
	}
}

func TestPolymarketSupportedCollateral(t *testing.T) {
	for _, collateral := range supportedCollateral {
		if !isSupportedCollateral(collateral) {
			t.Fatalf("supported collateral %s was rejected", collateral)
		}
	}
	if !isSupportedCollateral(negRiskWrappedCollateral) {
		t.Fatalf("wrapped neg-risk collateral %s was rejected", negRiskWrappedCollateral)
	}
	if isSupportedCollateral(common.HexToAddress("0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270")) {
		t.Fatal("WMATIC must not be interpreted as 6-decimal Polymarket collateral")
	}
}

func TestPolymarketV2ClickHouseE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ClickHouse integration test in short mode")
	}

	data := loadV2Fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := setupV2TestClickHouse(t, ctx)
	batches := generated.NewInsertBatches()
	ring, err := generated.NewProtoRingBuffer(16)
	if err != nil {
		t.Fatal(err)
	}
	state := generated.NewState()
	state.SetSnapshotsEnabled(false)
	events, err := generated.ParseJSONLProto(data, batches, ring, func(block *generated.ProtoEventBlock) error {
		return ProcessProto(state, block)
	})
	if err != nil {
		t.Fatalf("parse/process fixture: %v", err)
	}
	if events != 4 {
		t.Fatalf("parsed events=%d, want 4", events)
	}
	if err := batches.Insert(ctx, store.Conn(), store.DB()); err != nil {
		t.Fatalf("insert v2 event batches: %v", err)
	}

	got := queryV2Accounts(t, ctx, store)
	want := []v2AccountCount{
		{Maker: "f1f0e9fb4823c0cff89c9cb3e82760c73370d2e6", Fills: 2},
		{Maker: "f3338c0f5c52e48fbe883d731226b7820e70ba41", Fills: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ClickHouse v2 account query mismatch: got %#v, want %#v", got, want)
	}
}

type v2PositionKey struct {
	User    common.Address
	TokenID common.Hash
}

func snapshotV2Positions(state *generated.State) map[v2PositionKey]generated.MemoryUserPosition {
	positions := make(map[v2PositionKey]generated.MemoryUserPosition)
	state.HotState.UserPositions.Range(func(_ generated.UserPositionsClockKey, position generated.MemoryUserPosition) bool {
		if !position.Tombstone {
			positions[v2PositionKey{User: position.User, TokenID: position.TokenID}] = position
		}
		return true
	})
	return positions
}

func assertV2Position(
	t *testing.T,
	positions map[v2PositionKey]generated.MemoryUserPosition,
	user common.Address,
	tokenID common.Hash,
	amount string,
	avgPrice string,
	realizedPnL string,
	totalBought string,
	block uint64,
) {
	t.Helper()
	position, ok := positions[v2PositionKey{User: user, TokenID: tokenID}]
	if !ok {
		t.Fatalf("missing position user=%s token=%s", user, tokenID)
	}
	got := []string{
		position.Amount.String(protomath.Decimal256Scale18),
		position.AvgPrice.String(protomath.Decimal256Scale18),
		position.RealizedPnL.String(protomath.Decimal256Scale18),
		position.TotalBought.String(protomath.Decimal256Scale18),
	}
	want := []string{amount, avgPrice, realizedPnL, totalBought}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("position user=%s token=%s: got %v, want %v", user, tokenID, got, want)
	}
	if position.UpdatedAtBlock != block || position.BlockNumber != block {
		t.Fatalf("position block user=%s token=%s: updated=%d block=%d want=%d",
			user, tokenID, position.UpdatedAtBlock, position.BlockNumber, block)
	}
}

func loadV2Fixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(findProjectRoot(), v2FixturePath)
	raw, err := os.ReadFile(path)
	if err != nil {
		// Fall back to an uncompressed sibling so the fixture works either way.
		if plain, ok := strings.CutSuffix(path, ".zstd"); ok {
			if data, plainErr := os.ReadFile(plain); plainErr == nil {
				return data
			}
		}
		// The fixture is not committed (debugger/data/ is gitignored); regenerate
		// it with scripts/fetch_v2_fixture.sh. Skip rather than fail when absent.
		if os.IsNotExist(err) {
			t.Skipf("v2 fixture %s missing; run scripts/fetch_v2_fixture.sh to generate it", v2FixturePath)
		}
		t.Fatalf("read v2 fixture %s: %v", path, err)
	}
	return maybeDecompressZstd(t, raw)
}

// maybeDecompressZstd transparently inflates a zstd frame, returning the input
// unchanged when it is already plain JSONL. Detection is by magic number so the
// fixture can be stored compressed or raw without the test caring which.
func maybeDecompressZstd(t *testing.T, raw []byte) []byte {
	t.Helper()
	if !bytes.HasPrefix(raw, zstdMagic) {
		return raw
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("create zstd decoder: %v", err)
	}
	defer dec.Close()
	data, err := dec.DecodeAll(raw, nil)
	if err != nil {
		t.Fatalf("decompress v2 fixture: %v", err)
	}
	return data
}

func setupV2TestClickHouse(t *testing.T, ctx context.Context) *database.Store {
	t.Helper()
	host := envOrDefault("CLICKHOUSE_HOST", "127.0.0.1")
	port := envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9003)
	user := envOrDefault("CLICKHOUSE_USER", "default")
	password := envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := fmt.Sprintf("polymarket_v2_e2e_%d", time.Now().UnixNano())
	if !strings.HasPrefix(db, "polymarket_v2_e2e_") {
		t.Fatal("refusing to use a non-temporary ClickHouse database")
	}

	store, err := database.NewClickHouse(ctx, host, port, user, password, db)
	if err != nil {
		t.Skipf("ClickHouse unavailable at %s:%d: %v", host, port, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		if !strings.HasPrefix(db, "polymarket_v2_e2e_") {
			t.Errorf("refusing to clean up non-temporary database %q", db)
			return
		}
		if err := database.DropClickHouseDatabase(context.Background(), host, port, user, password, db); err != nil {
			t.Logf("drop temporary ClickHouse database %s: %v", db, err)
		}
	})

	schema := filepath.Join(findProjectRoot(), "examples/polymarket/generated/schema.sql")
	if err := store.ApplySQLFileWithDatabase(ctx, schema, "polymarket"); err != nil {
		t.Fatalf("apply generated schema: %v", err)
	}
	return store
}

type v2AccountCount struct {
	Maker string
	Fills uint64
}

func queryV2Accounts(t *testing.T, ctx context.Context, store *database.Store) []v2AccountCount {
	t.Helper()
	var makers proto.ColStr
	var fills proto.ColUInt64
	query := fmt.Sprintf(`
		SELECT lower(hex(maker)) AS maker_hex, count() AS fills
		FROM (
			SELECT maker FROM %s.exchange_v2_order_filled_v2_events
			UNION ALL
			SELECT maker FROM %s.neg_risk_exchange_v2_order_filled_v2_events
		)
		WHERE startsWith(lower(hex(maker)), 'f')
		GROUP BY maker
		ORDER BY maker_hex`,
		quoteV2Ident(store.DB()),
		quoteV2Ident(store.DB()),
	)
	var accounts []v2AccountCount
	err := store.Conn().Do(ctx, ch.Query{
		Body: query,
		Result: proto.Results{
			{Name: "maker_hex", Data: &makers},
			{Name: "fills", Data: &fills},
		},
		OnResult: func(_ context.Context, block proto.Block) error {
			for i := 0; i < block.Rows; i++ {
				accounts = append(accounts, v2AccountCount{Maker: makers.Row(i), Fills: fills[i]})
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query v2 accounts: %v", err)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Maker < accounts[j].Maker })
	return accounts
}

func quoteV2Ident(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}
