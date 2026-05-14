package parser

import (
	"strings"
	"testing"

	"github.com/franz101/sqd-go-v2/internal/config"
)

func TestBuildEventDecoderKeepsContractFiltersSeparate(t *testing.T) {
	decoders, filters, err := BuildEventDecoder([]config.ChainContractConfig{
		{
			Name:    "TokenA",
			Address: "0x0000000000000000000000000000000000000001",
			Events: []config.EventConfig{{
				Event: "Transfer(address indexed from, address indexed to, uint256 value)",
			}},
		},
		{
			Name:    "TokenB",
			Address: "0x0000000000000000000000000000000000000002",
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
