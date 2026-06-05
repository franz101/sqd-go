package experiment

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
)

func TestAppendFromLogSharedTopic0(t *testing.T) {
	// Both emit the same topic0: 0xd0a08e8c...
	// Exchange: 0x4bfb41d5b3570defd03c39a9a4d8de6bd8b8982e
	// NegRisk:  0xc5d563a36ae78145c45a50134d48a1215220f80a

	topic0 := common.HexToHash("0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6")
	// orderHash, maker, taker (topics 1,2,3 are indexed)
	topics := []common.Hash{
		topic0,
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000aaa"),
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000bbb"),
	}
	// Data: makerAssetId=5, takerAssetId=3, makerAmount=100, takerAmount=200, fee=2
	makerAssetID := uint256.NewInt(5)
	takerAssetID := uint256.NewInt(3)
	makerAmount := uint256.NewInt(100)
	takerAmount := uint256.NewInt(200)
	fee := uint256.NewInt(2)
	data := make([]byte, 5*32)
	makerAssetID.WriteToArray32((*[32]byte)(data[0:32]))
	takerAssetID.WriteToArray32((*[32]byte)(data[32:64]))
	makerAmount.WriteToArray32((*[32]byte)(data[64:96]))
	takerAmount.WriteToArray32((*[32]byte)(data[96:128]))
	fee.WriteToArray32((*[32]byte)(data[128:160]))

	meta := generated.EventMeta{BlockNumber: 1}

	// Test 1: Exchange contract address → should decode as ExchangeOrderFilled
	exchangeAddr := common.HexToAddress("0x4bfb41d5b3570defd03c39a9a4d8de6bd8b8982e")
	b1 := generated.NewProtoEventBlock()
	ok := b1.AppendFromLog(exchangeAddr, topics, data, meta)
	if !ok {
		t.Fatal("AppendFromLog returned false for Exchange address")
	}
	if b1.ExchangeOrderFilled_meta_index.Rows() != 1 {
		t.Errorf("ExchangeOrderFilled rows: want 1, got %d", b1.ExchangeOrderFilled_meta_index.Rows())
	}
	if b1.NegRiskExchangeOrderFilled_meta_index.Rows() != 0 {
		t.Errorf("NegRiskExchangeOrderFilled rows: want 0, got %d (should NOT have been appended)", b1.NegRiskExchangeOrderFilled_meta_index.Rows())
	}

	// Test 2: NegRisk contract address → should decode as NegRiskExchangeOrderFilled
	negRiskAddr := common.HexToAddress("0xc5d563a36ae78145c45a50134d48a1215220f80a")
	b2 := generated.NewProtoEventBlock()
	ok = b2.AppendFromLog(negRiskAddr, topics, data, meta)
	if !ok {
		t.Fatal("AppendFromLog returned false for NegRisk address")
	}
	if b2.NegRiskExchangeOrderFilled_meta_index.Rows() != 1 {
		t.Errorf("NegRiskExchangeOrderFilled rows: want 1, got %d", b2.NegRiskExchangeOrderFilled_meta_index.Rows())
	}
	if b2.ExchangeOrderFilled_meta_index.Rows() != 0 {
		t.Errorf("ExchangeOrderFilled rows: want 0, got %d (should NOT have been appended)", b2.ExchangeOrderFilled_meta_index.Rows())
	}

	// Test 3: Unknown address with shared topic0 → should return false
	unknownAddr := common.HexToAddress("0x0000000000000000000000000000000000000001")
	b3 := generated.NewProtoEventBlock()
	ok = b3.AppendFromLog(unknownAddr, topics, data, meta)
	if ok {
		t.Fatal("AppendFromLog should return false for unknown address with shared topic0")
	}
}

func TestAppendFromLogExchangeEventTypes(t *testing.T) {
	// Helper to make a block with an event and check the Sequence
	exchangeAddr := common.HexToAddress("0x4bfb41d5b3570defd03c39a9a4d8de6bd8b8982e")
	negRiskAddr := common.HexToAddress("0xc5d563a36ae78145c45a50134d48a1215220f80a")
	topic0 := common.HexToHash("0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6")
	topics := []common.Hash{
		topic0,
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000aaa"),
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000bbb"),
	}
	data := make([]byte, 5*32)
	meta := generated.EventMeta{BlockNumber: 1}

	b := generated.NewProtoEventBlock()
	
	// Append Exchange
	b.AppendFromLog(exchangeAddr, topics, data, meta)
	// Append NegRisk
	b.AppendFromLog(negRiskAddr, topics, data, meta)
	
	if len(b.Sequence) != 2 {
		t.Fatalf("Sequence length: want 2, got %d", len(b.Sequence))
	}
	
	wantExchangeType := uint8(6) // EventTypeExchangeOrderFilled
	wantNegRiskType := uint8(7)  // EventTypeNegRiskExchangeOrderFilled
	
	if b.Sequence[0] != wantExchangeType {
		t.Errorf("Sequence[0]: want EventTypeExchangeOrderFilled(%d), got %d", wantExchangeType, b.Sequence[0])
	}
	if b.Sequence[1] != wantNegRiskType {
		t.Errorf("Sequence[1]: want EventTypeNegRiskExchangeOrderFilled(%d), got %d", wantNegRiskType, b.Sequence[1])
	}
	
	t.Logf("PASS: Sequence = %v (Exchange=6, NegRisk=7)", b.Sequence)
}

