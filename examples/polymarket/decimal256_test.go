package polymarket

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// TestDecimal256ConversionBug tests the conversion from Decimal256 to decimal.Decimal
// This demonstrates the int64 overflow bug that causes negative amounts
func TestDecimal256ConversionBug(t *testing.T) {
	// The bug in collectPositions:
	// amt := decimal.NewFromInt(int64(pos.Amount.ScaledBig().Int64())).Div(decimal.NewFromInt(1e18))
	//
	// The problem: ScaledBig() returns a big.Int scaled by 1e18.
	// Calling .Int64() on it will overflow for values > int64 max (~9.2e18).

	// Example from the failing test:
	// TokenID: 0x0c6a8380...5f8a35dd has Amount: -1.51, TotalBought: -3.93
	// These negative values are caused by int64 overflow!

	// Simulate the values that would be stored in Decimal256
	// Decimal256 stores values scaled by 1e18
	// So a value of 1.51 tokens is stored as 1.51 * 1e18 = 1.51e18
	// And 3.93 tokens is stored as 3.93 * 1e18 = 3.93e18

	// These fit in int64 (max ~9.2e18), so the bug must be elsewhere
	// Let's check the actual values from the test output:
	// Amount: -1.512561402025345, TotalBought: -3.9319674031712502
	// These are scaled by 1e18 in storage:
	// Amount (scaled): 1.512561402025345e18
	// TotalBought (scaled): 3.9319674031712502e18

	// Wait, these SHOULD fit in int64... Let me check for larger values
	// from the third skipped position:
	// Amount: -6.7994200287236588, TotalBought: 8.207262788292182
	// 8.2 * 1e18 = 8.2e18 which still fits in int64 (max 9.2e18)

	// The issue might be in intermediate calculations or the actual stored
	// values being larger than we think. Let's check the actual stored values.

	// For now, let's demonstrate the principle with a value that WOULD overflow:
	overflowsInt64 := new(big.Int)
	overflowsInt64.SetString("10000000000000000000000", 10) // 1e22

	t.Logf("Testing with value that overflows int64: %s", overflowsInt64.String())
	t.Logf("Fits in int64: %v", overflowsInt64.IsInt64())

	// BUGGY conversion - calling Int64() on a value that doesn't fit
	buggyResult := overflowsInt64.Int64()
	t.Logf("Int64() result: %d (likely overflowed/wrong)", buggyResult)

	// The overflow causes the value to become negative or wrong
	if buggyResult < 0 {
		t.Logf("BUG: int64 overflow caused negative value!")
	}

	// CORRECT conversion - use NewFromBigInt directly
	correctResult := decimal.NewFromBigInt(overflowsInt64, 0)
	t.Logf("Correct conversion: %s", correctResult.String())
}

// TestDecimal256Int64Overflow shows the specific case
func TestDecimal256Int64Overflow(t *testing.T) {
	// This test shows what happens when we call Int64() on a big.Int
	// that doesn't fit in int64

	// int64 max = 9,223,372,036,854,775,807
	maxInt64 := int64(9223372036854775807)

	t.Logf("int64 max: %d", maxInt64)
	t.Logf("int64 max as decimal: %.2e", float64(maxInt64))

	// A value that's just above int64 max
	aboveMax := new(big.Int)
	aboveMax.SetString("9223372036854775808", 10) // int64 max + 1

	t.Logf("Value above int64 max: %s", aboveMax.String())
	t.Logf("IsInt64: %v", aboveMax.IsInt64())

	// Calling Int64() on this is undefined behavior in Go's big.Int
	// It returns an implementation-specific value (usually truncated)
	overflowed := aboveMax.Int64()
	t.Logf("Int64() result (overflowed): %d", overflowed)

	// In Go, big.Int.Int64() returns the low 64 bits
	// For 9223372036854775808 (0x8000000000000000), this is:
	// -9223372036854775808 in two's complement (int64 min)
	if overflowed == -9223372036854775808 {
		t.Logf("Overflow wrapped to int64 min!")
	}

	// Now test with a much larger value (like what we might see in scaled storage)
	largeValue := new(big.Int)
	largeValue.SetString("10000000000000000000000000000000000000000", 10) // 1e40

	t.Logf("\nTesting with large scaled value: %s", largeValue.String())
	t.Logf("IsInt64: %v", largeValue.IsInt64())

	overflowed2 := largeValue.Int64()
	t.Logf("Int64() result: %d", overflowed2)

	if overflowed2 < 0 {
		t.Logf("Large value overflowed to negative!")
	}
}

// TestDecimal256ScaledValues tests actual scaled values from our system
func TestDecimal256ScaledValues(t *testing.T) {
	// Decimal256 stores values with 1e18 scale
	// So a value like $351.56 is stored as 351.56 * 1e18 = 3.5156e20

	// Let's create some test values using the protomath package

	// Value 1: $100
	hundredInt := new(big.Int)
	hundredInt.SetInt64(100)
	hundredScaled := new(big.Int)
	hundredScaled.Mul(hundredInt, big.NewInt(1_000_000_000_000_000_000))

	t.Logf("100 (scaled by 1e18): %s", hundredScaled.String())
	t.Logf("Fits in int64: %v", hundredScaled.IsInt64())

	// Value 2: $1000
	thousandInt := new(big.Int)
	thousandInt.SetInt64(1000)
	thousandScaled := new(big.Int)
	thousandScaled.Mul(thousandInt, big.NewInt(1_000_000_000_000_000_000))

	t.Logf("1000 (scaled by 1e18): %s", thousandScaled.String())
	t.Logf("Fits in int64: %v", thousandScaled.IsInt64())

	// Value 3: $10000
	tenThousandInt := new(big.Int)
	tenThousandInt.SetInt64(10000)
	tenThousandScaled := new(big.Int)
	tenThousandScaled.Mul(tenThousandInt, big.NewInt(1_000_000_000_000_000_000))

	t.Logf("10000 (scaled by 1e18): %s", tenThousandScaled.String())
	t.Logf("Fits in int64: %v", tenThousandScaled.IsInt64())

	// Value 4: $100000 (this should overflow)
	hundredThousandInt := new(big.Int)
	hundredThousandInt.SetInt64(100000)
	hundredThousandScaled := new(big.Int)
	hundredThousandScaled.Mul(hundredThousandInt, big.NewInt(1_000_000_000_000_000_000))

	t.Logf("100000 (scaled by 1e18): %s", hundredThousandScaled.String())
	t.Logf("Fits in int64: %v", hundredThousandScaled.IsInt64())

	if !hundredThousandScaled.IsInt64() {
		t.Logf("100000 scaled does NOT fit in int64 - calling Int64() would overflow!")

		overflowed := hundredThousandScaled.Int64()
		t.Logf("Int64() result: %d", overflowed)

		if overflowed < 0 {
			t.Logf("OVERFLOW: Value became negative due to int64 overflow!")
		}
	}
}

// TestUint256OverflowBehavior tests how uint256.Int handles large values
func TestUint256OverflowBehavior(t *testing.T) {
	// The Decimal256 uses uint256.Int internally
	// Let's check how to properly convert to decimal.Decimal

	// Create a uint256.Int with a large value
	u := uint256.NewInt(100000)
	u.Mul(u, uint256.NewInt(1_000_000_000_000_000_000)) // Scale by 1e18

	t.Logf("uint256 value: %s", u.Dec())

	// Convert to big.Int
	bigInt := u.ToBig()
	t.Logf("As big.Int: %s", bigInt.String())

	// BUGGY way: cast to int64 (will overflow)
	buggyDecimal := decimal.NewFromInt(bigInt.Int64()).Div(decimal.NewFromInt(1e18))
	t.Logf("BUGGY conversion (via int64): %s", buggyDecimal.String())

	// CORRECT way: use NewFromBigInt
	correctDecimal := decimal.NewFromBigInt(bigInt, 0).Div(decimal.NewFromInt(1e18))
	t.Logf("CORRECT conversion (via NewFromBigInt): %s", correctDecimal.String())
}

// TestDecimal256FromUint256 tests proper conversion from Decimal256 to decimal
func TestDecimal256FromUint256(t *testing.T) {
	// Test the fix: use ScaledBig() with NewFromBigInt instead of Int64()

	// Create a uint256 value (simulating what Decimal256 stores)
	amount := uint256.NewInt(100000)                              // 100k tokens
	amount.Mul(amount, uint256.NewInt(1_000_000_000_000_000_000)) // Scale by 1e18

	// Convert to big.Int
	amountBig := amount.ToBig()

	t.Logf("Amount (scaled): %s", amountBig.String())
	t.Logf("Fits in int64: %v", amountBig.IsInt64())

	if !amountBig.IsInt64() {
		t.Logf("This value does NOT fit in int64!")

		// BUGGY method (what we currently do)
		buggyInt64 := amountBig.Int64()
		t.Logf("Int64() result: %d", buggyInt64)
		if buggyInt64 < 0 {
			t.Logf("BUG: int64 overflow caused negative value!")
		}

		buggyDecimal := decimal.NewFromInt(buggyInt64).Div(decimal.NewFromInt(1e18))
		t.Logf("BUGGY decimal: %s", buggyDecimal.String())
	}

	// CORRECT method (what we should do)
	correctDecimal := decimal.NewFromBigInt(amountBig, 0).Div(decimal.NewFromInt(1e18))
	t.Logf("CORRECT decimal: %s", correctDecimal.String())
}

// TestDecimalLargeCoefficient tests what happens with large coefficient and exponent -18
func TestDecimalLargeCoefficient(t *testing.T) {
	// Test what happens with large coefficient and exponent -18
	coeff := new(big.Int)
	coeff.SetString("549890000000", 10) // 549.89 billion
	d := decimal.NewFromBigInt(coeff, -18)
	t.Logf("Decimal: %s", d.String())
	t.Logf("Coeff: %s, Exp: %d", d.Coefficient().String(), d.Exponent())

	// Expected value: coeff * 10^exp = 549890000000 * 10^-18
	expected := "0.00000054989"
	t.Logf("Expected: %s", expected)

	// Compare
	if d.String() == expected {
		t.Log("MATCH!")
	} else {
		t.Errorf("MISMATCH! got %s, want %s", d.String(), expected)
	}
}
