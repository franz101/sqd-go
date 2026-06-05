package generated

import (
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	bn256 "github.com/ethereum/go-ethereum/crypto/bn256/cloudflare"
	"github.com/franz101/sqd-go/drafts/protomath"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

var NegRiskAdapterAddr = common.HexToAddress("0xd91E80cF2E7be2e162c6513ceD06f1dD0dA35296")
var NegRiskWrappedCollateralAddr = common.HexToAddress("0x3A3BD7bb9528E159577F7C2e685CC81A765002E2")

type UserPositionKey struct {
	User    common.Address
	TokenID common.Hash
}

type InternalPositionState struct {
	ID          UserPositionKey
	TokenID     uint256.Int
	Amount      decimal.Decimal
	AvgPrice    decimal.Decimal
	RealizedPnL decimal.Decimal
	TotalBought decimal.Decimal
}

func decimal256FromDecimal(d decimal.Decimal) protomath.Decimal256 {
	coeff := d.Shift(18).BigInt()
	out, ok := protomath.FromDecimal256ScaledBigInt(coeff)
	if !ok {
		panic(fmt.Sprintf("decimal %v cannot fit Decimal256", d))
	}
	return out
}

func decimalFromDecimal256(v protomath.Decimal256) decimal.Decimal {
	return decimal.NewFromBigInt(v.ScaledBig(), -18)
}

func GetConditionID(oracle common.Address, questionID common.Hash) common.Hash {
	var payload [84]byte
	copy(payload[:20], oracle.Bytes())
	copy(payload[20:52], questionID.Bytes())
	payload[83] = 0x02
	return crypto.Keccak256Hash(payload[:])
}

func (s *State) SaveUserPosition(p *InternalPositionState, meta EventMeta) {
	var h common.Hash
	p.TokenID.WriteToSlice(h[:])
	up := &Position{
		User:        p.ID.User,
		TokenID:     h,
		Amount:      decimal256FromDecimal(p.Amount),
		AvgPrice:    decimal256FromDecimal(p.AvgPrice),
		RealizedPnL: decimal256FromDecimal(p.RealizedPnL),
		TotalBought: decimal256FromDecimal(p.TotalBought),
	}
	s.Position.Save(up, meta)
}

func (s *State) GetUserPosition(user common.Address, tokenID uint256.Int) *InternalPositionState {
	var h common.Hash
	tokenID.WriteToSlice(h[:])
	up, ok := s.Position.Get(user, h)
	if !ok {
		return nil
	}
	return &InternalPositionState{
		ID:          UserPositionKey{User: user, TokenID: h},
		TokenID:     tokenID,
		Amount:      decimalFromDecimal256(up.Amount),
		AvgPrice:    decimalFromDecimal256(up.AvgPrice),
		RealizedPnL: decimalFromDecimal256(up.RealizedPnL),
		TotalBought: decimalFromDecimal256(up.TotalBought),
	}
}

func (s *State) GetCondition(conditionID common.Hash) (*Condition, bool) {
	return s.Condition.Get(conditionID)
}

func (s *State) SaveCondition(cond *Condition, meta EventMeta) {
	s.Condition.Save(cond, meta)
}

func (s *State) SaveNegRiskEvent(nr *NegRiskEvent, meta EventMeta) {
	s.NegRiskEvent.Save(nr, meta)
}

func (s *State) GetPositionID(collateral common.Address, collection common.Hash) uint256.Int {
	var buf [52]byte
	copy(buf[0:20], collateral[:])
	copy(buf[20:52], collection[:])
	var val uint256.Int
	val.SetBytes(crypto.Keccak256(buf[:]))
	return val
}

func (s *State) GetNegRiskPositionIDByCondition(conditionID common.Hash, outcome uint8) uint256.Int {
	indexSet := new(uint256.Int).Lsh(uint256.NewInt(1), uint(outcome))
	var buf [52]byte
	copy(buf[0:20], NegRiskWrappedCollateralAddr.Bytes())
	collID := s.GetCollectionID(common.Hash{}, conditionID, *indexSet)
	copy(buf[20:52], collID.Bytes())
	var val uint256.Int
	val.SetBytes(crypto.Keccak256(buf[:]))
	return val
}

func (s *State) GetNegRiskPositionID(marketID common.Hash, questionIndex uint32, outcomeIndex uint8) uint256.Int {
	questionID := s.GetNegRiskFallbackQuestionID(marketID, questionIndex)
	conditionID := GetConditionID(NegRiskAdapterAddr, questionID)
	return s.GetNegRiskPositionIDByCondition(conditionID, outcomeIndex)
}

func (s *State) GetNegRiskFallbackQuestionID(marketID common.Hash, questionIndex uint32) common.Hash {
	if questionIndex < 256 {
		questionID := marketID
		questionID[31] = byte(questionIndex)
		return questionID
	}
	var buf [36]byte
	copy(buf[:32], marketID[:])
	binary.BigEndian.PutUint32(buf[32:], questionIndex)
	return crypto.Keccak256Hash(buf[:])
}

var (
	ctPCompat            = uint256FromDecimalCompat("21888242871839275222246405745257275088696311157297823662689037894645226208583")
	ctBCompat            = big.NewInt(3)
	ctOneCompat          = big.NewInt(1)
	ctParityBitCompat    = new(big.Int).Lsh(big.NewInt(1), 254)
	ctLow254MaskCompat   = new(big.Int).Sub(new(big.Int).Set(ctParityBitCompat), ctOneCompat)
	ctSqrtExponentCompat = new(big.Int).Rsh(new(big.Int).Add(new(big.Int).Set(ctPCompat), ctOneCompat), 2)
)

func uint256FromDecimalCompat(v string) *big.Int {
	n, ok := new(big.Int).SetString(v, 10)
	if !ok {
		panic("invalid uint256 decimal constant")
	}
	return n
}

func (s *State) GetCollectionID(parentCollectionID common.Hash, conditionID common.Hash, indexSet uint256.Int) common.Hash {
	idxSetBig := indexSet.ToBig()
	x1 := hashConditionAndIndexSetCompat(conditionID, idxSetBig)
	odd := new(big.Int).Rsh(new(big.Int).Set(x1), 255).Sign() != 0

	var y1 *big.Int
	for {
		x1 = addModCompat(x1, ctOneCompat, ctPCompat)
		yy := addModCompat(mulModCompat(x1, mulModCompat(x1, x1, ctPCompat), ctPCompat), ctBCompat, ctPCompat)
		y1 = sqrtModPCompat(yy)
		if mulModCompat(y1, y1, ctPCompat).Cmp(yy) == 0 {
			break
		}
	}
	if (odd && y1.Bit(0) == 0) || (!odd && y1.Bit(0) == 1) {
		y1 = new(big.Int).Sub(ctPCompat, y1)
	}

	x2 := new(big.Int).SetBytes(parentCollectionID[:])
	if x2.Sign() != 0 {
		odd = new(big.Int).Rsh(new(big.Int).Set(x2), 254).Sign() != 0
		x2.And(x2, ctLow254MaskCompat)

		yy := addModCompat(mulModCompat(x2, mulModCompat(x2, x2, ctPCompat), ctPCompat), ctBCompat, ctPCompat)
		y2 := sqrtModPCompat(yy)
		if (odd && y2.Bit(0) == 0) || (!odd && y2.Bit(0) == 1) {
			y2 = new(big.Int).Sub(ctPCompat, y2)
		}
		if mulModCompat(y2, y2, ctPCompat).Cmp(yy) != 0 {
			panic("invalid parent collection ID")
		}

		x1, y1 = ecAddCompat(x1, y1, x2, y2)
	}

	if y1.Bit(0) == 1 {
		x1.Xor(x1, ctParityBitCompat)
	}

	return common.BigToHash(x1)
}

func hashConditionAndIndexSetCompat(conditionID common.Hash, indexSet *big.Int) *big.Int {
	idx := common.BigToHash(indexSet)
	h := crypto.Keccak256Hash(conditionID.Bytes(), idx.Bytes())
	return new(big.Int).SetBytes(h[:])
}

func addModCompat(a, b, m *big.Int) *big.Int {
	out := new(big.Int).Add(a, b)
	out.Mod(out, m)
	return out
}

func mulModCompat(a, b, m *big.Int) *big.Int {
	out := new(big.Int).Mul(a, b)
	out.Mod(out, m)
	return out
}

func sqrtModPCompat(x *big.Int) *big.Int {
	return new(big.Int).Exp(x, ctSqrtExponentCompat, ctPCompat)
}

func ecAddCompat(x1, y1, x2, y2 *big.Int) (*big.Int, *big.Int) {
	p1, err := affineToG1Compat(x1, y1)
	if err != nil {
		panic(fmt.Sprintf("ecadd failed for first point: %v", err))
	}
	p2, err := affineToG1Compat(x2, y2)
	if err != nil {
		panic(fmt.Sprintf("ecadd failed for second point: %v", err))
	}
	return g1ToAffineCompat(new(bn256.G1).Add(p1, p2))
}

func affineToG1Compat(x, y *big.Int) (*bn256.G1, error) {
	point := make([]byte, 64)
	xb := x.Bytes()
	yb := y.Bytes()
	copy(point[32-len(xb):32], xb)
	copy(point[64-len(yb):], yb)

	g := new(bn256.G1)
	_, err := g.Unmarshal(point)
	if err != nil {
		return nil, err
	}
	return g, nil
}

func g1ToAffineCompat(g *bn256.G1) (*big.Int, *big.Int) {
	m := g.Marshal()
	if len(m) != 64 {
		panic(fmt.Sprintf("unexpected marshaled G1 length: %d", len(m)))
	}
	return new(big.Int).SetBytes(m[:32]), new(big.Int).SetBytes(m[32:])
}
