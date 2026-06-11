#!/usr/bin/env bash
set -Eeuo pipefail

# bench_bps.sh — blocks-per-second benchmark: full V2 pipeline vs fetch-only ceiling.
#
# Phase 1 runs the dev-v2-live pipeline (resuming from the database checkpoint)
# for BENCH_DURATION seconds and measures blk/s between its first and last
# periodic stats lines, so startup and state load are excluded. Phase 2 runs
# debugger/fetchUntil.go from the same start block for the same duration — the
# same portal endpoint and event filters, but fetch + disk write only. The gap
# between the two is consumer-side work (decode, state math, ClickHouse).
#
# Notes:
# - Refuses to start while another sqd-go instance is running.
# - Phase 1 advances the database checkpoint; that is normal indexing progress.
# - Rates are only meaningful with backlog (start block well below chain head)
#   and are range-dependent: event density varies a lot with block height, so
#   compare runs only over the same range.

BENCH_DURATION="${BENCH_DURATION:-60}"
CLICKHOUSE_DATABASE="${CLICKHOUSE_DATABASE:-polymarket}"
START_BLOCK="${START_BLOCK:-23364531}" # used only when the database has no checkpoint
BENCH_DIR="${BENCH_DIR:-tmp/bench_bps/$(date +%Y%m%d_%H%M%S)}"

PIPELINE_LOG="$BENCH_DIR/pipeline.log"
FETCH_LOG="$BENCH_DIR/fetch.log"
FETCH_DATA="$BENCH_DIR/chunks"
PIPE_PID=""
FETCH_PID=""

cleanup() {
	[ -n "$PIPE_PID" ] && kill -INT "$PIPE_PID" 2>/dev/null || true
	[ -n "$FETCH_PID" ] && kill -INT "$FETCH_PID" 2>/dev/null || true
	rm -rf "$FETCH_DATA"
}
trap cleanup EXIT

stop_gracefully() { # pid
	kill -INT "$1" 2>/dev/null || true
	for _ in $(seq 1 30); do
		kill -0 "$1" 2>/dev/null || return 0
		sleep 1
	done
	kill -9 "$1" 2>/dev/null || true
}

ts_to_secs() { # HH:MM:SS.frac -> seconds
	awk -F: '{ print $1*3600 + $2*60 + $3 }' <<<"$1"
}

if pgrep -f "sqd-go start" >/dev/null 2>&1; then
	echo "error: an sqd-go instance is already running; stop it first (make stop, or Ctrl-C its tmux session)" >&2
	exit 1
fi

mkdir -p "$BENCH_DIR"
echo "== bench-bps: pipeline vs fetch-only | ${BENCH_DURATION}s each | db=$CLICKHOUSE_DATABASE =="
echo "logs: $BENCH_DIR"

# --- Phase 1: full V2 pipeline ---
echo "[1/2] pipeline: starting (resumes from checkpoint)..."
SQD_PARSE_DECODE_V2=1 CLICKHOUSE_DATABASE="$CLICKHOUSE_DATABASE" tmp/sqd-go start examples/polymarket \
	--blockchain polygon --start-block "$START_BLOCK" >"$PIPELINE_LOG" 2>&1 &
PIPE_PID=$!

# Startup can be slow on a resumed run: the cold-tier rebuild loads every
# hot-state row from ClickHouse before the first stats line (~2min at 65M
# blocks of history).
for _ in $(seq 1 360); do
	grep -q "stats periodic" "$PIPELINE_LOG" 2>/dev/null && break
	if ! kill -0 "$PIPE_PID" 2>/dev/null; then
		echo "error: pipeline exited during startup; last log lines:" >&2
		tail -20 "$PIPELINE_LOG" >&2
		exit 1
	fi
	sleep 1
done
if ! grep -q "stats periodic" "$PIPELINE_LOG"; then
	echo "error: pipeline produced no stats within 360s; see $PIPELINE_LOG" >&2
	exit 1
fi

echo "[1/2] pipeline: measuring for ${BENCH_DURATION}s..."
sleep "$BENCH_DURATION"
stop_gracefully "$PIPE_PID"
wait "$PIPE_PID" 2>/dev/null || true
PIPE_PID=""

first_stats=$(grep "stats periodic" "$PIPELINE_LOG" | head -1)
last_stats=$(grep "stats periodic" "$PIPELINE_LOG" | tail -1)
if [ "$first_stats" = "$last_stats" ]; then
	echo "error: only one stats line captured; increase BENCH_DURATION" >&2
	exit 1
fi
ckpt0=$(sed -E 's/.*checkpoint: ([0-9]+).*/\1/' <<<"$first_stats")
ckpt1=$(sed -E 's/.*checkpoint: ([0-9]+).*/\1/' <<<"$last_stats")
ev0=$(sed -E 's/.*total: [0-9]+ blocks, ([0-9]+) events.*/\1/' <<<"$first_stats")
ev1=$(sed -E 's/.*total: [0-9]+ blocks, ([0-9]+) events.*/\1/' <<<"$last_stats")
t0=$(ts_to_secs "$(awk '{print $2}' <<<"$first_stats")")
t1=$(ts_to_secs "$(awk '{print $2}' <<<"$last_stats")")
pipe_elapsed=$(awk -v a="$t0" -v b="$t1" 'BEGIN { d = b - a; if (d < 0) d += 86400; print d }')
pipe_blocks=$((ckpt1 - ckpt0))
pipe_events=$((ev1 - ev0))
pipe_bps=$(awk -v n="$pipe_blocks" -v s="$pipe_elapsed" 'BEGIN { printf "%.1f", n / s }')
pipe_eps=$(awk -v n="$pipe_events" -v s="$pipe_elapsed" 'BEGIN { printf "%.0f", n / s }')

# fetchUntil starts where the pipeline's measurement window started
bench_start=$(sed -nE 's/.*starting from block ([0-9]+).*/\1/p' "$PIPELINE_LOG" | head -1)
[ -n "$bench_start" ] || bench_start="$ckpt0"
echo "[1/2] pipeline: $pipe_blocks blocks in ${pipe_elapsed}s = $pipe_bps blk/s ($pipe_eps events/s), range $ckpt0 -> $ckpt1"

# --- Phase 2: fetch-only over the same range ---
echo "[2/2] fetchUntil: building and fetching from block $bench_start for ${BENCH_DURATION}s..."
go build -o "$BENCH_DIR/fetchuntil" debugger/fetchUntil.go
mkdir -p "$FETCH_DATA"
"$BENCH_DIR/fetchuntil" -start "$bench_start" -out "$FETCH_DATA" >"$FETCH_LOG" 2>&1 &
FETCH_PID=$!
sleep "$BENCH_DURATION"
stop_gracefully "$FETCH_PID"
wait "$FETCH_PID" 2>/dev/null || true
FETCH_PID=""

done_line=$(grep "\[DONE\]" "$FETCH_LOG" | tail -1 || true)
if [ -n "$done_line" ]; then
	fetch_blocks=$(sed -E 's/.*Total Blocks Saved: ([0-9]+).*/\1/' <<<"$done_line")
	fetch_elapsed=$(sed -E 's/.*Total Duration: ([0-9.]+)s.*/\1/' <<<"$done_line")
else
	stats_line=$(grep "\[STATS\]" "$FETCH_LOG" | tail -1 || true)
	if [ -z "$stats_line" ]; then
		echo "error: fetchUntil produced no stats; see $FETCH_LOG" >&2
		exit 1
	fi
	fetch_blocks=$(sed -E 's/.*Total Blocks: ([0-9]+).*/\1/' <<<"$stats_line")
	fetch_elapsed=$(sed -E 's/.*Elapsed: ([0-9.]+)s.*/\1/' <<<"$stats_line")
fi
fetch_bps=$(awk -v n="$fetch_blocks" -v s="$fetch_elapsed" 'BEGIN { printf "%.1f", n / s }')
echo "[2/2] fetchUntil: $fetch_blocks blocks in ${fetch_elapsed}s = $fetch_bps blk/s"

ratio=$(awk -v p="$pipe_bps" -v f="$fetch_bps" 'BEGIN { printf "%.0f", 100 * p / f }')
density=$(awk -v e="$pipe_events" -v b="$pipe_blocks" 'BEGIN { printf "%.1f", e / b }')

echo
echo "== bench-bps results (range from block $bench_start, $density events/block) =="
printf '%-28s %12s\n' "" "blk/s"
printf '%-28s %12s\n' "fetch-only (fetchUntil)" "$fetch_bps"
printf '%-28s %12s\n' "full pipeline (dev-v2-live)" "$pipe_bps"
echo
echo "pipeline runs at ${ratio}% of fetch-only speed (gap = decode + state math + ClickHouse)"
echo "logs kept in $BENCH_DIR (fetched chunks deleted)"
