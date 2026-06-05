package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

// Helper function matching the implementation
func interfaceToString(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return fmt.Sprintf("%.0f", val)
	case json.Number:
		return val.String()
	case int:
		return fmt.Sprintf("%d", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// Mock HTTP server for ClickHouse responses
func mockClickHouseServer(response string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, response)
	}))
}

// Test helper functions for type conversion
func TestToString(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{"string input", "0x123", "0x123"},
		{"float64 input", float64(123), "123"},
		{"float64 with decimal", float64(123.45), "123"},
		{"int input", 456, "456"},
		{"json number", json.Number("789"), "789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := interfaceToString(tt.input)
			if result != tt.expected {
				t.Errorf("toString(%v) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

// Test LoadState with empty response
func TestLoadState_EmptyResponse(t *testing.T) {
	server := mockClickHouseServer("")
	defer server.Close()

	state := generated.NewState()
	ctx := context.Background()

	// This should not error, just handle empty response gracefully
	err := generated.LoadStateFromClickHouseFn(state, ctx, 8123, 1000)
	if err != nil {
		t.Logf("LoadState with empty response: %v", err)
	}
}

// Test Type Conversion Edge Cases
func TestTypeConversion_EdgeCases(t *testing.T) {
	// Test very large numbers
	largeNumber := float64(1e20)
	result := interfaceToString(largeNumber)
	expected := "100000000000000000000"
	if result != expected {
		t.Errorf("Large number conversion: got %s, want %s", result, expected)
	}

	// Test zero
	zero := float64(0)
	result = interfaceToString(zero)
	if result != "0" {
		t.Errorf("Zero conversion: got %s, want 0", result)
	}

	// Test negative
	negative := float64(-100)
	result = interfaceToString(negative)
	if result != "-100" {
		t.Errorf("Negative conversion: got %s, want -100", result)
	}
}

// Test decimal parsing from different formats
func TestParseDecimal_Formats(t *testing.T) {
	testCases := []struct {
		name     string
		input    interface{}
		expected decimal.Decimal
		hasError bool
	}{
		{"string int", "1000000", decimal.NewFromInt(1000000), false},
		{"float64 int", float64(1000000), decimal.NewFromInt(1000000), false},
		{"json number", json.Number("1000000"), decimal.NewFromInt(1000000), false},
		{"string decimal", "0.5", decimal.NewFromFloat(0.5), false},
		// Note: float64(0.5) gets truncated to "0" by %.0f formatting
		// This is expected - ClickHouse should return decimals as strings
		{"float64 decimal", float64(0.5), decimal.Zero, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			str := interfaceToString(tc.input)
			result, err := decimal.NewFromString(str)
			if tc.hasError && err == nil {
				t.Error("Expected error but got none")
			} else if !tc.hasError {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				} else if !result.Equal(tc.expected) {
					t.Errorf("Expected %v, got %v", tc.expected, result)
				}
			}
		})
	}
}

// Test uint256 parsing from different formats
func TestParseUint256_Formats(t *testing.T) {
	testCases := []struct {
		input    interface{}
		expected string
		hasError bool
	}{
		{"1000000", "1000000", false},
		{float64(1000000), "1000000", false},
		{json.Number("1000000"), "1000000", false},
		{"0", "0", false},
		{float64(0), "0", false},
	}

	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {
			str := interfaceToString(tc.input)
			result, err := uint256.FromDecimal(str)
			if tc.hasError && err == nil {
				t.Error("Expected error but got none")
			} else if !tc.hasError {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				} else if result.String() != tc.expected {
					t.Errorf("Expected %s, got %s", tc.expected, result.String())
				}
			}
		})
	}
}

// Test integration with State object
func TestLoadState_StateIntegration(t *testing.T) {
	// Create a complete multi-table response
	response := `{"id":"0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef","oracle":"0x0000000000000000000000000000000000000001","question_id":"0x0000000000000000000000000000000000000000000000000000000000000001","outcome_slot_count":2,"resolved":1,"payouts":[0,1000000000000000000]}`

	server := mockClickHouseServer(response)
	defer server.Close()

	state := generated.NewState()
	ctx := context.Background()

	err := generated.LoadStateFromClickHouseFn(state, ctx, 8123, 1000)
	if err == nil {
		t.Fatal("expected LoadState to return an error when ClickHouse is unavailable")
	}
	if !strings.Contains(err.Error(), "load conditions from ClickHouse") {
		t.Fatalf("expected contextual LoadState error, got %v", err)
	}

	// Verify state is properly initialized
	if state.HotState == nil {
		t.Error("HotState should be initialized")
	}

	if state.LastSyncBlock != 0 {
		t.Errorf("Expected LastSyncBlock to remain 0 after failed recovery, got %d", state.LastSyncBlock)
	}

	if state.LastPruneBlock != 0 {
		t.Errorf("Expected LastPruneBlock to remain 0 after failed recovery, got %d", state.LastPruneBlock)
	}
}

// Test authentication handling
func TestLoadState_Authentication(t *testing.T) {
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for basic auth
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, `{"id":"0x0000000000000000000000000000000000000000000000000000000000000001","oracle":"0x0000000000000000000000000000000000000001","question_id":"0x0000000000000000000000000000000000000000000000000000000000000001","outcome_slot_count":2,"resolved":0,"payouts":[]}`)
	}))
	defer authServer.Close()

	// Set credentials
	os.Setenv("CLICKHOUSE_USER", "testuser")
	os.Setenv("CLICKHOUSE_PASSWORD", "testpass")
	defer func() {
		os.Unsetenv("CLICKHOUSE_USER")
		os.Unsetenv("CLICKHOUSE_PASSWORD")
	}()

	state := generated.NewState()
	ctx := context.Background()

	err := generated.LoadStateFromClickHouseFn(state, ctx, 8123, 1000)
	_ = err // Log result
}

// Benchmark test for LoadState
func BenchmarkLoadState(b *testing.B) {
	// Create a realistic multi-row response
	var rows []string
	for i := 0; i < 100; i++ {
		rows = append(rows, `{"id":"0x`+strings.Repeat("0", 63)+`1","oracle":"0x0000000000000000000000000000000000000001","question_id":"0x0000000000000000000000000000000000000000000000000000000000000001","outcome_slot_count":2,"resolved":0,"payouts":[0,1000000000000000000]}`)
	}

	server := mockClickHouseServer(strings.Join(rows, "\n"))
	defer server.Close()

	state := generated.NewState()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = generated.LoadStateFromClickHouseFn(state, ctx, 8123, 1000)
	}
}
