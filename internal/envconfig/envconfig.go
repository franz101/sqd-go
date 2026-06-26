// Package envconfig centralizes all environment variable configuration with
// documentation, validation, and typed accessors with sensible defaults.
//
// Environment variables are read once at application startup and cached.
// For runtime reconfiguration, use signal handlers or config file reloading.
package envconfig

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Getenv returns the environment variable value or default if not set.
func Getenv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// GetenvInt returns the environment variable value as int or default if not set/invalid.
func GetenvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}

// GetenvFloat returns the environment variable value as float64 or default if
// not set/invalid.
func GetenvFloat(key string, defaultVal float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return defaultVal
}

// GetenvBool returns the environment variable value as bool or default if not set.
func GetenvBool(key string, defaultVal bool) bool {
	if v := strings.TrimSpace(strings.ToLower(os.Getenv(key))); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return defaultVal
}

// GetenvDuration returns the environment variable value as duration or default if not set/invalid.
func GetenvDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}

// ============================================================================
// ClickHouse Configuration
// ============================================================================

const (
	// CLICKHOUSE_HOST is the ClickHouse server hostname or IP.
	// Default: "127.0.0.1"
	ClickHouseHost = "CLICKHOUSE_HOST"

	// CLICKHOUSE_NATIVE_PORT is the ClickHouse native protocol port.
	// Default: 9000
	ClickHouseNativePort = "CLICKHOUSE_NATIVE_PORT"

	// CLICKHOUSE_USER is the ClickHouse authentication user.
	// Default: "default"
	ClickHouseUser = "CLICKHOUSE_USER"

	// CLICKHOUSE_PASSWORD is the ClickHouse authentication password.
	// Default: "sqd-clickhouse"
	ClickHousePassword = "CLICKHOUSE_PASSWORD"

	// CLICKHOUSE_DATABASE is the ClickHouse database name.
	// Default: project config name
	ClickHouseDatabase = "CLICKHOUSE_DATABASE"

	// CLICKHOUSE_PRUNE_INTERVAL is the interval between ClickHouse table pruning operations.
	// Format: Go duration (e.g., "1h", "30m")
	// Default: system-specific
	ClickHousePruneInterval = "CLICKHOUSE_PRUNE_INTERVAL"
)

// ClickHouse returns the ClickHouse connection configuration.
func ClickHouse(databaseDefault string) (host, user, password, database string, port int) {
	host = Getenv(ClickHouseHost, "127.0.0.1")
	port = GetenvInt(ClickHouseNativePort, 9000)
	user = Getenv(ClickHouseUser, "default")
	password = Getenv(ClickHousePassword, "sqd-clickhouse")
	database = Getenv(ClickHouseDatabase, databaseDefault)
	return
}

// PruneIntervalBlocks returns the ClickHouse state pruning interval in blocks.
func PruneIntervalBlocks() uint64 {
	n := GetenvInt(ClickHousePruneInterval, 100000)
	if n <= 0 {
		return 100000
	}
	return uint64(n)
}

// ============================================================================
// Performance & Tuning
// ============================================================================

const (
	// SQD_PARSE_DECODE_V2 enables the fast JSONL parser and optimized ABI decoder.
	// Set to any non-empty value to enable.
	// Default: disabled (use legacy parser)
	ParseDecodeV2 = "SQD_PARSE_DECODE_V2"

	// SQD_TARGET_FETCH_SECONDS is the target latency for fetch operations in seconds.
	// The adaptive page size controller adjusts page size to hit this target.
	// Default: 6 seconds
	TargetFetchSeconds = "SQD_TARGET_FETCH_SECONDS"

	// SQD_STATS_INTERVAL is the interval between printing statistics in seconds.
	// A Go duration (for example "250ms" or "5m") is also accepted.
	// Default: 10 seconds
	StatsInterval = "SQD_STATS_INTERVAL"

	// SQD_COMMIT_INTERVAL is the target commit interval in blocks.
	// Commits happen every N blocks or when COMMIT_MAX_INTERVAL is reached.
	// Default: 4096 blocks
	CommitInterval = "SQD_COMMIT_INTERVAL"

	// SQD_COMMIT_MAX_INTERVAL is the maximum time between commits in seconds.
	// Forces a commit even if COMMIT_INTERVAL blocks haven't been processed.
	// A Go duration is also accepted by generated processors.
	// Default: 3 seconds
	CommitMaxInterval = "SQD_COMMIT_MAX_INTERVAL"

	// SQD_PARALLEL_FETCHERS is the number of parallel fetch workers.
	// Default: 6
	EnvParallelFetchers = "SQD_PARALLEL_FETCHERS"

	// SQD_PARALLEL_PAGE_SIZE is the block range each worker claims per page.
	// Default: 10000
	EnvParallelPageSize = "SQD_PARALLEL_PAGE_SIZE"

	// SQD_PARALLEL_RPS is the target request rate to the portal (shared).
	// Default: 5.0 (requests/sec)
	EnvParallelRPS = "SQD_PARALLEL_RPS"
)

// ParseDecodeV2Enabled returns true if the fast JSONL parser is enabled.
func ParseDecodeV2Enabled() bool {
	return GetenvBool(ParseDecodeV2, false)
}

// TargetFetchDuration returns the target fetch latency as duration.
func TargetFetchDuration() time.Duration {
	seconds := GetenvFloat(TargetFetchSeconds, 6)
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// StatsIntervalDuration returns the stats interval as duration.
func StatsIntervalDuration() time.Duration {
	const defaultInterval = 10 * time.Second
	v := strings.TrimSpace(os.Getenv(StatsInterval))
	if v == "" {
		return defaultInterval
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d >= 50*time.Millisecond {
			return d
		}
		return defaultInterval
	}
	if seconds, err := strconv.ParseFloat(v, 64); err == nil {
		d := time.Duration(seconds * float64(time.Second))
		if d >= 50*time.Millisecond {
			return d
		}
	}
	return defaultInterval
}

// CommitIntervalBlocks returns the commit interval in blocks.
func CommitIntervalBlocks() int {
	return GetenvInt(CommitInterval, 4096)
}

// CommitMaxIntervalSeconds returns the max commit interval in seconds.
func CommitMaxIntervalSeconds() int {
	v := strings.TrimSpace(os.Getenv(CommitMaxInterval))
	if v == "" {
		return 3
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return max(1, int(d/time.Second))
	}
	if seconds, err := strconv.Atoi(v); err == nil && seconds > 0 {
		return seconds
	}
	return 3
}

// ParallelFetchers returns the number of parallel fetch workers.
func ParallelFetchers() int {
	n := GetenvInt(EnvParallelFetchers, 6)
	if n < 1 {
		return 1
	}
	if n > 32 {
		return 32
	}
	return n
}

// ParallelPageSize returns the block range per worker page.
func ParallelPageSize() int {
	n := GetenvInt(EnvParallelPageSize, 10000)
	if n < 1000 {
		return 1000
	}
	return n
}

// ParallelRPS returns the target request rate to the portal.
func ParallelRPS() float64 {
	n := GetenvFloat(EnvParallelRPS, 5.0)
	if n <= 0 {
		return 5.0
	}
	return n
}

// ============================================================================
// Cold Cache Configuration
// ============================================================================

const (
	// SQD_COLDCACHE_MB is the cold cache memory budget in megabytes.
	// Actual memory usage may be up to 2x this value for bookkeeping.
	// Default: 0 (let the cold-cache package auto-size from system RAM)
	ColdCacheMB = "SQD_COLDCACHE_MB"

	// SQD_COLDFILTER_BITS is the size of the cold filter in bits.
	// Must be a power of 2 for optimal performance.
	// Default: 1 << 31 (2.1 billion bits, 256 MB)
	ColdFilterBits = "SQD_COLDFILTER_BITS"

	// SQD_BLOOM_KEYS is the number of hash functions for the Bloom filter.
	// Higher values reduce false positives but use more CPU.
	// Default: 7
	BloomKeys = "SQD_BLOOM_KEYS"

	// SQD_COLDCACHE_BACKEND selects the cold cache storage backend.
	// Options: "pebble" (default), "flat"
	// Default: "pebble"
	ColdCacheBackend = "SQD_COLDCACHE_BACKEND"

	// SQD_COLDCACHE_OPTIM selects the optimization profile for cold cache.
	// Options: "largemem" (large memory budget, fewer disk seeks), "" (default)
	// Default: "" (default profile)
	ColdCacheOptim = "SQD_COLDCACHE_OPTIM"
)

// ColdCacheSize returns the cold cache memory budget in bytes.
func ColdCacheSize() int64 {
	mb := GetenvInt(ColdCacheMB, 0)
	if mb <= 0 {
		return 0
	}
	return int64(mb) * 1024 * 1024
}

// ColdFilterSize returns the cold filter size in bits.
func ColdFilterSize() uint64 {
	if v := os.Getenv(ColdFilterBits); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	if v := os.Getenv(BloomKeys); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n * 10
		}
	}
	return 1 << 31
}

// BloomHashCount returns the number of Bloom filter hash functions.
func BloomHashCount() int {
	return GetenvInt(BloomKeys, 7)
}

// ColdCacheBackendType returns the selected cold cache backend.
func ColdCacheBackendType() string {
	return Getenv(ColdCacheBackend, "pebble")
}

// ColdCacheOptimizationProfile returns the cold cache optimization profile.
func ColdCacheOptimizationProfile() string {
	return Getenv(ColdCacheOptim, "")
}

// ============================================================================
// Metrics Configuration
// ============================================================================

const (
	// SQD_METRICS_CH enables ClickHouse metrics recording.
	// Set to any non-empty value to enable.
	// Default: disabled
	MetricsCH = "SQD_METRICS_CH"

	// SQD_METRICS_CH_INTERVAL is the metrics flush interval.
	// Format: Go duration (e.g., "1m", "30s")
	// Default: "1m"
	MetricsCHInterval = "SQD_METRICS_CH_INTERVAL"

	// SQD_METRICS_CH_TTL_DAYS is the metrics data retention period in days.
	// Default: 7 days
	MetricsCHTTLDays = "SQD_METRICS_CH_TTL_DAYS"
)

// MetricsCHEnabled returns true if ClickHouse metrics recording is enabled.
func MetricsCHEnabled() bool {
	return GetenvBool(MetricsCH, false)
}

// MetricsCHFlushInterval returns the metrics flush interval.
func MetricsCHFlushInterval() time.Duration {
	return GetenvDuration(MetricsCHInterval, 5*time.Second)
}

// MetricsCHTTL returns the metrics data retention period.
func MetricsCHTTL() time.Duration {
	days := GetenvInt(MetricsCHTTLDays, 7)
	return time.Duration(days) * 24 * time.Hour
}

// ============================================================================
// Portal & Fetching Configuration
// ============================================================================

const (
	// SQD_PORTAL_ENDPOINT is the URL of the Subsquiverse portal endpoint.
	// If not set, uses the default gateway URL.
	// Default: "" (use default gateway)
	PortalEndpoint = "SQD_PORTAL_ENDPOINT"

	// SQD_RECOVERY_MIN_BLOCK is the minimum block number for recovery.
	// Recovery won't start before this block.
	// Default: 0 (start from genesis)
	RecoveryMinBlock = "SQD_RECOVERY_MIN_BLOCK"
)

// PortalEndpoint returns the configured portal endpoint URL.
func PortalEndpointURL() string {
	return Getenv(PortalEndpoint, "")
}

// RecoveryMinBlockNumber returns the minimum block for recovery.
func RecoveryMinBlockNumber() uint64 {
	n := GetenvInt(RecoveryMinBlock, 0)
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// ============================================================================
// State & Debugging Configuration
// ============================================================================

const (
	// SQD_GO_REPLACE enables Go code replacement mode for development.
	// Set to the replacement code to inject.
	// Default: "" (disabled)
	GoReplace = "SQD_GO_REPLACE"

	// SQD_LOG_ROLLBACK_SQL enables logging of SQL rollback statements.
	// Set to "true" or "1" to enable.
	// Default: "false" (disabled)
	LogRollbackSQL = "SQD_LOG_ROLLBACK_SQL"
)

// GoReplacement returns the Go code replacement string.
func GoReplacement() string {
	return Getenv(GoReplace, "")
}

// LogRollbackSQLEnabled returns true if rollback SQL logging is enabled.
func LogRollbackSQLEnabled() bool {
	return GetenvBool(LogRollbackSQL, false)
}

// ============================================================================
// Prefetch Configuration (CLI Flags, not env vars)
// ============================================================================

// PrefetchConfig represents prefetch-related configuration.
// These are typically CLI flags rather than environment variables.
type PrefetchConfig struct {
	Enabled     bool
	Parallel    bool
	BatchSize   int
	Concurrency int
}

// DefaultPrefetchConfig returns the default prefetch configuration.
func DefaultPrefetchConfig() PrefetchConfig {
	return PrefetchConfig{
		Enabled:     false,
		Parallel:    false,
		BatchSize:   5000,
		Concurrency: 4,
	}
}
