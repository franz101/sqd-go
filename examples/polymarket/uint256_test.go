package polymarket

import (
	"math/big"
	"testing"

	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestUint256ToDecimalScaling tests the actual scaling issue
func TestUint256ToDecimalScaling(t *testing.T) {
	// Simulate a Solidity event value (wei)
	// Example: 549890000 outcome tokens bought
	// In wei, this would be 549890000 * 1e18
	tokenAmount := int64(549890000)
	weiValue := new(big.Int)
	weiValue.Mul(big.NewInt(tokenAmount), big.NewInt(1e18))

	t.Logf("Token amount: %d", tokenAmount)
	t.Logf("Wei value: %s", weiValue.String())

	// Convert to uint256
	u := new(uint256.Int)
	u.SetFromBig(weiValue)
	t.Logf("uint256 value: %s", u.Dec())

	// Convert using WeiToDecimal for 1e18-scaled values.
	d := WeiToDecimal(*u)
	t.Logf("Decimal (exp -18): %s", d.String())
	t.Logf("  Coeff: %s, Exp: %d", d.Coefficient().String(), d.Exponent())

	// Now simulate fromDecimal storing this
	coeff := d.Coefficient()
	exp := int(d.Exponent())
	shift := exp + 18
	t.Logf("Shift needed: %d", shift)

	scaled := new(big.Int).Mul(coeff, pow10Array(shift))
	t.Logf("Scaled value for Decimal256: %s", scaled.String())

	// Convert via fromDecimal
	d256, ok := protomath.FromDecimal256ScaledBigInt(scaled)
	if ok {
		t.Logf("Decimal256 created successfully")

		// Read back via toDecimal equivalent
		readBack := decimal.NewFromBigInt(d256.ScaledBig(), -18)
		t.Logf("Read back: %s", readBack.String())

		// Expected: 549890000
		expected := "549890000"
		if readBack.String() == expected {
			t.Logf("SUCCESS: Got expected value %s", expected)
		} else {
			t.Errorf("FAIL: Got %s, want %s", readBack.String(), expected)
		}
	}
}

// pow10Array returns 10^n for small n
func pow10Array(n int) *big.Int {
	if n < 0 || n >= 20 {
		panic("n out of range")
	}
	result := big.NewInt(1)
	for i := 0; i < n; i++ {
		result.Mul(result, big.NewInt(10))
	}
	return result
}
