package envconfig

import (
	"testing"
	"time"
)

func TestParallelRPS(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
	}{
		{"", 5},
		{"5", 5},
		{"10", 10},
		{"2.5", 2.5},
		{"0", 5},
		{"bad", 5},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(EnvParallelRPS, tc.value)
			if got := ParallelRPS(); got != tc.want {
				t.Fatalf("ParallelRPS(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestTargetFetchDuration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 6 * time.Second},
		{"2.5", 2500 * time.Millisecond},
		{"0", 0},
		{"-1", 0},
		{"bad", 6 * time.Second},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(TargetFetchSeconds, tc.value)
			if got := TargetFetchDuration(); got != tc.want {
				t.Fatalf("TargetFetchDuration(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestStatsIntervalDuration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 10 * time.Second},
		{"60", time.Minute},
		{"250ms", 250 * time.Millisecond},
		{"5m", 5 * time.Minute},
		{"0", 10 * time.Second},
		{"bad", 10 * time.Second},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(StatsInterval, tc.value)
			if got := StatsIntervalDuration(); got != tc.want {
				t.Fatalf("StatsIntervalDuration(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestColdCacheDefaultsPreserveAutoSizing(t *testing.T) {
	t.Setenv(ColdCacheMB, "")
	if got := ColdCacheSize(); got != 0 {
		t.Fatalf("ColdCacheSize() = %d, want 0 for auto sizing", got)
	}

	t.Setenv(ColdCacheMB, "2048")
	if got := ColdCacheSize(); got != 2048<<20 {
		t.Fatalf("ColdCacheSize() = %d, want %d", got, int64(2048<<20))
	}
}

func TestColdFilterSize(t *testing.T) {
	t.Setenv(ColdFilterBits, "")
	t.Setenv(BloomKeys, "")
	if got := ColdFilterSize(); got != 1<<31 {
		t.Fatalf("ColdFilterSize() = %d, want %d", got, uint64(1<<31))
	}

	t.Setenv(ColdFilterBits, "1234")
	if got := ColdFilterSize(); got != 1234 {
		t.Fatalf("ColdFilterSize() = %d, want 1234", got)
	}
}
