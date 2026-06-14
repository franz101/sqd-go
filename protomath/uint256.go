// Package protomath contains small draft helpers for doing math on ch-go proto
// values without materializing pointer-heavy decimal or big.Int state.
package protomath

import (
	"errors"
	"math/big"
	"math/bits"
	"strings"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/holiman/uint256"
	"github.com/shopspring/decimal"
)

const DecimalScale18 uint64 = 1_000_000_000_000_000_000

var decimalScale18Divisor = NewUint64Divisor(DecimalScale18)

var (
	errUInt256Syntax = errors.New("protomath: invalid UInt256 syntax")
	errUInt256Range  = errors.New("protomath: UInt256 value is out of range")
)

// UInt256 is a math-enabled subtype of ch-go's proto.UInt256.
//
// It keeps the ClickHouse wire/storage layout. Converting between UInt256 and
// proto.UInt256 is a value reinterpretation of four uint64 limbs, not a heap
// allocation.
type UInt256 proto.UInt256

// ColUInt256 is a draft math wrapper around proto.ColUInt256.
//
// Use Proto when passing the column to ch-go. The wrapper intentionally does
// not reimplement ch-go's column encoder methods.
type ColUInt256 proto.ColUInt256

type Uint64Divisor struct {
	value   uint64
	holiman uint256.Int
}

func NewUint64Divisor(value uint64) Uint64Divisor {
	if value == 0 {
		panic("protomath: UInt256 division by zero")
	}
	var h uint256.Int
	h.SetUint64(value)
	return Uint64Divisor{value: value, holiman: h}
}

func DecimalScale18Divisor() Uint64Divisor {
	return decimalScale18Divisor
}

func (d Uint64Divisor) Value() uint64 {
	return d.value
}

func FromProto(v proto.UInt256) UInt256 {
	return UInt256(v)
}

func FromUInt64(v uint64) UInt256 {
	return UInt256(proto.UInt256FromUInt64(v))
}

func FromUInt(v uint) UInt256 {
	return FromUInt64(uint64(v))
}

func FromUInt8(v uint8) UInt256 {
	return FromUInt64(uint64(v))
}

func FromUInt16(v uint16) UInt256 {
	return FromUInt64(uint64(v))
}

func FromUInt32(v uint32) UInt256 {
	return FromUInt64(uint64(v))
}

func FromProtoUInt128(v proto.UInt128) UInt256 {
	return UInt256(proto.UInt256{Low: v})
}

func ParseUInt256(value string) (UInt256, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return UInt256{}, errUInt256Syntax
	}
	raw = strings.ReplaceAll(raw, "_", "")
	raw = strings.ReplaceAll(raw, ",", "")
	if strings.HasPrefix(raw, "+") {
		raw = raw[1:]
	}
	if raw == "" || strings.HasPrefix(raw, "-") {
		return UInt256{}, errUInt256Syntax
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return UInt256{}, errUInt256Syntax
		}
	}

	v, err := uint256.FromDecimal(raw)
	if err != nil {
		return UInt256{}, errUInt256Range
	}
	return FromHoliman(*v), nil
}

func (v UInt256) Proto() proto.UInt256 {
	return proto.UInt256(v)
}

func FromHoliman(v uint256.Int) UInt256 {
	return UInt256(proto.UInt256{
		Low:  proto.UInt128{Low: v[0], High: v[1]},
		High: proto.UInt128{Low: v[2], High: v[3]},
	})
}

func (v UInt256) Holiman() uint256.Int {
	p := proto.UInt256(v)
	return uint256.Int{p.Low.Low, p.Low.High, p.High.Low, p.High.High}
}

func FromBig(v *big.Int) (UInt256, bool) {
	if v == nil || v.Sign() < 0 || v.BitLen() > 256 {
		return UInt256{}, false
	}

	words := v.Bits()
	limb := func(i int) uint64 {
		if i >= len(words) {
			return 0
		}
		return uint64(words[i])
	}

	return UInt256(proto.UInt256{
		Low:  proto.UInt128{Low: limb(0), High: limb(1)},
		High: proto.UInt128{Low: limb(2), High: limb(3)},
	}), true
}

func (v UInt256) IntoBig(out *big.Int) *big.Int {
	p := proto.UInt256(v)

	var limb big.Int
	out.SetUint64(p.High.High)
	out.Lsh(out, 64)
	limb.SetUint64(p.High.Low)
	out.Add(out, &limb)
	out.Lsh(out, 64)
	limb.SetUint64(p.Low.High)
	out.Add(out, &limb)
	out.Lsh(out, 64)
	limb.SetUint64(p.Low.Low)
	out.Add(out, &limb)
	return out
}

func (v UInt256) Big() *big.Int {
	var out big.Int
	return v.IntoBig(&out)
}

func (v UInt256) Decimal(scale int32) decimal.Decimal {
	return decimal.NewFromBigInt(v.Big(), -scale)
}

func (v UInt256) IsZero() bool {
	h := v.Holiman()
	return h.IsZero()
}

func (v UInt256) IsUint64() bool {
	h := v.Holiman()
	return h.IsUint64()
}

func (v UInt256) Uint64() uint64 {
	h := v.Holiman()
	return h.Uint64()
}

func (v UInt256) Cmp(other UInt256) int {
	x := v.Holiman()
	y := other.Holiman()
	return x.Cmp(&y)
}

func (v UInt256) Eq(other UInt256) bool {
	return v.Cmp(other) == 0
}

func (v UInt256) Lt(other UInt256) bool {
	return v.Cmp(other) < 0
}

func (v UInt256) Gt(other UInt256) bool {
	return v.Cmp(other) > 0
}

func (v UInt256) Add(other UInt256) UInt256 {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	z.Add(&x, &y)
	return FromHoliman(z)
}

func (v UInt256) AddOverflow(other UInt256) (UInt256, bool) {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	_, overflow := z.AddOverflow(&x, &y)
	return FromHoliman(z), overflow
}

func (v UInt256) AddUint64(other uint64) UInt256 {
	x := v.Holiman()
	var z uint256.Int
	z.AddUint64(&x, other)
	return FromHoliman(z)
}

func (v UInt256) Sub(other UInt256) UInt256 {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	z.Sub(&x, &y)
	return FromHoliman(z)
}

func (v UInt256) SubOverflow(other UInt256) (UInt256, bool) {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	_, overflow := z.SubOverflow(&x, &y)
	return FromHoliman(z), overflow
}

func (v UInt256) SubUint64(other uint64) UInt256 {
	x := v.Holiman()
	var z uint256.Int
	z.SubUint64(&x, other)
	return FromHoliman(z)
}

func (v UInt256) Mul(other UInt256) UInt256 {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	z.Mul(&x, &y)
	return FromHoliman(z)
}

func (v UInt256) MulOverflow(other UInt256) (UInt256, bool) {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	_, overflow := z.MulOverflow(&x, &y)
	return FromHoliman(z), overflow
}

func (v UInt256) Div(divisor UInt256) UInt256 {
	x := v.Holiman()
	y := divisor.Holiman()
	var q uint256.Int
	q.Div(&x, &y)
	return FromHoliman(q)
}

func (v UInt256) DivMod(divisor UInt256) (UInt256, UInt256) {
	x := v.Holiman()
	y := divisor.Holiman()
	var q, r uint256.Int
	q.DivMod(&x, &y, &r)
	return FromHoliman(q), FromHoliman(r)
}

func (v UInt256) Mod(divisor UInt256) UInt256 {
	x := v.Holiman()
	y := divisor.Holiman()
	var r uint256.Int
	r.Mod(&x, &y)
	return FromHoliman(r)
}

func (v UInt256) And(other UInt256) UInt256 {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	z.And(&x, &y)
	return FromHoliman(z)
}

func (v UInt256) Or(other UInt256) UInt256 {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	z.Or(&x, &y)
	return FromHoliman(z)
}

func (v UInt256) Xor(other UInt256) UInt256 {
	x := v.Holiman()
	y := other.Holiman()
	var z uint256.Int
	z.Xor(&x, &y)
	return FromHoliman(z)
}

func (v UInt256) Not() UInt256 {
	x := v.Holiman()
	var z uint256.Int
	z.Not(&x)
	return FromHoliman(z)
}

func (v UInt256) Lsh(bits uint) UInt256 {
	x := v.Holiman()
	var z uint256.Int
	z.Lsh(&x, bits)
	return FromHoliman(z)
}

func (v UInt256) Rsh(bits uint) UInt256 {
	x := v.Holiman()
	var z uint256.Int
	z.Rsh(&x, bits)
	return FromHoliman(z)
}

func (v UInt256) DivUint64(divisor uint64) (UInt256, uint64) {
	if divisor == 0 {
		panic("protomath: UInt256 division by zero")
	}

	p := proto.UInt256(v)
	var rem uint64
	q3, rem := bits.Div64(rem, p.High.High, divisor)
	q2, rem := bits.Div64(rem, p.High.Low, divisor)
	q1, rem := bits.Div64(rem, p.Low.High, divisor)
	q0, rem := bits.Div64(rem, p.Low.Low, divisor)

	return UInt256(proto.UInt256{
		Low:  proto.UInt128{Low: q0, High: q1},
		High: proto.UInt128{Low: q2, High: q3},
	}), rem
}

func (v UInt256) DivBy(divisor Uint64Divisor) (UInt256, uint64) {
	x := v.Holiman()
	var q, r uint256.Int
	q.DivMod(&x, &divisor.holiman, &r)
	return FromHoliman(q), r.Uint64()
}

func (v UInt256) Div1e18() (UInt256, uint64) {
	return v.DivBy(decimalScale18Divisor)
}

func WrapProtoCol(c *proto.ColUInt256) *ColUInt256 {
	return (*ColUInt256)(c)
}

func (c *ColUInt256) Proto() *proto.ColUInt256 {
	return (*proto.ColUInt256)(c)
}

func (c ColUInt256) Rows() int {
	return len(c)
}

func (c *ColUInt256) Reset() {
	*c = (*c)[:0]
}

func (c ColUInt256) Row(i int) UInt256 {
	return UInt256(c[i])
}

func (c *ColUInt256) Append(v UInt256) {
	*c = append(*c, proto.UInt256(v))
}

func (c *ColUInt256) AppendProto(v proto.UInt256) {
	*c = append(*c, v)
}

func (c *ColUInt256) AppendHoliman(v uint256.Int) {
	c.Append(FromHoliman(v))
}

func (c ColUInt256) DivUint64Into(divisor uint64, quotient []UInt256, remainder []uint64) {
	if len(quotient) < len(c) || len(remainder) < len(c) {
		panic("protomath: output slices are smaller than column")
	}
	for i, value := range c {
		quotient[i], remainder[i] = UInt256(value).DivUint64(divisor)
	}
}

func (c ColUInt256) DivByInto(divisor Uint64Divisor, quotient []UInt256, remainder []uint64) {
	if len(quotient) < len(c) || len(remainder) < len(c) {
		panic("protomath: output slices are smaller than column")
	}
	for i, value := range c {
		quotient[i], remainder[i] = UInt256(value).DivBy(divisor)
	}
}

func (c ColUInt256) Div1e18Into(quotient []UInt256, remainder []uint64) {
	c.DivByInto(decimalScale18Divisor, quotient, remainder)
}
