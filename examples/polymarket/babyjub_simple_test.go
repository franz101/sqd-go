package polymarket

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestBabyJubJubSimpleTest traces through the BabyJubJub computation for Test 2
func TestBabyJubJubSimpleTest(t *testing.T) {
	marketID := common.HexToHash("0x904aa321a48f737e2223e7b3007bf51d68b6a0d66bdda0c1e4bc581f55d62800")
	questionIndex := uint32(4)
	outcomeIndex := uint8(0)

	t.Log("=== Simple Trace ===")

	// Step 1: getNegRiskFallbackQuestionID
	questionID := getNegRiskFallbackQuestionID(marketID, questionIndex)
	t.Logf("QuestionID: %s", questionID.Hex())

	// Step 2: getConditionID
	conditionID := getConditionID(negRiskAdapterAddr, questionID)
	t.Logf("ConditionID: %s", conditionID.Hex())

	// Step 2.5: Hash payload for computeCollectionId
	hashPayload := make([]byte, 64)
	copy(hashPayload[:32], conditionID.Bytes())
	hashPayload[63] = byte(1 << outcomeIndex)
	t.Logf("HashPayload[63]: %d", hashPayload[63])

	hashResult := crypto.Keccak256Hash(hashPayload)
	t.Logf("HashResult: %s", hashResult.Hex())

	// Convert to BigInt (BE)
	x1 := new(big.Int).SetBytes(hashResult.Bytes())
	t.Logf("x1 initial (decimal): %s", x1.String())

	// Step 3: computeCollectionId
	collectionID := computeCollectionId(conditionID, outcomeIndex)
	t.Logf("CollectionID: %s", collectionID.Hex())
	t.Logf("Expected CollectionID: 0x465bbc80ae5c0024c411b2dc07c3448a69fd346269c637f7d061bbab359c887f")

	// Step 4: getPositionID
	positionID := getPositionID(negRiskWrappedCollateral, collectionID)
	t.Logf("PositionID (uint256): %s", positionID.Hex())

	// Convert to big.Int string for comparison
	positionIDBigInt := new(big.Int).SetBytes(positionID.Bytes())
	t.Logf("PositionID (decimal): %s", positionIDBigInt.String())

	expected := "11031149734538275426690039809123992018327740438980973428241361937177748285493"
	t.Logf("Expected (decimal): %s", expected)

	if positionIDBigInt.String() != expected {
		t.Errorf("Mismatch: got %s, want %s", positionIDBigInt.String(), expected)
	} else {
		t.Log("MATCH!")
	}
}
