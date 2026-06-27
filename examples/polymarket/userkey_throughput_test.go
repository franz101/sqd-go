package polymarket

// userkey_throughput_test.go — confirms the CORRECT shard key (User-only, proven
// in shardkey_test.go) keeps the sharding speedup. combined_test.go measured
// (user,tokenID) keying; the full processor must shard by User (PositionsConverted
// reads NO / writes YES for the same user). This checks User-keying still
// balances and scales on the real corpus, so correctness does not cost throughput.

import (
	"encoding/binary"
	"runtime"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
)

func shardOfUser(user common.Address, nShards int) int {
	return int(binary.LittleEndian.Uint64(user[:8]) % uint64(nShards))
}

func TestUserKeyedShardThroughput(t *testing.T) {
	pages := loadOFCorpus(t)
	if len(pages) == 0 {
		t.Skip("empty corpus")
	}
	var events []ofEvent
	for _, pg := range pages {
		events = collectOrderFilledV2(pg, events)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}

	serial := bestEvps(len(events), 8, func() {
		m := make(map[ofPosKey]generated.Position, len(events)/2)
		for i := range events {
			applyOrderFilled(m, &events[i])
		}
		_ = m
	})

	nShards := runtime.NumCPU()
	// partition by USER only — all of a maker's positions in one shard.
	buckets := make([][]int, nShards)
	for i := range events {
		buckets[shardOfUser(events[i].maker, nShards)] = append(buckets[shardOfUser(events[i].maker, nShards)], i)
	}
	maxSize := 0
	for _, b := range buckets {
		if len(b) > maxSize {
			maxSize = len(b)
		}
	}

	sharded := bestEvps(len(events), 8, func() {
		var wg sync.WaitGroup
		for s := 0; s < nShards; s++ {
			wg.Add(1)
			go func(s int) {
				defer wg.Done()
				m := make(map[ofPosKey]generated.Position, len(buckets[s]))
				for _, i := range buckets[s] {
					applyOrderFilled(m, &events[i])
				}
			}(s)
		}
		wg.Wait()
	})

	t.Logf("USER-keyed fold: serial %.2fM ev/s -> sharded x%d %.2fM ev/s (%.2fx); hottest shard %.1f%% (ideal %.1f%%)",
		serial, nShards, sharded, sharded/serial,
		float64(maxSize)/float64(len(events))*100, 100.0/float64(nShards))
}
