package coldcache

import "math/rand"

// Shared benchmark helpers used by optimization_bench_test.go and the optional
// (build-tagged) pebble-vs-bitcask comparison. Kept untagged so the default
// `go test ./coldcache` builds without the optional bitcask module.

const keySize = 52    // 20 + 32 bytes for User + TokenID
const valueSize = 152 // size of posLike struct

// randKey generates a random key of keySize bytes.
func randKey(r *rand.Rand) []byte {
	k := make([]byte, keySize)
	r.Read(k)
	return k
}

// randValue generates a random posLike value as raw bytes.
func randValue(r *rand.Rand) []byte {
	var v posLike
	r.Read(v.User[:])
	r.Read(v.TokenID[:])
	for i := range v.Amount {
		v.Amount[i] = r.Uint64()
	}
	for i := range v.AvgPrice {
		v.AvgPrice[i] = r.Uint64()
	}
	for i := range v.RealizedPnL {
		v.RealizedPnL[i] = r.Uint64()
	}
	for i := range v.TotalBought {
		v.TotalBought[i] = r.Uint64()
	}
	v.UpdatedAtBlock = r.Uint64()
	v.UpdatedAt = r.Int63()
	v.BlockNumber = r.Uint64()
	v.TxIndex = r.Uint64()
	v.LogIndex = r.Uint64()
	v.Tombstone = r.Intn(2) == 1
	return bytesOf(&v)
}
