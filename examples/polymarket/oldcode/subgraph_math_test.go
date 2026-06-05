package subgraph

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestUpdateAvgPriceDecimal_PreservesFractionalScale(t *testing.T) {
	got := updateAvgPriceDecimal(
		decimal.RequireFromString("0.5"),
		decimal.RequireFromString("10"),
		decimal.RequireFromString("0.6"),
		decimal.RequireFromString("10"),
	)
	want := decimal.RequireFromString("0.55")
	if !got.Equal(want) {
		t.Fatalf("weighted avg mismatch: got=%s want=%s", got, want)
	}
}

func TestApplyUserPositionBuyState_LargeAndFractionalValues(t *testing.T) {
	up := &UserPosition{
		Amount:      decimal.RequireFromString("1000000000000000000"),
		AvgPrice:    decimal.RequireFromString("0.333333333333333333"),
		RealizedPnL: decimal.Zero,
		TotalBought: decimal.RequireFromString("1000000000000000000"),
	}
	price := decimal.RequireFromString("0.777777777777777777")
	amount := decimal.RequireFromString("250000000000000000")
	pnlAdj := decimal.RequireFromString("-1.5")

	if !applyUserPositionBuyState(up, price, amount, pnlAdj) {
		t.Fatalf("expected buy update to apply")
	}

	if !up.Amount.Equal(decimal.RequireFromString("1250000000000000000")) {
		t.Fatalf("amount mismatch: %s", up.Amount)
	}
	if !up.TotalBought.Equal(decimal.RequireFromString("1250000000000000000")) {
		t.Fatalf("total bought mismatch: %s", up.TotalBought)
	}
	if !up.RealizedPnL.Equal(decimal.RequireFromString("-1.5")) {
		t.Fatalf("realized pnl mismatch: %s", up.RealizedPnL)
	}

	wantAvg := decimal.RequireFromString("0.4222222222222222")
	if !up.AvgPrice.Equal(wantAvg) {
		t.Fatalf("avg price mismatch: got=%s want=%s", up.AvgPrice, wantAvg)
	}
}
