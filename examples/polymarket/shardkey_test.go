package polymarket

// shardkey_test.go — proves the CORRECT shard key for the polymarket fold.
//
// The combined-scaling proof (combined_test.go) shards OrderFilled by
// (user, tokenID) and is bit-identical — but that is only safe because
// OrderFilled touches exactly one (user, tokenID) per event. The full processor
// has a cross-token, same-user dependency: handlePositionsConverted
// (custom_processor.go:1039-1093) READS the user's NO-token positions' AvgPrice
// (:1065) and WRITES the user's YES-token positions (:1092). NO and YES are
// different tokenIDs, so under (user, tokenID) sharding they land on different
// shards — the YES shard cannot see the NO value and computes a wrong price:
// silent financial-data corruption.
//
// This test reproduces that divergence and proves that sharding by USER (so all
// of a user's positions live in one shard) restores bit-identity with the serial
// fold. It is the regression gate for Lever 2's shard key.

import (
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/protomath"
	"github.com/holiman/uint256"
)

type skEventKind int

const (
	skBuyNo   skEventKind = iota // buy the NO token (builds AvgPrice)
	skConvert                    // read NO AvgPrice, buy the YES token at a derived price
)

type skEvent struct {
	kind          skEventKind
	user          common.Address
	noTok, yesTok uint256.Int
	price, amount protomath.Decimal256
}

// applySkEvent does the read+write for one event against a single shard's map —
// faithful in shape to handleOrderFilledValues (buy) and handlePositionsConverted
// (convert: read NO AvgPrice -> write YES). The read and write both go through
// the SAME map m, which is exactly why the shard key matters: if NO and YES are
// in different shards, the convert's NO read misses.
func applySkEvent(m map[ofPosKey]generated.Position, e *skEvent) {
	switch e.kind {
	case skBuyNo:
		key := ofPosKey{user: e.user, tokenID: tokenIDHash(e.noTok)}
		up, ok := m[key]
		if !ok {
			up = generated.Position{User: e.user, TokenID: key.tokenID}
		}
		applyBuyD256(&up, e.price, e.amount)
		m[key] = up
	case skConvert:
		// read NO-token AvgPrice (handlePositionsConverted:1065-1069)
		noKey := ofPosKey{user: e.user, tokenID: tokenIDHash(e.noTok)}
		var noAvg protomath.Decimal256
		if noUp, ok := m[noKey]; ok {
			noAvg = noUp.AvgPrice
		}
		// derive YES price from the NO avg (stand-in for computeNegRiskYesPriceD256)
		// and buy the YES token (handlePositionsConverted:1091-1093)
		yesKey := ofPosKey{user: e.user, tokenID: tokenIDHash(e.yesTok)}
		up, ok := m[yesKey]
		if !ok {
			up = generated.Position{User: e.user, TokenID: yesKey.tokenID}
		}
		applyBuyD256(&up, noAvg, e.amount)
		m[yesKey] = up
	}
}

// foldRouted processes the ordered stream into nShards independent maps, routing
// each event by `route`, then merges. Routing in a single pass (no goroutines) is
// equivalent to parallel execution: shards are independent and per-shard order is
// the global order. The merge is a disjoint union iff `route` never splits a key
// across shards.
func foldRouted(events []skEvent, nShards int, route func(*skEvent) int) map[ofPosKey]generated.Position {
	shards := make([]map[ofPosKey]generated.Position, nShards)
	for i := range shards {
		shards[i] = make(map[ofPosKey]generated.Position)
	}
	for i := range events {
		applySkEvent(shards[route(&events[i])], &events[i])
	}
	merged := make(map[ofPosKey]generated.Position)
	for _, m := range shards {
		for k, v := range m {
			merged[k] = v
		}
	}
	return merged
}

// skUser / skToken give the synthetic keys high-byte entropy, matching real
// keccak-derived addresses/tokenIDs. The shard hashes read the FIRST 8 bytes
// (user[:8], and tokenIDHash writes big-endian so its high bytes are first), so
// entropy must live there or the keys collapse to one shard (small sequential
// IDs would leave those bytes zero and hide the divergence this test exists to
// catch).
func skUser(i int) common.Address {
	var a common.Address
	binary.BigEndian.PutUint64(a[0:8], uint64(i+1)*0x9e3779b97f4a7c15)
	binary.BigEndian.PutUint64(a[8:16], uint64(i+1)*0xc2b2ae3d27d4eb4f)
	return a
}

func skToken(n uint64) uint256.Int {
	var b [32]byte
	binary.BigEndian.PutUint64(b[0:8], n*0x9e3779b97f4a7c15)
	binary.BigEndian.PutUint64(b[8:16], n*0xc2b2ae3d27d4eb4f)
	var t uint256.Int
	t.SetBytes(b[:])
	return t
}

func mapsEqualPos(a, b map[ofPosKey]generated.Position) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !posEq(va, vb) {
			return false
		}
	}
	return true
}

// TestShardKeyCorrectness proves (user,tokenID) sharding CORRUPTS the
// PositionsConverted-style cross-token dependency while (user) sharding is
// bit-identical to serial.
func TestShardKeyCorrectness(t *testing.T) {
	const nUsers = 500
	const buysPerUser = 4
	price := func(n int64) protomath.Decimal256 {
		v, _ := protomath.FromInt64(n, protomath.Decimal256Scale18)
		return v
	}
	amount := func(n int64) protomath.Decimal256 {
		v, _ := protomath.FromInt64(n, protomath.Decimal256Scale18)
		return v
	}

	var events []skEvent
	for i := 0; i < nUsers; i++ {
		u := skUser(i)
		noTok := skToken(uint64(2*i + 1))
		yesTok := skToken(uint64(2*i + 2))
		for b := 0; b < buysPerUser; b++ {
			events = append(events, skEvent{kind: skBuyNo, user: u, noTok: noTok, price: price(int64(40 + b)), amount: amount(10)})
		}
		// the cross-token convert: reads this user's NO AvgPrice, writes YES
		events = append(events, skEvent{kind: skConvert, user: u, noTok: noTok, yesTok: yesTok, amount: amount(5)})
	}

	// serial reference (one shard)
	serial := foldRouted(events, 1, func(*skEvent) int { return 0 })

	// route by the event's WRITE key.
	const nShards = 8
	routeUserTok := func(e *skEvent) int {
		tok := e.noTok
		if e.kind == skConvert {
			tok = e.yesTok // convert writes the YES token
		}
		return shardOf(ofPosKey{user: e.user, tokenID: tokenIDHash(tok)}, nShards)
	}
	routeUser := func(e *skEvent) int {
		h := binary.LittleEndian.Uint64(e.user[:8])
		return int(h % uint64(nShards))
	}

	userTok := foldRouted(events, nShards, routeUserTok)
	user := foldRouted(events, nShards, routeUser)

	// (user) sharding MUST equal serial — the fix.
	if !mapsEqualPos(user, serial) {
		t.Fatal("USER-keyed sharding diverged from serial — the fix is broken")
	}
	t.Logf("USER-keyed sharding: bit-identical to serial across %d positions (CORRECT)", len(serial))

	// (user,tokenID) sharding MUST diverge — proving the hazard is real. If this
	// ever stops diverging the test is no longer guarding anything.
	if mapsEqualPos(userTok, serial) {
		t.Fatal("(user,tokenID) sharding matched serial — expected divergence on the cross-token convert; the regression guard is ineffective")
	}
	// quantify the corruption: how many YES positions got a wrong AvgPrice
	wrong := 0
	for k, want := range serial {
		if got, ok := userTok[k]; !ok || !posEq(got, want) {
			wrong++
		}
	}
	t.Logf("(user,tokenID) sharding: %d/%d positions CORRUPTED by the cross-token convert (confirms the data-corruption hazard)", wrong, len(serial))
}
