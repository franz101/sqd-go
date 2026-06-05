package generated

import (
	"context"
	"math/big"
	"testing"

	ch "github.com/ClickHouse/ch-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

type mockStore struct{}

func (s *mockStore) Conn() *ch.Client { return nil }
func (s *mockStore) DB() string       { return "test_db" }

func TestCustomProcessingE2E(t *testing.T) {
	// Reset global state
	oldState := GlobalState
	GlobalState = NewMemoryState()
	defer func() { GlobalState = oldState }()

	ctx := context.Background()
	from := common.HexToAddress("0x1000000000000000000000000000000000000001")
	to := common.HexToAddress("0x2000000000000000000000000000000000000002")
	value := *uint256.NewInt(1000)

	entities := &Entities{
		LBTCTransfer: []LBTCTransfer{
			{
				From:  from,
				To:    to,
				Value: value,
				EventMeta: EventMeta{
					BlockNumber:      100,
					ContractAddress:  common.HexToAddress(LBTCTransferAddress),
					TransactionHash:  common.HexToHash("0xabc"),
					TransactionIndex: 0,
					LogIndex:         0,
				},
			},
		},
		BlockNumber: 100,
	}

	err := CustomProcessing(ctx, &mockStore{}, entities)
	if err != nil {
		t.Fatalf("CustomProcessing failed: %v", err)
	}

	// Verify the sender was debited
	fromPos, fromFound := GlobalState.Position(from)
	if !fromFound {
		t.Fatal("expected sender position to exist")
	}
	if fromPos.TransferCount != 1 {
		t.Errorf("expected sender transfer count 1, got %d", fromPos.TransferCount)
	}
	expectedFromBalance := big.NewInt(-1000)
	if fromPos.Balance.Cmp(expectedFromBalance) != 0 {
		t.Errorf("expected sender balance %s, got %s", expectedFromBalance.String(), fromPos.Balance.String())
	}

	// Verify the receiver was credited
	toPos, toFound := GlobalState.Position(to)
	if !toFound {
		t.Fatal("expected receiver position to exist")
	}
	if toPos.TransferCount != 1 {
		t.Errorf("expected receiver transfer count 1, got %d", toPos.TransferCount)
	}
	if toPos.Balance.Cmp(value.ToBig()) != 0 {
		t.Errorf("expected receiver balance %s, got %s", value.Dec(), toPos.Balance.String())
	}

	// Verify LastSyncBlock was updated
	if GlobalState.LastSyncBlock != 100 {
		t.Errorf("expected LastSyncBlock 100, got %d", GlobalState.LastSyncBlock)
	}
}

func TestCustomProcessingE2E_MultipleTransfers(t *testing.T) {
	oldState := GlobalState
	GlobalState = NewMemoryState()
	defer func() { GlobalState = oldState }()

	ctx := context.Background()
	from := common.HexToAddress("0x1000000000000000000000000000000000000001")
	to := common.HexToAddress("0x2000000000000000000000000000000000000002")

	entities := &Entities{
		LBTCTransfer: []LBTCTransfer{
			{From: from, To: to, Value: *uint256.NewInt(500), EventMeta: EventMeta{BlockNumber: 10}},
			{From: from, To: to, Value: *uint256.NewInt(300), EventMeta: EventMeta{BlockNumber: 10}},
		},
		BlockNumber: 10,
	}

	err := CustomProcessing(ctx, &mockStore{}, entities)
	if err != nil {
		t.Fatalf("CustomProcessing failed: %v", err)
	}

	toPos, toFound := GlobalState.Position(to)
	if !toFound {
		t.Fatal("expected receiver position to exist")
	}
	if toPos.TransferCount != 2 {
		t.Errorf("expected transfer count 2, got %d", toPos.TransferCount)
	}
	expectedBalance := uint256.NewInt(800)
	if toPos.Balance.Cmp(expectedBalance.ToBig()) != 0 {
		t.Errorf("expected balance %s, got %s", expectedBalance.Dec(), toPos.Balance.String())
	}
}

func TestCustomProcessingE2E_NilEntities(t *testing.T) {
	ctx := context.Background()
	err := CustomProcessing(ctx, &mockStore{}, nil)
	if err != nil {
		t.Fatalf("CustomProcessing with nil entities should not error: %v", err)
	}
}

func TestCustomProcessingE2E_EmptyTransfers(t *testing.T) {
	oldState := GlobalState
	GlobalState = NewMemoryState()
	defer func() { GlobalState = oldState }()

	ctx := context.Background()
	entities := &Entities{
		BlockNumber: 50,
	}

	err := CustomProcessing(ctx, &mockStore{}, entities)
	if err != nil {
		t.Fatalf("CustomProcessing with empty transfers should not error: %v", err)
	}
	// Sync should have updated with BlockNumber > 0
	if GlobalState.LastSyncBlock != 50 {
		t.Errorf("expected LastSyncBlock 50, got %d", GlobalState.LastSyncBlock)
	}
}

func TestAppendDecodedLog(t *testing.T) {
	entities := &Entities{}
	meta := EventMeta{
		BlockNumber:      42,
		ContractAddress:  common.HexToAddress(LBTCTransferAddress),
		TransactionHash:  common.HexToHash("0xdef"),
		TransactionIndex: 5,
		LogIndex:         3,
	}

	transfer := &LBTCTransfer{
		From:  common.HexToAddress("0x1000000000000000000000000000000000000001"),
		To:    common.HexToAddress("0x2000000000000000000000000000000000000002"),
		Value: *uint256.NewInt(42),
	}

	decoded := &DecodedLog{
		EventName: "Transfer",
		Topic0:    LBTCTransferTopic0,
		Value:     transfer,
	}

	ok := AppendDecodedLog(entities, decoded, meta)
	if !ok {
		t.Fatal("AppendDecodedLog should return true")
	}
	if len(entities.LBTCTransfer) != 1 {
		t.Errorf("expected 1 transfer, got %d", len(entities.LBTCTransfer))
	}
	if entities.BlockNumber != 42 {
		t.Errorf("expected BlockNumber 42, got %d", entities.BlockNumber)
	}
	if entities.LBTCTransfer[0].TransactionIndex != 5 {
		t.Errorf("expected TransactionIndex 5, got %d", entities.LBTCTransfer[0].TransactionIndex)
	}
}
