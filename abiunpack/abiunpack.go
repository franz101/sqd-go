package abiunpack

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

var hexNibble = func() [256]byte {
	var table [256]byte
	for i := range table {
		table[i] = 0xff
	}
	for c := byte('0'); c <= '9'; c++ {
		table[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		table[c] = c - 'a' + 10
	}
	for c := byte('A'); c <= 'F'; c++ {
		table[c] = c - 'A' + 10
	}
	return table
}()

// AppendHexBytes appends the bytes represented by s to dst.
// It matches common.FromHex for valid event data, including optional 0x
// prefixes and odd-length input, while allowing callers to reuse dst.
func AppendHexBytes(dst []byte, s string) []byte {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	if len(s) == 0 {
		return dst[:0]
	}
	outLen := (len(s) + 1) >> 1
	if cap(dst) < outLen {
		dst = make([]byte, 0, outLen)
	} else {
		dst = dst[:0]
	}
	if len(s)&1 == 1 {
		lo, ok := fromHexChar(s[0])
		if !ok {
			return dst
		}
		dst = append(dst, lo)
		s = s[1:]
	}
	for i := 0; i < len(s); i += 2 {
		hi, ok := fromHexChar(s[i])
		if !ok {
			return dst
		}
		lo, ok := fromHexChar(s[i+1])
		if !ok {
			return dst
		}
		dst = append(dst, hi<<4|lo)
	}
	return dst
}

// TopicBool returns the boolean value of a topic.
func TopicBool(topic string) bool {
	hash := DecodeTopicHash(topic)
	return hash[31] == 1
}

// DecodeTopicHash decodes a 32-byte indexed topic.
func DecodeTopicHash(topic string) common.Hash {
	var hash common.Hash
	if decodeCanonicalTopic(topic, hash[:]) {
		return hash
	}
	return common.HexToHash(topic)
}

// DecodeTopicAddress decodes an indexed address topic.
func DecodeTopicAddress(topic string) common.Address {
	var hash common.Hash
	if decodeCanonicalTopic(topic, hash[:]) {
		var address common.Address
		copy(address[:], hash[12:])
		return address
	}
	return common.HexToAddress(topic)
}

// DecodeAddressFromTopic decodes an indexed address topic efficiently by
// skipping the 12 leading zero-padding bytes (24 hex chars) and decoding only
// the final 20 bytes (40 hex chars) directly into an address.
func DecodeAddressFromTopic(topic string) common.Address {
	body := topic
	if len(body) >= 2 && body[0] == '0' && (body[1] == 'x' || body[1] == 'X') {
		body = body[2:]
	}
	if len(body) == 64 {
		var addr common.Address
		if decodeCanonicalAddress(body[24:], addr[:]) {
			return addr
		}
	}
	return common.HexToAddress(topic)
}

// AddressFromHex decodes a 20-byte hex address string (with optional 0x
// prefix) into a common.Address without allocating. It is byte-identical to
// common.HexToAddress for canonical 40-hex-digit input and falls back to it
// for anything irregular (wrong length, non-hex), so it is a drop-in
// replacement that only removes the intermediate []byte that HexToAddress
// allocates.
func AddressFromHex(s string) common.Address {
	var address common.Address
	if decodeCanonicalAddress(s, address[:]) {
		return address
	}
	return common.HexToAddress(s)
}

// HashFromHex decodes a 32-byte hex string (with optional 0x prefix) into a
// common.Hash without allocating. It falls back to common.HexToHash for
// irregular inputs to preserve go-ethereum behavior.
func HashFromHex(s string) common.Hash {
	var hash common.Hash
	if decodeCanonicalHash(s, hash[:]) {
		return hash
	}
	return common.HexToHash(s)
}

func decodeCanonicalAddress(s string, dst []byte) bool {
	if len(dst) != common.AddressLength {
		return false
	}
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	if len(s) != common.AddressLength*2 {
		return false
	}
	for i := 0; i < common.AddressLength; i++ {
		hi, ok := fromHexChar(s[i*2])
		if !ok {
			return false
		}
		lo, ok := fromHexChar(s[i*2+1])
		if !ok {
			return false
		}
		dst[i] = hi<<4 | lo
	}
	return true
}

func decodeCanonicalHash(s string, dst []byte) bool {
	if len(dst) != common.HashLength {
		return false
	}
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	if len(s) != common.HashLength*2 {
		return false
	}
	for i := 0; i < common.HashLength; i++ {
		hi, ok := fromHexChar(s[i*2])
		if !ok {
			return false
		}
		lo, ok := fromHexChar(s[i*2+1])
		if !ok {
			return false
		}
		dst[i] = hi<<4 | lo
	}
	return true
}

// DecodeTopicUint256 decodes a topic into a uint256.Int.
func DecodeTopicUint256(topic string, dst *uint256.Int) {
	hash := DecodeTopicHash(topic)
	dst.SetBytes32(hash[:])
}

// DecodeTopicFixedBytes decodes a topic into a fixed-size byte slice.
func DecodeTopicFixedBytes(topic string, dst []byte, n int) {
	if n <= 0 {
		return
	}
	if n > len(dst) {
		n = len(dst)
	}
	if n > 32 {
		n = 32
	}
	hash := DecodeTopicHash(topic)
	copy(dst[:n], hash[:n])
}

func decodeCanonicalTopic(topic string, dst []byte) bool {
	return decodeCanonicalHash(topic, dst)
}

func fromHexChar(c byte) (byte, bool) {
	v := hexNibble[c]
	return v, v != 0xff
}

// Word returns the 32-byte word at the given word index.
func Word(data []byte, wordIdx int) ([]byte, bool) {
	start := wordIdx << 5
	end := start + 32
	if start < 0 || end > len(data) || end < start {
		return nil, false
	}
	return data[start:end], true
}

// Uint64Word parses a 32-byte word as a uint64, checking for overflow.
func Uint64Word(word []byte) (uint64, bool) {
	if len(word) != 32 {
		return 0, false
	}
	var n uint256.Int
	n.SetBytes32(word)
	if !n.IsUint64() {
		return 0, false
	}
	return n.Uint64(), true
}

// Bool parses a 32-byte word as a boolean.
func Bool(word []byte) (bool, bool) {
	if len(word) != 32 {
		return false, false
	}
	var n uint256.Int
	n.SetBytes32(word)
	if !n.IsUint64() {
		return false, false
	}
	val := n.Uint64()
	if val > 1 {
		return false, false
	}
	return val == 1, true
}

// Offset returns the offset pointed to by the headWord.
func Offset(data []byte, headWord int) (int, bool) {
	word, ok := Word(data, headWord)
	if !ok {
		return 0, false
	}
	offset, ok := Uint64Word(word)
	if !ok || offset > uint64(len(data)) || offset%32 != 0 {
		return 0, false
	}
	return int(offset), true
}

// DynamicWordRange returns the start index and length of a dynamic array/bytes.
func DynamicWordRange(data []byte, headWord int) (int, uint64, bool) {
	start, ok := Offset(data, headWord)
	if !ok {
		return 0, 0, false
	}
	lengthWord, ok := Word(data, start/32)
	if !ok {
		return 0, 0, false
	}
	length, ok := Uint64Word(lengthWord)
	if !ok {
		return 0, 0, false
	}
	bodyStart := start + 32
	if length > uint64(len(data)-bodyStart) {
		return 0, 0, false
	}
	return bodyStart, length, true
}

// Bytes decodes a dynamic bytes field.
func Bytes(data []byte, headWord int) ([]byte, bool) {
	bodyStart, length, ok := DynamicWordRange(data, headWord)
	if !ok {
		return nil, false
	}
	out := make([]byte, int(length))
	copy(out, data[bodyStart:bodyStart+int(length)])
	return out, true
}

// Uint256Array decodes a dynamic uint256[] field.
func Uint256Array(data []byte, headWord int) ([]uint256.Int, bool) {
	bodyStart, length, ok := DynamicWordRange(data, headWord)
	if !ok {
		return nil, false
	}
	if length > uint64(len(data)-bodyStart)>>5 {
		return nil, false
	}
	out := make([]uint256.Int, int(length))
	for i := range out {
		out[i].SetBytes32(data[bodyStart : bodyStart+32])
		bodyStart += 32
	}
	return out, true
}

// AddressArray decodes a dynamic address[] field.
func AddressArray(data []byte, headWord int) ([]common.Address, bool) {
	bodyStart, length, ok := DynamicWordRange(data, headWord)
	if !ok {
		return nil, false
	}
	if length > uint64(len(data)-bodyStart)>>5 {
		return nil, false
	}
	out := make([]common.Address, int(length))
	for i := range out {
		out[i] = common.BytesToAddress(data[bodyStart+12 : bodyStart+32])
		bodyStart += 32
	}
	return out, true
}

// HashArray decodes a dynamic bytes32[] or hash[] field.
func HashArray(data []byte, headWord int) ([]common.Hash, bool) {
	bodyStart, length, ok := DynamicWordRange(data, headWord)
	if !ok {
		return nil, false
	}
	if length > uint64(len(data)-bodyStart)>>5 {
		return nil, false
	}
	out := make([]common.Hash, int(length))
	for i := range out {
		out[i] = common.BytesToHash(data[bodyStart : bodyStart+32])
		bodyStart += 32
	}
	return out, true
}

// BoolArray decodes a dynamic bool[] field.
func BoolArray(data []byte, headWord int) ([]bool, bool) {
	bodyStart, length, ok := DynamicWordRange(data, headWord)
	if !ok {
		return nil, false
	}
	if length > uint64(len(data)-bodyStart)>>5 {
		return nil, false
	}
	out := make([]bool, int(length))
	for i := range out {
		val, ok := Bool(data[bodyStart : bodyStart+32])
		if !ok {
			return nil, false
		}
		out[i] = val
		bodyStart += 32
	}
	return out, true
}
