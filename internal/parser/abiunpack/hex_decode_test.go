package abiunpack

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// These cover the no-alloc address decoder (AddressFromHex) and the existing
// DecodeTopicHash. They must be byte-identical to the go-ethereum helpers they
// replace, and allocation-free on canonical input — that allocation cut is the
// entire reason for the swap.

var addrInputs = []string{
	"0x8236a87084f8b84306f72007f36f2618a5634494",
	"0x4D97DCd97eC945f40cF65F87097ACe5EA0476045", // mixed case (checksummed)
	"8236a87084f8b84306f72007f36f2618a5634494",   // no 0x prefix
	"0x0000000000000000000000000000000000000000", // zero address
	"0xffffffffffffffffffffffffffffffffffffffff", // all-f
	"0xABCDEF0123456789abcdef0123456789ABCDEF01",
	// Irregular inputs must still match common.HexToAddress (fallback path).
	"",
	"0x",
	"0x1234",
	"not-hex-at-all",
	"0xZZ36a87084f8b84306f72007f36f2618a5634494",
}

var hashInputs = []string{
	"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
	"0xAB3760C3BD2BB38B5BCF54DC79802ED67338B4CF29F3054DED67ED24661E4177", // upper case
	"ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",   // no 0x
	"0x0000000000000000000000000000000000000000000000000000000000000000", // zero
	"0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", // all-f
	// Irregular: short, empty, non-hex — must match common.HexToHash fallback.
	"",
	"0x",
	"0xdeadbeef",
	"0xZZf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
}

func TestAddressFromHexMatchesCommon(t *testing.T) {
	for _, in := range addrInputs {
		if got, want := AddressFromHex(in), common.HexToAddress(in); got != want {
			t.Errorf("AddressFromHex(%q) = %s, want %s", in, got.Hex(), want.Hex())
		}
	}
}

func TestDecodeTopicHashMatchesCommon(t *testing.T) {
	for _, in := range hashInputs {
		if got, want := DecodeTopicHash(in), common.HexToHash(in); got != want {
			t.Errorf("DecodeTopicHash(%q) = %s, want %s", in, got.Hex(), want.Hex())
		}
	}
}

// Fuzz-style sweep: every byte value in every position of an address/hash must
// round-trip identically to go-ethereum.
func TestHexDecodersByteSweep(t *testing.T) {
	const hexdigits = "0123456789abcdefABCDEF"
	for _, c := range hexdigits {
		addr := "0x" + strings.Repeat(string(c), 40)
		if got, want := AddressFromHex(addr), common.HexToAddress(addr); got != want {
			t.Errorf("AddressFromHex(%q) = %s, want %s", addr, got.Hex(), want.Hex())
		}
		hash := "0x" + strings.Repeat(string(c), 64)
		if got, want := DecodeTopicHash(hash), common.HexToHash(hash); got != want {
			t.Errorf("DecodeTopicHash(%q) = %s, want %s", hash, got.Hex(), want.Hex())
		}
	}
}

var addrSink common.Address

func TestAddressFromHexZeroAlloc(t *testing.T) {
	addr := "0x8236a87084f8b84306f72007f36f2618a5634494"
	if n := testing.AllocsPerRun(1000, func() { addrSink = AddressFromHex(addr) }); n != 0 {
		t.Errorf("AddressFromHex allocates %.1f objects/call, want 0", n)
	}
	// Sanity: the go-ethereum decoder it replaced does allocate, so the swap
	// is a real cut, not a no-op.
	if n := testing.AllocsPerRun(1000, func() { addrSink = common.HexToAddress(addr) }); n == 0 {
		t.Error("expected common.HexToAddress to allocate; test premise is stale")
	}
}
