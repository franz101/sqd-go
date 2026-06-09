package polymarket

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestCTFTokenUnits tests what units CTF outcome tokens use
func TestCTFTokenUnits(t *testing.T) {
	// According to CTF docs, outcome tokens use "integer amounts"
	// where 1 full stake = 10^18 outcome tokens (or 10^6 for some versions)

	// Let's test different assumptions:
	testCases := []struct {
		name              string
		weiValue          string
		scaleFactor       int64 // 1e18 for wei, 1e6 for million, etc.
		expectedTokens    string
	}{
		{
			name:           "Assume wei (1e18) scale",
			weiValue:       "549890000000000000000000000", // 549.89M * 1e18
			scaleFactor:    1e18,
			expectedTokens: "549890000",
		},
		{
			name:           "Assume million (1e6) scale",
			weiValue:       "549890000000000", // 549.89M * 1e6
			scaleFactor:    1e6,
			expectedTokens: "549890000",
		},
		{
			name:           "Assume raw integer (no scale)",
			weiValue:       "549890000", // 549.89M raw
			scaleFactor:    1,
			expectedTokens: "549890000",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Parse from big.Int
			bigInt := new(big.Int)
			_, ok := bigInt.SetString(tc.weiValue, 10)
			if !ok {
				t.Fatalf("Failed to parse wei value")
			}

			var u uint256.Int
			u.SetFromBig(bigInt)

			// Current Uint256ToDecimal uses exponent -18
			d := Uint256ToDecimal(u)
			t.Logf("Input: %s", tc.weiValue)
			t.Logf("Uint256ToDecimal result: %s", d.String())

			// What would the token amount be if we divide by scaleFactor?
			tokens := decimal.NewFromBigInt(bigInt, 0).Div(decimal.NewFromInt(tc.scaleFactor))
			t.Logf("Tokens (dividing by %d): %s", tc.scaleFactor, tokens.String())

			if tokens.String() == tc.expectedTokens {
				t.Logf("MATCH! Scale factor %d works", tc.scaleFactor)
			}
		})
	}
}
