//go:build ignore
// +build ignore

package experiment

import (
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/holiman/uint256"
)

var collectionIDSink common.Hash
var collectionIDBenchIndexSets = [2]uint256.Int{
	*uint256.NewInt(1),
	*uint256.NewInt(2),
}

func TestCollectionIDNoParentFastMatchesGenerated(t *testing.T) {
	state := generated.NewState()
	for i := 0; i < 1000; i++ {
		conditionID := collectionIDBenchCondition(i)
		for outcome := uint8(0); outcome < 2; outcome++ {
			indexSet := collectionIDBenchIndexSets[outcome]
			want := state.GetCollectionID(common.Hash{}, conditionID, indexSet)

			gotSqrt := CollectionIDNoParentFastSqrt(conditionID, outcome)
			if gotSqrt != want {
				t.Fatalf("sqrt fast mismatch i=%d outcome=%d\nwant %s\n got %s", i, outcome, want, gotSqrt)
			}

			gotSqrtPooled := CollectionIDNoParentFastSqrtPooled(conditionID, outcome)
			if gotSqrtPooled != want {
				t.Fatalf("sqrt pooled fast mismatch i=%d outcome=%d\nwant %s\n got %s", i, outcome, want, gotSqrtPooled)
			}

			gotLegendre := CollectionIDNoParentFastLegendre(conditionID, outcome)
			if gotLegendre != want {
				t.Fatalf("legendre fast mismatch i=%d outcome=%d\nwant %s\n got %s", i, outcome, want, gotLegendre)
			}

			gotOriginal := CollectionIDNoParentOriginalBig(conditionID, outcome)
			if gotOriginal != want {
				t.Fatalf("original clone mismatch i=%d outcome=%d\nwant %s\n got %s", i, outcome, want, gotOriginal)
			}
		}
	}
}

func BenchmarkCollectionIDNoParentOriginalBig(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		collectionIDSink = CollectionIDNoParentOriginalBig(collectionIDBenchCondition(i), uint8(i&1))
	}
}

func BenchmarkCollectionIDNoParentFastSqrt(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		collectionIDSink = CollectionIDNoParentFastSqrt(collectionIDBenchCondition(i), uint8(i&1))
	}
}

func BenchmarkCollectionIDNoParentFastSqrtPooled(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		collectionIDSink = CollectionIDNoParentFastSqrtPooled(collectionIDBenchCondition(i), uint8(i&1))
	}
}

func BenchmarkCollectionIDNoParentFastLegendre(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		collectionIDSink = CollectionIDNoParentFastLegendre(collectionIDBenchCondition(i), uint8(i&1))
	}
}

func BenchmarkCollectionIDGeneratedCacheMiss(b *testing.B) {
	state := generated.NewState()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		collectionIDSink = state.GetCollectionID(common.Hash{}, collectionIDBenchCondition(i), collectionIDBenchIndexSets[i&1])
	}
}

func collectionIDBenchCondition(i int) common.Hash {
	var h common.Hash
	copy(h[:], "collection-id-bench-condition")
	binary.BigEndian.PutUint64(h[24:], uint64(i))
	return h
}
