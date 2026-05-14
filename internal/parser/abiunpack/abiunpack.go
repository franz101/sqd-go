package abiunpack

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// TopicBool returns the boolean value of a topic.
func TopicBool(topic string) bool {
	hash := common.HexToHash(topic)
	return hash[31] == 1
}

// DecodeTopicUint256 decodes a topic into a uint256.Int.
func DecodeTopicUint256(topic string, dst *uint256.Int) {
	hash := common.HexToHash(topic)
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
	hash := common.HexToHash(topic)
	copy(dst[:n], hash[:n])
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