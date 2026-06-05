package parser

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/internal/config"
)

func TestBuildEventDecoderKeepsContractFiltersSeparate(t *testing.T) {
	decoders, filters, err := BuildEventDecoder([]config.ChainContractConfig{
		{
			Name:    "TokenA",
			Address: config.Address{"0x0000000000000000000000000000000000000001"},
			Events: []config.EventConfig{{
				Event: "Transfer(address indexed from, address indexed to, uint256 value)",
			}},
		},
		{
			Name:    "TokenB",
			Address: config.Address{"0x0000000000000000000000000000000000000002"},
			Events: []config.EventConfig{{
				Event: "Approval(address indexed owner, address indexed spender, uint256 value)",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filters) != 2 {
		t.Fatalf("filters length = %d, want separate filters for both contracts", len(filters))
	}
	for i, filter := range filters {
		if len(filter.Address) != 1 {
			t.Fatalf("filters[%d].Address length = %d, want 1", i, len(filter.Address))
		}
		if filter.Address[0] != strings.ToLower(filter.Address[0]) {
			t.Fatalf("filters[%d].Address = %q, want lowercase", i, filter.Address[0])
		}
		if len(filter.Topic0) != 1 {
			t.Fatalf("filters[%d].Topic0 length = %d, want 1", i, len(filter.Topic0))
		}
	}
	for _, decoder := range decoders {
		if decoder.MatchesAddress("0x0000000000000000000000000000000000000003") {
			t.Fatal("decoder matched an address outside the configured contract filters")
		}
	}
}

func TestNormalizeParamValueKeepsAddressHashRawButJSONStable(t *testing.T) {
	address := common.HexToAddress("0x8236a87084f8B84306f72007F36F2618A5634494")
	hash := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

	addrVal, ok := normalizeParamValue(address).(common.Address)
	if !ok || addrVal != address {
		t.Fatalf("address normalized to %#v, want raw common.Address", normalizeParamValue(address))
	}
	hashVal, ok := normalizeParamValue(hash).(common.Hash)
	if !ok || hashVal != hash {
		t.Fatalf("hash normalized to %#v, want raw common.Hash", normalizeParamValue(hash))
	}

	raw, err := json.Marshal(map[string]any{
		"address": normalizeParamValue(address),
		"hash":    normalizeParamValue(hash),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.ToLower(string(raw))
	if !strings.Contains(got, strings.ToLower(address.Hex())) || !strings.Contains(got, strings.ToLower(hash.Hex())) {
		t.Fatalf("json = %s, want address/hash hex strings", got)
	}
}
