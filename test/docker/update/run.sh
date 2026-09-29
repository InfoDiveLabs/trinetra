#!/usr/bin/env bash
# End-to-end test of Trinetra's signed self-update path (docs/handbook's
# "Release keys and releasing" section, internal/update, cmd/trinetra-release,
# `trinetra update`/`install --require-signed`).
#
# The "host" container (compose.yml) runs the real trinetra binaries --
# every one of them built with -tags trinetra_testkeys, so ProductionKeys()
# trusts the deterministic test key set (update.TestKeySet()) instead of
# this repo's real (currently empty) production keys -- against two fakes
# built at image time: relsrv (a stand-in for the GitHub REST API, serving
# pre-built, pre-signed release fixtures out of /releases -- see
# build-fixtures.sh) and mocktg (the mock Telegram API shared with the other
# docker harnesses). The host has no real systemd: `systemctl` is a small
# shell shim (fake-systemctl.sh) and the self-update guard's restart/launch
# are replaced by the TRINETRA_E2E_RESTART_CMD/TRINETRA_E2E_GUARD_CMD hooks
# (internal/trinetra/update_e2e_hooks_testkeys.go, read only in a
# trinetra_testkeys build) baked into the image's environment.
#
# Nine scenarios, run in one continuous narrative so the version floor only
# ever moves forward (0.5.0 -> 0.5.1 -> ... -> 0.5.6), printing
# "PASS n name" / "FAIL n name: why" as it goes, same style as
# test/docker/fleet/run.sh. Pipes never end in `grep -q` (see that file's own
# note): a match's writer would die of SIGPIPE and, under `pipefail`, a long
# enough match turns success into a reported failure.
#
# The self-update loop that turns a commit/rollback into a Telegram alert
# only ticks every 5 minutes (updateLoopInterval, update_daemon.go) -- by
# design, not a harness shortcut -- so scenarios 2 and 6 each wait up to
# that long for their mocktg message. Budget accordingly (make update-e2e /
# scripts/test-all.sh's update-e2e stage allow 30 minutes).
set -euo pipefail
cd "$(dirname "$0")"

T_START=$(date +%s)
STEP=""

compose() { docker compose -f compose.yml "$@"; }
on() { local svc=$1; shift; compose exec -T "$svc" "$@"; }

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/swupdate.XXXXXX")

dump_evidence() {
	echo "---------------- evidence ----------------"
	echo "--- host: tail of /var/log/trinetra.log ---"
	on host sh -c 'tail -n 60 /var/log/trinetra.log 2>/dev/null || echo "(no log)"' 2>/dev/null || true
	echo "--- host: tail of /var/log/trinetra-guard.log ---"
	on host sh -c 'tail -n 60 /var/log/trinetra-guard.log 2>/dev/null || echo "(no log)"' 2>/dev/null || true
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

step() { STEP="$1"; echo "== $1 =="; }
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
# guard_gone: true when no guard process is running. A negation local to
# THIS shell (not `! pgrep ...` run inside a `sh -c` string on the
# container) -- a pgrep pattern wrapped in `sh -c "...pattern..."` would
# match its OWN wrapping sh process, since that process's command line
# contains the pattern text too, and never report "gone".
guard_gone() { ! on host pgrep -f '^/usr/local/bin/trinetra update guard$' >/dev/null 2>&1; }
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
pass "installed from $BUNDLE, floor and running both 0.5.0"

# ---------------------------------------------------------------------------
step "2 check sees 0.5.1; apply upgrades and notifies"
CHECK=$(on host trinetra update check 2>&1) || fail "update check: $CHECK"
grep -q "^update available: 0.5.1 (channel beta)$" <<<"$CHECK" || fail "update check output: $CHECK"
APPLY=$(on host trinetra update apply 2>&1) || fail "update apply: $APPLY"
echo "  $APPLY"
wait_until 90 "guard commits 0.5.1" last_outcome_is committed
running_is 0.5.1 || fail "running after apply: $(status_field .running)"
floor_is 0.5.1 || fail "floor after apply: $(status_field .floor)"
pending_is_null || fail "pending left over after commit: $(status_field .pending)"
# updateLoopInterval is 5 minutes (update_daemon.go) -- this is the harness's
# one genuinely slow wait, by design (see this file's header note).
wait_until 330 "mocktg got the 'updated 0.5.0 -> 0.5.1' message" mocktg_has "updated 0.5.0 → 0.5.1"
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
# The guard's own deadline is 90s from ITS restart (update_guard.go), which
# starts a few seconds after this apply call returns; wait a bit past that
# so a slow poll cycle right at the boundary is not a false failure.
wait_until 120 "guard rolls back 0.5.2" last_outcome_is rolled_back
running_is 0.5.1 || fail "running after rollback: $(status_field .running)"
floor_is 0.5.1 || fail "floor moved after a rolled-back update: $(status_field .floor)"
pending_is_null || fail "pending left over after rollback: $(status_field .pending)"
bad_versions_has 0.5.2 || fail "0.5.2 not recorded in bad_versions: $(on host cat /var/lib/trinetra/update/state.json)"
wait_until 330 "mocktg got the critical rollback alert" mocktg_has "rolled back to 0.5.1"
mocktg_has "0.5.2" || fail "rollback alert does not name 0.5.2"
OUT=$(refused 0.5.2)
grep -qi "failed its health check here before" <<<"$OUT" || fail "re-apply of a known-bad version without --force: $OUT"
pass "0.5.2 rolled back to 0.5.1 within 90s, critical alert sent, bad_versions recorded, re-apply refused without --force"

# ---------------------------------------------------------------------------
step "7 guard killed mid health-check resumes on next daemon start"
OLD_PID=$(on host pgrep -f '^/usr/local/bin/trinetra daemon$') || fail "no running daemon before apply 0.5.6"
APPLY=$(on host trinetra update apply --version 0.5.6 2>&1) || fail "apply 0.5.6: $APPLY"
echo "  $APPLY"
# Wait for evidence the guard has already restarted the daemon (a new PID)
# and is now in its health-poll loop -- more deterministic than a blind
# sleep -- then kill it before a post-restart sample can exist (fast_interval
# defaults to 5s), so it never reaches commitPending.
wait_until 15 "guard restarted the daemon onto 0.5.6" \
	on host sh -c "pgrep -f '^/usr/local/bin/trinetra daemon\$' | grep -vxF '$OLD_PID' >/dev/null"
on host pkill -9 -f '^/usr/local/bin/trinetra update guard$' || fail "no running guard process to kill"
wait_until 10 "guard process gone" guard_gone
pending_is_null && fail "pending was already cleared before the guard could be killed (race: the health check committed before it could be killed)"
echo "  pending stuck after killing the guard: $(status_field .pending)"
RESTART=$(on host /usr/local/libexec/e2e-restart.sh 2>&1) || fail "simulated daemon restart: $RESTART"
wait_until 30 "daemon back up" daemon_up
wait_until 60 "resumed guard resolves the pending update" pending_is_null
last_outcome_is committed || fail "resumed guard did not commit a healthy 0.5.6: $(status_field '.last')"
running_is 0.5.6 || fail "running after resume: $(status_field .running)"
floor_is 0.5.6 || fail "floor after resume: $(status_field .floor)"
pass "next daemon start resumed the killed guard and committed 0.5.6, no pending left"

# ---------------------------------------------------------------------------
step "8 apply --version 0.5.0 refused as downgrade"
OUT=$(refused 0.5.0)
grep -qi "downgrade\|lower than the highest version" <<<"$OUT" || fail "unexpected refusal: $OUT"
running_is 0.5.6 || fail "running changed after a refused downgrade: $(status_field .running)"
floor_is 0.5.6 || fail "floor changed after a refused downgrade: $(status_field .floor)"
pass "refused ($OUT)"

# ---------------------------------------------------------------------------
step "9 config get and dump never print the GitHub token"
GET_ALL=$(on host trinetra config get)
grep -F "e2etoken" <<<"$GET_ALL" >/dev/null && fail "trinetra config get printed the raw token"
grep -q '"github_token": "(set)"' <<<"$GET_ALL" || fail "trinetra config get does not show update.github_token redacted: $GET_ALL"
GET_ONE=$(on host trinetra config get update.github_token)
[ "$GET_ONE" = "(set)" ] || fail "trinetra config get update.github_token = %q, want (set)"
DUMP=$(on host trinetra dump --metric cpu --since 1h 2>&1) || fail "trinetra dump: $DUMP"
grep -F "e2etoken" <<<"$DUMP" >/dev/null && fail "trinetra dump printed the raw token"
pass "config get (full and single-key) and dump all redact/omit the token"

STEP=""
echo "ALL PASS ($(($(date +%s) - T_START))s)"
