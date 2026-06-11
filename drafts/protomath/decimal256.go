package protomath

import (
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
	"unicode"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/holiman/uint256"
)

const MaxDecimal256Scale int32 = 76

var (
	errDecimalScale  = errors.New("protomath: decimal scale must be between 0 and 76")
	errDecimalSyntax = errors.New("protomath: invalid decimal syntax")
	errDecimalRange  = errors.New("protomath: decimal coefficient is out of Decimal256 range")

	decimal256MaxPositive = uint256.Int{
		0xffffffffffffffff,
		0xffffffffffffffff,
		0xffffffffffffffff,
		0x7fffffffffffffff,
	}
	decimal256MinMagnitude = uint256.Int{
		0,
		0,
		0,
		0x8000000000000000,
	}
)

// Decimal256Scale describes a fixed ClickHouse Decimal256 scale.
//
// Keep one value per hot path and reuse it. The multiplier is a stack-friendly
// uint256.Int, not a heap decimal object.
type Decimal256Scale struct {
	scale      int32
	multiplier uint256.Int
}

var Decimal256Scale18 = MustDecimal256Scale(18)

func NewDecimal256Scale(scale int32) (Decimal256Scale, error) {
	if scale < 0 || scale > MaxDecimal256Scale {
		return Decimal256Scale{}, errDecimalScale
	}
	var multiplier uint256.Int
	multiplier.SetOne()
	ten := uint256.NewInt(10)
	for i := int32(0); i < scale; i++ {
		multiplier.Mul(&multiplier, ten)
	}
	return Decimal256Scale{scale: scale, multiplier: multiplier}, nil
}

func MustDecimal256Scale(scale int32) Decimal256Scale {
	out, err := NewDecimal256Scale(scale)
	if err != nil {
		panic(err)
	}
	return out
}

func (s Decimal256Scale) Scale() int32 {
	return s.scale
}

func (s Decimal256Scale) Multiplier() UInt256 {
	return FromHoliman(s.multiplier)
}

// Decimal256 is a math-enabled subtype of ch-go's proto.Decimal256.
//
// It is a signed two's-complement scaled integer, matching ClickHouse's
// Decimal256 column representation.
type Decimal256 proto.Decimal256

// ColDecimal256 is a draft math wrapper around proto.ColDecimal256.
type ColDecimal256 proto.ColDecimal256

func FromProtoDecimal256(v proto.Decimal256) Decimal256 {
	return Decimal256(v)
}

func (v Decimal256) Proto() proto.Decimal256 {
	return proto.Decimal256(v)
}

func FromScaledInt64(v int64) Decimal256 {
	return decimal256FromRaw(uint256.Int{
		uint64(v),
		signExtend64(v),
		signExtend64(v),
		signExtend64(v),
	})
}

func FromInt64(v int64, scale Decimal256Scale) (Decimal256, bool) {
	var mag uint256.Int
	if v < 0 {
		mag.SetUint64(uint64(-(v + 1)))
		mag.AddUint64(&mag, 1)
	} else {
		mag.SetUint64(uint64(v))
	}
	var raw uint256.Int
	_, overflow := raw.MulOverflow(&mag, &scale.multiplier)
	if overflow {
		return Decimal256{}, false
	}
	return decimal256FromSignMag(v < 0, raw)
}

// FromUInt256AsDecimal256 reinterprets an unsigned scaled coefficient as a
// non-negative ClickHouse Decimal256 coefficient.
func FromUInt256AsDecimal256(v UInt256) (Decimal256, bool) {
	raw := v.Holiman()
	return decimal256FromSignMag(false, raw)
}

func FromDecimal256BigInt(value *big.Int, scale Decimal256Scale) (Decimal256, bool) {
	if value == nil {
		return Decimal256{}, false
	}

	var mag big.Int
	neg := value.Sign() < 0
	if neg {
		mag.Neg(value)
	} else {
		mag.Set(value)
	}

	coeff, ok := FromBig(&mag)
	if !ok {
		return Decimal256{}, false
	}
	scaled, overflow := coeff.MulOverflow(scale.Multiplier())
	if overflow {
		return Decimal256{}, false
	}
	return decimal256FromSignMag(neg, scaled.Holiman())
}

func FromDecimal256ScaledBigInt(coefficient *big.Int) (Decimal256, bool) {
	if coefficient == nil {
		return Decimal256{}, false
	}

	var mag big.Int
	neg := coefficient.Sign() < 0
	if neg {
		mag.Neg(coefficient)
	} else {
		mag.Set(coefficient)
	}

	unsigned, ok := FromBig(&mag)
	if !ok {
		return Decimal256{}, false
	}
	return decimal256FromSignMag(neg, unsigned.Holiman())
}

func FromDecimal256LittleEndianBytes(raw []byte) (Decimal256, error) {
	if len(raw) != 32 {
		return Decimal256{}, errDecimalSyntax
	}
	return decimal256FromRaw(uint256.Int{
		binary.LittleEndian.Uint64(raw[0:8]),
		binary.LittleEndian.Uint64(raw[8:16]),
		binary.LittleEndian.Uint64(raw[16:24]),
		binary.LittleEndian.Uint64(raw[24:32]),
	}), nil
}

func FromDecimal256BigEndianBytes(raw []byte) (Decimal256, error) {
	if len(raw) != 32 {
		return Decimal256{}, errDecimalSyntax
	}
	return decimal256FromRaw(uint256.Int{
		binary.BigEndian.Uint64(raw[24:32]),
		binary.BigEndian.Uint64(raw[16:24]),
		binary.BigEndian.Uint64(raw[8:16]),
		binary.BigEndian.Uint64(raw[0:8]),
	}), nil
}

func ParseDecimal256(value string, scale Decimal256Scale) (Decimal256, error) {
	normalized, neg, err := normalizeDecimalInput(value)
	if err != nil {
		return Decimal256{}, err
	}

	whole, frac, ok := strings.Cut(normalized, ".")
	if !ok {
		whole = normalized
	}
	if whole == "" {
		whole = "0"
	}
	if len(frac) > int(scale.scale) {
		return Decimal256{}, errDecimalScale
	}

	digits := strings.TrimLeft(whole+frac+strings.Repeat("0", int(scale.scale)-len(frac)), "0")
	if digits == "" {
		return Decimal256{}, nil
	}

	mag, err := uint256.FromDecimal(digits)
	if err != nil {
		return Decimal256{}, errDecimalRange
	}
	out, ok := decimal256FromSignMag(neg, *mag)
	if !ok {
		return Decimal256{}, errDecimalRange
	}
	return out, nil
}

func (v Decimal256) Raw() UInt256 {
	return FromHoliman(v.raw())
}

func (v Decimal256) ScaledBigInto(out *big.Int) *big.Int {
	neg, mag := v.signMagnitude()
	FromHoliman(mag).IntoBig(out)
	if neg {
		out.Neg(out)
	}
	return out
}

func (v Decimal256) ScaledBig() *big.Int {
	var out big.Int
	return v.ScaledBigInto(&out)
}

func (v Decimal256) PutLittleEndianBytes(out *[32]byte) {
	raw := v.raw()
	binary.LittleEndian.PutUint64(out[0:8], raw[0])
	binary.LittleEndian.PutUint64(out[8:16], raw[1])
	binary.LittleEndian.PutUint64(out[16:24], raw[2])
	binary.LittleEndian.PutUint64(out[24:32], raw[3])
}

func (v Decimal256) PutBigEndianBytes(out *[32]byte) {
	raw := v.raw()
	binary.BigEndian.PutUint64(out[0:8], raw[3])
	binary.BigEndian.PutUint64(out[8:16], raw[2])
	binary.BigEndian.PutUint64(out[16:24], raw[1])
	binary.BigEndian.PutUint64(out[24:32], raw[0])
}

func (v Decimal256) IsZero() bool {
	raw := v.raw()
	return raw.IsZero()
}

func (v Decimal256) IsNegative() bool {
	raw := v.raw()
	return decimal256RawNegative(raw)
}

func (v Decimal256) Neg() (Decimal256, bool) {
	raw := v.raw()
	if decimal256RawIsMin(raw) {
		return Decimal256{}, false
	}
	var out uint256.Int
	out.Neg(&raw)
	return decimal256FromRaw(out), true
}

func (v Decimal256) AbsRaw() UInt256 {
	_, mag := v.signMagnitude()
	return FromHoliman(mag)
}

func (v Decimal256) Cmp(other Decimal256) int {
	x := v.raw()
	y := other.raw()
	xNeg := decimal256RawNegative(x)
	yNeg := decimal256RawNegative(y)
	if xNeg != yNeg {
		if xNeg {
			return -1
		}
		return 1
	}
	return x.Cmp(&y)
}

func (v Decimal256) Eq(other Decimal256) bool {
	return v.Cmp(other) == 0
}

func (v Decimal256) Lt(other Decimal256) bool {
	return v.Cmp(other) < 0
}

func (v Decimal256) Gt(other Decimal256) bool {
	return v.Cmp(other) > 0
}

func (v Decimal256) Add(other Decimal256) (Decimal256, bool) {
	x := v.raw()
	y := other.raw()
	xNeg := decimal256RawNegative(x)
	yNeg := decimal256RawNegative(y)

	var out uint256.Int
	out.Add(&x, &y)
	outNeg := decimal256RawNegative(out)
	return decimal256FromRaw(out), !(xNeg == yNeg && xNeg != outNeg)
}

func (v Decimal256) Sub(other Decimal256) (Decimal256, bool) {
	x := v.raw()
	y := other.raw()
	xNeg := decimal256RawNegative(x)
	yNeg := decimal256RawNegative(y)

	var out uint256.Int
	out.Sub(&x, &y)
	outNeg := decimal256RawNegative(out)
	return decimal256FromRaw(out), !(xNeg != yNeg && xNeg != outNeg)
}

func (v Decimal256) Mul(other Decimal256, scale Decimal256Scale) (Decimal256, bool) {
	xNeg, xMag := v.signMagnitude()
	yNeg, yMag := other.signMagnitude()

	var mag uint256.Int
	_, overflow := mag.MulDivOverflow(&xMag, &yMag, &scale.multiplier)
	if overflow {
		return Decimal256{}, false
	}
	return decimal256FromSignMag(xNeg != yNeg, mag)
}

func (v Decimal256) Div(other Decimal256, scale Decimal256Scale) (Decimal256, bool) {
	xNeg, xMag := v.signMagnitude()
	yNeg, yMag := other.signMagnitude()
	if yMag.IsZero() {
		return Decimal256{}, false
	}

	var mag uint256.Int
	_, overflow := mag.MulDivOverflow(&xMag, &scale.multiplier, &yMag)
	if overflow {
		return Decimal256{}, false
	}
	return decimal256FromSignMag(xNeg != yNeg, mag)
}

func (v Decimal256) Mod(other Decimal256) (Decimal256, bool) {
	xNeg, xMag := v.signMagnitude()
	_, yMag := other.signMagnitude()
	if yMag.IsZero() {
		return Decimal256{}, false
	}

	var mag uint256.Int
	mag.Mod(&xMag, &yMag)
	return decimal256FromSignMag(xNeg, mag)
}

func (v Decimal256) String(scale Decimal256Scale) string {
	neg, mag := v.signMagnitude()
	raw := mag.Dec()
	if scale.scale == 0 {
		if neg && raw != "0" {
			return "-" + raw
		}
		return raw
	}

	places := int(scale.scale)
	if len(raw) <= places {
		raw = strings.Repeat("0", places-len(raw)+1) + raw
	}
	point := len(raw) - places
	out := raw[:point] + "." + raw[point:]
	if neg && out != "0."+strings.Repeat("0", places) {
		return "-" + out
	}
	return out
}

func WrapProtoDecimal256Col(c *proto.ColDecimal256) *ColDecimal256 {
	return (*ColDecimal256)(c)
}

func (c *ColDecimal256) Proto() *proto.ColDecimal256 {
	return (*proto.ColDecimal256)(c)
}

func (c ColDecimal256) Rows() int {
	return len(c)
}

func (c *ColDecimal256) Reset() {
	*c = (*c)[:0]
}

func (c ColDecimal256) Row(i int) Decimal256 {
	return Decimal256(c[i])
}

func (c *ColDecimal256) Append(v Decimal256) {
	*c = append(*c, proto.Decimal256(v))
}

func (c *ColDecimal256) AppendProto(v proto.Decimal256) {
	*c = append(*c, v)
}

func (c ColDecimal256) AddInto(other ColDecimal256, out []Decimal256) bool {
	if len(c) != len(other) || len(out) < len(c) {
		panic("protomath: decimal column length mismatch")
	}
	ok := true
	for i := range c {
		var rowOK bool
		out[i], rowOK = Decimal256(c[i]).Add(Decimal256(other[i]))
		ok = ok && rowOK
	}
	return ok
}

func (c ColDecimal256) SubInto(other ColDecimal256, out []Decimal256) bool {
	if len(c) != len(other) || len(out) < len(c) {
		panic("protomath: decimal column length mismatch")
	}
	ok := true
	for i := range c {
		var rowOK bool
		out[i], rowOK = Decimal256(c[i]).Sub(Decimal256(other[i]))
		ok = ok && rowOK
	}
	return ok
}

func (c ColDecimal256) MulInto(other ColDecimal256, scale Decimal256Scale, out []Decimal256) bool {
	if len(c) != len(other) || len(out) < len(c) {
		panic("protomath: decimal column length mismatch")
	}
	ok := true
	for i := range c {
		var rowOK bool
		out[i], rowOK = Decimal256(c[i]).Mul(Decimal256(other[i]), scale)
		ok = ok && rowOK
	}
	return ok
}

func (c ColDecimal256) DivInto(other ColDecimal256, scale Decimal256Scale, out []Decimal256) bool {
	if len(c) != len(other) || len(out) < len(c) {
		panic("protomath: decimal column length mismatch")
	}
	ok := true
	for i := range c {
		var rowOK bool
		out[i], rowOK = Decimal256(c[i]).Div(Decimal256(other[i]), scale)
		ok = ok && rowOK
	}
	return ok
}

func (c ColDecimal256) ModInto(other ColDecimal256, out []Decimal256) bool {
	if len(c) != len(other) || len(out) < len(c) {
		panic("protomath: decimal column length mismatch")
	}
	ok := true
	for i := range c {
		var rowOK bool
		out[i], rowOK = Decimal256(c[i]).Mod(Decimal256(other[i]))
		ok = ok && rowOK
	}
	return ok
}

func (v Decimal256) raw() uint256.Int {
	p := proto.Int256(proto.Decimal256(v))
	return uint256.Int{p.Low.Low, p.Low.High, p.High.Low, p.High.High}
}

func (v Decimal256) signMagnitude() (bool, uint256.Int) {
	raw := v.raw()
	if !decimal256RawNegative(raw) {
		return false, raw
	}
	var mag uint256.Int
	mag.Neg(&raw)
	return true, mag
}

func decimal256FromRaw(raw uint256.Int) Decimal256 {
	return Decimal256(proto.Decimal256(proto.Int256{
		Low:  proto.UInt128{Low: raw[0], High: raw[1]},
		High: proto.UInt128{Low: raw[2], High: raw[3]},
	}))
}

func decimal256FromSignMag(neg bool, mag uint256.Int) (Decimal256, bool) {
	if mag.IsZero() {
		return Decimal256{}, true
	}
	if neg {
		if mag.Cmp(&decimal256MinMagnitude) > 0 {
			return Decimal256{}, false
		}
		var raw uint256.Int
		raw.Neg(&mag)
		return decimal256FromRaw(raw), true
	}
	if mag.Cmp(&decimal256MaxPositive) > 0 {
		return Decimal256{}, false
	}
	return decimal256FromRaw(mag), true
}

func decimal256RawNegative(raw uint256.Int) bool {
	return raw[3]&0x8000000000000000 != 0
}

func decimal256RawIsMin(raw uint256.Int) bool {
	return raw[0] == 0 &&
		raw[1] == 0 &&
		raw[2] == 0 &&
		raw[3] == 0x8000000000000000
}

func normalizeDecimalInput(value string) (string, bool, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return "", false, errDecimalSyntax
	}

	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return "", false, errDecimalSyntax
	}

	decimalIndex := decimalSeparatorIndex(s)
	var b strings.Builder
	b.Grow(len(s))
	seenDigit := false
	for i, r := range s {
		switch {
		case unicode.IsDigit(r):
			seenDigit = true
			b.WriteRune(r)
		case r == '.' || r == ',':
			if i == decimalIndex {
				b.WriteByte('.')
			}
		case r == '_' || unicode.IsSpace(r):
			continue
		default:
			return "", false, errDecimalSyntax
		}
	}
	if !seenDigit {
		return "", false, errDecimalSyntax
	}
	out := b.String()
	if strings.Count(out, ".") > 1 {
		return "", false, errDecimalSyntax
	}
	return out, neg, nil
}

func decimalSeparatorIndex(s string) int {
	lastDot := strings.LastIndexByte(s, '.')
	lastComma := strings.LastIndexByte(s, ',')
	if lastDot >= 0 && lastComma >= 0 {
		if lastDot > lastComma {
			return lastDot
		}
		return lastComma
	}
	if lastDot >= 0 {
		return lastDot
	}
	if lastComma < 0 || commaGroupsValid(s) {
		return -1
	}
	return lastComma
}

func commaGroupsValid(s string) bool {
	parts := strings.Split(s, ",")
	if len(parts) <= 1 || len(parts[0]) == 0 || len(parts[0]) > 3 {
		return false
	}
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) != 3 {
			return false
		}
		for _, r := range parts[i] {
			if !unicode.IsDigit(r) {
				return false
			}
		}
	}
	for _, r := range parts[0] {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func signExtend64(v int64) uint64 {
	if v < 0 {
		return 0xffffffffffffffff
	}
	return 0
}
