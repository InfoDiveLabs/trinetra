#!/bin/sh
# Restarts the trinetra daemon without systemd, for the update-e2e harness's
# systemd-less host container. Two callers, always the same effect as a real
# `systemctl restart trinetra`:
#
#   - the fake systemctl (systemctl.sh) delegates its "restart trinetra" case
#     here, so `trinetra install` (which always shells out to
#     `systemctl restart trinetra`) works with no real systemd present;
#   - TRINETRA_E2E_RESTART_CMD names this same script, so the self-update
#     guard's restart (update_guard.go's realGuardDeps, only read in a
#     trinetra_testkeys build -- see internal/trinetra/update_e2e_hooks_testkeys.go)
#     uses it too, instead of `systemctl restart trinetra`.
#
# setsid detaches the daemon into its own session, so it survives both this
# script's own exit and the process-group kill runWithTimeout (iface.go)
# would otherwise send if the CALLER of this script (a `trinetra ...`
# subprocess) ever hit its exec timeout.
set -eu

pkill -f '^/usr/local/bin/trinetra daemon$' >/dev/null 2>&1 || true
# Give the old process a moment to release the control socket and pidfile
# before the new one claims them.
i=0
while [ "$i" -lt 25 ] && pgrep -f '^/usr/local/bin/trinetra daemon$' >/dev/null 2>&1; do
	sleep 0.2
	i=$((i + 1))
done

setsid /usr/local/bin/trinetra daemon >>/var/log/trinetra.log 2>&1 </dev/null &
