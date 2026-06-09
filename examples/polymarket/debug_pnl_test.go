package polymarket

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestDecimal256RoundTripSmallValues tests the round-trip conversion
// of small decimal values through Decimal256.
func TestDecimal256RoundTripSmallValues(t *testing.T) {
	tests := []decimal.Decimal{
		decimal.NewFromFloat(0.532905),
		decimal.NewFromFloat(0.002377),
		decimal.NewFromFloat(0.5),
		decimal.NewFromFloat(0.1),
	}

	for _, input := range tests {
		t.Run(input.String(), func(t *testing.T) {
			// Convert to Decimal256
			d256 := fromDecimal(input)
			scaled := d256.ScaledBig()
			t.Logf("Input: %s (exp=%d, coeff=%s)", input.String(), input.Exponent(), input.Coefficient())
			t.Logf("Scaled: %s", scaled.String())

			// Convert back
			output := toDecimal(d256)
			t.Logf("Output: %s", output.String())

			// Check if round-trip preserves value
			diff := input.Sub(output).Abs()
			maxDiff := decimal.NewFromFloat(0.000001)
			if diff.Cmp(maxDiff) > 0 {
				t.Errorf("Round-trip failed: input=%s, output=%s, diff=%s", input.String(), output.String(), diff.String())
			}
		})
	}
}

// TestPnLCalculationRoundTrip simulates the PnL calculation round-trip
// that happens in updateUserPositionWithSell.
func TestPnLCalculationRoundTrip(t *testing.T) {
	// Simulate a position with avgPrice = 0.497623
	avgPrice := decimal.NewFromFloat(0.497623)
	initialRealizedPnL := decimal.Zero

	// Convert to Decimal256
	avgPriceD256 := fromDecimal(avgPrice)
	realizedPnLD256 := fromDecimal(initialRealizedPnL)

	// Simulate a sell of 224.192556 tokens at 0.5
	sellAmount := decimal.NewFromFloat(224.192556)
	sellPrice := decimal.NewFromFloat(0.5)

	// Calculate PnL: amount * (price - avgPrice)
	// In the real code, this uses toDecimal on the Decimal256 values
	currentAvgPrice := toDecimal(avgPriceD256)
	currentRealizedPnL := toDecimal(realizedPnLD256)
	pnl := sellAmount.Mul(sellPrice.Sub(currentAvgPrice))
	newRealizedPnL := currentRealizedPnL.Add(pnl)

	t.Logf("Sell amount: %s", sellAmount.String())
	t.Logf("Sell price: %s", sellPrice.String())
	t.Logf("Current avg price: %s", currentAvgPrice.String())
	t.Logf("Price diff: %s", sellPrice.Sub(currentAvgPrice).String())
	t.Logf("Calculated PnL: %s", pnl.String())
	t.Logf("New realized PnL: %s", newRealizedPnL.String())

	// Convert back to Decimal256 (simulating save)
	newRealizedPnLD256 := fromDecimal(newRealizedPnL)
	t.Logf("Saved scaled value: %s", newRealizedPnLD256.ScaledBig().String())

	// Read back (simulating load)
	readBackRealizedPnL := toDecimal(newRealizedPnLD256)
	t.Logf("Read back value: %s", readBackRealizedPnL.String())

	// Check if value is preserved
	if readBackRealizedPnL.IsZero() {
		t.Fatal("PnL was lost during round-trip!")
	}

	diff := newRealizedPnL.Sub(readBackRealizedPnL).Abs()
	maxDiff := decimal.NewFromFloat(0.000001)
	if diff.Cmp(maxDiff) > 0 {
		t.Errorf("PnL changed during round-trip: expected=%s, got=%s, diff=%s", newRealizedPnL.String(), readBackRealizedPnL.String(), diff.String())
	}
}

// TestPnLSmallValuesDebug specifically tests small PnL values
func TestPnLSmallValuesDebug(t *testing.T) {
	tests := []decimal.Decimal{
		decimal.NewFromFloat(0.532905),
		decimal.NewFromFloat(0.002377),
		decimal.NewFromFloat(0.000001),
	}

	for _, pnl := range tests {
		t.Run(pnl.String(), func(t *testing.T) {
			// Start with zero realized PnL
			currentRealizedPnL := fromDecimal(decimal.Zero)

			// Add new PnL
			newRealizedPnL := toDecimal(currentRealizedPnL).Add(pnl)
			t.Logf("Adding PnL: %s to existing: %s = %s", pnl.String(), toDecimal(currentRealizedPnL).String(), newRealizedPnL.String())

			// Save to Decimal256
			saved := fromDecimal(newRealizedPnL)
			t.Logf("Saved scaled: %s", saved.ScaledBig().String())

			// Read back
			readBack := toDecimal(saved)
			t.Logf("Read back: %s", readBack.String())

			// Check if value is preserved
			if readBack.IsZero() && !newRealizedPnL.IsZero() {
				t.Errorf("PnL was lost! Expected %s, got 0", newRealizedPnL.String())
			}

			diff := newRealizedPnL.Sub(readBack).Abs()
			maxDiff := decimal.NewFromFloat(0.000001)
			if diff.Cmp(maxDiff) > 0 {
				t.Errorf("PnL changed: expected=%s, got=%s, diff=%s", newRealizedPnL.String(), readBack.String(), diff.String())
			}
		})
	}
}
