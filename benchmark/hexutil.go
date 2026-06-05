package main

import (
	"encoding/binary"

	"github.com/ClickHouse/ch-go/proto"
)

var hexTable [256]uint8

func init() {
	for i := range hexTable {
		hexTable[i] = 0xFF
	}
	for c := byte('0'); c <= '9'; c++ {
		hexTable[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		hexTable[c] = c - 'a' + 10
	}
	for c := byte('A'); c <= 'F'; c++ {
		hexTable[c] = c - 'A' + 10
	}
}

func hexDecode32(dst *[32]byte, src string, off int) {
	if off+64 > len(src) {
		return
	}
	_ = src[off+63]
	dst[0] = hexTable[src[off+0]]<<4 | hexTable[src[off+1]]
	dst[1] = hexTable[src[off+2]]<<4 | hexTable[src[off+3]]
	dst[2] = hexTable[src[off+4]]<<4 | hexTable[src[off+5]]
	dst[3] = hexTable[src[off+6]]<<4 | hexTable[src[off+7]]
	dst[4] = hexTable[src[off+8]]<<4 | hexTable[src[off+9]]
	dst[5] = hexTable[src[off+10]]<<4 | hexTable[src[off+11]]
	dst[6] = hexTable[src[off+12]]<<4 | hexTable[src[off+13]]
	dst[7] = hexTable[src[off+14]]<<4 | hexTable[src[off+15]]
	dst[8] = hexTable[src[off+16]]<<4 | hexTable[src[off+17]]
	dst[9] = hexTable[src[off+18]]<<4 | hexTable[src[off+19]]
	dst[10] = hexTable[src[off+20]]<<4 | hexTable[src[off+21]]
	dst[11] = hexTable[src[off+22]]<<4 | hexTable[src[off+23]]
	dst[12] = hexTable[src[off+24]]<<4 | hexTable[src[off+25]]
	dst[13] = hexTable[src[off+26]]<<4 | hexTable[src[off+27]]
	dst[14] = hexTable[src[off+28]]<<4 | hexTable[src[off+29]]
	dst[15] = hexTable[src[off+30]]<<4 | hexTable[src[off+31]]
	dst[16] = hexTable[src[off+32]]<<4 | hexTable[src[off+33]]
	dst[17] = hexTable[src[off+34]]<<4 | hexTable[src[off+35]]
	dst[18] = hexTable[src[off+36]]<<4 | hexTable[src[off+37]]
	dst[19] = hexTable[src[off+38]]<<4 | hexTable[src[off+39]]
	dst[20] = hexTable[src[off+40]]<<4 | hexTable[src[off+41]]
	dst[21] = hexTable[src[off+42]]<<4 | hexTable[src[off+43]]
	dst[22] = hexTable[src[off+44]]<<4 | hexTable[src[off+45]]
	dst[23] = hexTable[src[off+46]]<<4 | hexTable[src[off+47]]
	dst[24] = hexTable[src[off+48]]<<4 | hexTable[src[off+49]]
	dst[25] = hexTable[src[off+50]]<<4 | hexTable[src[off+51]]
	dst[26] = hexTable[src[off+52]]<<4 | hexTable[src[off+53]]
	dst[27] = hexTable[src[off+54]]<<4 | hexTable[src[off+55]]
	dst[28] = hexTable[src[off+56]]<<4 | hexTable[src[off+57]]
	dst[29] = hexTable[src[off+58]]<<4 | hexTable[src[off+59]]
	dst[30] = hexTable[src[off+60]]<<4 | hexTable[src[off+61]]
	dst[31] = hexTable[src[off+62]]<<4 | hexTable[src[off+63]]
}

func hexDecode20(dst *[20]byte, s string) {
	if len(s) < 42 {
		return
	}
	_ = s[41]
	dst[0] = hexTable[s[2+0]]<<4 | hexTable[s[3+0]]
	dst[1] = hexTable[s[2+2]]<<4 | hexTable[s[3+2]]
	dst[2] = hexTable[s[2+4]]<<4 | hexTable[s[3+4]]
	dst[3] = hexTable[s[2+6]]<<4 | hexTable[s[3+6]]
	dst[4] = hexTable[s[2+8]]<<4 | hexTable[s[3+8]]
	dst[5] = hexTable[s[2+10]]<<4 | hexTable[s[3+10]]
	dst[6] = hexTable[s[2+12]]<<4 | hexTable[s[3+12]]
	dst[7] = hexTable[s[2+14]]<<4 | hexTable[s[3+14]]
	dst[8] = hexTable[s[2+16]]<<4 | hexTable[s[3+16]]
	dst[9] = hexTable[s[2+18]]<<4 | hexTable[s[3+18]]
	dst[10] = hexTable[s[2+20]]<<4 | hexTable[s[3+20]]
	dst[11] = hexTable[s[2+22]]<<4 | hexTable[s[3+22]]
	dst[12] = hexTable[s[2+24]]<<4 | hexTable[s[3+24]]
	dst[13] = hexTable[s[2+26]]<<4 | hexTable[s[3+26]]
	dst[14] = hexTable[s[2+28]]<<4 | hexTable[s[3+28]]
	dst[15] = hexTable[s[2+30]]<<4 | hexTable[s[3+30]]
	dst[16] = hexTable[s[2+32]]<<4 | hexTable[s[3+32]]
	dst[17] = hexTable[s[2+34]]<<4 | hexTable[s[3+34]]
	dst[18] = hexTable[s[2+36]]<<4 | hexTable[s[3+36]]
	dst[19] = hexTable[s[2+38]]<<4 | hexTable[s[3+38]]
}

// hexDecode32Off decodes a 32-byte hex string at the given offset
// Returns the 32-byte big-endian buffer (Ethereum format)
func hexDecode32Off(src string, off int) *[32]byte {
	var buf [32]byte
	hexDecode32(&buf, src, off)
	return &buf
}

// bytesToUInt256 converts 32 big-endian bytes to proto.UInt256 (little-endian limbs)
func bytesToUInt256(beBytes *[32]byte) proto.UInt256 {
	// Ethereum uses big-endian, proto.UInt256 uses little-endian limbs
	return proto.UInt256{
		Low: proto.UInt128{
			Low:  binary.BigEndian.Uint64(beBytes[24:32]), // first 64 bits (least significant in BE)
			High: binary.BigEndian.Uint64(beBytes[16:24]), // second 64 bits
		},
		High: proto.UInt128{
			Low:  binary.BigEndian.Uint64(beBytes[8:16]),  // third 64 bits
			High: binary.BigEndian.Uint64(beBytes[0:8]),   // fourth 64 bits (most significant in BE)
		},
	}
}

// uint256FromHex decodes a hex-encoded uint256 string directly to proto.UInt256
// The hex string should start with "0x" prefix, offset is in hex characters (not bytes)
func uint256FromHex(hexStr string, offsetChar int) proto.UInt256 {
	beBytes := hexDecode32Off(hexStr, offsetChar)
	return bytesToUInt256(beBytes)
}
