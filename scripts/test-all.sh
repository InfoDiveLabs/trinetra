#!/usr/bin/env bash
# test-all.sh -- run every check unattended and write a short summary.
#
#   scripts/test-all.sh [--quick] [--with-validate] [--out DIR]
#
# Stages (in order): gofmt, vet, unit (go test -race), build, fleet-e2e,
# migration-e2e, update-e2e, and optionally validate (the older single-host
# Docker scenarios; off by default while issue #135 keeps scenario 3 red).
# --quick stops after build (no Docker).
#
# Every stage runs even if an earlier one failed, so one run gives the whole
# picture. Each stage's output goes to <out>/<stage>.log; <out>/summary.txt
# lists PASS/FAIL with durations and the tail of each failed log. The exit
# status is non-zero if any stage failed.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT="$ROOT/.test-results/$(date +%Y%m%d-%H%M%S)"
QUICK=0
VALIDATE=0
while [ $# -gt 0 ]; do
	case "$1" in
	--quick) QUICK=1 ;;
	--with-validate) VALIDATE=1 ;;
	--out) OUT="$2"; shift ;;
	-h | --help) sed -n '2,15p' "$0"; exit 0 ;;
	*) echo "unknown flag: $1" >&2; exit 2 ;;
	esac
	shift
done
mkdir -p "$OUT"
cd "$ROOT" || exit 2

SUMMARY="$OUT/summary.txt"
FAILED=0
: >"$SUMMARY"

# run NAME TIMEOUT_SECONDS CMD... : run CMD with a wall-clock limit, log it,
# and record the result. The limit is enforced with a watchdog so this works
# on macOS without GNU timeout.
run() {
	local name=$1 limit=$2
	shift 2
	local log="$OUT/$name.log" start end rc
	start=$(date +%s)
	echo "== $name ($(date +%H:%M:%S))" | tee -a "$OUT/progress.log"
	("$@") >"$log" 2>&1 &
	local pid=$!
	(sleep "$limit" && kill -TERM "$pid" 2>/dev/null && echo "TIMEOUT after ${limit}s" >>"$log") &
	local dog=$!
	wait "$pid"
	rc=$?
	kill "$dog" 2>/dev/null
	wait "$dog" 2>/dev/null
	end=$(date +%s)
	if [ "$rc" -eq 0 ]; then
		printf 'PASS  %-14s %5ss\n' "$name" "$((end - start))" >>"$SUMMARY"
	else
		FAILED=1
		printf 'FAIL  %-14s %5ss  (exit %s, log: %s)\n' "$name" "$((end - start))" "$rc" "$log" >>"$SUMMARY"
		{
			echo "---- last 40 lines of $name ----"
			tail -n 40 "$log"
			echo
		} >>"$OUT/failures.txt"
	fi
}

gofmt_check() {
	local bad
	bad=$(gofmt -l internal cmd test)
	[ -z "$bad" ] || { echo "unformatted files:"; echo "$bad"; return 1; }
}

run gofmt 120 gofmt_check
run vet 600 go vet ./...
run unit 1800 go test -race -count=1 ./...
run build 600 make build
if [ "$QUICK" -eq 0 ]; then
	run fleet-e2e 3600 make fleet-e2e
	run migration-e2e 1800 make migration-e2e
	run update-e2e 1800 make update-e2e
	[ "$VALIDATE" -eq 1 ] && run validate 1800 make validate
fi

if [ -f "$OUT/failures.txt" ]; then
	cat "$OUT/failures.txt" >>"$SUMMARY"
fi
echo "results: $OUT" >>"$SUMMARY"
cat "$SUMMARY"
exit "$FAILED"
