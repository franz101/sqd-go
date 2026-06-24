package generated

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// These tests cover the generated marshalCold*/unmarshalCold* binary codec that
// lets pointer-bearing hot entities (Condition's []uint256.Int payouts, Market/
// NegRiskEvent's []common.Hash question_ids) ride the Pebble cold tier instead of
// falling back to a per-miss ClickHouse SELECT.

var (
	maxU256 = uint256.MustFromHex("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	oneE18  = uint256.MustFromHex("0xde0b6b3a7640000") // 10^18
)

func uint256SliceEq(a, b []uint256.Int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Cmp(&b[i]) != 0 {
			return false
		}
	}
	return true
}

func hashSliceEq(a, b []common.Hash) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func conditionsEq(a, b MemoryCondition) bool {
	return a.ID == b.ID && a.Oracle == b.Oracle && a.QuestionID == b.QuestionID &&
		a.OutcomeSlotCount == b.OutcomeSlotCount && a.Resolved == b.Resolved &&
		uint256SliceEq(a.Payouts, b.Payouts) &&
		a.UpdatedAtBlock == b.UpdatedAtBlock && a.UpdatedAt == b.UpdatedAt &&
		a.BlockNumber == b.BlockNumber && a.TxIndex == b.TxIndex && a.LogIndex == b.LogIndex &&
		a.Tombstone == b.Tombstone
}

func TestColdCodecConditionRoundTrip(t *testing.T) {
	cases := []MemoryCondition{
		{}, // zero value (nil payouts) -> decodes to empty payouts
		{
			ID:               common.HexToHash("0x01"),
			Oracle:           common.HexToAddress("0x0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"),
			QuestionID:       common.HexToHash("0x02"),
			OutcomeSlotCount: 2,
			Resolved:         false,
			Payouts:          []uint256.Int{}, // unresolved: empty payouts
		},
		{
			ID:               common.HexToHash("0xdeadbeef"),
			OutcomeSlotCount: 3,
			Resolved:         true,
			// Covers 0, a typical DeFi magnitude, and the full 256-bit range.
			Payouts:        []uint256.Int{*uint256.NewInt(0), *oneE18, *maxU256},
			UpdatedAtBlock: 84564797,
			UpdatedAt:      1700000000123,
			BlockNumber:    84564797,
			TxIndex:        5,
			LogIndex:       12,
			Tombstone:      true,
		},
	}
	for i, want := range cases {
		data := marshalColdMemoryCondition(want)
		got, ok := unmarshalColdMemoryCondition(data)
		if !ok {
			t.Fatalf("case %d: unmarshalColdMemoryCondition returned false", i)
		}
		if !conditionsEq(got, want) {
			t.Fatalf("case %d: round-trip mismatch\n got: %+v\nwant: %+v", i, got, want)
		}
	}
}

func TestColdCodecConditionDeterministic(t *testing.T) {
	c := MemoryCondition{
		ID:               common.HexToHash("0xab"),
		OutcomeSlotCount: 2,
		Resolved:         true,
		Payouts:          []uint256.Int{*oneE18, *maxU256},
		UpdatedAt:        42,
	}
	a := marshalColdMemoryCondition(c)
	b := marshalColdMemoryCondition(c)
	if !bytes.Equal(a, b) {
		t.Fatalf("marshalColdMemoryCondition is not deterministic")
	}
}

func TestColdCodecConditionTruncated(t *testing.T) {
	c := MemoryCondition{
		ID:               common.HexToHash("0x99"),
		OutcomeSlotCount: 1,
		Resolved:         true,
		Payouts:          []uint256.Int{*maxU256},
	}
	full := marshalColdMemoryCondition(c)
	if _, ok := unmarshalColdMemoryCondition(full); !ok {
		t.Fatalf("full-length input should decode")
	}
	for n := 0; n < len(full); n++ {
		if _, ok := unmarshalColdMemoryCondition(full[:n]); ok {
			// Only the exact full length should succeed; any truncation must fail.
			t.Fatalf("truncated input of length %d (full=%d) unexpectedly decoded", n, len(full))
		}
	}
	if _, ok := unmarshalColdMemoryCondition(nil); ok {
		t.Fatalf("nil input should not decode")
	}
}

func marketLikeEq(a, b MemoryMarket) bool {
	return a.ID == b.ID && a.QuestionCount == b.QuestionCount && hashSliceEq(a.QuestionIDs, b.QuestionIDs) &&
		a.UpdatedAtBlock == b.UpdatedAtBlock && a.UpdatedAt == b.UpdatedAt &&
		a.BlockNumber == b.BlockNumber && a.TxIndex == b.TxIndex && a.LogIndex == b.LogIndex &&
		a.Tombstone == b.Tombstone
}

func TestColdCodecMarketRoundTrip(t *testing.T) {
	cases := []MemoryMarket{
		{},
		{QuestionIDs: []common.Hash{}},
		{
			ID:             common.HexToHash("0xmarket"),
			QuestionCount:  2,
			QuestionIDs:    []common.Hash{common.HexToHash("0x10"), common.HexToHash("0x20"), common.HexToHash("0x30")},
			UpdatedAtBlock: 100, UpdatedAt: 999, BlockNumber: 100, TxIndex: 1, LogIndex: 2,
			Tombstone: true,
		},
	}
	for i, want := range cases {
		data := marshalColdMemoryMarket(want)
		got, ok := unmarshalColdMemoryMarket(data)
		if !ok {
			t.Fatalf("case %d: unmarshalColdMemoryMarket returned false", i)
		}
		if !marketLikeEq(got, want) {
			t.Fatalf("case %d: round-trip mismatch\n got: %+v\nwant: %+v", i, got, want)
		}
	}
}

func negRiskEq(a, b MemoryNegRiskEvent) bool {
	return a.ID == b.ID && a.QuestionCount == b.QuestionCount && hashSliceEq(a.QuestionIDs, b.QuestionIDs) &&
		a.UpdatedAtBlock == b.UpdatedAtBlock && a.UpdatedAt == b.UpdatedAt &&
		a.BlockNumber == b.BlockNumber && a.TxIndex == b.TxIndex && a.LogIndex == b.LogIndex &&
		a.Tombstone == b.Tombstone
}

func TestColdCodecNegRiskEventRoundTrip(t *testing.T) {
	cases := []MemoryNegRiskEvent{
		{},
		{
			ID:            common.HexToHash("0xnr"),
			QuestionCount: 4,
			QuestionIDs: []common.Hash{
				common.HexToHash("0xa"), common.HexToHash("0xb"),
				common.HexToHash("0xc"), common.HexToHash("0xd"),
			},
			UpdatedAt: 1234567890, Tombstone: true,
		},
	}
	for i, want := range cases {
		data := marshalColdMemoryNegRiskEvent(want)
		got, ok := unmarshalColdMemoryNegRiskEvent(data)
		if !ok {
			t.Fatalf("case %d: unmarshalColdMemoryNegRiskEvent returned false", i)
		}
		if !negRiskEq(got, want) {
			t.Fatalf("case %d: round-trip mismatch\n got: %+v\nwant: %+v", i, got, want)
		}
	}
}
