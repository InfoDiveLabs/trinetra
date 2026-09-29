#!/bin/sh
# A stand-in for systemctl on the update-e2e harness's systemd-less host
# container, installed at /usr/local/bin/systemctl (there is no real one in
# this image). It implements what trinetra calls, behaving like systemd for
# the units trinetra installs:
#
#   daemon-reload                 no-op (no unit database to reload)
#   enable <unit>                 no-op
#   enable --now|start trinetra-update-watchdog.timer
#                                 starts the timer emulation (fake-timer.sh)
#                                 unless it already runs -- like systemd,
#                                 enabling an active timer again changes
#                                 nothing
#   disable --now|stop trinetra-update-watchdog.timer
#                                 stops the timer emulation
#   restart trinetra              delegates to e2e-restart.sh
#   is-active [--quiet] trinetra  0 iff a trinetra daemon process runs
#   is-active [--quiet] trinetra-update-watchdog.timer
#                                 0 iff the timer emulation runs
#   anything else                 accepted as a harmless no-op
set -eu

TIMER=trinetra-update-watchdog.timer
RUN=/run/fake-systemd
mkdir -p "$RUN"

timer_running() { ! flock -n "$RUN/$TIMER.lock" true 2>/dev/null; }

start_timer() {
	timer_running && return 0
	setsid flock -o -n "$RUN/$TIMER.lock" /usr/local/libexec/fake-timer.sh \
		>>/var/log/trinetra-watchdog.log 2>&1 </dev/null &
	# Wait until the loop holds its lock, so a second enable right after
	# this one sees it running.
	i=0
	while [ "$i" -lt 50 ] && ! timer_running; do
		sleep 0.1
		i=$((i + 1))
	done
}

stop_timer() {
	pkill -f '/usr/local/libexec/fake-timer.sh' >/dev/null 2>&1 || true
}

last=""
for a in "$@"; do last=$a; done

case "${1:-}" in
daemon-reload)
	exit 0
	;;
enable | start)
	if [ "$last" = "$TIMER" ] && { [ "$1" = start ] || [ "${2:-}" = --now ]; }; then
		start_timer
	fi
	exit 0
	;;
disable | stop)
	if [ "$last" = "$TIMER" ]; then stop_timer; fi
	exit 0
	;;
restart)
	exec /usr/local/libexec/e2e-restart.sh
	;;
is-active)
	case "$last" in
	"$TIMER")
		timer_running && exit 0
		exit 1
		;;
	esac
	if pgrep -f '^/usr/local/bin/trinetra daemon$' >/dev/null 2>&1; then
		exit 0
	fi
	exit 1
	;;
*)
	exit 0
	;;
esac
