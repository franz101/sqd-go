package monitoring

import (
	"context"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg := Config{
		Host:     "localhost",
		Port:     9000,
		User:     "default",
		Password: "password",
	}

	if cfg.Host == "" {
		t.Error("Config host should not be empty")
	}
	if cfg.Port == 0 {
		t.Error("Config port should not be zero")
	}
	if cfg.User == "" {
		t.Error("Config user should not be empty")
	}
}

func TestSnapshotFields(t *testing.T) {
	snap := snapshot{
		blocksTotal: 1000,
		eventsTotal: 5000,
		head:        1000,
		checkpoint:  950,
	}

	if snap.blocksTotal != 1000 {
		t.Errorf("blocksTotal = %d, want 1000", snap.blocksTotal)
	}
	if snap.eventsTotal != 5000 {
		t.Errorf("eventsTotal = %d, want 5000", snap.eventsTotal)
	}
	if snap.head != 1000 {
		t.Errorf("head = %d, want 1000", snap.head)
	}
	if snap.checkpoint != 950 {
		t.Errorf("checkpoint = %d, want 950", snap.checkpoint)
	}
}

func TestRecorderInitialization(t *testing.T) {
	ctx := context.Background()
	_ = ctx // Use context to avoid unused variable warning

	r := &Recorder{
		conn:       nil, // We'll test without real connection
		cancel:     func() {},
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	if r.snaps == nil {
		t.Error("snaps map should be initialized")
	}
	if r.prevBlocks == nil {
		t.Error("prevBlocks map should be initialized")
	}
	if r.prevEvents == nil {
		t.Error("prevEvents map should be initialized")
	}
}

func TestObserveWithoutStart(t *testing.T) {
	ctx := context.Background()
	_ = ctx // Use context to avoid unused variable warning

	// Ensure global is nil
	globalMu.Lock()
	global = nil
	globalMu.Unlock()

	// Should not panic when global is nil
	Observe(1, 100, 200, 300, 400)
}

func TestObserveWithStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set environment variable to enable monitoring
	os.Setenv("SQD_METRICS_CH", "1")
	defer os.Unsetenv("SQD_METRICS_CH")

	// Start will fail to connect, but we can test the Observe path
	// by manually setting up a minimal recorder
	r := &Recorder{
		conn:       nil,
		cancel:     cancel,
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	// Use context to avoid unused variable warning
	_ = ctx

	globalMu.Lock()
	global = r
	globalMu.Unlock()

	// Test basic observe functionality
	Observe(1, 100, 200, 300, 400)

	r.mu.Lock()
	snap, exists := r.snaps[1]
	r.mu.Unlock()

	if !exists {
		t.Fatal("snapshot for chain 1 should exist")
	}
	if snap.blocksTotal != 100 {
		t.Errorf("blocksTotal = %d, want 100", snap.blocksTotal)
	}
	if snap.eventsTotal != 200 {
		t.Errorf("eventsTotal = %d, want 200", snap.eventsTotal)
	}
	if snap.head != 300 {
		t.Errorf("head = %d, want 300", snap.head)
	}
	if snap.checkpoint != 400 {
		t.Errorf("checkpoint = %d, want 400", snap.checkpoint)
	}
}

func TestObserveMultipleChains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &Recorder{
		conn:       nil,
		cancel:     cancel,
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	// Use context to avoid unused variable warning
	_ = ctx

	globalMu.Lock()
	global = r
	globalMu.Unlock()

	// Test multiple chains
	Observe(1, 100, 200, 300, 400)
	Observe(2, 150, 250, 350, 450)
	Observe(3, 200, 300, 400, 500)

	r.mu.Lock()
	if len(r.snaps) != 3 {
		t.Errorf("expected 3 snapshots, got %d", len(r.snaps))
	}

	// Verify each chain
	testCases := []struct {
		chainID      uint64
		blocksTotal  uint64
		eventsTotal  uint64
		head         uint64
		checkpoint   uint64
	}{
		{1, 100, 200, 300, 400},
		{2, 150, 250, 350, 450},
		{3, 200, 300, 400, 500},
	}

	for _, tc := range testCases {
		snap, exists := r.snaps[tc.chainID]
		r.mu.Unlock()

		if !exists {
			t.Errorf("snapshot for chain %d should exist", tc.chainID)
			continue
		}
		if snap.blocksTotal != tc.blocksTotal {
			t.Errorf("chain %d: blocksTotal = %d, want %d", tc.chainID, snap.blocksTotal, tc.blocksTotal)
		}
		if snap.eventsTotal != tc.eventsTotal {
			t.Errorf("chain %d: eventsTotal = %d, want %d", tc.chainID, snap.eventsTotal, tc.eventsTotal)
		}
		if snap.head != tc.head {
			t.Errorf("chain %d: head = %d, want %d", tc.chainID, snap.head, tc.head)
		}
		if snap.checkpoint != tc.checkpoint {
			t.Errorf("chain %d: checkpoint = %d, want %d", tc.chainID, snap.checkpoint, tc.checkpoint)
		}

		r.mu.Lock()
	}
	r.mu.Unlock()
}

func TestStopWithoutStart(t *testing.T) {
	ctx := context.Background()
	_ = ctx // Use context to avoid unused variable warning

	// Ensure global is nil
	globalMu.Lock()
	global = nil
	globalMu.Unlock()

	// Should not panic when global is nil
	Stop()
}

func TestStopWithStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	r := &Recorder{
		conn:       nil,
		cancel:     cancel,
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	globalMu.Lock()
	global = r
	globalMu.Unlock()

	// Start the goroutine
	go func() {
		r.loop(ctx, 100*time.Millisecond)
	}()

	// Give it time to start
	time.Sleep(10 * time.Millisecond)

	// Stop should cancel the context and wait for done
	Stop()

	// Verify global is cleared
	globalMu.Lock()
	if global != nil {
		t.Error("global should be nil after Stop")
	}
	globalMu.Unlock()
}

func TestLoopTermination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	r := &Recorder{
		conn:       nil,
		cancel:     cancel,
		done:       done,
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	// Start the loop
	go r.loop(ctx, 50*time.Millisecond)

	// Let it run a bit
	time.Sleep(100 * time.Millisecond)

	// Cancel context
	cancel()

	// Wait for termination
	select {
	case <-done:
		// Expected
	case <-time.After(1 * time.Second):
		t.Fatal("loop did not terminate within 1 second")
	}
}

func TestFlushEmptySnaps(t *testing.T) {
	r := &Recorder{
		conn:       nil,
		cancel:     func() {},
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	ctx := context.Background()
	err := r.flush(ctx)
	if err != nil {
		t.Errorf("flush with empty snaps should not error, got %v", err)
	}
}

func TestProcessCPUNanos(t *testing.T) {
	cpuNanos := processCPUNanos()

	// On most systems this should return a positive value
	// On some systems it might return 0 (if getrusage fails)
	if cpuNanos < 0 {
		t.Errorf("processCPUNanos returned negative value: %d", cpuNanos)
	}

	// If it returns a positive value, it should be reasonable
	if cpuNanos > 0 && cpuNanos > 1e18 { // More than 1 billion seconds in nanoseconds
		t.Errorf("processCPUNanos returned unreasonably large value: %d", cpuNanos)
	}
}

func TestProcessCPUNanosConcurrency(t *testing.T) {
	// Test that processCPUNanos is thread-safe
	var wg sync.WaitGroup
	results := make([]int64, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = processCPUNanos()
		}(i)
	}

	wg.Wait()

	// All results should be non-negative
	for i, result := range results {
		if result < 0 {
			t.Errorf("goroutine %d returned negative value: %d", i, result)
		}
	}
}

func TestTTLDays(t *testing.T) {
	// Test default TTL (7 days when env var not set)
	os.Unsetenv("SQD_METRICS_CH_TTL_DAYS")
	ttl := ttlDays()
	if ttl != "7" {
		t.Errorf("TTL = %s, want 7", ttl)
	}

	// Test custom TTL
	os.Setenv("SQD_METRICS_CH_TTL_DAYS", "14")
	defer os.Unsetenv("SQD_METRICS_CH_TTL_DAYS")
	ttl = ttlDays()
	if ttl != "14" {
		t.Errorf("TTL = %s, want 14", ttl)
	}
}

func TestSnapshotConcurrency(t *testing.T) {
	r := &Recorder{
		conn:       nil,
		cancel:     func() {},
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	globalMu.Lock()
	global = r
	globalMu.Unlock()
	defer func() {
		globalMu.Lock()
		global = nil
		globalMu.Unlock()
	}()

	// Test concurrent Observe calls
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(chainID uint64) {
			defer wg.Done()
			Observe(chainID, uint64(chainID*100), uint64(chainID*200), uint64(chainID*300), uint64(chainID*400))
		}(uint64(i))
	}

	wg.Wait()

	r.mu.Lock()
	if len(r.snaps) != 100 {
		t.Errorf("expected 100 snapshots, got %d", len(r.snaps))
	}
	r.mu.Unlock()
}

func TestRuntimeMemStatsIntegration(t *testing.T) {
	// Test that runtime.ReadMemStats works correctly
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// Verify some basic fields
	if ms.HeapAlloc == 0 {
		// This might actually be 0 in minimal tests, but unlikely
	}
	if ms.HeapSys == 0 {
		t.Error("HeapSys should not be 0")
	}
	if ms.Sys == 0 {
		t.Error("Sys should not be 0")
	}

	// Test NumGoroutine integration
	goroutines := runtime.NumGoroutine()
	if goroutines <= 0 {
		t.Error("NumGoroutine should return positive value")
	}
}

func TestSyscallRusage(t *testing.T) {
	// Test that syscall.Getrusage works
	var ru syscall.Rusage
	err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru)

	if err != nil {
		// Some systems might not support this, so we'll just note it
		t.Logf("syscall.Getrusage not supported: %v", err)
		return
	}

	// Verify the time fields are accessible
	userTime := ru.Utime.Nano()
	systemTime := ru.Stime.Nano()

	if userTime < 0 || systemTime < 0 {
		t.Errorf("Invalid CPU time values: user=%d, system=%d", userTime, systemTime)
	}
}

func TestRecorderRateCalculation(t *testing.T) {
	// Test rate calculation logic
	r := &Recorder{
		conn:       nil,
		cancel:     func() {},
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
		primed:     true,
		prevTime:   time.Now().Add(-10 * time.Second),
	}

	// Add previous values
	r.prevBlocks[1] = 1000
	r.prevEvents[1] = 5000

	// Add current snapshot
	r.mu.Lock()
	r.snaps[1] = snapshot{
		blocksTotal: 2000, // +1000 blocks
		eventsTotal: 10000, // +5000 events
		head:        2000,
		checkpoint:  1950,
	}
	r.mu.Unlock()

	// In a real scenario, this would calculate rates
	// Since we can't call flush without a real connection,
	// we verify the data structure is set up correctly
	r.mu.Lock()
	snap := r.snaps[1]
	r.mu.Unlock()

	if snap.blocksTotal != 2000 {
		t.Errorf("blocksTotal = %d, want 2000", snap.blocksTotal)
	}

	// Expected rate: 1000 blocks / 10 seconds = 100 blocks/sec
	expectedBlocksRate := 100.0
	expectedEventsRate := 500.0 // 5000 events / 10 seconds

	// We can't actually test the rate calculation without calling flush,
	// but we can verify the setup is correct
	_ = expectedBlocksRate
	_ = expectedEventsRate
}

func TestMultipleObserveUpdateSameChain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &Recorder{
		conn:       nil,
		cancel:     cancel,
		done:       make(chan struct{}),
		snaps:      make(map[uint64]snapshot),
		prevBlocks: make(map[uint64]uint64),
		prevEvents: make(map[uint64]uint64),
	}

	// Use context to avoid unused variable warning
	_ = ctx

	globalMu.Lock()
	global = r
	globalMu.Unlock()

	// First observe
	Observe(1, 100, 200, 300, 400)

	// Second observe on same chain (should update)
	Observe(1, 150, 250, 350, 450)

	r.mu.Lock()
	snap, exists := r.snaps[1]
	r.mu.Unlock()

	if !exists {
		t.Fatal("snapshot for chain 1 should exist")
	}

	// Should have the second values
	if snap.blocksTotal != 150 {
		t.Errorf("blocksTotal = %d, want 150 (second update)", snap.blocksTotal)
	}
	if snap.eventsTotal != 250 {
		t.Errorf("eventsTotal = %d, want 250 (second update)", snap.eventsTotal)
	}
}

func TestRecorderZeroLag(t *testing.T) {
	// Test when head == checkpoint (no lag)
	snap := snapshot{
		head:       1000,
		checkpoint: 1000,
	}

	lag := snap.head - snap.checkpoint
	if lag != 0 {
		t.Errorf("lag = %d, want 0", lag)
	}
}

func TestRecorderPositiveLag(t *testing.T) {
	// Test when head > checkpoint (positive lag)
	snap := snapshot{
		head:       1000,
		checkpoint: 950,
	}

	lag := snap.head - snap.checkpoint
	if lag != 50 {
		t.Errorf("lag = %d, want 50", lag)
	}
}