package abiunpack

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

var hexBytesBenchSink []byte

func TestAppendHexBytesMatchesCommonFromHex(t *testing.T) {
	tests := []string{
		"",
		"0x",
		"0x0",
		"0x1234",
		"123",
		"0XABCDEF",
		strings.Repeat("01", 96),
	}
	for _, tt := range tests {
		got := AppendHexBytes(nil, tt)
		want := common.FromHex(tt)
		if !bytes.Equal(got, want) {
			t.Fatalf("AppendHexBytes(%q) = %x, want %x", tt, got, want)
		}
	}
}

func TestDecodeTopicHelpers(t *testing.T) {
	topic := "0x0000000000000000000000008236a87084f8b84306f72007f36f2618a5634494"
	if got, want := DecodeTopicAddress(topic), common.HexToAddress(topic); got != want {
		t.Fatalf("DecodeTopicAddress = %s, want %s", got, want)
	}

	hashTopic := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	if got, want := DecodeTopicHash(hashTopic), common.HexToHash(hashTopic); got != want {
		t.Fatalf("DecodeTopicHash = %s, want %s", got, want)
	}
}

func BenchmarkHexDataCommonFromHex(b *testing.B) {
	data := "0x" + strings.Repeat("0123456789abcdef", 16)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hexBytesBenchSink = common.FromHex(data)
	}
}

func BenchmarkHexDataAppendHexBytes(b *testing.B) {
	data := "0x" + strings.Repeat("0123456789abcdef", 16)
	var scratch []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		scratch = AppendHexBytes(scratch, data)
	}
	hexBytesBenchSink = scratch
}

func BenchmarkTopicDecodeAddress(b *testing.B) {
	topic := "0x0000000000000000000000008236a87084f8b84306f72007f36f2618a5634494"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = DecodeTopicAddress(topic)
	}
}

func BenchmarkTopicDecodeHash(b *testing.B) {
	topic := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = DecodeTopicHash(topic)
	}
}

func BenchmarkTopicDecodeUint256(b *testing.B) {
	topic := "0x0000000000000000000000000000000000000000000000000000000000003039"
	var out uint256.Int
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		DecodeTopicUint256(topic, &out)
	}
}

func BenchmarkTopicDecodeBool(b *testing.B) {
	topic := "0x0000000000000000000000000000000000000000000000000000000000000001"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = TopicBool(topic)
	}
}
