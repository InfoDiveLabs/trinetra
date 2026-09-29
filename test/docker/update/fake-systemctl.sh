#!/bin/sh
# A stand-in for systemctl on the update-e2e harness's systemd-less host
# container, installed at /usr/local/bin/systemctl (ahead of any real one --
# there is none in this image). It only implements what
# internal/trinetra/systemd.go's cmdInstall (`trinetra install` always shells
# out to `systemctl daemon-reload`, `enable trinetra`, `restart trinetra`)
# and update_guard.go's systemdControlHealth.Active (`systemctl is-active
# --quiet trinetra`, used by every build, hooked or not) need:
#
#   daemon-reload, enable ...   no-ops (nothing to reload/enable without a
#                               real systemd unit database)
#   restart trinetra            delegates to e2e-restart.sh
#   is-active [--quiet] trinetra   exits 0 iff a trinetra daemon process is
#                               currently running, else 1
#   anything else                accepted as a harmless no-op, so an
#                               unexpected systemctl call never crashes the
#                               harness outright
set -eu

case "${1:-}" in
daemon-reload | enable)
	exit 0
	;;
restart)
	exec /usr/local/libexec/e2e-restart.sh
	;;
is-active)
	if pgrep -f '^/usr/local/bin/trinetra daemon$' >/dev/null 2>&1; then
		exit 0
	fi
	exit 1
	;;
*)
	exit 0
	;;
esac
