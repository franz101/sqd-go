package codegen

import (
	"strings"
	"testing"
)

// Regression: an event with a uint256[] argument that is NOT configured as a
// hot-state table still emits ProtoEventBlock append/decode code referencing
// hotStateUInt256Slice (encode) and hotStateUint256Slice (decode). The helper
// definitions must follow event usage, not just custom/hot-state tables —
// otherwise generated code references helpers that were never defined.
func TestHotStateUint256SliceHelpersEmittedForEventsWithoutHotStateTables(t *testing.T) {
	events := []eventSpec{
		{
			GoTypeName: "ConditionResolution",
			TableName:  "condition_resolutions",
			Args: []eventArg{
				{
					Name:           "payoutNumerators",
					ColumnName:     "payout_numerators",
					GoFieldName:    "PayoutNumerators",
					GoType:         "[]uint256.Int",
					SolidityType:   "uint256[]",
					ClickHouseType: "Array(UInt256)",
				},
			},
		},
	}

	out, err := generateHotStateGo(nil, events) // nil: no custom/hot-state tables
	if err != nil {
		t.Fatalf("generateHotStateGo: %v", err)
	}
	src := string(out)

	for _, fn := range []string{
		"func hotStateUInt256(",      // scalar encode (used by slice encode helper)
		"func hotStateUint256(",      // scalar decode (used by slice decode helper)
		"func hotStateUInt256Slice(", // slice encode (referenced by ProtoEventBlock append)
		"func hotStateUint256Slice(", // slice decode (referenced by view access)
	} {
		if !strings.Contains(src, fn) {
			t.Errorf("generated hotstate missing helper definition %q", fn)
		}
	}

	if !strings.Contains(src, `"github.com/holiman/uint256"`) {
		t.Errorf("generated hotstate missing uint256 import for event-driven helpers")
	}
}
