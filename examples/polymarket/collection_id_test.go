package polymarket

import (
	"crypto/sha256"
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestCollectionIDRealConditionVectors(t *testing.T) {
	conditionID := common.HexToHash("0x8571597013046477495fe6fb5c9d9bfb97791ce9310d8dae5f79cdb0676faf33")
	expected := [...]common.Hash{
		common.HexToHash("0x01bebcc8d00f98a5e4d9be58e7b175aa83604e619bc87ba749856cec8fb69235"),
		common.HexToHash("0x171de854816ec5843aa767382bbbcf2c03e107f10c17214fbe7f0f5cca78141f"),
	}
	for outcome, want := range expected {
		indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcome))
		if got := getCollectionID(common.Hash{}, conditionID, indexSet); got != want {
			t.Fatalf("generic outcome %d: got %s, want %s", outcome, got, want)
		}
		if got := getCollectionIDForOutcome(common.Hash{}, conditionID, uint8(outcome)); got != want {
			t.Fatalf("fast outcome %d: got %s, want %s", outcome, got, want)
		}
	}
}

func TestCollectionIndexWordsMatchBigInts(t *testing.T) {
	for outcome := range collectionIndexWords {
		indexSet := new(big.Int).Lsh(big.NewInt(1), uint(outcome))
		if got, want := collectionIndexWords[outcome], bigIntTo32Bytes(indexSet); got != want {
			t.Fatalf("outcome %d: got %x, want %x", outcome, got, want)
		}
	}
}

func TestCollectionIDFixedMatchesLegacy(t *testing.T) {
	var seed [40]byte
	for i := 0; i < 256; i++ {
		binary.BigEndian.PutUint64(seed[32:], uint64(i))
		condition := common.Hash(sha256.Sum256(seed[:]))
		indexWord := collectionIndexWords[i%len(collectionIndexWords)]

		if got, want := computeCollectionIDFixed(common.Hash{}, condition, indexWord),
			computeCollectionIDLegacy(common.Hash{}, condition, indexWord); got != want {
			t.Fatalf("vector %d without parent: got %s, want %s", i, got, want)
		}
	}
}

func TestCollectionIDFixedMatchesLegacyWithParent(t *testing.T) {
	var seed [40]byte
	for i := 0; i < 64; i++ {
		binary.BigEndian.PutUint64(seed[32:], uint64(i))
		parentCondition := common.Hash(sha256.Sum256(seed[:]))
		parent := computeCollectionIDLegacy(
			common.Hash{},
			parentCondition,
			collectionIndexWords[(i+1)%len(collectionIndexWords)],
		)

		binary.BigEndian.PutUint64(seed[32:], uint64(i+1000))
		condition := common.Hash(sha256.Sum256(seed[:]))
		indexWord := collectionIndexWords[i%len(collectionIndexWords)]
		if got, want := computeCollectionIDFixed(parent, condition, indexWord),
			computeCollectionIDLegacy(parent, condition, indexWord); got != want {
			t.Fatalf("vector %d with parent %s: got %s, want %s", i, parent, got, want)
		}
	}
}
