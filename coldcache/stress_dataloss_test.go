//go:build stress
// +build stress

package coldcache

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestColdTierNoDataLossAtScale stress-tests the cold tier + negative filter at
// real-world scale (default 8 GiB on disk). It writes D distinct keys (block 0)
// until the Pebble dir reaches the target, then OVERWRITES every key (block 1) in
// a second pass so each lives in a later SST version. It then verifies, for ALL D
// keys: the lookup succeeds (no data loss), returns the LATEST value (block 1, not
// a stale block-0 read), and MightContain is true (no filter false negative — the
// failure mode that makes the authoritative gate reset a real position to zero).
//
//	Run: SQD_STRESS_DIR=./tmp/stress-cold SQD_STRESS_BYTES=8589934592 \
//	     go test -tags stress -run TestColdTierNoDataLossAtScale -v -timeout 60m ./coldcache/
func TestColdTierNoDataLossAtScale(t *testing.T) {
	target := int64(8) << 30
	if v := os.Getenv("SQD_STRESS_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			target = n
		}
	}
	dir := os.Getenv("SQD_STRESS_DIR")
	if dir == "" {
		dir = filepath.Join("tmp", "stress-cold")
	}
	_ = os.RemoveAll(dir)

	const valSize = 256 // realistic MemoryUserPosition-ish payload
	key := func(i uint64) []byte {
		k := make([]byte, 52) // UserPositionsClockKey size (user 20 + token 32)
		binary.LittleEndian.PutUint64(k[0:8], i)
		binary.LittleEndian.PutUint64(k[20:28], i*2654435761)
		return k
	}
	val := func(i, block uint64) []byte {
		v := make([]byte, valSize)
		binary.LittleEndian.PutUint64(v[0:8], block)
		binary.LittleEndian.PutUint64(v[8:16], i)
		return v
	}
	dirBytes := func() int64 {
		var sz int64
		_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
			if err == nil && fi != nil && !fi.IsDir() {
				sz += fi.Size()
			}
			return nil
		})
		return sz
	}

	s, err := Open(dir, 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.neg == nil {
		t.Fatal("negative filter not enabled (need it for the false-negative check)")
	}

	// Pass 1: write distinct keys (block 0) until the dir hits the target.
	t0 := time.Now()
	var D uint64
	const chunk = 200_000
	for dirBytes() < target {
		b := s.NewWriteBatch()
		for n := 0; n < chunk; n++ {
			if err := b.Put(key(D), val(D, 0)); err != nil {
				t.Fatalf("put %d: %v", D, err)
			}
			D++
		}
		if err := b.Close(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		t.Logf("pass1: %d keys, dir=%.2f GiB (%.0fs)", D, float64(dirBytes())/(1<<30), time.Since(t0).Seconds())
	}
	t.Logf("pass1 done: D=%d keys, dir=%.2f GiB", D, float64(dirBytes())/(1<<30))

	// Pass 2: OVERWRITE every key with block 1 (later SST version).
	for i := uint64(0); i < D; i += chunk {
		b := s.NewWriteBatch()
		for j := i; j < i+chunk && j < D; j++ {
			if err := b.Put(key(j), val(j, 1)); err != nil {
				t.Fatalf("overwrite %d: %v", j, err)
			}
		}
		if err := b.Close(); err != nil {
			t.Fatalf("flush2: %v", err)
		}
	}
	t.Logf("pass2 done: overwrote %d keys, dir=%.2f GiB (%.0fs)", D, float64(dirBytes())/(1<<30), time.Since(t0).Seconds())

	// Verify ALL keys: present, latest (block 1), filter-positive.
	var lost, stale, wrongKey, falseNeg uint64
	dst := make([]byte, valSize)
	for i := uint64(0); i < D; i++ {
		k := key(i)
		ok, err := s.GetInto(dst, k)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if !ok {
			lost++
			continue
		}
		if binary.LittleEndian.Uint64(dst[0:8]) != 1 {
			stale++
		}
		if binary.LittleEndian.Uint64(dst[8:16]) != i {
			wrongKey++
		}
		if !s.MightContain(k) {
			falseNeg++
		}
	}
	t.Logf("verify: D=%d lost=%d stale=%d wrongKey=%d falseNeg=%d (%.0fs total)", D, lost, stale, wrongKey, falseNeg, time.Since(t0).Seconds())
	if lost != 0 {
		t.Errorf("DATA LOSS: %d/%d keys missing on lookup at %.0f GiB", lost, D, float64(target)/(1<<30))
	}
	if stale != 0 {
		t.Errorf("STALE READ: %d/%d keys returned block 0 after overwrite to block 1", stale, D)
	}
	if wrongKey != 0 {
		t.Errorf("CORRUPT VALUE: %d/%d keys returned a value tagged with the wrong key index", wrongKey, D)
	}
	if falseNeg != 0 {
		t.Errorf("FILTER FALSE NEGATIVE: %d/%d written keys report MightContain=false (would reset real positions)", falseNeg, D)
	}
}
