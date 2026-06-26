package polymarket

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// BabyJubJub curve parameters for computeCollectionId
var (
	// P is the prime modulus of the BabyJubJub curve field
	P = func() *big.Int {
		result, _ := new(big.Int).SetString("21888242871839275222246405745257275088696311157297823662689037894645226208583", 10)
		return result
	}()

	// B is the coefficient in the curve equation: y^2 = x^3 + 3
	B = big.NewInt(3)

	// oddToggle is the bit to toggle for odd MSB
	oddToggle = new(big.Int).Lsh(big.NewInt(1), 254)

	// PMinus1Over2 is (P-1)/2, used for Legendre symbol
	PMinus1Over2 = func() *big.Int {
		result := new(big.Int).Sub(P, big.NewInt(1))
		result.Div(result, big.NewInt(2))
		return result
	}()
)

// legendreSymbol computes a^((P-1)/2) mod P
// Returns 1 if a is a quadratic residue, -1 if not, 0 if a is 0
func legendreSymbol(a *big.Int) *big.Int {
	if a.Sign() == 0 {
		return big.NewInt(0)
	}
	// Compute a^((P-1)/2) mod P
	result := new(big.Int).Exp(a, PMinus1Over2, P)
	return result
}

// computeCollectionId computes the collection ID for a condition ID and outcome index
// following the BabyJubJub curve algorithm from the CTF library.
// This is equivalent to computeCollectionId in ctf-utils.ts
func computeCollectionId(conditionID common.Hash, outcomeIndex uint8) common.Hash {
	// Create hash payload: 64 bytes with conditionId (32 bytes) + indexSet (32 bytes)
	hashPayload := make([]byte, 64)
	// First 32 bytes is conditionId
	copy(hashPayload[:32], conditionID.Bytes())
	// Second 32 bytes is index set - put outcomeIndex at byte 63 (indexSet = 1 << outcomeIndex)
	hashPayload[63] = byte(1 << outcomeIndex)

	// Hash the payload
	hashResult := crypto.Keccak256Hash(hashPayload)

	x1 := new(big.Int).SetBytes(hashResult.Bytes())

	// Check if MSB is set (bit 255)
	odd := x1.Bit(255) == 1

	// Find a point on the curve: increment x1 UNTIL legendreSymbol(x^3 + 3) == 1
	// IMPORTANT: We increment x1 FIRST, then check (matches TypeScript ctf-utils.ts)
	yy := new(big.Int)
	for {
		// Increment x1 first (matches TypeScript: do { x1 += 1; check; } while)
		x1.Add(x1, big.NewInt(1))
		x1.Mod(x1, P)

		// Compute x^3 + 3 mod P
		yy.Mul(x1, x1) // x^2
		yy.Mul(yy, x1) // x^3
		yy.Add(yy, B)  // x^3 + 3
		yy.Mod(yy, P)  // mod P

		// Check if yy is a quadratic residue
		legendre := legendreSymbol(yy)
		if legendre.Cmp(big.NewInt(1)) == 0 {
			break // Found a point on the curve
		}
	}

	// If the original MSB was set, toggle bit 254
	if odd {
		if x1.Bit(254) == 0 {
			x1.SetBit(x1, 254, 1)
		} else {
			x1.SetBit(x1, 254, 0)
		}
	}

	// Convert back to 32 bytes (big-endian, matching Rust to_bytes_be)
	x1Bytes := x1.Bytes()
	var result common.Hash
	start := 32 - len(x1Bytes)
	copy(result[start:], x1Bytes)
	return result
}

// getNegRiskCollectionID computes the collection ID for neg-risk tokens
// using the BabyJubJub curve algorithm.
func getNegRiskCollectionID(conditionID common.Hash, outcomeIndex uint8) common.Hash {
	return computeCollectionId(conditionID, outcomeIndex)
}
