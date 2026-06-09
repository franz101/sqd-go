package polymarket

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestBabyJubJubDebug is a debug test to trace through the BabyJubJub computation
func TestBabyJubJubDebug(t *testing.T) {
	conditionID := common.HexToHash("0xdb4ab1dbbedd6aeec17aa6e3f66262cff0e3b04742dd3acdf99652575e5422f8")
	outcomeIndex := uint8(0)

	t.Log("=== DEBUG: BabyJubJub Collection ID Parity Trace ===")
	t.Logf("ConditionID: %s", conditionID.Hex())

	// Step 1: computeCollectionId payload
	hashPayload := make([]byte, 64)
	copy(hashPayload[:32], conditionID.Bytes())
	hashPayload[63] = byte(1 << outcomeIndex)
	t.Logf("1. HashPayload: %s", hex.EncodeToString(hashPayload))

	// Hash the payload
	hashResult := crypto.Keccak256Hash(hashPayload)
	t.Logf("2. HashResult (BE): %s", hashResult.Hex())

	// Reverse for little-endian BigInt conversion to match AssemblyScript
	hashBytes := hashResult.Bytes()
	reversed := make([]byte, len(hashBytes))
	for i := 0; i < len(hashBytes); i++ {
		reversed[i] = hashBytes[len(hashBytes)-1-i]
	}
	t.Logf("3. HashResult (LE): %s", hex.EncodeToString(reversed))

	// Try BOTH reversed and unreversed to see which one works!
	for _, mode := range []string{"Reversed (LE)", "Unreversed (BE)"} {
		t.Logf("--- Running Curve search for %s ---", mode)
		var x1 *big.Int
		if mode == "Reversed (LE)" {
			x1 = new(big.Int).SetBytes(reversed)
		} else {
			x1 = new(big.Int).SetBytes(hashBytes)
		}
		t.Logf("x1 initial: %s", x1.String())

		// Check if MSB is set (bit 255)
		odd := x1.Bit(255) == 1
		t.Logf("Odd MSB: %v", odd)

		// Curve loop
		count := 0
		yy := new(big.Int)
		for {
			x1.Add(x1, big.NewInt(1))
			count++

			// Compute x^3 + 3 mod P
			yy.Mul(x1, x1) // x^2
			yy.Mul(yy, x1) // x^3
			yy.Add(yy, B)  // x^3 + 3
			yy.Mod(yy, P)  // mod P

			legendre := legendreSymbol(yy)
			if legendre.Cmp(big.NewInt(1)) == 0 {
				break
			}
			if count > 10000 {
				t.Log("ERROR: Could not find point")
				break
			}
		}
		t.Logf("Found point after %d iterations", count)
		t.Logf("x1 after curve search: %s", x1.String())

		if odd {
			if x1.Bit(254) == 0 {
				x1.SetBit(x1, 254, 1)
			} else {
				x1.SetBit(x1, 254, 0)
			}
			t.Logf("x1 after bit toggle: %s", x1.String())
		}

		// Convert back to 32 bytes
		x1Bytes := x1.Bytes()
		var collectionID common.Hash
		start := 32 - len(x1Bytes)
		copy(collectionID[start:], x1Bytes)
		t.Logf("Resulting CollectionID: %s", collectionID.Hex())
	}

	t.Log("\n=== Expected from TypeScript ===")
	t.Log("Expected CollectionID: 0x12adf3dfeaddeef8f31fa86654bf367c5c7b1e854dff407d7c87ff76af4ad16d")
}
