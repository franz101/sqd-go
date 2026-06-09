package polymarket

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// TestBabyJubJubCollectionIdParity1 tests parity with TypeScript test case 1
// From: polymarket-subgraph/tests/getNegRiskPositionId.test.ts
func TestBabyJubJubCollectionIdParity1(t *testing.T) {
	negRiskMarketId := common.HexToHash("0xcc4727a6394620b9c8ae82db3db50a34d5ca9828675547bcc4cddf5e86b63000")
	questionIndex := uint32(7)

	// TypeScript expected values (as decimal strings from BigInt.toString())
	expectedPositionId0 := "96833685517457790753237027711749956491556223430098771101130535462280443103710"
	expectedPositionId1 := "112683192116716745370273337699109698649408967993699289993927321945615517688893"

	// Compute using Go implementation
	computed0 := getNegRiskPositionID(negRiskMarketId, questionIndex, 0)
	computed1 := getNegRiskPositionID(negRiskMarketId, questionIndex, 1)

	// Convert uint256 to big.Int for string comparison
	computed0Str := new(big.Int).SetBytes(computed0.Bytes()).String()
	computed1Str := new(big.Int).SetBytes(computed1.Bytes()).String()

	t.Logf("MarketID: %s", negRiskMarketId.Hex())
	t.Logf("QuestionIndex: %d", questionIndex)
	t.Logf("Expected PositionId0: %s", expectedPositionId0)
	t.Logf("Computed PositionId0: %s", computed0Str)
	t.Logf("Expected PositionId1: %s", expectedPositionId1)
	t.Logf("Computed PositionId1: %s", computed1Str)

	if computed0Str != expectedPositionId0 {
		t.Errorf("PositionId0 mismatch: got %s, want %s", computed0Str, expectedPositionId0)
	}
	if computed1Str != expectedPositionId1 {
		t.Errorf("PositionId1 mismatch: got %s, want %s", computed1Str, expectedPositionId1)
	}
}

// TestBabyJubJubCollectionIdParity2 tests parity with TypeScript test case 2
func TestBabyJubJubCollectionIdParity2(t *testing.T) {
	negRiskMarketId := common.HexToHash("0x904aa321a48f737e2223e7b3007bf51d68b6a0d66bdda0c1e4bc581f55d62800")
	questionIndex := uint32(4)

	expectedPositionId0 := "11031149734538275426690039809123992018327740438980973428241361937177748285493"
	expectedPositionId1 := "92849115097658926029726616555072992123532598747617388960074918380114146610948"

	computed0 := getNegRiskPositionID(negRiskMarketId, questionIndex, 0)
	computed1 := getNegRiskPositionID(negRiskMarketId, questionIndex, 1)

	computed0Str := new(big.Int).SetBytes(computed0.Bytes()).String()
	computed1Str := new(big.Int).SetBytes(computed1.Bytes()).String()

	t.Logf("MarketID: %s", negRiskMarketId.Hex())
	t.Logf("QuestionIndex: %d", questionIndex)
	t.Logf("Expected PositionId0: %s", expectedPositionId0)
	t.Logf("Computed PositionId0: %s", computed0Str)
	t.Logf("Expected PositionId1: %s", expectedPositionId1)
	t.Logf("Computed PositionId1: %s", computed1Str)

	if computed0Str != expectedPositionId0 {
		t.Errorf("PositionId0 mismatch: got %s, want %s", computed0Str, expectedPositionId0)
	}
	if computed1Str != expectedPositionId1 {
		t.Errorf("PositionId1 mismatch: got %s, want %s", computed1Str, expectedPositionId1)
	}
}

// TestBabyJubJubCollectionIdParity3 tests parity with TypeScript test case 3
func TestBabyJubJubCollectionIdParity3(t *testing.T) {
	negRiskMarketId := common.HexToHash("0x904aa321a48f737e2223e7b3007bf51d68b6a0d66bdda0c1e4bc581f55d62800")
	questionIndex := uint32(3)

	expectedPositionId0 := "92934986068759649975171712359405804888500621431140776758674716227798619042594"
	expectedPositionId1 := "83272680118121060051327450493118657102857345150945269348505485036103238138715"

	computed0 := getNegRiskPositionID(negRiskMarketId, questionIndex, 0)
	computed1 := getNegRiskPositionID(negRiskMarketId, questionIndex, 1)

	computed0Str := new(big.Int).SetBytes(computed0.Bytes()).String()
	computed1Str := new(big.Int).SetBytes(computed1.Bytes()).String()

	t.Logf("MarketID: %s", negRiskMarketId.Hex())
	t.Logf("QuestionIndex: %d", questionIndex)
	t.Logf("Expected PositionId0: %s", expectedPositionId0)
	t.Logf("Computed PositionId0: %s", computed0Str)
	t.Logf("Expected PositionId1: %s", expectedPositionId1)
	t.Logf("Computed PositionId1: %s", computed1Str)

	if computed0Str != expectedPositionId0 {
		t.Errorf("PositionId0 mismatch: got %s, want %s", computed0Str, expectedPositionId0)
	}
	if computed1Str != expectedPositionId1 {
		t.Errorf("PositionId1 mismatch: got %s, want %s", computed1Str, expectedPositionId1)
	}
}

// TestBabyJubJubCollectionIdParity4 tests parity with TypeScript test case 4
func TestBabyJubJubCollectionIdParity4(t *testing.T) {
	negRiskMarketId := common.HexToHash("0x5e596465dca57c10c8b175f901974e2de2877498410b0210d0a21b57e14da000")
	questionIndex := uint32(4)

	expectedPositionId0 := "30637845681714148498359907433169105263223689440526909041094893305583115580796"
	expectedPositionId1 := "111796127100720291855951404495290728144208289103084969375425640210971192620108"

	computed0 := getNegRiskPositionID(negRiskMarketId, questionIndex, 0)
	computed1 := getNegRiskPositionID(negRiskMarketId, questionIndex, 1)

	computed0Str := new(big.Int).SetBytes(computed0.Bytes()).String()
	computed1Str := new(big.Int).SetBytes(computed1.Bytes()).String()

	t.Logf("MarketID: %s", negRiskMarketId.Hex())
	t.Logf("QuestionIndex: %d", questionIndex)
	t.Logf("Expected PositionId0: %s", expectedPositionId0)
	t.Logf("Computed PositionId0: %s", computed0Str)
	t.Logf("Expected PositionId1: %s", expectedPositionId1)
	t.Logf("Computed PositionId1: %s", computed1Str)

	if computed0Str != expectedPositionId0 {
		t.Errorf("PositionId0 mismatch: got %s, want %s", computed0Str, expectedPositionId0)
	}
	if computed1Str != expectedPositionId1 {
		t.Errorf("PositionId1 mismatch: got %s, want %s", computed1Str, expectedPositionId1)
	}
}

// TestBabyJubJubComputeCollectionId tests the computeCollectionId function directly
func TestBabyJubJubComputeCollectionId(t *testing.T) {
	// Test with known values from the TypeScript implementation
	conditionID := common.HexToHash("0x8a4c788f043023b8b28a762216d037e9f148532b")
	outcomeIndex := uint8(1)

	// This should compute a collection ID using BabyJubJub curve
	collectionId := computeCollectionId(conditionID, outcomeIndex)

	t.Logf("ConditionID: %s", conditionID.Hex())
	t.Logf("OutcomeIndex: %d", outcomeIndex)
	t.Logf("Computed CollectionID: %s", collectionId.Hex())

	// The collection ID should be a valid 32-byte hash
	if collectionId == (common.Hash{}) {
		t.Error("Collection ID is zero")
	}
}

// TestBabyJubJubLegendreSymbol tests the Legendre symbol computation
func TestBabyJubJubLegendreSymbol(t *testing.T) {
	// Test known quadratic residues modulo P
	// For P, 4 is a quadratic residue (2^2 = 4)
	four := big.NewInt(4)
	result := legendreSymbol(four)

	// Legendre symbol of 4 should be 1 (it's a quadratic residue)
	if result.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("Legendre symbol of 4 should be 1, got %s", result.String())
	}

	// Test a non-quadratic residue
	// For BabyJubJub P, many values are non-residues
	// The result should be -1 (P-1) for non-residues or 0 for zero
	zero := big.NewInt(0)
	result = legendreSymbol(zero)
	if result.Cmp(big.NewInt(0)) != 0 {
		t.Errorf("Legendre symbol of 0 should be 0, got %s", result.String())
	}
}

// TestBabyJubJubCollectionIdIncrementOrder tests that x1 is incremented BEFORE checking
// This is a critical difference from the original buggy implementation
func TestBabyJubJubCollectionIdIncrementOrder(t *testing.T) {
	conditionID := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")
	outcomeIndex := uint8(0)

	// Hash the payload
	hashPayload := make([]byte, 64)
	copy(hashPayload[:32], conditionID.Bytes())
	hashPayload[63] = byte(1 << outcomeIndex)

	hashResult := crypto.Keccak256Hash(hashPayload)
	hashBytes := hashResult.Bytes()
	reversed := make([]byte, len(hashBytes))
	for i := 0; i < len(hashBytes); i++ {
		reversed[i] = hashBytes[len(hashBytes)-1-i]
	}
	x1Initial := new(big.Int).SetBytes(reversed)

	// Compute the collection ID
	collectionId := computeCollectionId(conditionID, outcomeIndex)

	// Now extract the x value from the collection ID and verify it was incremented
	// The final x1 in the collection ID should be different from the initial hash
	// (unless it happened to find a point immediately, which is unlikely)
	collectionBytes := collectionId.Bytes()
	collectionX := new(big.Int).SetBytes(collectionBytes)

	// If MSB (bit 255) was not set, the collection X should be greater than x1Initial
	// because we incremented at least once
	odd := x1Initial.Bit(255) == 1
	if !odd && collectionX.Cmp(x1Initial) <= 0 {
		t.Errorf("Collection X (%s) should be greater than initial X (%s) when incremented",
			collectionX.String(), x1Initial.String())
	}

	t.Logf("Initial X: %s", x1Initial.String())
	t.Logf("Collection X: %s", collectionX.String())
	t.Logf("Odd MSB: %v", odd)
}

// TestGetNegRiskPositionIDByCondition tests the position ID generation from condition ID
func TestGetNegRiskPositionIDByCondition(t *testing.T) {
	conditionID := common.HexToHash("0x8a4c788f043023b8b28a762216d037e9f148532b")

	// Test both outcome indices (0 = YES, 1 = NO)
	posIDYes := getNegRiskPositionIDByCondition(conditionID, 0)
	posIDNo := getNegRiskPositionIDByCondition(conditionID, 1)

	t.Logf("ConditionID: %s", conditionID.Hex())
	t.Logf("YES Token PositionID: %s", posIDYes.Hex())
	t.Logf("NO Token PositionID: %s", posIDNo.Hex())

	// The position IDs should be different
	if posIDYes == posIDNo {
		t.Error("YES and NO position IDs should be different")
	}

	// Both should be non-zero
	if posIDYes.IsZero() {
		t.Error("YES position ID is zero")
	}
	if posIDNo.IsZero() {
		t.Error("NO position ID is zero")
	}
}

// TestGetNegRiskPositionID tests the full position ID generation from market ID
func TestGetNegRiskPositionID(t *testing.T) {
	marketID := common.HexToHash("0xcc4727a6394620b9c8ae82db3db50a34d5ca9828675547bcc4cddf5e86b63000")
	questionIndex := uint32(7)

	posID0 := getNegRiskPositionID(marketID, questionIndex, 0)
	posID1 := getNegRiskPositionID(marketID, questionIndex, 1)

	t.Logf("MarketID: %s", marketID.Hex())
	t.Logf("QuestionIndex: %d", questionIndex)
	t.Logf("PositionID 0: %s", posID0.Hex())
	t.Logf("PositionID 1: %s", posID1.Hex())

	// The position IDs should be different
	if posID0 == posID1 {
		t.Error("Position IDs for different outcomes should be different")
	}

	// Both should be non-zero
	if posID0.IsZero() {
		t.Error("PositionID 0 is zero")
	}
	if posID1.IsZero() {
		t.Error("PositionID 1 is zero")
	}
}

// TestBabyJubJubUInt256Support tests that uint256.Int is properly supported
func TestBabyJubJubUInt256Support(t *testing.T) {
	// Test that we can properly handle uint256.Int values
	// This is important for position ID generation

	// Create a test position ID
	testValue := new(uint256.Int)
	testValue.SetUint64(12345)

	// Convert to big.Int
	bigIntValue := new(big.Int).SetBytes(testValue.Bytes())
	if bigIntValue.Uint64() != 12345 {
		t.Errorf("uint256 to big.Int conversion failed: got %d, want 12345", bigIntValue.Uint64())
	}

	// Create a uint256 from a big.Int
	largeValue := new(big.Int)
	largeValue.SetString("96833685517457790753237027711749956491556223430098771101130535462280443103710", 10)
	uint256Value := new(uint256.Int).SetBytes(largeValue.Bytes())

	// Convert back and verify
	result := new(big.Int).SetBytes(uint256Value.Bytes())
	if result.String() != largeValue.String() {
		t.Errorf("big.Int to uint256 to big.Int round-trip failed: got %s, want %s", result.String(), largeValue.String())
	}

	t.Logf("uint256.Int support verified: %s", uint256Value.Dec())
}
