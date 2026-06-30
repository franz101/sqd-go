package ingestion

import (
	"context"
	"testing"
	"time"

	"github.com/franz101/sqd-go/internal/client"
)

func TestMaxUint64(t *testing.T) {
	tests := []struct {
		a, b     uint64
		expected uint64
	}{
		{1, 2, 2},
		{5, 3, 5},
		{0, 0, 0},
		{100, 100, 100},
		{18446744073709551615, 0, 18446744073709551615}, // max uint64
	}

	for _, tt := range tests {
		result := max(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("max(%d, %d) = %d, want %d", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestMaxInt64(t *testing.T) {
	tests := []struct {
		a, b     int64
		expected int64
	}{
		{1, 2, 2},
		{5, 3, 5},
		{0, 0, 0},
		{-1, 1, 1},
		{-5, -3, -3},
		{9223372036854775807, 0, 9223372036854775807}, // max int64
	}

	for _, tt := range tests {
		result := maxInt64(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("maxInt64(%d, %d) = %d, want %d", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestNonNegativeDurationDelta(t *testing.T) {
	tests := []struct {
		name     string
		current  time.Duration
		previous time.Duration
		expected time.Duration
	}{
		{"positive delta", 100 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
		{"zero delta", 100 * time.Millisecond, 100 * time.Millisecond, 0},
		{"current less than previous", 50 * time.Millisecond, 100 * time.Millisecond, 0},
		{"both zero", 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := nonNegativeDurationDelta(tt.current, tt.previous)
			if result != tt.expected {
				t.Errorf("nonNegativeDurationDelta(%v, %v) = %v, want %v", tt.current, tt.previous, result, tt.expected)
			}
		})
	}
}

func TestPct(t *testing.T) {
	tests := []struct {
		name     string
		part     time.Duration
		total    time.Duration
		expected float64
	}{
		{"half", 50 * time.Millisecond, 100 * time.Millisecond, 50.0},
		{"quarter", 25 * time.Millisecond, 100 * time.Millisecond, 25.0},
		{"zero part", 0, 100 * time.Millisecond, 0.0},
		{"equal", 100 * time.Millisecond, 100 * time.Millisecond, 100.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pct(tt.part, tt.total)
			if result != tt.expected {
				t.Errorf("pct(%v, %v) = %f, want %f", tt.part, tt.total, result, tt.expected)
			}
		})
	}
}

func TestMinEndBlock(t *testing.T) {
	tests := []struct {
		name     string
		current  *uint64
		candidate uint64
		expected *uint64
	}{
		{"both set", uint64Ptr(100), 50, uint64Ptr(50)},
		{"current smaller", uint64Ptr(50), 100, uint64Ptr(50)},
		{"current nil", nil, 100, uint64Ptr(100)},
		{"equal values", uint64Ptr(100), 100, uint64Ptr(100)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := minEndBlock(tt.current, tt.candidate)
			if result == nil || tt.expected == nil {
				if result != tt.expected {
					t.Errorf("minEndBlock(%v, %d) = %v, want %v", tt.current, tt.candidate, result, tt.expected)
				}
			} else if *result != *tt.expected {
				t.Errorf("minEndBlock(%v, %d) = %d, want %d", tt.current, tt.candidate, *result, *tt.expected)
			}
		})
	}
}

func TestShouldWaitForEmptyCursorResponse(t *testing.T) {
	tests := []struct {
		name              string
		effectiveEndBlock *uint64
		expected          bool
	}{
		{"no end block", nil, true},
		{"with end block", uint64Ptr(1000), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldWaitForEmptyCursorResponse(tt.effectiveEndBlock)
			if result != tt.expected {
				t.Errorf("shouldWaitForEmptyCursorResponse(%v) = %t, want %t", tt.effectiveEndBlock, result, tt.expected)
			}
		})
	}
}

func TestEmptyCursorCheckpoint(t *testing.T) {
	// Create mock BlockRef for testing
	blockRef950 := &client.BlockRef{Number: 950}

	tests := []struct {
		name         string
		currentBlock uint64
		head         client.Head
		expected     uint64
		expectedOk   bool
	}{
		{"normal case", 900, client.Head{Finalized: blockRef950}, 950, true},
		{"no finalized", 1000, client.Head{}, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := emptyCursorCheckpoint(tt.currentBlock, tt.head)
			if ok != tt.expectedOk {
				t.Errorf("emptyCursorCheckpoint(%d, %v) ok = %t, want %t", tt.currentBlock, tt.head, ok, tt.expectedOk)
			}
			if ok && result != tt.expected {
				t.Errorf("emptyCursorCheckpoint(%d, %v) = %d, want %d", tt.currentBlock, tt.head, result, tt.expected)
			}
		})
	}
}

func TestCloneStrings(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{"normal slice", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"empty slice", []string{}, []string{}},
		{"nil slice", nil, []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cloneStrings(tt.input)
			if len(result) != len(tt.expected) {
				t.Fatalf("cloneStrings(%v) length = %d, want %d", tt.input, len(result), len(tt.expected))
			}
			for i, val := range result {
				if val != tt.expected[i] {
					t.Errorf("cloneStrings(%v)[%d] = %q, want %q", tt.input, i, val, tt.expected[i])
				}
			}
			// Verify it's a true copy
			if len(result) > 0 {
				result[0] = "modified"
				if tt.input != nil && len(tt.input) > 0 && tt.input[0] == "modified" {
					t.Error("cloneStrings did not create a true copy")
				}
			}
		})
	}
}

func TestChainEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		chainID  uint64
		hot      bool
		expected string
	}{
		{"mainnet hot", 1, true, "https://portal.sqd.dev/datasets/ethereum-mainnet/stream"},
		{"mainnet cold", 1, false, "https://portal.sqd.dev/datasets/ethereum-mainnet/finalized-stream"},
		{"polygon hot", 137, true, "https://portal.sqd.dev/datasets/polygon-mainnet/stream"},
		{"polygon cold", 137, false, "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := chainEndpoint(tt.chainID, tt.hot)
			if result != tt.expected {
				t.Errorf("chainEndpoint(%d, %t) = %q, want %q", tt.chainID, tt.hot, result, tt.expected)
			}
		})
	}
}

func TestWaitForNextCursorPoll(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		expectOk bool
	}{
		{"short interval", 10 * time.Millisecond, true},
		{"immediate", 0, true},
		{"long interval", 100 * time.Millisecond, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			err := waitForNextCursorPoll(ctx, tt.interval)
			if tt.expectOk && err != nil {
				t.Errorf("waitForNextCursorPoll(%v) unexpected error: %v", tt.interval, err)
			}
		})
	}
}

func TestRetainReplayJSONLPage(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected []byte
	}{
		{"normal page", []byte(`{"header":{...},"logs":[...]}`), []byte(`{"header":{...},"logs":[...]}`)},
		{"empty page", []byte{}, []byte{}},
		{"nil page", nil, []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := retainReplayJSONLPage(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("retainReplayJSONLPage(%v) length = %d, want %d", tt.input, len(result), len(tt.expected))
			}
		})
	}
}

func TestToSnake(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"CamelCase", "camel_case"},
		{"already_snake", "already_snake"},
		{"PascalCase", "pascal_case"},
		{"HTTPServer", "httpserver"},
		{"MultiWordString", "multi_word_string"},
		{"", "event"},
		{"A", "a"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := toSnake(tt.input)
			if result != tt.expected {
				t.Errorf("toSnake(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestNormalizeSolidityType(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"uint256", "uint256"},
		{"address", "address"},
		{"bool", "bool"},
		{"bytes32", "bytes32"},
		{"uint8", "uint8"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizeSolidityType(tt.input)
			if result != tt.expected {
				t.Errorf("normalizeSolidityType(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestClickHouseType(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"uint256", "UInt256"},
		{"uint8", "UInt256"},
		{"address", "FixedString(20)"},
		{"bool", "UInt8"},
		{"bytes32", "FixedString(32)"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := clickHouseType(tt.input)
			if result != tt.expected {
				t.Errorf("clickHouseType(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsBytesN(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"bytes32", true},
		{"bytes8", true},
		{"bytes1", true},
		{"bytes", false},
		{"address", false},
		{"uint256", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := isBytesN(tt.input)
			if result != tt.expected {
				t.Errorf("isBytesN(%q) = %t, want %t", tt.input, result, tt.expected)
			}
		})
	}
}

func TestBytesNSize(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"bytes32", 32},
		{"bytes8", 8},
		{"bytes1", 1},
		{"bytes64", 64},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := bytesNSize(tt.input)
			if result != tt.expected {
				t.Errorf("bytesNSize(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

// Helper function
func uint64Ptr(u uint64) *uint64 {
	return &u
}