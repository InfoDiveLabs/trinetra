#!/bin/sh
# TRINETRA_E2E_GUARD_CMD target: launches `trinetra update guard` detached,
# for the update-e2e harness's systemd-less host container. Named by
# realLaunchGuard (update_cmd.go) in place of
# `systemd-run --unit trinetra-update-guard --collect --quiet trinetra update guard`
# -- only read in a trinetra_testkeys build, see
# internal/trinetra/update_e2e_hooks_testkeys.go. setsid gives the guard its
# own session, same reasoning as e2e-restart.sh.
set -eu
# Real systemd-run --unit trinetra-update-guard --collect (what this
# replaces) refuses to start a second transient unit under the same fixed
# name while the first is still running. That single-instance behavior is
# load-bearing, not incidental: runGuard's first action is its own
# restart() call, which starts a fresh daemon process whose own startup
# hook (resumePendingOnStart, daemon.go) sees the SAME still-set Pending
# and would launch ANOTHER guard -- and that one's restart() would do the
# same again, unboundedly, without this check.
if pgrep -f '^/usr/local/bin/trinetra update guard$' >/dev/null 2>&1; then
	exit 0
fi
setsid /usr/local/bin/trinetra update guard >>/var/log/trinetra-guard.log 2>&1 </dev/null &
