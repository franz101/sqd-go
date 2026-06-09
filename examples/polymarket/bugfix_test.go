package polymarket

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestQuestionPreparedBeforeMarketPrepared verifies that handleQuestionPrepared
// correctly creates NegRiskEvent when QuestionPrepared fires before MarketPrepared.
//
// Bug: If QuestionPrepared fires before MarketPrepared, the original code
// would return early and lose the question, resulting in QuestionCount == 0
// and PositionConverted events being skipped.
//
// Fix: handleQuestionPrepared now creates NegRiskEvent if it doesn't exist.
func TestQuestionPreparedBeforeMarketPrepared(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State

	marketID := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	questionID := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")

	// Simulate QuestionPrepared firing BEFORE MarketPrepared (the bug scenario)
	questionPrepEv := &generated.NegRiskAdapterQuestionPrepared{
		EventMeta: generated.EventMeta{
			BlockNumber:      1000,
			BlockTimestamp:   time.Unix(1000, 0),
			TransactionIndex: 0,
			LogIndex:         0,
		},
		MarketID:  marketID,
		QuestionID: questionID,
		Index:    *uint256.NewInt(1), // questionIndex = 0 (bit position 0)
		Data:     []byte{},
	}

	// Process the QuestionPrepared event
	handleQuestionPrepared(state, questionPrepEv)

	// Verify NegRiskEvent was created even though MarketPrepared hasn't fired yet
	nr, ok := state.NegRiskEvent.Get(marketID)
	if !ok {
		t.Fatal("NegRiskEvent was not created by QuestionPrepared")
	}

	// Verify QuestionCount was incremented
	if nr.QuestionCount != 1 {
		t.Errorf("QuestionCount should be 1, got %d", nr.QuestionCount)
	}

	// Verify QuestionID was stored
	if len(nr.QuestionIDs) < 1 {
		t.Fatal("QuestionIDs was not initialized")
	}
	if nr.QuestionIDs[0] != questionID {
		t.Errorf("QuestionID mismatch, got %s", nr.QuestionIDs[0].Hex())
	}

	// Now simulate MarketPrepared
	marketPrepEv := &generated.NegRiskAdapterMarketPrepared{
		EventMeta: generated.EventMeta{
			BlockNumber:      1001,
			BlockTimestamp:   time.Unix(1001, 0),
			TransactionIndex: 0,
			LogIndex:         0,
		},
		MarketID: marketID,
		Creator: wallet,
		FeeBips:  uint256.Int{},
		Data:    []byte{},
	}

	// Process MarketPrepared (should not overwrite existing question)
	handleMarketPrepared(state, marketPrepEv)

	// Verify QuestionCount is still 1 (not reset to 0)
	nr, ok = state.NegRiskEvent.Get(marketID)
	if !ok {
		t.Fatal("NegRiskEvent was lost after MarketPrepared")
	}
	if nr.QuestionCount != 1 {
		t.Errorf("QuestionCount should still be 1 after MarketPrepared, got %d", nr.QuestionCount)
	}
}

// TestPositionsConvertedSellsAtFiftyCents verifies that handlePositionsConverted
// sells NO tokens at fiftyCents (0.5) instead of the user's avgPrice.
//
// Bug: The original code sold NO tokens at the user's avgPrice, which would
// result in 0 PnL (price - avgPrice = 0). The correct behavior is to sell at
// fiftyCents to generate PnL when avgPrice differs from 0.5.
//
// Fix: Changed sell.price from currentAvg to fiftyCents.
func TestPositionsConvertedSellsAtFiftyCents(t *testing.T) {
	proc, err := generated.NewProcessor(true)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	state := proc.State

	marketID := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")

	// Set up NegRiskEvent with 2 questions
	nr := &generated.NegRiskEvent{
		ID:           marketID,
		QuestionCount: 2,
		QuestionIDs:   []common.Hash{
			common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111"),
			common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222"),
		},
	}
	state.NegRiskEvent.Save(nr, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Set up a user position for the NO token of question 0 with avgPrice = 0.49
	// Buy 1000 NO tokens at 0.49
	amount := decimal.NewFromInt(1000)
	avgPrice := decimal.NewFromFloat(0.49)

	questionIndex := uint32(0)
	outcomeIndex := uint8(1) // NO outcome
	posID := getNegRiskPositionID(marketID, questionIndex, outcomeIndex)

	updateUserPositionWithBuy(state, wallet, posID, avgPrice, amount, decimal.Zero, generated.EventMeta{
		BlockNumber:    1000,
		BlockTimestamp: time.Unix(1000, 0),
	})

	// Verify initial position
	up := getUserPosition(state, wallet, posID)
	if up == nil {
		t.Fatal("position was not created")
	}
	initialAmount := toDecimal(up.Amount)
	if !initialAmount.Equal(amount) {
		t.Errorf("initial amount mismatch, got %s", initialAmount.String())
	}
	initialAvgPrice := toDecimal(up.AvgPrice)
	if !initialAvgPrice.Equal(avgPrice) {
		t.Errorf("initial avgPrice mismatch, got %s", initialAvgPrice.String())
	}

	// Simulate PositionConverted event: convert 100 NO tokens (indexSet = 0b01, meaning question 0 is selected)
	// indexSet = 1 means question 0's NO tokens should be sold
	convEv := &generated.NegRiskAdapterPositionsConverted{
		EventMeta: generated.EventMeta{
			BlockNumber:      1001,
			BlockTimestamp:   time.Unix(1001, 0),
			TransactionIndex: 0,
			LogIndex:         0,
		},
		MarketID:    marketID,
		Stakeholder: wallet,
		IndexSet:    *uint256.NewInt(1), // Select question 0 (NO tokens)
		Amount:      *uint256.NewInt(100 * 1e6), // 100 tokens in raw units
	}

	// Store initial RealizedPnL
	initialRealizedPnL := toDecimal(up.RealizedPnL)

	// Process the conversion
	handlePositionsConverted(state, convEv)

	// Verify the position after conversion
	up = getUserPosition(state, wallet, posID)
	if up == nil {
		t.Fatal("position was deleted after conversion")
	}

	// Verify amount decreased by 100
	finalAmount := toDecimal(up.Amount)
	expectedAmount := amount.Sub(decimal.NewFromInt(100))
	if !finalAmount.Equal(expectedAmount) {
		t.Errorf("amount after conversion: got %s, want %s", finalAmount.String(), expectedAmount.String())
	}

	// Verify RealizedPnL was generated
	// Expected PnL = 100 * (0.5 - 0.49) = 1.0
	finalRealizedPnL := toDecimal(up.RealizedPnL)
	pnlChange := finalRealizedPnL.Sub(initialRealizedPnL)
	expectedPnL := decimal.NewFromFloat(1.0)

	if !pnlChange.Equal(expectedPnL) {
		t.Errorf("PnL change: got %s, want %s", pnlChange.String(), expectedPnL.String())
	}

	// Also verify that avgPrice didn't change (still 0.49)
	finalAvgPrice := toDecimal(up.AvgPrice)
	if !finalAvgPrice.Equal(avgPrice) {
		t.Errorf("avgPrice should not change after sell, got %s", finalAvgPrice.String())
	}
}

// TestFromDecimalSmallValues verifies that fromDecimal correctly handles
// small decimal values without precision loss.
//
// Bug: Small PnL values (e.g., 0.532588...) were being converted to 0
// when saved to Decimal256, causing PnL to be lost.
//
// This test verifies the fix.
func TestFromDecimalSmallValues(t *testing.T) {
	tests := []struct {
		name     string
		input    decimal.Decimal
		expected string // Expected scaled value as string
	}{
		{
			name:     "PnL of 0.532588",
			input:    decimal.NewFromFloat(0.5325887891838789600444),
			expected: "532588789183878960", // Scaled by 1e18, should be 532588789183878960044400, but precision may vary
		},
		{
			name:     "Small positive value 0.1",
			input:    decimal.NewFromFloat(0.1),
			expected: "100000000000000000", // 0.1 * 1e18 = 100000000000000000
		},
		{
			name:     "Small positive value 0.01",
			input:    decimal.NewFromFloat(0.01),
			expected: "10000000000000000", // 0.01 * 1e18 = 10000000000000000
		},
		{
			name:     "Fifty cents",
			input:    decimal.NewFromFloat(0.5),
			expected: "500000000000000000", // 0.5 * 1e18 = 500000000000000000
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fromDecimal(tt.input)
			scaled := result.ScaledBig()

			// The expected value might have some precision loss, so check closeness
			expectedBigInt, ok := new(big.Int).SetString(tt.expected, 10)
			if !ok {
				t.Fatalf("invalid expected value: %s", tt.expected)
			}

			// Allow small precision differences (within 1000)
			diff := new(big.Int).Abs(scaled.Sub(scaled, expectedBigInt))
			maxDiff := big.NewInt(1000)
			if diff.Cmp(maxDiff) > 0 {
				t.Errorf("fromDecimal(%s): scaled = %s, want close to %s (diff = %s)",
					tt.input.String(), scaled.String(), expectedBigInt.String(), diff.String())
			}

			// Verify round-trip conversion
			roundTrip := toDecimal(result)
			if !roundTrip.Equal(tt.input) {
				// Allow some precision loss in round-trip
				diffDecimal := roundTrip.Sub(tt.input).Abs()
				maxDiffDecimal := decimal.NewFromFloat(0.000001)
				if diffDecimal.Cmp(maxDiffDecimal) > 0 {
					t.Errorf("round-trip precision loss: got %s, want %s (diff = %s)",
						roundTrip.String(), tt.input.String(), diffDecimal.String())
				}
			}
		})
	}
}

// BenchmarkFromDecimal benchmarks the fromDecimal conversion function.
func BenchmarkFromDecimal(b *testing.B) {
	// Common values used in PnL calculations
	values := []decimal.Decimal{
		decimal.NewFromFloat(0.5),
		decimal.NewFromFloat(0.49),
		decimal.NewFromFloat(0.5325887891838789600444),
		decimal.NewFromFloat(10.5),
		decimal.NewFromFloat(100.25),
		decimal.NewFromFloat(1000.0),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, v := range values {
			_ = fromDecimal(v)
		}
	}
}

// BenchmarkUpdateUserPositionWithSell benchmarks the sell operation.
func BenchmarkUpdateUserPositionWithSell(b *testing.B) {
	proc, _ := generated.NewProcessor(true)
	state := proc.State
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")

	// Set up a test position
	tokenID := getNegRiskPositionID(
		common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"),
		0,
		1, // NO outcome
	)

	// Buy 1000 tokens at 0.49
	updateUserPositionWithBuy(state, wallet, tokenID,
		decimal.NewFromFloat(0.49),
		decimal.NewFromInt(1000),
		decimal.Zero,
		generated.EventMeta{BlockNumber: 1000},
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		updateUserPositionWithSell(state, wallet, tokenID,
			decimal.NewFromFloat(0.5),  // Sell at 0.5
			decimal.NewFromInt(100),    // Sell 100 tokens
			generated.EventMeta{BlockNumber: 1001},
		)
	}
}

// BenchmarkHandlePositionsConverted benchmarks the full conversion operation.
func BenchmarkHandlePositionsConverted(b *testing.B) {
	proc, _ := generated.NewProcessor(true)
	state := proc.State

	marketID := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	wallet := common.HexToAddress("0x1111111111111111111111111111111111111111")

	// Set up NegRiskEvent with 4 questions
	nr := &generated.NegRiskEvent{
		ID:           marketID,
		QuestionCount: 4,
		QuestionIDs:   make([]common.Hash, 4),
	}
	state.NegRiskEvent.Save(nr, generated.EventMeta{BlockNumber: 1000})

	// Set up user positions for 2 NO tokens (questions 0 and 1)
	for i := uint32(0); i < 2; i++ {
		posID := getNegRiskPositionID(marketID, i, 1) // NO outcome
		updateUserPositionWithBuy(state, wallet, posID,
			decimal.NewFromFloat(0.49), // avgPrice = 0.49
			decimal.NewFromInt(1000),    // 1000 tokens
			decimal.Zero,
			generated.EventMeta{BlockNumber: 1000},
		)
	}

	convEv := &generated.NegRiskAdapterPositionsConverted{
		EventMeta:   generated.EventMeta{BlockNumber: 1001, BlockTimestamp: time.Unix(1001, 0)},
		MarketID:    marketID,
		Stakeholder: wallet,
		IndexSet:    *uint256.NewInt(3), // Select questions 0 and 1 (NO tokens)
		Amount:      *uint256.NewInt(100 * 1e6),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		handlePositionsConverted(state, convEv)
	}
}
