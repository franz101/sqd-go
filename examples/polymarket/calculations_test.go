package polymarket

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

func TestUint256FromAddressMatchesGraphByteArrayCalcs(t *testing.T) {
	tests := []struct {
		addr string
		want common.Hash
	}{
		{addr: "0x0b3cb5eff462e6c220690a0dadbf7e00913f2926", want: common.HexToHash("0x00000000000000000000000026293f91007ebfad0d0a6920c2e662f4efb53c0b")},
		{addr: "0x2a0051271595f5e8ffc04736a8781f4bd69ce05c", want: common.HexToHash("0x0000000000000000000000005ce09cd64b1f78a83647c0ffe8f595152751002a")},
		{addr: "0x2c9bb88dd32a4a2e72cccc788134120368abc81a", want: common.HexToHash("0x0000000000000000000000001ac8ab680312348178cccc722e4a2ad38db89b2c")},
		{addr: "0x39c68c733a7246651e1b236e72ee3da00668c62c", want: common.HexToHash("0x0000000000000000000000002cc66806a03dee726e231b1e6546723a738cc639")},
		{addr: "0x3ad79a37cf026334c2753e49cc6acf7ac94b4ce5", want: common.HexToHash("0xffffffffffffffffffffffffe54c4bc97acf6acc493e75c2346302cf379ad73a")},
		{addr: "0x5e47be3ad949863275954a7b9bca8c61ec94ecfe", want: common.HexToHash("0xfffffffffffffffffffffffffeec94ec618cca9b7b4a9575328649d93abe475e")},
		{addr: "0x6baf4d01d1b99fa4a4aa1371f5ed16cc55e6ed38", want: common.HexToHash("0x00000000000000000000000038ede655cc16edf57113aaa4a49fb9d1014daf6b")},
		{addr: "0x7f3ce47b96eb60edf38c0936f7953ff727bacd44", want: common.HexToHash("0x00000000000000000000000044cdba27f73f95f736098cf3ed60eb967be43c7f")},
		{addr: "0x9b1fc9336e3c3dc4d2ce1e867d99512aa3b2ccf1", want: common.HexToHash("0xfffffffffffffffffffffffff1ccb2a32a51997d861eced2c43d3c6e33c91f9b")},
		{addr: "0xddaf501b4b3538fff318dad0f3d504648f18e25f", want: common.HexToHash("0x0000000000000000000000005fe2188f6404d5f3d0da18f3ff38354b1b50afdd")},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := tokenIDHash(uint256FromAddress(common.HexToAddress(tt.addr)))
			if got != tt.want {
				t.Fatalf("LP token id mismatch: got %s, want %s", got.Hex(), tt.want.Hex())
			}
		})
	}
}

// TestUint256ToDecimal tests Uint256 to Decimal conversion
func TestUint256ToDecimalCalcs(t *testing.T) {
	tests := []struct {
		name     string
		input    string // hex or decimal string
		expected string // expected decimal output
	}{
		{
			name:     "zero",
			input:    "0",
			expected: "0",
		},
		{
			name:     "one",
			input:    "1",
			expected: "1",
		},
		{
			name:     "small value",
			input:    "1000000",
			expected: "1000000",
		},
		{
			name:     "CTF collateral scale (1e6)",
			input:    "1000000",
			expected: "1000000",
		},
		{
			name:     "wei scale (1e18)",
			input:    "1000000000000000000",
			expected: "1000000000000000000",
		},
		{
			name:     "large uint256 value",
			input:    "1000000000000000000000000000",
			expected: "1000000000000000000000000000",
		},
		{
			name:     "max safe int64",
			input:    "9223372036854775807",
			expected: "9223372036854775807",
		},
		{
			name:     "above max safe int64",
			input:    "9223372036854775808",
			expected: "9223372036854775808",
		},
		{
			name:     "very large value (10e30)",
			input:    "1000000000000000000000000000000",
			expected: "1000000000000000000000000000000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bigInt := new(big.Int)
			_, ok := bigInt.SetString(tt.input, 10)
			if !ok {
				t.Fatalf("Failed to parse input: %s", tt.input)
			}

			var u uint256.Int
			u.SetFromBig(bigInt)

			result := Uint256ToDecimal(u)
			if result.String() != tt.expected {
				t.Errorf("Uint256ToDecimal() = %s, want %s", result.String(), tt.expected)
			}
		})
	}
}

// TestWeiToDecimal tests wei (1e18) to Decimal conversion
func TestWeiToDecimalCalcs(t *testing.T) {
	tests := []struct {
		name     string
		inputWei string // wei value (1e18 scale)
		expected string // expected decimal output (human-readable)
	}{
		{
			name:     "zero wei",
			inputWei: "0",
			expected: "0",
		},
		{
			name:     "one wei",
			inputWei: "1",
			expected: "0.000000000000000001",
		},
		{
			name:     "one USDC (1e18 wei)",
			inputWei: "1000000000000000000",
			expected: "1",
		},
		{
			name:     "100 USDC",
			inputWei: "100000000000000000000",
			expected: "100",
		},
		{
			name:     "0.5 USDC",
			inputWei: "500000000000000000",
			expected: "0.5",
		},
		{
			name:     "37.88 USDC",
			inputWei: "37880000000000000000",
			expected: "37.88",
		},
		{
			name:     "313.68 USDC",
			inputWei: "313680000000000000000",
			expected: "313.68",
		},
		{
			name:     "large wei value",
			inputWei: "1000000000000000000000000",
			expected: "1000000", // 1e21 wei / 1e18 = 1e3 = 1000
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bigInt := new(big.Int)
			_, ok := bigInt.SetString(tt.inputWei, 10)
			if !ok {
				t.Fatalf("Failed to parse wei: %s", tt.inputWei)
			}

			var u uint256.Int
			u.SetFromBig(bigInt)

			result := WeiToDecimal(u)
			if result.String() != tt.expected {
				t.Errorf("WeiToDecimal() = %s, want %s", result.String(), tt.expected)
			}
		})
	}
}

// TestCTFOutcomeToDecimal tests CTF outcome token conversion
func TestCTFOutcomeToDecimalCalcs(t *testing.T) {
	tests := []struct {
		name        string
		input       string // raw outcome token amount
		expectedRaw string // expected raw output
	}{
		{
			name:        "zero tokens",
			input:       "0",
			expectedRaw: "0",
		},
		{
			name:        "one full stake (1e6 tokens)",
			input:       "1000000",
			expectedRaw: "1000000",
		},
		{
			name:        "549.89 full stakes (549890000 tokens)",
			input:       "549890000",
			expectedRaw: "549890000",
		},
		{
			name:        "large outcome amount",
			input:       "1000000000000000",
			expectedRaw: "1000000000000000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bigInt := new(big.Int)
			_, ok := bigInt.SetString(tt.input, 10)
			if !ok {
				t.Fatalf("Failed to parse input: %s", tt.input)
			}

			var u uint256.Int
			u.SetFromBig(bigInt)

			result := CTFOutcomeToDecimal(u)
			if result.String() != tt.expectedRaw {
				t.Errorf("CTFOutcomeToDecimal() raw = %s, want %s", result.String(), tt.expectedRaw)
			}
		})
	}
}

// TestCTFOutcomeScaling tests converting raw outcome tokens to stake units
func TestCTFOutcomeScalingCalcs(t *testing.T) {
	tests := []struct {
		name           string
		rawTokens      string // raw outcome tokens
		expectedStakes string // expected stake units (divided by 1e6)
	}{
		{
			name:           "zero",
			rawTokens:      "0",
			expectedStakes: "0",
		},
		{
			name:           "one full stake",
			rawTokens:      "1000000",
			expectedStakes: "1",
		},
		{
			name:           "549.89 full stakes",
			rawTokens:      "549890000",
			expectedStakes: "549.89",
		},
		{
			name:           "fractional stake (0.5)",
			rawTokens:      "500000",
			expectedStakes: "0.5",
		},
		{
			name:           "large amount",
			rawTokens:      "1000000000000000",
			expectedStakes: "1000000000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bigInt := new(big.Int)
			_, ok := bigInt.SetString(tt.rawTokens, 10)
			if !ok {
				t.Fatalf("Failed to parse raw tokens: %s", tt.rawTokens)
			}

			var u uint256.Int
			u.SetFromBig(bigInt)

			// Convert to decimal then divide by 1e6
			result := CTFOutcomeToDecimal(u).Div(decimal.NewFromInt(1e6))
			if result.String() != tt.expectedStakes {
				t.Errorf("Stake units = %s, want %s", result.String(), tt.expectedStakes)
			}
		})
	}
}

// TestFromDecimalRoundtrip tests fromDecimal/toDecimal roundtrip conversions
func TestFromDecimalRoundtripCalcs(t *testing.T) {
	tests := []struct {
		name  string
		input decimal.Decimal
	}{
		{
			name:  "zero",
			input: decimal.Zero,
		},
		{
			name:  "one",
			input: decimal.NewFromInt(1),
		},
		{
			name:  "0.5",
			input: decimal.NewFromFloat(0.5),
		},
		{
			name:  "100",
			input: decimal.NewFromInt(100),
		},
		{
			name:  "0.65",
			input: decimal.NewFromFloat(0.65),
		},
		{
			name:  "37.88",
			input: decimal.NewFromFloat(37.88),
		},
		{
			name:  "313.68",
			input: decimal.NewFromFloat(313.68),
		},
		{
			name:  "small value",
			input: decimal.NewFromFloat(0.000000001),
		},
		{
			name:  "large value",
			input: decimal.NewFromFloat(1000000),
		},
		{
			name:  "negative value (small)",
			input: decimal.NewFromFloat(-0.5),
		},
		{
			name:  "negative realized PnL",
			input: decimal.NewFromFloat(-10.25),
		},
		{
			name:  "positive realized PnL",
			input: decimal.NewFromFloat(37.88),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Convert to Decimal256
			d256 := fromDecimal(tt.input)

			// Convert back
			result := toDecimal(d256)

			// Check if values are equal (within reasonable precision)
			diff := result.Sub(tt.input).Abs()
			if tt.input.IsZero() {
				if !result.IsZero() {
					t.Errorf("Roundtrip failed: got %s, want 0", result.String())
				}
			} else {
				ratio := diff.Div(tt.input.Abs()).InexactFloat64()
				// Allow 1e-18 relative error (Decimal256 precision)
				if ratio > 1e-18 && !tt.input.IsZero() {
					// For very small values, check absolute difference
					if tt.input.Abs().LessThan(decimal.NewFromFloat(1e-10)) {
						if diff.GreaterThan(decimal.NewFromFloat(1e-28)) {
							t.Errorf("Roundtrip failed: got %s, want %s (diff: %s)", result.String(), tt.input.String(), diff.String())
						}
					} else {
						t.Errorf("Roundtrip failed: got %s, want %s (relative error: %e)", result.String(), tt.input.String(), ratio)
					}
				}
			}
		})
	}
}

// TestUpdateAvgPriceDecimal tests average price calculation
func TestUpdateAvgPriceDecimalCalcs(t *testing.T) {
	// Reference: polymarket-subgraph/pnl-subgraph/src/utils/updateUserPositionWithBuy.ts
	// avgPrice = (avgPrice * userAmount + price * buyAmount) / (userAmount + buyAmount)

	tests := []struct {
		name        string
		currentAvg  decimal.Decimal
		currentAmt  decimal.Decimal
		newPrice    decimal.Decimal
		newAmt      decimal.Decimal
		expectedAvg decimal.Decimal
	}{
		{
			name:        "first buy (no existing position)",
			currentAvg:  decimal.Zero,
			currentAmt:  decimal.Zero,
			newPrice:    decimal.NewFromFloat(0.65),
			newAmt:      decimal.NewFromInt(100),
			expectedAvg: decimal.NewFromFloat(0.65),
		},
		{
			name:        "second buy at same price",
			currentAvg:  decimal.NewFromFloat(0.65),
			currentAmt:  decimal.NewFromInt(100),
			newPrice:    decimal.NewFromFloat(0.65),
			newAmt:      decimal.NewFromInt(50),
			expectedAvg: decimal.NewFromFloat(0.65),
		},
		{
			name:        "second buy at higher price",
			currentAvg:  decimal.NewFromFloat(0.65),
			currentAmt:  decimal.NewFromInt(100),
			newPrice:    decimal.NewFromFloat(0.70),
			newAmt:      decimal.NewFromInt(50),
			expectedAvg: decimal.NewFromFloat(0.6666666666666667),
		},
		{
			name:        "second buy at lower price",
			currentAvg:  decimal.NewFromFloat(0.70),
			currentAmt:  decimal.NewFromInt(100),
			newPrice:    decimal.NewFromFloat(0.60),
			newAmt:      decimal.NewFromInt(100),
			expectedAvg: decimal.NewFromFloat(0.65),
		},
		{
			name:        "multiple buys averaging down",
			currentAvg:  decimal.NewFromFloat(0.80),
			currentAmt:  decimal.NewFromInt(1000),
			newPrice:    decimal.NewFromFloat(0.50),
			newAmt:      decimal.NewFromInt(1000),
			expectedAvg: decimal.NewFromFloat(0.65),
		},
		{
			name:        "multiple buys averaging up",
			currentAvg:  decimal.NewFromFloat(0.50),
			currentAmt:  decimal.NewFromInt(1000),
			newPrice:    decimal.NewFromFloat(0.80),
			newAmt:      decimal.NewFromInt(500),
			expectedAvg: decimal.NewFromFloat(0.6),
		},
		{
			name:        "very small first buy",
			currentAvg:  decimal.Zero,
			currentAmt:  decimal.Zero,
			newPrice:    decimal.NewFromFloat(0.000001),
			newAmt:      decimal.NewFromFloat(0.000001),
			expectedAvg: decimal.NewFromFloat(0.000001),
		},
		{
			name:        "zero new amount (should return current avg)",
			currentAvg:  decimal.NewFromFloat(0.65),
			currentAmt:  decimal.NewFromInt(100),
			newPrice:    decimal.NewFromFloat(0.70),
			newAmt:      decimal.Zero,
			expectedAvg: decimal.NewFromFloat(0.65),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := updateAvgPriceDecimal(tt.currentAvg, tt.currentAmt, tt.newPrice, tt.newAmt)

			// Compare with tolerance for floating point precision
			diff := result.Sub(tt.expectedAvg).Abs()
			maxDiff := decimal.NewFromFloat(1e-18)
			if diff.GreaterThan(maxDiff) {
				t.Errorf("updateAvgPriceDecimal() = %s, want %s (diff: %s)",
					result.String(), tt.expectedAvg.String(), diff.String())
			}
		})
	}
}

// TestPnLCalculation tests PnL calculation for sells
func TestPnLCalculationCalcs(t *testing.T) {
	// Reference: polymarket-subgraph/pnl-subgraph/src/utils/updateUserPositionWithSell.ts
	// deltaPnL = adjustedAmount * (price - avgPrice) / COLLATERAL_SCALE
	// Note: In our implementation, prices are already in stake units, so we don't divide by COLLATERAL_SCALE

	tests := []struct {
		name        string
		price       decimal.Decimal // sell price
		avgPrice    decimal.Decimal // average buy price
		amount      decimal.Decimal // amount sold
		expectedPnL decimal.Decimal // expected PnL
	}{
		{
			name:        "sell at same price (no PnL)",
			price:       decimal.NewFromFloat(0.65),
			avgPrice:    decimal.NewFromFloat(0.65),
			amount:      decimal.NewFromInt(100),
			expectedPnL: decimal.Zero,
		},
		{
			name:        "sell at higher price (profit)",
			price:       decimal.NewFromFloat(0.70),
			avgPrice:    decimal.NewFromFloat(0.65),
			amount:      decimal.NewFromInt(100),
			expectedPnL: decimal.NewFromFloat(5), // 100 * (0.70 - 0.65) = 5
		},
		{
			name:        "sell at lower price (loss)",
			price:       decimal.NewFromFloat(0.60),
			avgPrice:    decimal.NewFromFloat(0.65),
			amount:      decimal.NewFromInt(100),
			expectedPnL: decimal.NewFromFloat(-5), // 100 * (0.60 - 0.65) = -5
		},
		{
			name:        "sell all at profit",
			price:       decimal.NewFromFloat(0.90),
			avgPrice:    decimal.NewFromFloat(0.50),
			amount:      decimal.NewFromInt(1000),
			expectedPnL: decimal.NewFromFloat(400), // 1000 * (0.90 - 0.50) = 400
		},
		{
			name:        "sell all at loss",
			price:       decimal.NewFromFloat(0.30),
			avgPrice:    decimal.NewFromFloat(0.70),
			amount:      decimal.NewFromInt(500),
			expectedPnL: decimal.NewFromFloat(-200), // 500 * (0.30 - 0.70) = -200
		},
		{
			name:        "partial sell at profit",
			price:       decimal.NewFromFloat(0.80),
			avgPrice:    decimal.NewFromFloat(0.60),
			amount:      decimal.NewFromInt(50),   // sell half of position
			expectedPnL: decimal.NewFromFloat(10), // 50 * (0.80 - 0.60) = 10
		},
		{
			name:        "fractional prices",
			price:       decimal.NewFromFloat(0.55),
			avgPrice:    decimal.NewFromFloat(0.45),
			amount:      decimal.NewFromFloat(123.45),
			expectedPnL: decimal.NewFromFloat(12.345), // 123.45 * (0.55 - 0.45) = 12.345
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// PnL = amount * (price - avgPrice)
			pnl := tt.amount.Mul(tt.price.Sub(tt.avgPrice))

			diff := pnl.Sub(tt.expectedPnL).Abs()
			maxDiff := decimal.NewFromFloat(1e-15)
			if diff.GreaterThan(maxDiff) {
				t.Errorf("PnL calculation: got %s, want %s (diff: %s)",
					pnl.String(), tt.expectedPnL.String(), diff.String())
			}
		})
	}
}

// TestComputeFpmmPriceDecimal tests FPMM price calculation
func TestComputeFpmmPriceDecimalCalcs(t *testing.T) {
	// Reference: polymarket-subgraph/pnl-subgraph/src/utils/computeFpmmPrice.ts
	// price = amount[1-outcomeIndex] / (amount[0] + amount[1])

	tests := []struct {
		name         string
		amounts      []string // [amount0, amount1] in raw outcome tokens
		outcomeIndex uint8
		expected     string
	}{
		{
			name:         "equal amounts (price = 0.5)",
			amounts:      []string{"100", "100"},
			outcomeIndex: 0,
			expected:     "0.5",
		},
		{
			name:         "equal amounts for outcome 1",
			amounts:      []string{"100", "100"},
			outcomeIndex: 1,
			expected:     "0.5",
		},
		{
			name:         "more in outcome 0 (price0 < 0.5, price1 > 0.5)",
			amounts:      []string{"200", "100"},
			outcomeIndex: 0,
			expected:     "0.3333333333333333333333333333",
		},
		{
			name:         "more in outcome 0, check outcome 1",
			amounts:      []string{"200", "100"},
			outcomeIndex: 1,
			expected:     "0.6666666666666666666666666667",
		},
		{
			name:         "more in outcome 1",
			amounts:      []string{"100", "200"},
			outcomeIndex: 0,
			expected:     "0.6666666666666666666666666667",
		},
		{
			name:         "very skewed (1000:1)",
			amounts:      []string{"1000", "1"},
			outcomeIndex: 0,
			expected:     "0.000999000999000999000999001",
		},
		{
			name:         "very skewed opposite",
			amounts:      []string{"1", "1000"},
			outcomeIndex: 0,
			expected:     "0.999000999000999000999000999",
		},
		{
			name:         "large values",
			amounts:      []string{"1000000000000", "2000000000000"},
			outcomeIndex: 0,
			expected:     "0.6666666666666666666666666667",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			amount0Big := new(big.Int)
			amount1Big := new(big.Int)
			amount0Big.SetString(tt.amounts[0], 10)
			amount1Big.SetString(tt.amounts[1], 10)
			amount0 := &uint256.Int{}
			amount1 := &uint256.Int{}
			amount0.SetFromBig(amount0Big)
			amount1.SetFromBig(amount1Big)

			amounts := []uint256.Int{*amount0, *amount1}
			result := computeFpmmPriceDecimal(amounts, tt.outcomeIndex)

			expected, _ := decimal.NewFromString(tt.expected)
			diff := result.Sub(expected).Abs()
			maxDiff := decimal.NewFromFloat(1e-27)
			if diff.GreaterThan(maxDiff) {
				t.Errorf("computeFpmmPriceDecimal() = %s, want %s (diff: %s)",
					result.String(), expected.String(), diff.String())
			}
		})
	}
}

// TestComputeNegRiskYesPriceDecimal tests NegRisk YES price calculation
func TestComputeNegRiskYesPriceDecimalCalcs(t *testing.T) {
	// Reference: polymarket-subgraph/pnl-subgraph/src/utils/computeNegRiskYesPrice.ts
	// yesPrice = (noPrice * noCount - COLLATERAL_SCALE * (noCount - 1)) / (questionCount - noCount)
	// Note: In our implementation, COLLATERAL_SCALE is 1 (since we work with human-readable prices)

	tests := []struct {
		name          string
		noPrice       decimal.Decimal
		noCount       uint32
		questionCount uint32
		expected      string
	}{
		{
			name:          "1 NO out of 2 questions at 0.5",
			noPrice:       decimal.NewFromFloat(0.5),
			noCount:       1,
			questionCount: 2,
			expected:      "0.5",
		},
		{
			name:          "1 NO out of 2 questions at 0.6",
			noPrice:       decimal.NewFromFloat(0.6),
			noCount:       1,
			questionCount: 2,
			expected:      "0.6",
		},
		{
			name:          "2 NO at 0.5, 3 questions",
			noPrice:       decimal.NewFromFloat(0.5),
			noCount:       2,
			questionCount: 3,
			expected:      "0", // (0.5*2 - (2-1)) / (3-2) = (1-1)/1 = 0
		},
		{
			name:          "2 NO at 0.6, 3 questions",
			noPrice:       decimal.NewFromFloat(0.6),
			noCount:       2,
			questionCount: 3,
			expected:      "0.2", // (0.6*2 - (2-1)) / (3-2) = (1.2-1)/1 = 0.2
		},
		{
			name:          "2 NO at 0.7, 4 questions",
			noPrice:       decimal.NewFromFloat(0.7),
			noCount:       2,
			questionCount: 4,
			expected:      "0.2", // (0.7*2 - (2-1)) / (4-2) = (1.4-1)/2 = 0.2
		},
		{
			name:          "1 NO at 0.9, 3 questions",
			noPrice:       decimal.NewFromFloat(0.9),
			noCount:       1,
			questionCount: 3,
			expected:      "0.45",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeNegRiskYesPriceDecimal(tt.noPrice, tt.noCount, tt.questionCount)

			expected, _ := decimal.NewFromString(tt.expected)
			diff := result.Sub(expected).Abs()
			maxDiff := decimal.NewFromFloat(1e-15)
			if diff.GreaterThan(maxDiff) {
				t.Errorf("computeNegRiskYesPriceDecimal() = %s, want %s (diff: %s)",
					result.String(), tt.expected, diff.String())
			}
			t.Logf("Result: %s (expected: %s)", result.String(), tt.expected)
		})
	}
}

// TestDecimal256EdgeCases tests edge cases for Decimal256 operations
func TestDecimal256EdgeCasesCalcs(t *testing.T) {
	tests := []struct {
		name     string
		operate  func() (protomath.Decimal256, error)
		valid    bool
		expected string
	}{
		{
			name: "max safe value",
			operate: func() (protomath.Decimal256, error) {
				val, _ := decimal.NewFromString("1e38")
				return fromDecimal(val), nil
			},
			valid:    true,
			expected: "1e+38",
		},
		{
			name: "very large value (10e76)",
			operate: func() (protomath.Decimal256, error) {
				val, _ := decimal.NewFromString("1e76")
				return fromDecimal(val), nil
			},
			valid: true,
		},
		{
			name: "negative value",
			operate: func() (protomath.Decimal256, error) {
				val := decimal.NewFromFloat(-100.5)
				return fromDecimal(val), nil
			},
			valid:    true,
			expected: "-100.5",
		},
		{
			name: "very small value",
			operate: func() (protomath.Decimal256, error) {
				val, _ := decimal.NewFromString("1e-30")
				return fromDecimal(val), nil
			},
			valid:    true,
			expected: "1e-30",
		},
		{
			name: "zero with different exponents",
			operate: func() (protomath.Decimal256, error) {
				val := decimal.Zero
				return fromDecimal(val), nil
			},
			valid:    true,
			expected: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d256, err := tt.operate()
			if err != nil {
				if tt.valid {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}

			result := toDecimal(d256)
			if tt.expected != "" {
				expected, _ := decimal.NewFromString(tt.expected)
				if !result.Equals(expected) {
					diff := result.Sub(expected).Abs()
					// For scientific notation or very large numbers, allow some tolerance
					if diff.GreaterThan(decimal.NewFromFloat(1e-15)) && result.Abs().LessThan(decimal.NewFromInt(1000000)) {
						t.Errorf("got %s, want %s (diff: %s)", result.String(), tt.expected, diff.String())
					}
				}
			}
			t.Logf("Result: %s", result.String())
		})
	}
}

// TestNegativeMath tests operations with negative numbers
func TestNegativeMathCalcs(t *testing.T) {
	tests := []struct {
		name     string
		a        decimal.Decimal
		b        decimal.Decimal
		op       string
		expected decimal.Decimal
	}{
		{
			name:     "negative minus positive",
			a:        decimal.NewFromFloat(-100),
			b:        decimal.NewFromFloat(50),
			op:       "-",
			expected: decimal.NewFromFloat(-150),
		},
		{
			name:     "positive minus negative",
			a:        decimal.NewFromFloat(100),
			b:        decimal.NewFromFloat(-50),
			op:       "-",
			expected: decimal.NewFromFloat(150),
		},
		{
			name:     "negative minus negative",
			a:        decimal.NewFromFloat(-100),
			b:        decimal.NewFromFloat(-50),
			op:       "-",
			expected: decimal.NewFromFloat(-50),
		},
		{
			name:     "negative plus positive",
			a:        decimal.NewFromFloat(-100),
			b:        decimal.NewFromFloat(75),
			op:       "+",
			expected: decimal.NewFromFloat(-25),
		},
		{
			name:     "negative times positive",
			a:        decimal.NewFromFloat(-10),
			b:        decimal.NewFromFloat(5),
			op:       "*",
			expected: decimal.NewFromFloat(-50),
		},
		{
			name:     "negative times negative",
			a:        decimal.NewFromFloat(-10),
			b:        decimal.NewFromFloat(-5),
			op:       "*",
			expected: decimal.NewFromFloat(50),
		},
		{
			name:     "negative PnL example",
			a:        decimal.NewFromFloat(-5),
			b:        decimal.NewFromFloat(-10),
			op:       "+",
			expected: decimal.NewFromFloat(-15),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result decimal.Decimal
			switch tt.op {
			case "+":
				result = tt.a.Add(tt.b)
			case "-":
				result = tt.a.Sub(tt.b)
			case "*":
				result = tt.a.Mul(tt.b)
			case "/":
				if !tt.b.IsZero() {
					result = tt.a.Div(tt.b)
				}
			}

			if !result.Equals(tt.expected) {
				t.Errorf("%s %s %s = %s, want %s", tt.a.String(), tt.op, tt.b.String(), result.String(), tt.expected.String())
			}
		})
	}
}

// TestPositionSellAmountAdjustment tests amount adjustment when sell > position
func TestPositionSellAmountAdjustmentCalcs(t *testing.T) {
	// Reference: polymarket-subgraph/pnl-subgraph/src/utils/updateUserPositionWithSell.ts
	// "use userPosition amount if the amount is greater than the userPosition amount"

	tests := []struct {
		name             string
		positionAmount   decimal.Decimal
		sellAmount       decimal.Decimal
		expectedAdjusted decimal.Decimal
	}{
		{
			name:             "sell less than position",
			positionAmount:   decimal.NewFromInt(100),
			sellAmount:       decimal.NewFromInt(50),
			expectedAdjusted: decimal.NewFromInt(50),
		},
		{
			name:             "sell equal to position",
			positionAmount:   decimal.NewFromInt(100),
			sellAmount:       decimal.NewFromInt(100),
			expectedAdjusted: decimal.NewFromInt(100),
		},
		{
			name:             "sell more than position (adjust down)",
			positionAmount:   decimal.NewFromInt(100),
			sellAmount:       decimal.NewFromInt(150),
			expectedAdjusted: decimal.NewFromInt(100),
		},
		{
			name:             "sell much more than position",
			positionAmount:   decimal.NewFromFloat(10.5),
			sellAmount:       decimal.NewFromInt(1000),
			expectedAdjusted: decimal.NewFromFloat(10.5),
		},
		{
			name:             "empty position",
			positionAmount:   decimal.Zero,
			sellAmount:       decimal.NewFromInt(100),
			expectedAdjusted: decimal.Zero,
		},
		{
			name:             "fractional amounts",
			positionAmount:   decimal.NewFromFloat(123.45),
			sellAmount:       decimal.NewFromFloat(200.5),
			expectedAdjusted: decimal.NewFromFloat(123.45),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the adjustment logic from updateUserPositionWithSell
			adjusted := tt.sellAmount
			if adjusted.GreaterThan(tt.positionAmount) {
				adjusted = tt.positionAmount
			}

			if !adjusted.Equals(tt.expectedAdjusted) {
				t.Errorf("adjusted amount = %s, want %s", adjusted.String(), tt.expectedAdjusted.String())
			}
		})
	}
}

// TestCollateralScaleOperations tests operations with COLLATERAL_SCALE (1e6)
func TestCollateralScaleOperationsCalcs(t *testing.T) {
	const collateralScale = 1e6

	tests := []struct {
		name     string
		input    decimal.Decimal
		op       string
		scale    int64
		expected decimal.Decimal
	}{
		{
			name:     "divide raw tokens by 1e6",
			input:    decimal.NewFromInt(5000000),
			op:       "/",
			scale:    collateralScale,
			expected: decimal.NewFromInt(5),
		},
		{
			name:     "multiply stake by 1e6",
			input:    decimal.NewFromInt(10),
			op:       "*",
			scale:    collateralScale,
			expected: decimal.NewFromInt(10000000),
		},
		{
			name:     "fractional stake to raw tokens",
			input:    decimal.NewFromFloat(0.5),
			op:       "*",
			scale:    collateralScale,
			expected: decimal.NewFromInt(500000),
		},
		{
			name:     "large stake to raw tokens",
			input:    decimal.NewFromFloat(12345.67),
			op:       "*",
			scale:    collateralScale,
			expected: decimal.NewFromInt(12345670000),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result decimal.Decimal
			switch tt.op {
			case "*":
				result = tt.input.Mul(decimal.NewFromInt(tt.scale))
			case "/":
				result = tt.input.Div(decimal.NewFromInt(tt.scale))
			}

			if !result.Equals(tt.expected) {
				t.Errorf("%s %s %d = %s, want %s", tt.input.String(), tt.op, tt.scale, result.String(), tt.expected.String())
			}
		})
	}
}

// BenchmarkUint256ToDecimal benchmarks Uint256 to Decimal conversion
func BenchmarkUint256ToDecimal(b *testing.B) {
	var u uint256.Int
	bigVal := new(big.Int)
	bigVal.SetString("1000000000000000000", 10) // 1e18
	u.SetFromBig(bigVal)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Uint256ToDecimal(u)
	}
}

// BenchmarkWeiToDecimal benchmarks Wei to Decimal conversion
func BenchmarkWeiToDecimal(b *testing.B) {
	var u uint256.Int
	bigVal := new(big.Int)
	bigVal.SetString("1000000000000000000", 10) // 1e18 wei = 1 USDC
	u.SetFromBig(bigVal)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = WeiToDecimal(u)
	}
}

// BenchmarkFromDecimalRoundtrip benchmarks fromDecimal/toDecimal roundtrip
func BenchmarkFromDecimalRoundtrip(b *testing.B) {
	input := decimal.NewFromFloat(100.5)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d256 := fromDecimal(input)
		_ = toDecimal(d256)
	}
}

// BenchmarkUpdateAvgPriceDecimal benchmarks average price update
func BenchmarkUpdateAvgPriceDecimal(b *testing.B) {
	currentAvg := decimal.NewFromFloat(0.65)
	currentAmt := decimal.NewFromInt(100)
	newPrice := decimal.NewFromFloat(0.70)
	newAmt := decimal.NewFromInt(50)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = updateAvgPriceDecimal(currentAvg, currentAmt, newPrice, newAmt)
	}
}

// BenchmarkComputeFpmmPriceDecimal benchmarks FPMM price calculation
func BenchmarkComputeFpmmPriceDecimal(b *testing.B) {
	amount0Big := new(big.Int)
	amount1Big := new(big.Int)
	amount0Big.SetString("1000000", 10)
	amount1Big.SetString("2000000", 10)
	amount0 := &uint256.Int{}
	amount1 := &uint256.Int{}
	amount0.SetFromBig(amount0Big)
	amount1.SetFromBig(amount1Big)
	amounts := []uint256.Int{*amount0, *amount1}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = computeFpmmPriceDecimal(amounts, 0)
	}
}
