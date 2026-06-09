package polymarket

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestBabyJubJubManualStep manually computes the BabyJubJub collection ID
// to trace through the algorithm step by step
func TestBabyJubJubManualStep(t *testing.T) {
	// From TypeScript test case 1
	negRiskMarketId := common.HexToHash("0xcc4727a6394620b9c8ae82db3db50a34d5ca9828675547bcc4cddf5e86b63000")
	questionIndex := uint32(7)
	outcomeIndex := uint8(0)

	t.Log("=== Step-by-step BabyJubJub computation ===")

	// Step 1: getNegRiskQuestionId
	questionID := getNegRiskFallbackQuestionID(negRiskMarketId, questionIndex)
	t.Logf("QuestionID: %s", questionID.Hex())
	// Expected: 0xcc4727a6394620b9c8ae82db3db50a34d5ca9828675547bcc4cddf5e86b63007

	// Step 2: getConditionId
	// Create 84-byte payload
	conditionPayload := make([]byte, 84)
	copy(conditionPayload[:20], negRiskAdapterAddr.Bytes()) // 20 bytes oracle
	copy(conditionPayload[20:52], questionID.Bytes())      // 32 bytes questionId
	conditionPayload[83] = 0x02                              // 1 byte for outcomeSlotCount
	conditionID := crypto.Keccak256Hash(conditionPayload)
	t.Logf("ConditionID: %s", conditionID.Hex())

	// Step 3: computeCollectionId
	// Create hash payload: 64 bytes
	hashPayload := make([]byte, 64)
	copy(hashPayload[:32], conditionID.Bytes())
	hashPayload[63] = byte(1 << outcomeIndex) // indexSet = 1 << outcomeIndex
	t.Logf("HashPayload[63]: %d", hashPayload[63])

	// Hash
	hashResult := crypto.Keccak256Hash(hashPayload)
	t.Logf("HashResult (BE): %s", hashResult.Hex())

	// Reverse for LE
	hashBytes := hashResult.Bytes()
	reversed := make([]byte, 32)
	for i := 0; i < 32; i++ {
		reversed[i] = hashBytes[31-i]
	}
	t.Logf("HashResult (LE): %s", hex.EncodeToString(reversed))

	// Convert to BigInt without reversing to see if it matches
	x1 := new(big.Int).SetBytes(hashResult.Bytes())
	t.Logf("x1 initial (decimal): %s", x1.String())
	odd := x1.Bit(255) == 1
	t.Logf("MSB (255) set: %v", odd)

	// Find point on curve
	count := 0
	yy := new(big.Int)
	B := big.NewInt(3)
	P, _ := new(big.Int).SetString("21888242871839275222246405745257275088696311157297823662689037894645226208583", 10)
	PMinus1Over2 := new(big.Int).Sub(P, big.NewInt(1))
	PMinus1Over2.Div(PMinus1Over2, big.NewInt(2))

	for {
		x1.Add(x1, big.NewInt(1))
		x1.Mod(x1, P)
		count++

		yy.Mul(x1, x1)
		yy.Mul(yy, x1)
		yy.Add(yy, B)
		yy.Mod(yy, P)

		legendre := new(big.Int).Exp(yy, PMinus1Over2, P)
		if legendre.Cmp(big.NewInt(1)) == 0 {
			break
		}

		if count > 10000 {
			t.Fatalf("Could not find point after %d iterations", count)
		}
	}
	t.Logf("Found point after %d iterations", count)
	t.Logf("x1 after curve search (decimal): %s", x1.String())

	// Toggle bit 254 if MSB was set
	if odd {
		if x1.Bit(254) == 0 {
			x1.SetBit(x1, 254, 1)
		} else {
			x1.SetBit(x1, 254, 0)
		}
		t.Logf("x1 after bit toggle (decimal): %s", x1.String())
	}

	// Convert to hex (32 bytes LE)
	x1Hex := x1.Text(16)
	for len(x1Hex) < 64 {
		x1Hex = "0" + x1Hex
	}
	t.Logf("x1 hex (LE): %s", x1Hex)

	// Convert to bytes
	collectionBytes := common.FromHex(x1Hex)
	var collectionID common.Hash
	copy(collectionID[:], collectionBytes)
	t.Logf("CollectionID: %s", collectionID.Hex())

	// Step 4: computePositionIdFromCollectionId
	positionPayload := make([]byte, 52)
	copy(positionPayload[:20], negRiskWrappedCollateral.Bytes())
	copy(positionPayload[20:52], collectionID.Bytes())
	positionHash := crypto.Keccak256Hash(positionPayload)
	t.Logf("PositionID: %s", positionHash.Hex())
	t.Logf("PositionID (decimal): %s", new(big.Int).SetBytes(positionHash.Bytes()).String())

	// Expected from TypeScript
	expected := "96833685517457790753237027711749956491556223430098771101130535462280443103710"
	t.Logf("Expected (decimal): %s", expected)

	actual := new(big.Int).SetBytes(positionHash.Bytes()).String()
	if actual != expected {
		t.Errorf("PositionID mismatch: got %s, want %s", actual, expected)
	}
}
