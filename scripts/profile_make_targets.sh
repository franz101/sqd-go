#!/usr/bin/env bash
set -Eeuo pipefail

PROFILE_DIR="${PROFILE_DIR:-tmp/profiles}"
PROFILE_DURATION="${PROFILE_DURATION:-45}"
PROFILE_STARTUP_TIMEOUT="${PROFILE_STARTUP_TIMEOUT:-300}"
PROFILE_SHUTDOWN_TIMEOUT="${PROFILE_SHUTDOWN_TIMEOUT:-90}"
RUN_ID="${RUN_ID:-$(date +%Y%m%d_%H%M%S)}"

if (($# == 0)); then
	targets=(dev dev-v1 dev-v3)
else
	targets=("$@")
fi

mkdir -p "$PROFILE_DIR"

extract_re='LOAD STATE|starting from block|PROFILE|FETCH:|PARSE:|DECODE:|INSERT:|CUSTOM:|TOTAL:|Throughput:|Mem Alloc:|cpu profile written'

echo "$RUN_ID" > "$PROFILE_DIR/latest_run_id.txt"
echo "RUN_ID=$RUN_ID"
echo "PROFILE_DIR=$PROFILE_DIR"
echo "PROFILE_DURATION=${PROFILE_DURATION}s"
echo "targets=${targets[*]}"

run_target() {
	local index="$1"
	local target="$2"
	local label safe_label log cpu top summary pid start_deadline shutdown_deadline status

	safe_label="${target//[^A-Za-z0-9_.-]/_}"
	label="$(printf '%02d_%s' "$index" "$safe_label")"
	log="$PROFILE_DIR/${RUN_ID}_${label}.log"
	cpu="$PROFILE_DIR/${RUN_ID}_${label}.cpu.pprof"
	top="$PROFILE_DIR/${RUN_ID}_${label}.pprof_top.txt"
	summary="$PROFILE_DIR/${RUN_ID}_${label}.summary.txt"

	rm -f "$log" "$cpu" "$top" "$summary"

	echo
	echo "==> $target"
	echo "log=$log"
	echo "cpu=$cpu"

	setsid env POLYMARKET_ARGS="--cpuprofile $cpu" make "$target" >"$log" 2>&1 &
	pid="$!"

	start_deadline=$((SECONDS + PROFILE_STARTUP_TIMEOUT))
	while ((SECONDS < start_deadline)); do
		if [[ -f "$log" ]] && rg -q 'starting from block.*cursor mode|cursor mode.*starting from block' "$log"; then
			echo "measurement_start=$target"
			sleep "$PROFILE_DURATION"
			break
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "ERROR: $target exited before cursor-mode start; see $log" >&2
			wait "$pid" || true
			return 1
		fi
		sleep 1
	done

	if ! [[ -f "$log" ]] || ! rg -q 'starting from block.*cursor mode|cursor mode.*starting from block' "$log"; then
		echo "ERROR: timed out waiting for cursor-mode start in $target; see $log" >&2
		kill -INT "-$pid" 2>/dev/null || true
		wait "$pid" || true
		return 1
	fi

	echo "stopping=$target"
	kill -INT "-$pid" 2>/dev/null || kill -INT "$pid" 2>/dev/null || true

	shutdown_deadline=$((SECONDS + PROFILE_SHUTDOWN_TIMEOUT))
	status=0
	while ((SECONDS < shutdown_deadline)); do
		if ! kill -0 "$pid" 2>/dev/null; then
			wait "$pid" || status="$?"
			break
		fi
		sleep 1
	done

	if kill -0 "$pid" 2>/dev/null; then
		echo "ERROR: $target did not stop after SIGINT; see $log" >&2
		kill -TERM "-$pid" 2>/dev/null || true
		wait "$pid" || true
		return 1
	fi

	rg "$extract_re" "$log" > "$summary" || true
	if [[ -s "$cpu" ]]; then
		go tool pprof -top -nodecount=20 "$cpu" > "$top"
	else
		echo "WARN: missing or empty CPU profile: $cpu" >&2
	fi

	echo "exit_status=$status"
	echo "summary=$summary"
	echo "pprof_top=$top"
}

for i in "${!targets[@]}"; do
	run_target "$((i + 1))" "${targets[$i]}"
done

combined="$PROFILE_DIR/${RUN_ID}_combined.summary.txt"
rg "$extract_re" "$PROFILE_DIR/${RUN_ID}_"*.log > "$combined" || true

echo
echo "combined_summary=$combined"
