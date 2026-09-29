#!/usr/bin/env bash
# End-to-end test of Trinetra's signed self-update path (docs/handbook's
# "Release keys and releasing" section, internal/update, cmd/trinetra-release,
# `trinetra update`/`install --require-signed`).
#
# The "host" container (compose.yml) runs the real trinetra binaries --
# every one of them built with -tags trinetra_testkeys, so ProductionKeys()
# trusts the deterministic test key set (updatetest.TestKeySet()) instead of
# this repo's real (currently empty) production keys -- against two fakes
# built at image time: relsrv (a stand-in for the GitHub REST API, serving
# pre-built, pre-signed release fixtures out of /releases -- see
# build-fixtures.sh) and mocktg (the mock Telegram API shared with the other
# docker harnesses). The host has no real systemd: `systemctl` is a shell
# shim (fake-systemctl.sh) that also emulates trinetra-update-watchdog.timer
# (fake-timer.sh: runs the installed watchdog service's ExecStart -- the
# pinned guard with `update guard --if-pending` -- right away and then every
# OnUnitActiveSec), and the guard's restart/launch are replaced by the
# TRINETRA_E2E_RESTART_CMD/TRINETRA_E2E_GUARD_CMD hooks
# (internal/trinetra/update_e2e_hooks_testkeys.go, read only in a
# trinetra_testkeys build) baked into the image's environment; the guard
# launcher emulates systemd-run's one-active-unit-per-name rule with a flock.
#
# Twelve scenarios, run in one continuous narrative so the version floor only
# ever moves forward (0.5.0 -> 0.5.1 -> ... -> 0.5.7), printing
# "PASS n name" / "FAIL n name: why" as it goes, same style as
# test/docker/fleet/run.sh. Pipes never end in `grep -q` (see that file's own
# note): a match's writer would die of SIGPIPE and, under `pipefail`, a long
# enough match turns success into a reported failure.
#
# Timing: on a real host the self-update loop that turns a commit/rollback
# into a Telegram alert ticks every 5 minutes (updateLoopInterval,
# update_daemon.go), a pending update gets a 90s health window
# (updateHealthDeadline) and the watchdog timer fires every minute. The
# image shortens all three for this harness (Dockerfile: loop 5s, health
# window 30s -- TRINETRA_E2E_UPDATE_LOOP_INTERVAL/TRINETRA_E2E_HEALTH_DEADLINE,
# honoured only by a trinetra_testkeys build -- and watchdog 10s,
# TRINETRA_E2E_WATCHDOG_INTERVAL, read only by fake-timer.sh). The
# scenarios' logic and assertions do not depend on the values; every wait
# below is a bounded poll sized from them (LOOP/HEALTH/WATCHDOG) with
# generous slack for a loaded machine.
set -euo pipefail
cd "$(dirname "$0")"

T_START=$(date +%s)
STEP=""
# The image's shortened cadences (Dockerfile), in seconds, to size waits.
LOOP=5
HEALTH=30
WATCHDOG=10

compose() { docker compose -f compose.yml "$@"; }
on() { local svc=$1; shift; compose exec -T "$svc" "$@"; }

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/swupdate.XXXXXX")

dump_evidence() {
	echo "---------------- evidence ----------------"
	echo "--- host: tail of /var/log/trinetra.log ---"
	on host sh -c 'tail -n 60 /var/log/trinetra.log 2>/dev/null || echo "(no log)"' 2>/dev/null || true
	echo "--- host: tail of /var/log/trinetra-guard.log ---"
	on host sh -c 'tail -n 60 /var/log/trinetra-guard.log 2>/dev/null || echo "(no log)"' 2>/dev/null || true
	echo "--- host: tail of /var/log/trinetra-watchdog.log ---"
	on host sh -c 'tail -n 40 /var/log/trinetra-watchdog.log 2>/dev/null || echo "(no log)"' 2>/dev/null || true
	echo "--- host: trinetra update status --json ---"
	on host trinetra update status --json 2>&1 || true
	echo "--- host: update state.json ---"
	on host cat /var/lib/trinetra/update/state.json 2>&1 || true
	echo "--- mocktg: messages ---"
	on mocktg curl -s http://localhost:8080/_messages 2>/dev/null || true
	echo
	echo "--- compose ps ---"
	compose ps 2>&1 || true
	echo "------------------------------------------"
}

cleanup() {
	local rc=$?
	if [ "$rc" -ne 0 ] && [ -n "$STEP" ]; then dump_evidence; fi
	if [ "$rc" -ne 0 ] && [ -n "${KEEP_ON_FAIL:-}" ]; then
		echo "KEEP_ON_FAIL set: leaving the containers up (docker compose -f $(pwd)/compose.yml down -v)"
		rm -rf "$SCRATCH"
		exit "$rc"
	fi
	echo "== cleanup =="
	compose down -v -t 2 >/dev/null 2>&1 || true
	rm -rf "$SCRATCH"
	echo "runtime: $(($(date +%s) - T_START))s"
	exit "$rc"
}
trap cleanup EXIT

step() { STEP="$1"; echo "== $1 == (+$(($(date +%s) - T_START))s)"; }
pass() { echo "PASS $STEP${1:+: $1}"; }
fail() { echo "FAIL $STEP: $*"; exit 1; }

# wait_until <timeout-seconds> <what> <command...>: poll every second.
wait_until() {
	local timeout=$1 what=$2
	shift 2
	local deadline=$(($(date +%s) + timeout))
	while ! "$@" >/dev/null 2>&1; do
		if [ "$(date +%s)" -ge "$deadline" ]; then
			fail "timed out after ${timeout}s waiting for: $what"
		fi
		sleep 1
	done
}

# status_json: the host's `trinetra update status --json`, as a compact
# single-line JSON document (jq -c) so callers can grep/jq it directly.
status_json() { on host trinetra update status --json; }
status_field() { status_json | jq -r "$1"; } # <jq filter>
floor_is() { [ "$(status_field .floor)" = "$1" ]; }
running_is() { [ "$(status_field .running)" = "$1" ]; }
pending_is_null() { [ "$(status_field .pending)" = "null" ]; }
last_outcome_is() { [ "$(status_field '.last.outcome // empty')" = "$1" ]; }
bad_versions_has() { on host sh -c 'jq -r ".bad_versions[]?" /var/lib/trinetra/update/state.json 2>/dev/null' | grep -F "$1" >/dev/null; }
# Pipes below end in `grep ... >/dev/null`, never `grep -q` (see this file's
# own header note).
mocktg_has() { on mocktg curl -s http://localhost:8080/_messages | grep -F "$1" >/dev/null; }
daemon_up() { on host pgrep -f '^/usr/local/bin/trinetra daemon$' >/dev/null 2>&1; }
# The guard always runs from the pinned binary (R14): launched after an
# apply as `... update guard`, and by the watchdog as `... update guard
# --if-pending`.
GUARD_BIN=/usr/local/lib/trinetra/guard/trinetra
GUARD_RE="^$GUARD_BIN update guard"
# guard_gone: true when no guard process is running. A negation local to
# THIS shell (not `! pgrep ...` run inside a `sh -c` string on the
# container) -- a pgrep pattern wrapped in `sh -c "...pattern..."` would
# match its OWN wrapping sh process, since that process's command line
# contains the pattern text too, and never report "gone".
guard_gone() { ! on host pgrep -f "$GUARD_RE" >/dev/null 2>&1; }
guard_running() { on host pgrep -f "$GUARD_RE" >/dev/null 2>&1; }
# kill_guard: SIGKILLs every running guard and, in the same container exec
# (so no watchdog run can slip in between), prints the killed PIDs on the
# first line and the pending update as it stood right after the kill on the
# second. The ^-anchored pattern cannot match the wrapping sh itself.
kill_guard() {
	on host sh -c "pids=\$(pgrep -f '$GUARD_RE' | tr '\n' ' '); [ -n \"\$pids\" ] || exit 1; kill -9 \$pids; echo \"\$pids\"; trinetra update status --json | jq -c .pending"
}
# pids_gone <pids>: none of these processes exists any more. With the
# watchdog firing every WATCHDOG seconds a NEW guard may legitimately be
# running by now; what matters is that the killed ones are gone.
pids_gone() { on host sh -c "for p in $1; do kill -0 \$p 2>/dev/null && exit 1; done; exit 0"; }
watchdog_active() { on host systemctl is-active --quiet trinetra-update-watchdog.timer; }
last_at() { status_field '.last.at // 0'; }
last_newer_than() { [ "$(last_at)" -gt "$1" ]; } # <unix seconds>
crash_count() { on host sh -c 'grep -c "e2e crash" /var/log/trinetra.log 2>/dev/null || true'; }
crashed_since() { [ "$(crash_count)" -gt "$1" ]; } # <previous count>
sha_of() { on host sh -c "sha256sum $1 2>/dev/null | cut -d' ' -f1"; }
staged_dir_exists() { on host test -d "/var/lib/trinetra/update/staging/$1"; }

# refused <version> [--force]: runs `trinetra update apply --version
# <version>` (optionally --force) and expects it to fail; echoes stderr.
refused() {
	local v=$1
	shift
	local out
	if out=$(on host trinetra update apply --version "$v" "$@" 2>&1); then
		fail "apply --version $v $* unexpectedly succeeded: $out"
	fi
	echo "$out"
}

# ---------------------------------------------------------------------------
step "1 install 0.5.0 --require-signed from a signed bundle"
compose down -v -t 1 >/dev/null 2>&1 || true
compose build --quiet
UP=$(compose up -d 2>&1) || fail "compose up: $UP"
wait_until 30 "relsrv" on relsrv curl -sf -H "Authorization: Bearer e2etoken" \
	http://localhost:8080/repos/InfoDiveLabs/trinetra/releases/tags/channels
wait_until 30 "mock telegram" on mocktg curl -sf http://localhost:8080/_messages

BUNDLE=/releases/v0.5.0
BIN="$BUNDLE/trinetra"
on host "$BIN" config set update.channel beta >/dev/null
on host "$BIN" config set update.source github >/dev/null
on host "$BIN" config set update.github_token e2etoken >/dev/null
on host "$BIN" telegram set-token TESTTOKEN >/dev/null
on host "$BIN" config set telegram.chat_id 999 >/dev/null

OUT=$(on host "$BIN" install --require-signed 2>&1) || fail "install --require-signed: $OUT"
echo "  $OUT"
wait_until 30 "daemon up" daemon_up
wait_until 30 "control socket answers" on host trinetra update status
floor_is 0.5.0 || fail "floor after install: $(status_field .floor)"
running_is 0.5.0 || fail "running after install: $(status_field .running)"
watchdog_active || fail "install did not enable trinetra-update-watchdog.timer"
on host test -x "$GUARD_BIN" || fail "install did not write the pinned guard $GUARD_BIN"
pass "installed from $BUNDLE, floor and running both 0.5.0, watchdog timer armed"

# ---------------------------------------------------------------------------
step "2 check sees 0.5.1; apply upgrades and notifies"
CHECK=$(on host trinetra update check 2>&1) || fail "update check: $CHECK"
grep -q "^update available: 0.5.1 (channel beta)$" <<<"$CHECK" || fail "update check output: $CHECK"
APPLY=$(on host trinetra update apply 2>&1) || fail "update apply: $APPLY"
echo "  $APPLY"
wait_until $((HEALTH + 60)) "guard commits 0.5.1" last_outcome_is committed
running_is 0.5.1 || fail "running after apply: $(status_field .running)"
floor_is 0.5.1 || fail "floor after apply: $(status_field .floor)"
pending_is_null || fail "pending left over after commit: $(status_field .pending)"
# Delivered on the self-update loop's next tick (LOOP here, 5 minutes on a
# real host -- see this file's header note).
wait_until $((LOOP * 6 + 30)) "mocktg got the 'updated 0.5.0 -> 0.5.1' message" mocktg_has "updated 0.5.0 → 0.5.1"
pass "0.5.0 -> 0.5.1: running, floor, and the mocktg notification all confirm"

# ---------------------------------------------------------------------------
step "3 bad CI signature release refused"
OUT=$(refused 0.5.3-badci)
grep -qi "signature" <<<"$OUT" || fail "unexpected refusal: $OUT"
staged_dir_exists 0.5.3-badci && fail "0.5.3-badci was staged despite the bad CI signature"
running_is 0.5.1 || fail "running changed after a refused apply: $(status_field .running)"
pass "refused ($OUT), nothing staged"

# ---------------------------------------------------------------------------
step "4 missing maintainer signature refused"
OUT=$(refused 0.5.4-badmaint)
grep -qi "signature" <<<"$OUT" || fail "unexpected refusal: $OUT"
staged_dir_exists 0.5.4-badmaint && fail "0.5.4-badmaint was staged despite the missing maintainer signature"
running_is 0.5.1 || fail "running changed after a refused apply: $(status_field .running)"
pass "refused ($OUT), nothing staged"

# ---------------------------------------------------------------------------
step "5 tampered binary (manifest hash mismatch) refused"
BEFORE=$(sha_of /usr/local/bin/trinetra)
OUT=$(refused 0.5.5-tampered)
grep -qi "sha256" <<<"$OUT" || fail "unexpected refusal: $OUT"
AFTER=$(sha_of /usr/local/bin/trinetra)
[ "$BEFORE" = "$AFTER" ] || fail "installed trinetra binary changed despite the refused tampered apply ($BEFORE -> $AFTER)"
running_is 0.5.1 || fail "running changed after a refused apply: $(status_field .running)"
pass "refused ($OUT), installed binary unchanged ($BEFORE)"

# ---------------------------------------------------------------------------
step "6 apply 0.5.2 (crashes on start) rolls back"
APPLY=$(on host trinetra update apply --version 0.5.2 2>&1) || fail "apply 0.5.2: $APPLY"
echo "  $APPLY"
# The guard's own deadline is HEALTH seconds from ITS restart
# (update_guard.go), which starts a few seconds after this apply call
# returns; wait well past that so a slow poll cycle right at the boundary is
# not a false failure.
wait_until $((HEALTH + 60)) "guard rolls back 0.5.2" last_outcome_is rolled_back
running_is 0.5.1 || fail "running after rollback: $(status_field .running)"
floor_is 0.5.1 || fail "floor moved after a rolled-back update: $(status_field .floor)"
pending_is_null || fail "pending left over after rollback: $(status_field .pending)"
bad_versions_has 0.5.2 || fail "0.5.2 not recorded in bad_versions: $(on host cat /var/lib/trinetra/update/state.json)"
wait_until $((LOOP * 6 + 30)) "mocktg got the critical rollback alert" mocktg_has "rolled back to 0.5.1"
mocktg_has "0.5.2" || fail "rollback alert does not name 0.5.2"
OUT=$(refused 0.5.2)
grep -qi "failed its health check here before" <<<"$OUT" || fail "re-apply of a known-bad version without --force: $OUT"
pass "0.5.2 rolled back to 0.5.1 once its ${HEALTH}s health window ran out, critical alert sent, bad_versions recorded, re-apply refused without --force"

# ---------------------------------------------------------------------------
step "7 guard killed while a crash-on-start build is pending: the watchdog rolls back"
# The real-systemd rehearsal's scenario 10: the new build's `daemon` exits
# at once, the guard watching it is killed, and nothing started from the new
# build can recover. The watchdog runs the PINNED guard, so it can.
BEFORE_AT=$(last_at)
CRASHES=$(crash_count)
APPLY=$(on host trinetra update apply --version 0.5.2 --force 2>&1) || fail "apply 0.5.2 --force: $APPLY"
echo "  $APPLY"
wait_until 30 "guard restarted onto the crashing 0.5.2" crashed_since "$CRASHES"
KILLED=$(kill_guard) || fail "no running guard process to kill"
KILLED_PIDS=$(head -n 1 <<<"$KILLED")
PENDING_AT_KILL=$(sed -n 2p <<<"$KILLED")
wait_until 10 "killed guard process ($KILLED_PIDS) gone" pids_gone "$KILLED_PIDS"
[ "$PENDING_AT_KILL" != "null" ] || fail "pending already cleared before the guard was killed"
echo "  pending stuck after killing the guard: $PENDING_AT_KILL"
# Next watchdog run (<= WATCHDOG) + a full HEALTH window from its own restart.
wait_until $((WATCHDOG + HEALTH + 60)) "the watchdog's guard rolls back 0.5.2" last_newer_than "$BEFORE_AT"
last_outcome_is rolled_back || fail "watchdog did not roll back: $(status_field '.last')"
pending_is_null || fail "pending left over: $(status_field .pending)"
running_is 0.5.1 || fail "running after watchdog rollback: $(status_field .running)"
wait_until 30 "daemon back up on 0.5.1" daemon_up
pass "killed guard + crash-looping 0.5.2: the watchdog restored 0.5.1 without any manual step"

# ---------------------------------------------------------------------------
step "8 guard killed mid health-check: the watchdog resumes and commits"
OLD_PID=$(on host pgrep -f '^/usr/local/bin/trinetra daemon$') || fail "no running daemon before apply 0.5.6"
APPLY=$(on host trinetra update apply --version 0.5.6 2>&1) || fail "apply 0.5.6: $APPLY"
echo "  $APPLY"
# Wait for evidence the guard has already restarted the daemon (a new PID)
# and is now in its health-poll loop, then kill it before a post-restart
# sample can exist (fast_interval defaults to 5s), so it never commits.
wait_until 15 "guard restarted the daemon onto 0.5.6" \
	on host sh -c "pgrep -f '^/usr/local/bin/trinetra daemon\$' | grep -vxF '$OLD_PID' >/dev/null"
KILLED=$(kill_guard) || fail "no running guard process to kill"
KILLED_PIDS=$(head -n 1 <<<"$KILLED")
PENDING_AT_KILL=$(sed -n 2p <<<"$KILLED")
wait_until 10 "killed guard process ($KILLED_PIDS) gone" pids_gone "$KILLED_PIDS"
[ "$PENDING_AT_KILL" != "null" ] || fail "pending was already cleared before the guard could be killed (race: the health check committed first)"
echo "  pending stuck after killing the guard: $PENDING_AT_KILL"
# No manual restart: the watchdog timer alone must resolve it.
wait_until $((WATCHDOG + HEALTH + 60)) "the watchdog's guard resolves the pending update" pending_is_null
last_outcome_is committed || fail "watchdog did not commit a healthy 0.5.6: $(status_field '.last')"
running_is 0.5.6 || fail "running after resume: $(status_field .running)"
floor_is 0.5.6 || fail "floor after resume: $(status_field .floor)"
pass "the watchdog resumed the killed guard and committed 0.5.6, no pending left"

# ---------------------------------------------------------------------------
step "9 apply killed mid-swap: the watchdog restores the previous build"
BEFORE_SHA=$(sha_of /usr/local/bin/trinetra)
BEFORE_AT=$(last_at)
on host rm -f /tmp/swap-paused
# TRINETRA_E2E_SWAP_PAUSE_FILE (trinetra_testkeys only) freezes the apply
# right after its first binary rename; it is then SIGKILLed there.
on host sh -c 'TRINETRA_E2E_SWAP_PAUSE_FILE=/tmp/swap-paused setsid trinetra update apply --version 0.5.7 >/tmp/apply-0.5.7.log 2>&1 </dev/null &'
wait_until 60 "apply paused after its first rename" on host test -f /tmp/swap-paused
[ "$(status_field '.pending.phase')" = "swapping" ] || fail "pending not recorded as swapping before the renames: $(status_field .pending)"
[ "$(sha_of /usr/local/bin/trinetra)" != "$BEFORE_SHA" ] || fail "core binary not yet replaced at the pause point"
on host pkill -9 -f '^trinetra update apply --version 0.5.7' || on host pkill -9 -f 'update apply --version 0.5.7' || fail "no apply process to kill"
wait_until $((WATCHDOG + 60)) "the watchdog resolves the interrupted swap" last_newer_than "$BEFORE_AT"
last_outcome_is rolled_back || fail "interrupted swap not rolled back: $(status_field '.last')"
status_field '.last.detail' | grep -F "interrupted" >/dev/null || fail "rollback detail does not mention the interrupted swap: $(status_field '.last.detail')"
pending_is_null || fail "pending left over: $(status_field .pending)"
[ "$(sha_of /usr/local/bin/trinetra)" = "$BEFORE_SHA" ] || fail "core binary not restored to 0.5.6"
running_is 0.5.6 || fail "running after the interrupted swap: $(status_field .running)"
floor_is 0.5.6 || fail "floor moved: $(status_field .floor)"
bad_versions_has 0.5.7 && fail "0.5.7 marked bad although it never ran"
BEFORE_AT=$(last_at)
APPLY=$(on host trinetra update apply --version 0.5.7 2>&1) || fail "re-apply 0.5.7: $APPLY"
# Wait for the commit itself: while an update is pending, `status`'s floor
# already shows max(persisted floor, running binary) (review M18).
wait_until $((HEALTH + 60)) "guard commits 0.5.7" last_newer_than "$BEFORE_AT"
last_outcome_is committed || fail "re-apply of 0.5.7 not committed: $(status_field '.last')"
pending_is_null || fail "pending left over after re-apply: $(status_field .pending)"
running_is 0.5.7 || fail "running after re-apply: $(status_field .running)"
floor_is 0.5.7 || fail "floor after re-apply: $(status_field .floor)"
pass "killed mid-swap: previous build restored by the watchdog; 0.5.7 then applied cleanly"

# ---------------------------------------------------------------------------
step "10 apply --version 0.5.0 refused as downgrade"
OUT=$(refused 0.5.0)
grep -qi "downgrade\|lower than the highest version" <<<"$OUT" || fail "unexpected refusal: $OUT"
running_is 0.5.7 || fail "running changed after a refused downgrade: $(status_field .running)"
floor_is 0.5.7 || fail "floor changed after a refused downgrade: $(status_field .floor)"
pass "refused ($OUT)"

# ---------------------------------------------------------------------------
step "11 config get and dump never print the GitHub token"
GET_ALL=$(on host trinetra config get)
grep -F "e2etoken" <<<"$GET_ALL" >/dev/null && fail "trinetra config get printed the raw token"
grep -q '"github_token": "(set)"' <<<"$GET_ALL" || fail "trinetra config get does not show update.github_token redacted: $GET_ALL"
GET_ONE=$(on host trinetra config get update.github_token)
[ "$GET_ONE" = "(set)" ] || fail "trinetra config get update.github_token = %q, want (set)"
DUMP=$(on host trinetra dump --metric cpu --since 1h 2>&1) || fail "trinetra dump: $DUMP"
grep -F "e2etoken" <<<"$DUMP" >/dev/null && fail "trinetra dump printed the raw token"
pass "config get (full and single-key) and dump all redact/omit the token"

# ---------------------------------------------------------------------------
step "12 audit trail and watchdog still armed"
AUDIT=$(on host cat /var/lib/trinetra/update/audit.jsonl 2>&1) || fail "no update audit log: $AUDIT"
for want in '"action":"update.apply"' '"action":"update.commit"' '"action":"update.rolled_back"' '"actor":"guard"'; do
	grep -F "$want" <<<"$AUDIT" >/dev/null || fail "audit log lacks $want: $AUDIT"
done
watchdog_active || fail "watchdog timer not active at the end"
# The watchdog's own `--if-pending` guard runs every WATCHDOG seconds and
# exits at once with nothing pending; a stray guard never exits.
wait_until 15 "no guard left running with nothing pending" guard_gone
pass "apply/commit/rollback audited with actors; watchdog armed; no stray guard"

STEP=""
echo "ALL PASS ($(($(date +%s) - T_START))s)"
