# sqd-go Makefile
# Top-level targets for development, testing, and deployment.
# The codegen is example-agnostic (template engine, internal/template/*.tmpl);
# all polymarket specifics live in examples/polymarket/{config.yaml,custom_*.go}.

BUILD_DIR := tmp

.PHONY: build dev-build test vet benchmark benchmark-fast \
	test-config-matrix benchmark-live-matrix \
	codegen-uniswap start-uniswap dev-uniswap restart-uniswap uniswap-e2e uniswap-fast \
	codegen-polymarket dev-polymarket-live polymarket-fast-tmux polymarket-stop \
	polymarket-fast-tmux-profile polymarket-stop-profile \
	polymarket-faster-build polymarket-faster-reindex polymarket-faster-reindex-89M polymarket-faster-stop \
	reindex reindex-89M \
	db-reset stop \
	metabase-setup metabase-test metabase-test-sql

# Load .env file if it exists (for local ClickHouse credentials)
ifneq (,$(wildcard .env))
include .env
export
endif

# ClickHouse container detection and defaults
DETECTOR_CONTAINER := $(shell docker ps --filter "publish=9003" --format "{{.Names}}" | head -n 1)
CLICKHOUSE_CONTAINER ?= $(if $(DETECTOR_CONTAINER),$(DETECTOR_CONTAINER),$(shell docker ps --filter "name=clickhouse" --format "{{.Names}}" | head -n 1))
CLICKHOUSE_PASSWORD ?= sqd-clickhouse
CLICKHOUSE_DATABASE ?= case_1_lbtc_event_only

# Uniswap example configuration
UNISWAP_DIR := examples/uniswap

# Focused benchmark controls. Repeating short benches is more useful for this
# codebase than one long, noisy run.
BENCH_TIME ?= 2s
BENCH_COUNT ?= 3

# === Build & Test ===

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/sqd-go .

dev-build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/main .

test:
	go test ./... -count=1 -timeout 120s

test-config-matrix:
	scripts/test_make_matrix.sh

vet:
	go vet ./...

benchmark:
	$(MAKE) benchmark-fast

benchmark-fast:
	go test ./internal/codegen -run '^$$' -bench 'BenchmarkUInt256(View_At|View_ForEach|Slice_Conversion|Slice_Iteration)$$' -benchmem -benchtime=$(BENCH_TIME) -count=$(BENCH_COUNT)
	go test ./internal/ingestion -run '^$$' -bench '^BenchmarkReplayBufferGetBlockFull$$' -benchmem -benchtime=$(BENCH_TIME) -count=$(BENCH_COUNT)
	SQD_COLDFILTER_BITS=0 SQD_COLDCACHE_OPTIM= go test ./coldcache -run '^$$' -bench 'BenchmarkColdHit_(Get|GetInto)$$|BenchmarkEvictionSpill_(PerKeyPut|BatchedPut)$$' -benchmem -benchtime=$(BENCH_TIME) -count=$(BENCH_COUNT)

benchmark-live-matrix:
	scripts/benchmark_live_matrix.sh

# === Uniswap Development ===

codegen-uniswap:
	go run . codegen $(UNISWAP_DIR)

start-uniswap:
	go run . start $(UNISWAP_DIR)

dev-uniswap:
	go run . dev $(UNISWAP_DIR)

uniswap-fast:
	SQD_PARSE_DECODE_V2=1 SQD_METRICS_CH=1 SQD_METRICS_CH_INTERVAL=2s SQD_PARALLEL_RPS=10 SQD_PARALLEL_FETCHERS=12 go run . dev $(UNISWAP_DIR) --parallel-fetch --no-replay --restart

uniswap-fast-tmux:
	-tmux kill-session -t sqd-fast 2>/dev/null
	tmux new-session -d -s sqd-fast
	tmux send-keys -t sqd-fast "SQD_PARSE_DECODE_V2=1 SQD_METRICS_CH=1 SQD_METRICS_CH_INTERVAL=2s SQD_PARALLEL_RPS=10 SQD_PARALLEL_FETCHERS=12 go run . dev examples/uniswap --parallel-fetch --no-replay" Enter
	tmux attach-session -t sqd-fast

restart-uniswap:
	go run . start $(UNISWAP_DIR) --restart

uniswap-e2e: codegen-uniswap build
	go test ./$(UNISWAP_DIR)/generated/ -run 'TestCustom.*E2E|TestAppendDecodedLog' -v -count=1
	$(BUILD_DIR)/sqd-go start $(UNISWAP_DIR) --restart

# === Polymarket Development (examples/polymarket; Polygon, chain 137) ===
# Custom-processor project: --state is mandatory (regenerates, blank-imports the
# project so init() registers the processor, builds, re-execs). The fast path is
# default now: SQD_PARSE_DECODE_V2 is on unless NO_*=1, the state-prune is the
# bounded windowed prune, and the cold tier is Pebble v2 (MinLZ). No polymarket
# specifics live in the codegen — only in examples/polymarket.

POLYMARKET_DIR := examples/polymarket
POLYMARKET_DATABASE ?= polymarket
POLYMARKET_START_BLOCK ?= 25000000
# Prune every N blocks; also sets the prune window [block-N, block] that the
# active-key/keep-set subqueries scan (custom_processor.go clamps activeLower to
# block-N). TWO competing costs:
#  - MEMORY (was the lever): the active-key membership subquery USED to do GROUP BY
#    user,token_id (an AggregatingTransform CH mutations do NOT spill) -> RSS scaled
#    with distinct keys -> 1M window = 44.2M keys = 70 GiB = Code 241 OOM crash-loop.
#    FIXED in compaction.go.tmpl (GROUP BY -> LIMIT 1 BY, streams in PK order); now
#    proven peaks: 100k=1.8 GiB, 500k=7.2 GiB. Memory is no longer the binding limit.
#  - DISK/IO (now the lever): each prune runs OPTIMIZE TABLE ... FINAL, a FULL-table
#    rewrite of the large state tables (memory_user_positions ~53 GiB) regardless of N.
#    At 100k the FINAL fires every ~30 min, churning transient/inactive parts and
#    draining the shared /dev/md2 disk ~17 GiB/hr (net permanent growth is only
#    ~2.5 GiB/hr) -> at a ~16h backfill ETA it fills the 69 GiB free in ~4h and CH
#    write-fails. 500k makes the FINAL fire 5x less often (every ~2.3h), so disk churn
#    drops toward the permanent rate. 500k is the sweet spot: memory-safe (7.2 GiB,
#    proven) AND disk-safe. Lower toward 250k only if a denser region pushes prune RSS
#    near CH's ~70 GiB ceiling. NOTE: on resume a full-window prune fires early, so the
#    window must be safe standalone (500k is).
POLYMARKET_PRUNE_INTERVAL ?= 500000
POLYMARKET_TMUX_SESSION ?= sqd-polymarket-live
POLYMARKET_TMUX_LOG ?= tmp/polymarket-fast.log
# Cold-tier Pebble block cache per open. Default 1024: this box runs many cold
# tiers (~15 opens x 8GB default = OOM) alongside ClickHouse's ~82GB ceiling, so
# cap it. 1024 measured RSS 80->12GB with no throughput loss. Override if needed.
POLYMARKET_COLDCACHE_MB ?= 1024
# Cold-tier Pebble tuning profile (coldcache.go SQD_COLDCACHE_OPTIM switch).
# bigmem128 (128MB memtables + per-level Bloom filter) measured -20%
# compaction CPU, -78% avg L0 sublevels (read amplification), and -75% miss
# latency vs baseline on a 5M-entry synthetic burst; see
# coldcache/COMPACTION_BENCHMARKS.md. Defaults on: it was previously an
# unset-by-default override that had to be passed on every invocation, which
# both watchdog scripts (scripts/poly_watchdog.sh, poly_watchdog_check.sh)
# don't do — every auto-restart silently fell back to the unoptimized
# baseline profile. Costs up to ~512MB extra resident (128MB x
# MemTableStopWritesThreshold=4) on top of POLYMARKET_COLDCACHE_MB. Override
# with SQD_COLDCACHE_OPTIM=<other-profile> or empty string if needed.
POLYMARKET_COLDCACHE_OPTIM ?= bigmem128
# Recovery floor (SQD_RECOVERY_MIN_BLOCK): on every restart, [LOAD STATE]
# rebuilds the ephemeral cold tier from ClickHouse with 8 parallel queries
# that do `ORDER BY pk DESC, block_number DESC LIMIT 1 BY pk` over the WHOLE
# table to find the latest row per key (memory_user_positions is 200M+ rows /
# 14GiB+ and growing — confirmed via system.processes this took >15 minutes
# unfiltered). Setting this floor makes that pass load full *values* only for
# updated_at_block >= floor; a second, much cheaper keys-only pass (always
# runs regardless of this var, see hot_state_filter_keys.go) still adds every
# older key to the cold tier's negative Bloom filter, so a later touch of a
# pre-floor position still correctly falls back to ClickHouse instead of
# being treated as new — see STATERECOVER.md for the full investigation.
# Empty = today's behavior (full unfiltered scan). Safety note: this can only
# silently zero a real position if EnableColdCache's `authoritative` flag is
# true, which ingestion.go only sets on a from-genesis (empty ClickHouse)
# start — any restart of an already-populated DB (which this always is after
# the first run) gets authoritative=false, so a worst-case bug here means
# slower ClickHouse fallback reads, not corruption.
POLYMARKET_RECOVERY_MIN_BLOCK ?=
# Soft heap cap for the Go process. Backfill (cursor mode) bumps GOGC to 200 for
# throughput (ingestion.go), letting the heap grow ~3x the live set. GOMEMLIMIT is a
# SOFT cap: the runtime GCs to stay under it ONLY for reclaimable garbage. Measured at
# 40GiB the worker still climbed to 61GB RSS and kept rising ~2.8GB/min in beast mode —
# because the parallel fetcher races ahead and the *buffered, not-yet-consumed blocks*
# are LIVE heap (GC can't reclaim them), so the cap is exceeded. The real lever is
# bounding that race-ahead (fewer FETCHERS, below). 32GiB leaves comfortable box
# headroom; the watchdog (scripts/poly_watchdog.sh) restarts if RSS still nears OOM.
# Bumped 32->48GiB to give the 2M hot cache (POLYMARKET_STATE_CACHE_CAPACITY below)
# live-heap headroom: measured RSS 27-29GB stable at FETCHERS=6, well under 48 and
# under the watchdog's 70GB bail. (The old 61GB-at-40GiB blow-up was FETCHERS=16
# beast mode, not this default.)
POLYMARKET_GOMEMLIMIT ?= 48GiB
# Parallel-fetch tuning. CORRECTION to an earlier note: these DO drive memory. The
# fetcher prefetches blocks ahead of the single-threaded consumer; with 16 fetchers in
# beast mode (sparse blocks scanned fast) the in-flight buffer grew ~2.8GB/min and
# OOM-killed the box. FETCHERS=6/RPS=10 bounds the race-ahead to a memory-safe window
# for this shared box (robustness over peak throughput for unattended runs). Raise on a
# dedicated box. Portal shows no throttling up to ~60 RPS, so RPS is purely a gentleness
# knob; FETCHERS is the memory knob.
# 20 is the fetch sweet spot (this target's header comment; no portal throttling
# observed up to 60) and the value the 234 blk/s cap=2M run used, so it's the
# default now to make `make polymarket-fast-tmux` reproduce that config out of the box.
POLYMARKET_RPS ?= 20
POLYMARKET_FETCHERS ?= 6
# Parse-side parallelism. Default 1 (single-goroutine parse,
# parseBatchForInsertsOne): the pooled multi-goroutine path
# (parseBatchForInsertsParallel, engaged whenever this resolves to >1) recycles
# its *ProtoEventBlock rings on every page via Reset(), which invalidates any
# still-unconsumed proto pointer as soon as the next page is parsed —
# independent of replayBufCap/ring size. Proven unsafe at production consumer
# lag (examples/polymarket/generated/parse_batch_coupling_test.go:
# TestParallelPathProtoLifetimeDefect, 38996/40000 slots corrupted) vs. proven
# safe on the single-goroutine path at the same lag (0 corrupted). See
# AUDIT.md. Do not raise this above 1 until that defect is actually fixed
# (options are documented there) — SQD_PARSE_WORKERS previously defaulted to
# runtime.NumCPU() here (unset), which silently re-enabled the unsafe path on
# every restart and caused two live crashes.
POLYMARKET_PARSE_WORKERS ?= 1
# Hot CLOCK-cache capacity per entity (SQD_STATE_CACHE_CAPACITY, read at
# newState()/custom_processor.go). Default empty => binary's built-in 100000.
# Raising it above the active UserPosition working set keeps positions hot,
# eliminating cold Pebble reads AND hot->cold eviction writes (the ~45% of the
# custom stage's real CPU those two account for under churn — see ECS_FINDINGS.md)
# AND the compaction load that inflates every stage's wall-clock timer. RAM cost
# ~= 250B/slot x 5 caches x capacity (2_000_000 ~= 2.5GB). Requires GOMEMLIMIT
# headroom (paired 48GiB below). LIVE-VALIDATED 2026-07-01: 2_000_000 ran a
# reindex-from-88M at 234-247 blk/s vs 126-137 at the old 100000 default (same
# RPS/GOMEMLIMIT) = ~1.8x blk/s / ~1.5x events/s, RSS 27-29GB. The win is fewer
# hot<->cold Pebble reads + eviction-writes + compaction contention (ECS_FINDINGS.md).
POLYMARKET_STATE_CACHE_CAPACITY ?= 2000000
# Cold-tier directory. The default os.TempDir() is /tmp, which on this box is tmpfs
# (RAM): a 13GB+ cold tier then lives in RAM and competes with the heap. Point TMPDIR
# at the ext4 disk so the cold tier is truly on disk (the block cache keeps hot reads
# fast). Set to /tmp to go back to RAM-backed (faster, but counts against RAM).
POLYMARKET_TMPDIR ?= $(CURDIR)/tmp/coldtier
POLYMARKET_ARGS ?=

codegen-polymarket:
	go run . codegen $(POLYMARKET_DIR)

# Live backfill from POLYMARKET_START_BLOCK, following the chain head (--end-block 0).
# Resumes by default (no --restart, which would DROP the DB); pass extra flags via
# POLYMARKET_ARGS, e.g. POLYMARKET_ARGS=--reindex-from <block>.
dev-polymarket-live: codegen-polymarket build
	CLICKHOUSE_DATABASE=$(POLYMARKET_DATABASE) CLICKHOUSE_PRUNE_INTERVAL=$(POLYMARKET_PRUNE_INTERVAL) \
		$(BUILD_DIR)/sqd-go start $(POLYMARKET_DIR) --blockchain polygon \
		--start-block $(POLYMARKET_START_BLOCK) --end-block 0 $(POLYMARKET_ARGS)

# Fast detached backfill: parallel fetch + read-set prefetch in a tmux session.
# RPS=20 is the measured fetch sweet spot (sweep: 5->20 RPS ~+25%, plateaus ~20;
# no portal throttling observed up to 60). Attach: tmux attach -t $(POLYMARKET_TMUX_SESSION).
polymarket-fast-tmux: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION) (attach: tmux attach -t $(POLYMARKET_TMUX_SESSION))"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)) $(POLYMARKET_TMPDIR); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)" \
			"cd $(CURDIR) && TMPDIR=$(POLYMARKET_TMPDIR) GOMEMLIMIT=$(POLYMARKET_GOMEMLIMIT) SQD_PARALLEL_RPS=$(POLYMARKET_RPS) SQD_PARALLEL_FETCHERS=$(POLYMARKET_FETCHERS) SQD_PARSE_WORKERS=$(POLYMARKET_PARSE_WORKERS) SQD_STATE_CACHE_CAPACITY=$(POLYMARKET_STATE_CACHE_CAPACITY) SQD_METRICS_CH=1 SQD_STATS_INTERVAL=300 SQD_COLDCACHE_MB=$(POLYMARKET_COLDCACHE_MB) \
			$(MAKE) dev-polymarket-live SQD_COLDCACHE_OPTIM=$(POLYMARKET_COLDCACHE_OPTIM) SQD_RECOVERY_MIN_BLOCK=$(POLYMARKET_RECOVERY_MIN_BLOCK) POLYMARKET_START_BLOCK=$(POLYMARKET_START_BLOCK) POLYMARKET_ARGS=\"--state --parallel-fetch --prefetch $(POLYMARKET_ARGS)\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG)"; \
		echo "started fast backfill: $(POLYMARKET_TMUX_SESSION) (attach: tmux attach -t $(POLYMARKET_TMUX_SESSION); log: $(POLYMARKET_TMUX_LOG))"; \
	fi

polymarket-stop:
	-tmux kill-session -t "$(POLYMARKET_TMUX_SESSION)" 2>/dev/null

# Diagnostic variant of polymarket-fast-tmux: same command, same resume-from-
# checkpoint behavior, plus --cpuprofile/--fgprofile so `go tool pprof` can
# show where wall-clock actually goes in the parse/custom stages. fgprof
# samples every goroutine's stack (on-CPU AND off-CPU/blocked, e.g. waiting on
# a ClickHouse network round-trip or a lock) via runtime.GoroutineProfile,
# unlike --cpuprofile which only samples goroutines actually running on a
# core — run both together for the full picture. Separate tmux session
# (POLYMARKET_TMUX_SESSION-profile) and log so it doesn't collide with a
# concurrently-running normal session. --state forwards both flags through to
# the exec'd child (internal/cli/run_state.go's execStateChild), and the
# child listens for SIGTERM/SIGINT to flush both profiles cleanly on exit —
# use `kill -TERM <pid>` on the sqd-state-* process, not `tmux kill-session`
# (which sends SIGHUP the app doesn't handle, so the profile files would be
# empty/truncated).
POLYMARKET_CPUPROFILE ?= $(CURDIR)/tmp/polymarket-fast.cpu.prof
POLYMARKET_FGPROFILE ?= $(CURDIR)/tmp/polymarket-fast.fg.prof
polymarket-fast-tmux-profile: codegen-polymarket build
	@if tmux has-session -t "$(POLYMARKET_TMUX_SESSION)-profile" 2>/dev/null; then \
		echo "tmux session already running: $(POLYMARKET_TMUX_SESSION)-profile (attach: tmux attach -t $(POLYMARKET_TMUX_SESSION)-profile)"; \
	else \
		mkdir -p $(dir $(POLYMARKET_TMUX_LOG)) $(POLYMARKET_TMPDIR); \
		tmux new-session -d -s "$(POLYMARKET_TMUX_SESSION)-profile" \
			"cd $(CURDIR) && TMPDIR=$(POLYMARKET_TMPDIR) GOMEMLIMIT=$(POLYMARKET_GOMEMLIMIT) SQD_PARALLEL_RPS=$(POLYMARKET_RPS) SQD_PARALLEL_FETCHERS=$(POLYMARKET_FETCHERS) SQD_PARSE_WORKERS=$(POLYMARKET_PARSE_WORKERS) SQD_METRICS_CH=1 SQD_STATS_INTERVAL=300 SQD_COLDCACHE_MB=$(POLYMARKET_COLDCACHE_MB) \
			$(MAKE) dev-polymarket-live SQD_COLDCACHE_OPTIM=$(POLYMARKET_COLDCACHE_OPTIM) SQD_RECOVERY_MIN_BLOCK=$(POLYMARKET_RECOVERY_MIN_BLOCK) POLYMARKET_START_BLOCK=$(POLYMARKET_START_BLOCK) POLYMARKET_ARGS=\"--state --parallel-fetch --prefetch --cpuprofile $(POLYMARKET_CPUPROFILE) --fgprofile $(POLYMARKET_FGPROFILE) $(POLYMARKET_ARGS)\" 2>&1 | tee -a $(CURDIR)/$(POLYMARKET_TMUX_LOG).profile"; \
		echo "started profiling backfill: $(POLYMARKET_TMUX_SESSION)-profile (attach: tmux attach -t $(POLYMARKET_TMUX_SESSION)-profile; log: $(POLYMARKET_TMUX_LOG).profile; profiles: $(POLYMARKET_CPUPROFILE), $(POLYMARKET_FGPROFILE) on clean stop)"; \
	fi

polymarket-stop-profile:
	-tmux kill-session -t "$(POLYMARKET_TMUX_SESSION)-profile" 2>/dev/null

# === Polymarket-Faster Development (examples/polymarket-faster; Drop-in Replacement) ===
# Standalone reindexer that bypasses the full ingestion pipeline for maximum speed.
# Uses the portal → FastJSONLParser → OrderFilled decode → disruptor ring → ClickHouse
# pipeline (examples/polymarket-faster/indexer.go, engine.go). Targeted at backfills
# and reindex-from-N scenarios where the full processor's state management isn't needed.
#
# Key differences from polymarket-fast:
#   - No custom processor, no state pruning, no cold tier — raw event insertion only
#   - ~10x faster for pure backfill (tested: 62M range, 3335 events in 0.4s)
#   - Drop-in replacement for reindex-from-N: same ClickHouse schema, same addresses/topics
#
# Usage:
#   make polymarket-faster-reindex          # reindex from 89M (default)
#   make polymarket-faster-reindex START=85M END=86M  # custom range
#   make polymarket-faster-reindex-89M      # explicit 89M shorthand

POLYMARKET_FASTER_DIR := examples/polymarket-faster
POLYMARKET_FASTER_DB ?= polymarket_fast
POLYMARKET_FASTER_START_BLOCK ?= 89000000
POLYMARKET_FASTER_END_BLOCK ?= 89400000
POLYMARKET_FASTER_WORKERS ?= 4
POLYMARKET_FASTER_SPAN ?= 1024
POLYMARKET_FASTER_FLUSH ?= 5000
POLYMARKET_FASTER_SHARD_CAP ?= 4194304  # 1<<22
POLYMARKET_FASTER_ARGS ?=
# Inherit ClickHouse connection from .env or use defaults
POLYMARKET_FASTER_CLICKHOUSE_HOST ?= $(CLICKHOUSE_HOST)
POLYMARKET_FASTER_CLICKHOUSE_PORT ?= $(CLICKHOUSE_NATIVE_PORT)
POLYMARKET_FASTER_CLICKHOUSE_USER ?= $(CLICKHOUSE_USER)
POLYMARKET_FASTER_CLICKHOUSE_PASSWORD ?= $(CLICKHOUSE_PASSWORD)

polymarket-faster-build:
	go build -o $(BUILD_DIR)/polymarket-fast ./$(POLYMARKET_FASTER_DIR)/cmd

polymarket-faster-reindex: polymarket-faster-build
	CLICKHOUSE_DATABASE=$(POLYMARKET_FASTER_DB) \
	CLICKHOUSE_HOST=$(or $(POLYMARKET_FASTER_CLICKHOUSE_HOST),127.0.0.1) \
	CLICKHOUSE_NATIVE_PORT=$(or $(POLYMARKET_FASTER_CLICKHOUSE_PORT),9000) \
	CLICKHOUSE_USER=$(or $(POLYMARKET_FASTER_CLICKHOUSE_USER),default) \
	CLICKHOUSE_PASSWORD=$(or $(POLYMARKET_FASTER_CLICKHOUSE_PASSWORD),) \
	$(BUILD_DIR)/polymarket-fast \
		-start $(POLYMARKET_FASTER_START_BLOCK) \
		-end $(POLYMARKET_FASTER_END_BLOCK) \
		-workers $(POLYMARKET_FASTER_WORKERS) \
		-span $(POLYMARKET_FASTER_SPAN) \
		-shard-cap $(POLYMARKET_FASTER_SHARD_CAP) \
		-flush $(POLYMARKET_FASTER_FLUSH) \
		$(POLYMARKET_FASTER_ARGS)

polymarket-faster-reindex-89M: polymarket-faster-build
	CLICKHOUSE_DATABASE=$(POLYMARKET_FASTER_DB) \
	CLICKHOUSE_HOST=$(or $(POLYMARKET_FASTER_CLICKHOUSE_HOST),127.0.0.1) \
	CLICKHOUSE_NATIVE_PORT=$(or $(POLYMARKET_FASTER_CLICKHOUSE_PORT),9000) \
	CLICKHOUSE_USER=$(or $(POLYMARKET_FASTER_CLICKHOUSE_USER),default) \
	CLICKHOUSE_PASSWORD=$(or $(POLYMARKET_FASTER_CLICKHOUSE_PASSWORD),) \
	$(BUILD_DIR)/polymarket-fast \
		-start 89000000 -end 89400000 -workers 4 -span 1024 -shard-cap 4194304 -flush 5000

polymarket-faster-stop:
	-pkill -TERM polymarket-fast

# === Convenience Targets ===

# Default reindex using polymarket-faster from block 89M
reindex: polymarket-faster-reindex

# Explicit 89M reindex alias
reindex-89M: polymarket-faster-reindex-89M

# === Database Operations ===

db-reset:
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS $(CLICKHOUSE_DATABASE) SYNC"
	docker exec $(CLICKHOUSE_CONTAINER) clickhouse-client --password $(CLICKHOUSE_PASSWORD) --query "DROP DATABASE IF EXISTS uniswap SYNC"

stop:
	go run . stop

# === Metabase Dashboard Tests ===

# Setup Metabase dashboard with ClickHouse connection and questions
metabase-setup:
	cd examples/polymarket/analysis && ./setup_metabase_full.sh

# Test Metabase dashboard (requires Metabase running)
metabase-test:
	cd examples/polymarket/analysis && ./test_metabase_dashboard.sh

# Quick SQL views validation (no Metabase required)
metabase-test-sql:
	cd examples/polymarket/analysis && ./test_sql_views.sh
