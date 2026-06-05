package experiment

import (
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

var (
	expCtP                = expUint256FromDecimal("21888242871839275222246405745257275088696311157297823662689037894645226208583")
	expCtB                = *uint256.NewInt(3)
	expCtOne              = *uint256.NewInt(1)
	expCtParityBit        = *new(uint256.Int).Lsh(uint256.NewInt(1), 254)
	expCtSqrtExponent     = expUint256FromDecimal("5472060717959818805561601436314318772174077789324455915672259473661306552146")
	expCtLegendreExponent = *new(uint256.Int).Rsh(new(uint256.Int).Sub(&expCtP, uint256.NewInt(1)), 1)
	expCtPMu              = uint256.Reciprocal(&expCtP)
	expHashPayloadPool    = sync.Pool{New: func() any { return new([64]byte) }}
)

func CollectionIDNoParentOriginalBig(conditionID common.Hash, outcome uint8) common.Hash {
	indexSet := expOutcomeIndexSet(outcome)
	return collectionIDNoParentOriginalBigIndex(conditionID, indexSet)
}

func CollectionIDNoParentFastSqrt(conditionID common.Hash, outcome uint8) common.Hash {
	indexSet := expOutcomeIndexSet(outcome)
	return collectionIDNoParentFastSqrtIndex(conditionID, indexSet)
}

func CollectionIDNoParentFastLegendre(conditionID common.Hash, outcome uint8) common.Hash {
	indexSet := expOutcomeIndexSet(outcome)
	return collectionIDNoParentFastLegendreIndex(conditionID, indexSet)
}

func CollectionIDNoParentFastSqrtPooled(conditionID common.Hash, outcome uint8) common.Hash {
	indexSet := expOutcomeIndexSet(outcome)
	return collectionIDNoParentFastSqrtPooledIndex(conditionID, indexSet)
}

func collectionIDNoParentOriginalBigIndex(conditionID common.Hash, indexSet uint256.Int) common.Hash {
	x1 := expHashConditionAndIndexSet(conditionID, indexSet)
	odd := expGetBit(&x1, 255) == 1

	var y1 uint256.Int
	for {
		x1.AddMod(&x1, &expCtOne, &expCtP)
		yy := expCurveYYScratch(&x1)
		y1 = expSqrtModPBig(&yy)
		var y1Sq uint256.Int
		y1Sq.MulMod(&y1, &y1, &expCtP)
		if y1Sq.Eq(&yy) {
			break
		}
	}
	if (odd && expGetBit(&y1, 0) == 0) || (!odd && expGetBit(&y1, 0) == 1) {
		y1.Sub(&expCtP, &y1)
	}
	if expGetBit(&y1, 0) == 1 {
		x1.Xor(&x1, &expCtParityBit)
	}

	var res common.Hash
	x1.WriteToSlice(res[:])
	return res
}

func collectionIDNoParentFastSqrtIndex(conditionID common.Hash, indexSet uint256.Int) common.Hash {
	x1 := expHashConditionAndIndexSet(conditionID, indexSet)
	odd := expGetBit(&x1, 255) == 1

	for {
		x1.AddMod(&x1, &expCtOne, &expCtP)
		yy := expCurveYYScratch(&x1)
		y := expPowModP(&yy, &expCtSqrtExponent)
		var ySq uint256.Int
		ySq.MulModWithReciprocal(&y, &y, &expCtP, &expCtPMu)
		if ySq.Eq(&yy) {
			break
		}
	}
	if odd {
		x1.Xor(&x1, &expCtParityBit)
	}

	var res common.Hash
	x1.WriteToSlice(res[:])
	return res
}

func collectionIDNoParentFastSqrtPooledIndex(conditionID common.Hash, indexSet uint256.Int) common.Hash {
	x1 := expHashConditionAndIndexSetPooled(conditionID, indexSet)
	odd := expGetBit(&x1, 255) == 1

	for {
		x1.AddMod(&x1, &expCtOne, &expCtP)
		yy := expCurveYYScratch(&x1)
		y := expPowModP(&yy, &expCtSqrtExponent)
		var ySq uint256.Int
		ySq.MulModWithReciprocal(&y, &y, &expCtP, &expCtPMu)
		if ySq.Eq(&yy) {
			break
		}
	}
	if odd {
		x1.Xor(&x1, &expCtParityBit)
	}

	var res common.Hash
	x1.WriteToSlice(res[:])
	return res
}

func collectionIDNoParentFastLegendreIndex(conditionID common.Hash, indexSet uint256.Int) common.Hash {
	x1 := expHashConditionAndIndexSet(conditionID, indexSet)
	odd := expGetBit(&x1, 255) == 1

	for {
		x1.AddMod(&x1, &expCtOne, &expCtP)
		yy := expCurveYYScratch(&x1)
		legendre := expPowModP(&yy, &expCtLegendreExponent)
		if legendre.Eq(&expCtOne) {
			break
		}
	}
	if odd {
		x1.Xor(&x1, &expCtParityBit)
	}

	var res common.Hash
	x1.WriteToSlice(res[:])
	return res
}

func expCurveYYScratch(x *uint256.Int) uint256.Int {
	var xSq, xCu, yy uint256.Int
	xSq.MulModWithReciprocal(x, x, &expCtP, &expCtPMu)
	xCu.MulModWithReciprocal(&xSq, x, &expCtP, &expCtPMu)
	yy.AddMod(&xCu, &expCtB, &expCtP)
	return yy
}

func expPowModP(base, exponent *uint256.Int) uint256.Int {
	var res uint256.Int
	res[0] = 1
	multiplier := *base
	bitLen := exponent.BitLen()
	for bit := 0; bit < bitLen; bit++ {
		if expGetBit(exponent, bit) == 1 {
			res.MulModWithReciprocal(&res, &multiplier, &expCtP, &expCtPMu)
		}
		multiplier.MulModWithReciprocal(&multiplier, &multiplier, &expCtP, &expCtPMu)
	}
	return res
}

func expHashConditionAndIndexSet(conditionID common.Hash, indexSet uint256.Int) uint256.Int {
	var payload [64]byte
	copy(payload[:32], conditionID[:])
	indexSet.WriteToSlice(payload[32:])
	h := crypto.Keccak256Hash(payload[:])
	var z uint256.Int
	z.SetBytes(h[:])
	return z
}

func expHashConditionAndIndexSetPooled(conditionID common.Hash, indexSet uint256.Int) uint256.Int {
	payload := expHashPayloadPool.Get().(*[64]byte)
	copy(payload[:32], conditionID[:])
	indexSet.WriteToSlice(payload[32:])
	h := crypto.Keccak256Hash(payload[:])
	expHashPayloadPool.Put(payload)
	var z uint256.Int
	z.SetBytes(h[:])
	return z
}

func expOutcomeIndexSet(outcome uint8) uint256.Int {
	var z uint256.Int
	z[outcome/64] = uint64(1) << (outcome % 64)
	return z
}

func expSqrtModPBig(x *uint256.Int) uint256.Int {
	xb := x.ToBig()
	eb := expCtSqrtExponent.ToBig()
	pb := expCtP.ToBig()
	res := new(big.Int).Exp(xb, eb, pb)
	var z uint256.Int
	z.SetFromBig(res)
	return z
}

func expUint256FromDecimal(s string) uint256.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("invalid decimal uint256")
	}
	var z uint256.Int
	z.SetFromBig(n)
	return z
}

func expGetBit(z *uint256.Int, i int) uint {
	if i < 0 || i >= 256 {
		return 0
	}
	if (z[i/64] & (uint64(1) << (i % 64))) != 0 {
		return 1
	}
	return 0
}
