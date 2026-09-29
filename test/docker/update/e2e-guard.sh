#!/bin/sh
# TRINETRA_E2E_GUARD_CMD target, emulating
#   systemd-run --unit trinetra-update-guard --collect --quiet <pinned guard> update guard
# on the update-e2e harness's systemd-less host container. launchGuardUnit
# (internal/trinetra/update_cmd.go, trinetra_testkeys builds only) passes the
# unit name as $1 and the pinned guard binary's path as $2.
#
# Like systemd-run, it refuses to start a second transient unit under a name
# that is still active (a flock held for the unit's lifetime stands in for
# the unit; -o so the daemon the guard restarts does not inherit the lock fd
# and keep the "unit" active forever), and runs the guard detached (setsid) so it outlives the apply
# that launched it. The guard's own guard.lock is what actually keeps two
# guards (this one and the watchdog's) from working on the same update.
set -eu
unit=$1
guard=$2
RUN=/run/fake-systemd
mkdir -p "$RUN"
if ! flock -n "$RUN/$unit.lock" true; then
	echo "Failed to start transient service unit: Unit $unit.service was already loaded or has a fragment file." >&2
	exit 1
fi
setsid flock -o -n "$RUN/$unit.lock" "$guard" update guard >>/var/log/trinetra-guard.log 2>&1 </dev/null &
