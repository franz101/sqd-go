package clock_test

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/serroba/cache/clock"
)

// ============================================================
// SIEVE (sharded, string keys)
// ============================================================

type sieveNode struct {
	key     string
	value   string
	visited uint32
	next    *sieveNode
	prev    *sieveNode
}

type sieveShard struct {
	mu       sync.RWMutex
	items    map[string]*sieveNode
	capacity int
	head     *sieveNode
	tail     *sieveNode
	hand     *sieveNode
}

type Sieve struct {
	shards    []*sieveShard
	shardMask uint32
}

func newSieve(capacity, shardCount int) *Sieve {
	if shardCount&(shardCount-1) != 0 {
		shardCount = 1 << uint(log2ceil(shardCount))
	}
	s := &Sieve{
		shards:    make([]*sieveShard, shardCount),
		shardMask: uint32(shardCount - 1),
	}
	shardCap := capacity / shardCount
	if shardCap < 2 {
		shardCap = 2
	}
	for i := 0; i < shardCount; i++ {
		s.shards[i] = &sieveShard{
			items:    make(map[string]*sieveNode),
			capacity: shardCap,
		}
	}
	return s
}

func log2ceil(n int) int {
	v := 0
	for (1 << v) < n {
		v++
	}
	return v
}

func (s *Sieve) getShard(key string) *sieveShard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return s.shards[h.Sum32()&s.shardMask]
}

func (s *Sieve) Get(key string) (string, bool) {
	shard := s.getShard(key)
	shard.mu.RLock()
	node, exists := shard.items[key]
	shard.mu.RUnlock()
	if !exists {
		return "", false
	}
	atomic.StoreUint32(&node.visited, 1)
	return node.value, true
}

func (s *Sieve) Set(key, value string) {
	shard := s.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if node, exists := shard.items[key]; exists {
		node.value = value
		atomic.StoreUint32(&node.visited, 1)
		return
	}
	if len(shard.items) >= shard.capacity {
		shard.evict()
	}
	n := &sieveNode{key: key, value: value}
	if shard.head == nil {
		shard.head = n
		shard.tail = n
	} else {
		n.next = shard.head
		shard.head.prev = n
		shard.head = n
	}
	shard.items[key] = n
}

func (sh *sieveShard) evict() {
	obj := sh.hand
	if obj == nil {
		obj = sh.tail
	}
	for obj != nil {
		if atomic.LoadUint32(&obj.visited) == 1 {
			atomic.StoreUint32(&obj.visited, 0)
			obj = obj.prev
			if obj == nil {
				obj = sh.tail
			}
		} else {
			sh.hand = obj.prev
			if obj.prev != nil {
				obj.prev.next = obj.next
			} else {
				sh.head = obj.next
			}
			if obj.next != nil {
				obj.next.prev = obj.prev
			} else {
				sh.tail = obj.prev
			}
			delete(sh.items, obj.key)
			return
		}
	}
}

// ============================================================
// CLOCK (single mutex, string keys)
// ============================================================

type Clock struct {
	inner *clock.Cache[string, string]
}

func newClock(capacity uint64) *Clock {
	return &Clock{
		inner: clock.New[string, string](capacity),
	}
}

func (c *Clock) Get(key string) (string, bool) {
	return c.inner.Get(key)
}

func (c *Clock) Set(key, value string) {
	c.inner.Set(key, value)
}

// ============================================================
// THROUGHPUT HELPERS
// ============================================================

func benchSieve(b *testing.B, cacheSize, keySpace, shards, goroutines int, readRatio float64) {
	s := newSieve(cacheSize, shards)
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < cacheSize*8/10; i++ {
		s.Set(fmt.Sprintf("%09d", rng.Intn(keySpace)), "prefill")
	}
	runtime.GC()
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func(seed int64) {
				defer wg.Done()
				lr := rand.New(rand.NewSource(seed))
				for i := 0; i < 333; i++ {
					k := fmt.Sprintf("%09d", lr.Intn(keySpace))
					if lr.Float64() < readRatio {
						s.Get(k)
					} else {
						s.Set(k, fmt.Sprintf("%08d", lr.Intn(1000000)))
					}
				}
			}(int64(g) + 42)
		}
		wg.Wait()
	}
}

func benchClock(b *testing.B, cacheSize, keySpace, goroutines int, readRatio float64) {
	c := newClock(uint64(cacheSize))
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < cacheSize*8/10; i++ {
		c.Set(fmt.Sprintf("%09d", rng.Intn(keySpace)), "prefill")
	}
	runtime.GC()
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func(seed int64) {
				defer wg.Done()
				lr := rand.New(rand.NewSource(seed))
				for i := 0; i < 333; i++ {
					k := fmt.Sprintf("%09d", lr.Intn(keySpace))
					if lr.Float64() < readRatio {
						c.Get(k)
					} else {
						c.Set(k, fmt.Sprintf("%08d", lr.Intn(1000000)))
					}
				}
			}(int64(g) + 42)
		}
		wg.Wait()
	}
}

// ============================================================
// BENCHMARK REGISTRATIONS — 300 goroutines × 333 ops = 99,900 ops/iter
// ============================================================

// READ-HEAVY (95% reads)
func BenchmarkSieve_95r_1Kcap(b *testing.B)   { benchSieve(b, 1000, 2000, 16, 300, 0.95) }
func BenchmarkClock_95r_1Kcap(b *testing.B)   { benchClock(b, 1000, 2000, 300, 0.95) }
func BenchmarkSieve_95r_10Kcap(b *testing.B)  { benchSieve(b, 10000, 20000, 16, 300, 0.95) }
func BenchmarkClock_95r_10Kcap(b *testing.B)  { benchClock(b, 10000, 20000, 300, 0.95) }
func BenchmarkSieve_95r_100Kcap(b *testing.B) { benchSieve(b, 100000, 200000, 16, 300, 0.95) }
func BenchmarkClock_95r_100Kcap(b *testing.B) { benchClock(b, 100000, 200000, 300, 0.95) }

// BALANCED (50% reads, 50% writes)
func BenchmarkSieve_50r_1Kcap(b *testing.B)   { benchSieve(b, 1000, 2000, 16, 300, 0.50) }
func BenchmarkClock_50r_1Kcap(b *testing.B)   { benchClock(b, 1000, 2000, 300, 0.50) }
func BenchmarkSieve_50r_10Kcap(b *testing.B)  { benchSieve(b, 10000, 20000, 16, 300, 0.50) }
func BenchmarkClock_50r_10Kcap(b *testing.B)  { benchClock(b, 10000, 20000, 300, 0.50) }
func BenchmarkSieve_50r_100Kcap(b *testing.B) { benchSieve(b, 100000, 200000, 16, 300, 0.50) }
func BenchmarkClock_50r_100Kcap(b *testing.B) { benchClock(b, 100000, 200000, 300, 0.50) }

// WRITE-HEAVY (5% reads)
func BenchmarkSieve_05r_1Kcap(b *testing.B)   { benchSieve(b, 1000, 2000, 16, 300, 0.05) }
func BenchmarkClock_05r_1Kcap(b *testing.B)   { benchClock(b, 1000, 2000, 300, 0.05) }
func BenchmarkSieve_05r_10Kcap(b *testing.B)  { benchSieve(b, 10000, 20000, 16, 300, 0.05) }
func BenchmarkClock_05r_10Kcap(b *testing.B)  { benchClock(b, 10000, 20000, 300, 0.05) }
func BenchmarkSieve_05r_100Kcap(b *testing.B) { benchSieve(b, 100000, 200000, 16, 300, 0.05) }
func BenchmarkClock_05r_100Kcap(b *testing.B) { benchClock(b, 100000, 200000, 300, 0.05) }

// SIEVE SHARD SCALING (95% reads, 10K-cap)
func BenchmarkSieve_95r_10Kcap_04sh(b *testing.B)  { benchSieve(b, 10000, 20000, 4, 300, 0.95) }
func BenchmarkSieve_95r_10Kcap_08sh(b *testing.B)  { benchSieve(b, 10000, 20000, 8, 300, 0.95) }
func BenchmarkSieve_95r_10Kcap_16sh(b *testing.B)  { benchSieve(b, 10000, 20000, 16, 300, 0.95) }
func BenchmarkSieve_95r_10Kcap_32sh(b *testing.B)  { benchSieve(b, 10000, 20000, 32, 300, 0.95) }
func BenchmarkSieve_95r_10Kcap_64sh(b *testing.B)  { benchSieve(b, 10000, 20000, 64, 300, 0.95) }
func BenchmarkSieve_95r_10Kcap_128sh(b *testing.B) { benchSieve(b, 10000, 20000, 128, 300, 0.95) }
func BenchmarkSieve_95r_10Kcap_256sh(b *testing.B) { benchSieve(b, 10000, 20000, 256, 300, 0.95) }

// ============================================================
// WALL-CLOCK REPORT (runs once, prints comparison table)
// ============================================================

func TestThroughputReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock report in short mode")
	}

	type config struct {
		name      string
		cacheSize int
		keySpace  int
		shards    int
		readRatio float64
	}

	configs := []config{
		{"95%read_1Kcap", 1000, 2000, 16, 0.95},
		{"95%read_10Kcap", 10000, 20000, 16, 0.95},
		{"95%read_100Kcap", 100000, 200000, 16, 0.95},
		{"50%read_1Kcap", 1000, 2000, 16, 0.50},
		{"50%read_10Kcap", 10000, 20000, 16, 0.50},
		{"50%read_100Kcap", 100000, 200000, 16, 0.50},
		{"05%read_1Kcap", 1000, 2000, 16, 0.05},
		{"05%read_10Kcap", 10000, 20000, 16, 0.05},
		{"05%read_100Kcap", 100000, 200000, 16, 0.05},
	}

	const goroutines = 300
	const opsPerG = 333
	totalOps := goroutines * opsPerG

	fmt.Println()
	fmt.Println("══════════════════════════════════════════════════════════════════════════════════")
	fmt.Printf("  CACHE BENCHMARK REPORT\n")
	fmt.Printf("  Workload: %d goroutines × %d ops = %d ops per trial\n", goroutines, opsPerG, totalOps)
	fmt.Println("  Target: 100,000+ ops/sec sustained")
	fmt.Println("══════════════════════════════════════════════════════════════════════════════════")
	fmt.Println()
	fmt.Printf("%-22s %14s %14s %10s %10s\n", "CONFIG", "SIEVE (ops/s)", "CLOCK (ops/s)", "WINNER", "RATIO")
	fmt.Println("──────────────────────────────────────────────────────────────────────────────────")

	for _, cfg := range configs {
		const trials = 5

		// --- SIEVE ---
		var sieveSum int64
		for tr := 0; tr < trials; tr++ {
			s := newSieve(cfg.cacheSize, cfg.shards)
			rng := rand.New(rand.NewSource(99))
			nfill := cfg.cacheSize * 8 / 10
			for i := 0; i < nfill; i++ {
				s.Set(fmt.Sprintf("%09d", rng.Intn(cfg.keySpace)), "prefill")
			}
			runtime.GC()

			start := time.Now()
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				go func(seed int64) {
					defer wg.Done()
					lr := rand.New(rand.NewSource(seed))
					for i := 0; i < opsPerG; i++ {
						k := fmt.Sprintf("%09d", lr.Intn(cfg.keySpace))
						if lr.Float64() < cfg.readRatio {
							s.Get(k)
						} else {
							s.Set(k, fmt.Sprintf("%08d", lr.Intn(1000000)))
						}
					}
				}(int64(g) + 42)
			}
			wg.Wait()
			sieveSum += int64(float64(totalOps) / time.Since(start).Seconds())
		}
		sieveAvg := sieveSum / trials

		// --- CLOCK ---
		var clockSum int64
		for tr := 0; tr < trials; tr++ {
			c := newClock(uint64(cfg.cacheSize))
			rng := rand.New(rand.NewSource(99))
			nfill := cfg.cacheSize * 8 / 10
			for i := 0; i < nfill; i++ {
				c.Set(fmt.Sprintf("%09d", rng.Intn(cfg.keySpace)), "prefill")
			}
			runtime.GC()

			start := time.Now()
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				go func(seed int64) {
					defer wg.Done()
					lr := rand.New(rand.NewSource(seed))
					for i := 0; i < opsPerG; i++ {
						k := fmt.Sprintf("%09d", lr.Intn(cfg.keySpace))
						if lr.Float64() < cfg.readRatio {
							c.Get(k)
						} else {
							c.Set(k, fmt.Sprintf("%08d", lr.Intn(1000000)))
						}
					}
				}(int64(g) + 42)
			}
			wg.Wait()
			clockSum += int64(float64(totalOps) / time.Since(start).Seconds())
		}
		clockAvg := clockSum / trials

		winner := "SIEVE"
		ratio := float64(sieveAvg) / float64(clockAvg)
		if clockAvg > sieveAvg {
			winner = "CLOCK"
			ratio = float64(clockAvg) / float64(sieveAvg)
		}

		fmt.Printf("%-22s %14s %14s %10s %9.2fx\n",
			cfg.name,
			formatOps(sieveAvg),
			formatOps(clockAvg),
			winner,
			ratio,
		)
	}

	// --- SIEVE SHARD SWEEP ---
	fmt.Println()
	fmt.Println("─── SIEVE SHARD SWEEP (95% reads, 10K-cap, 300 goroutines) ───")
	fmt.Println()
	fmt.Printf("%-22s %14s\n", "SHARDS", "OPS/SEC")
	fmt.Println("──────────────────────────────────────────")

	for _, shards := range []int{4, 8, 16, 32, 64, 128, 256} {
		var sum int64
		for tr := 0; tr < 5; tr++ {
			s := newSieve(10000, shards)
			rng := rand.New(rand.NewSource(99))
			for i := 0; i < 8000; i++ {
				s.Set(fmt.Sprintf("%09d", rng.Intn(20000)), "prefill")
			}
			runtime.GC()

			start := time.Now()
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				go func(seed int64) {
					defer wg.Done()
					lr := rand.New(rand.NewSource(seed))
					for i := 0; i < opsPerG; i++ {
						k := fmt.Sprintf("%09d", lr.Intn(20000))
						if lr.Float64() < 0.95 {
							s.Get(k)
						} else {
							s.Set(k, fmt.Sprintf("%08d", lr.Intn(1000000)))
						}
					}
				}(int64(g) + 42)
			}
			wg.Wait()
			sum += int64(float64(totalOps) / time.Since(start).Seconds())
		}
		fmt.Printf("%-22s %14s\n", fmt.Sprintf("%d", shards), formatOps(sum/5))
	}

	fmt.Println()
	fmt.Println("══════════════════════════════════════════════════════════════════════════════════")
}

func formatOps(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.2f M", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.2f K", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
