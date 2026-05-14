package database

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

func TestUInt256ValueColumnAppendParses10Pow77(t *testing.T) {
	n := "1" + strings.Repeat("0", 77)
	parsed, err := uint256.FromDecimal(n)
	if err != nil {
		t.Fatalf("holiman uint256.FromDecimal(%q): %v", n, err)
	}

	c := &uint256ValueColumn{name: "value"}
	c.append(n)

	if c.col.Rows() != 1 {
		t.Fatalf("rows = %d, want 1", c.col.Rows())
	}
	got := c.col.Row(0)
	want := protoUInt256(*parsed)
	if got != want {
		t.Fatalf("UInt256 column value = %#v, want %#v", got, want)
	}
	if got == (proto.UInt256{}) {
		t.Fatal("UInt256 column value is zero")
	}
}

func TestFixedStringValueColumnAppendAddressHexAsBytes(t *testing.T) {
	address := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	c := newFixedStringValueColumn("address", common.AddressLength)

	c.append(address.Hex())

	got := c.col.Row(0)
	want := address.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("FixedString(20) bytes = %x, want %x", got, want)
	}
	if bytes.HasPrefix(got, []byte("0x")) {
		t.Fatalf("FixedString(20) stored ASCII hex prefix: %q", got[:2])
	}
}

func TestFixedStringValueColumnAppendHashHexAsBytes(t *testing.T) {
	hash := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	c := newFixedStringValueColumn("topic0", common.HashLength)

	c.append(hash.Hex())

	got := c.col.Row(0)
	want := hash.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("FixedString(32) bytes = %x, want %x", got, want)
	}
	if bytes.HasPrefix(got, []byte("0x")) {
		t.Fatalf("FixedString(32) stored ASCII hex prefix: %q", got[:2])
	}
}

func TestFixedStringValueColumnAppendRawAddressAndHashBytes(t *testing.T) {
	address := common.HexToAddress("0x00000000000000000000000000000000000000ab")
	hash := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")

	addrCol := newFixedStringValueColumn("address", common.AddressLength)
	addrCol.append(address.Bytes())
	if got, want := addrCol.col.Row(0), address.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("raw address bytes = %x, want %x", got, want)
	}

	hashCol := newFixedStringValueColumn("hash", common.HashLength)
	hashCol.append(hash.Bytes())
	if got, want := hashCol.col.Row(0), hash.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("raw hash bytes = %x, want %x", got, want)
	}
}

func TestFixedStringHexRoundTripMatchesCanonicalString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		size int
	}{
		{
			name: "address",
			in:   "0x8236a87084f8B84306f72007F36F2618A5634494",
			size: common.AddressLength,
		},
		{
			name: "hash",
			in:   "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
			size: common.HashLength,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := newFixedStringValueColumn(tt.name, tt.size)
			col.append(tt.in)

			got := "0x" + strings.ToLower(common.Bytes2Hex(col.col.Row(0)))
			want := strings.ToLower(tt.in)
			if got != want {
				t.Fatalf("string -> bytes -> FixedString -> hex() -> string = %q, want %q", got, want)
			}
		})
	}
}
