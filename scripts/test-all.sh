#!/usr/bin/env bash
# test-all.sh -- run every check unattended and write a short summary.
#
#   scripts/test-all.sh [--quick] [--with-validate] [--serial | --pair] [--out DIR]
#
# Stages: gofmt, vet, unit (go test -race), build, then the three Docker
# suites fleet-e2e, migration-e2e and update-e2e, and optionally validate
# (the older single-host Docker scenarios; off by default while issue #135
# keeps scenario 3 red). --quick stops after build (no Docker).
#
# The Docker suites run concurrently by default: each has its own compose
# project, networks, image tag and mktemp scratch dir, and none publishes a
# host port, so they cannot collide. --serial runs them one after another
# (the old behaviour); --pair runs fleet-e2e alongside migration-e2e then
# update-e2e (half the peak load, about the same wall time, since fleet-e2e
# is the longest). Before the concurrent suites start, an "images" stage
# builds the fleet and update images (concurrently): their Go compiles are
# the heavy part, and fleet-e2e's timing-sensitive steps should not run
# while the update image is still compiling ten binaries. Each suite's own
# build step is then a cache hit. validate always runs on its own afterwards
# (it loads the CPU with stress-ng and drives the host Docker daemon
# directly).
#
# Every stage runs even if an earlier one failed, so one run gives the whole
# picture. Each stage's output goes to <out>/<stage>.log; <out>/summary.txt
# lists PASS/FAIL with durations, the total wall time and the tail of each
# failed log. The exit status is non-zero if any stage failed.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT="$ROOT/.test-results/$(date +%Y%m%d-%H%M%S)"
QUICK=0
VALIDATE=0
MODE=parallel
while [ $# -gt 0 ]; do
	case "$1" in
	--quick) QUICK=1 ;;
	--with-validate) VALIDATE=1 ;;
	--serial) MODE=serial ;;
	--pair) MODE=pair ;;
	--out) OUT="$2"; shift ;;
	-h | --help) sed -n '2,27p' "$0"; exit 0 ;;
	*) echo "unknown flag: $1" >&2; exit 2 ;;
	esac
	shift
done
mkdir -p "$OUT"
cd "$ROOT" || exit 2

SUMMARY="$OUT/summary.txt"
T0=$(date +%s)
STAGES=()
: >"$SUMMARY"

# stage NAME TIMEOUT_SECONDS CMD... : run CMD with a wall-clock limit, log it
# to <out>/NAME.log and record the result in <out>/NAME.result (one summary
# line) -- a file rather than a variable so it also works when the stage runs
# in a background subshell. The limit is enforced with a watchdog so this
# works on macOS without GNU timeout.
stage() {
	local name=$1 limit=$2
	shift 2
	local log="$OUT/$name.log" start end rc
	start=$(date +%s)
	echo "== $name start ($(date +%H:%M:%S))" >>"$OUT/progress.log"
	echo "== $name ($(date +%H:%M:%S))"
	("$@") >"$log" 2>&1 &
	local pid=$!
	(sleep "$limit" && kill -TERM "$pid" 2>/dev/null && echo "TIMEOUT after ${limit}s" >>"$log") &
	local dog=$!
	wait "$pid"
	rc=$?
	kill "$dog" 2>/dev/null
	wait "$dog" 2>/dev/null
	end=$(date +%s)
	echo "== $name end ($(date +%H:%M:%S), exit $rc, $((end - start))s)" >>"$OUT/progress.log"
	if [ "$rc" -eq 0 ]; then
		printf 'PASS  %-14s %5ss\n' "$name" "$((end - start))" >"$OUT/$name.result"
	else
		printf 'FAIL  %-14s %5ss  (exit %s, log: %s)\n' "$name" "$((end - start))" "$rc" "$log" >"$OUT/$name.result"
	fi
}

# run NAME TIMEOUT CMD... : a stage, run in the foreground.
run() {
	STAGES+=("$1")
	stage "$@"
}

# lane NAME TIMEOUT CMD [-- NAME TIMEOUT CMD ...]: runs one or more stages
# one after another in a background lane (each stage's CMD is a single word,
# which is all the Docker stages need). Callers `wait` for every lane.
lane() {
	local specs=("$@") i
	for ((i = 0; i < ${#specs[@]}; i += 3)); do STAGES+=("${specs[i]}"); done
	(
		for ((i = 0; i < ${#specs[@]}; i += 3)); do
			stage "${specs[i]}" "${specs[i + 1]}" ${specs[i + 2]}
		done
	) &
}
# An interrupted run (Ctrl-C) stops its background lanes too; each suite's
# own EXIT trap then tears its compose project down.
trap 'kill $(jobs -p) 2>/dev/null; exit 130' INT TERM

gofmt_check() {
	local bad
	bad=$(gofmt -l internal cmd test)
	[ -z "$bad" ] || { echo "unformatted files:"; echo "$bad"; return 1; }
}

# build_images: the fleet and update e2e images, built concurrently (each
# suite rebuilds its own image first anyway, so if this fails the suites
# still run and report their own build error).
build_images() {
	local rc=0 p1 p2
	docker compose -f test/docker/fleet/compose.yml build --quiet &
	p1=$!
	docker compose -f test/docker/update/compose.yml build --quiet &
	p2=$!
	wait "$p1" || rc=1
	wait "$p2" || rc=1
	return "$rc"
}

run gofmt 120 gofmt_check
run vet 600 go vet ./...
run unit 1800 go test -race -count=1 ./...
run build 600 make build
if [ "$QUICK" -eq 0 ]; then
	[ "$MODE" = serial ] || run images 1800 build_images
	case "$MODE" in
	serial)
		run fleet-e2e 3600 make fleet-e2e
		run migration-e2e 1800 make migration-e2e
		run update-e2e 1800 make update-e2e
		;;
	pair)
		lane fleet-e2e 3600 "make fleet-e2e"
		lane migration-e2e 1800 "make migration-e2e" update-e2e 1800 "make update-e2e"
		wait
		;;
	*)
		lane fleet-e2e 3600 "make fleet-e2e"
		lane migration-e2e 1800 "make migration-e2e"
		lane update-e2e 1800 "make update-e2e"
		wait
		;;
	esac
	[ "$VALIDATE" -eq 1 ] && run validate 1800 make validate
fi

FAILED=0
for name in "${STAGES[@]}"; do
	res="$OUT/$name.result"
	if [ ! -f "$res" ]; then
		printf 'FAIL  %-14s     ?s  (no result recorded)\n' "$name" >"$res"
	fi
	cat "$res" >>"$SUMMARY"
	if grep -q '^FAIL' "$res"; then
		FAILED=1
		{
			echo "---- last 40 lines of $name ----"
			tail -n 40 "$OUT/$name.log" 2>/dev/null
			echo
		} >>"$OUT/failures.txt"
	fi
done
printf 'total wall time: %ss (docker stages: %s)\n' "$(($(date +%s) - T0))" "$([ "$QUICK" -eq 1 ] && echo skipped || echo "$MODE")" >>"$SUMMARY"
if [ -f "$OUT/failures.txt" ]; then
	cat "$OUT/failures.txt" >>"$SUMMARY"
fi
echo "results: $OUT" >>"$SUMMARY"
cat "$SUMMARY"
exit "$FAILED"
