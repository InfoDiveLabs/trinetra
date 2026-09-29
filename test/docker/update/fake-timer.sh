#!/bin/sh
# Emulates trinetra-update-watchdog.timer on the update-e2e harness's
# systemd-less host (started by fake-systemctl.sh on `enable --now`, holding
# /run/fake-systemd/trinetra-update-watchdog.timer.lock while it runs, which
# is how the fake systemctl knows it is active).
#
# Like the real timer it fires the watchdog service right away when enabled
# on a host that booted long ago (OnBootSec already passed), then again
# OnUnitActiveSec after each run finished. Like a oneshot service, runs never
# overlap. The service's ExecStart is read from the installed unit file on
# every run, so what runs is exactly what `trinetra install`/`update apply`
# wrote -- the pinned guard binary with `update guard --if-pending`.
set -u

UNITS=/etc/systemd/system
SERVICE=$UNITS/trinetra-update-watchdog.service
TIMER=$UNITS/trinetra-update-watchdog.timer

# seconds <systemd time span>: "1min", "90s", "2min", "30".
seconds() {
	case "$1" in
	*min) echo $((${1%min} * 60)) ;;
	*s) echo "${1%s}" ;;
	*) echo "$1" ;;
	esac
}

while :; do
	exec_start=$(sed -n 's/^ExecStart=//p' "$SERVICE" 2>/dev/null | head -n 1)
	if [ -n "$exec_start" ]; then
		echo "$(date -u +%FT%TZ) watchdog: $exec_start"
		# shellcheck disable=SC2086 # ExecStart is a plain argv, split on spaces like systemd would.
		$exec_start || echo "$(date -u +%FT%TZ) watchdog: exit $?"
	fi
	period=$(sed -n 's/^OnUnitActiveSec=//p' "$TIMER" 2>/dev/null | head -n 1)
	# TRINETRA_E2E_WATCHDOG_INTERVAL (set in the image) shortens the unit's
	# own 1-minute cadence for the harness; nothing in trinetra reads it.
	period=${TRINETRA_E2E_WATCHDOG_INTERVAL:-${period:-1min}}
	sleep "$(seconds "$period")"
done
