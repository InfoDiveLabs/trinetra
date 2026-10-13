#!/usr/bin/env bash
# End-to-end test of the serverwatch -> trinetra migration (`trinetra install`
# on a host that still has a v0.4.x-style serverwatch install).
#
# A privileged container runs systemd as PID 1 (compose.yml), so both the
# legacy `serverwatch install` and `trinetra install` really drive systemctl.
# The legacy binaries are built from LEGACY_REF, the last serverwatch-named
# commit, exported with `git archive` into a named build context.
#
#   1. legacy install: /usr/local/bin/serverwatch(+ctl,+web), /etc/serverwatch,
#      /var/lib/serverwatch, serverwatch.service, all via `serverwatch install`
#   2. real state: samples, a firing alert, Telegram enrolment through the mock
#      (/start <pin>), fleet master identity (only when the legacy build has
#      `fleet`: v0.4.1 predates it; LEGACY_REF=v0.4.1), config paths inside
#      /etc/serverwatch, and a web users.json (a hand-written stand-in: a
#      passkey cannot be registered from the CLI)
#   3. stop, snapshot, then serverwatch.service is started again on a test
#      hold drop-in (ExecStart=/bin/sleep infinity): the unit is active and
#      enabled, as on a live host, while the data stays frozen
#   4. the new daemon, and a config-writing CLI command, refuse to run next to
#      the unmigrated install
#   5. refusals: legacy paths plus trinetra data at a new path -> install
#      refuses and changes nothing (files, units, service state, binaries)
#   6. migration of the running service: systemctl stop/disable, checksums
#      before/after, config rewrite, marker, units, binaries, compat links
#   7. trinetra.service (systemd, the unit install wrote) reads the migrated
#      state: status, samples, alerts, Telegram enrolment, fleet CA, web users
#   8. re-running install is a plain reinstall
#
# Prints "PASS <step>" / "FAIL <step>: <why>" and exits non-zero on the first
# failure after dumping evidence. Containers are always removed on exit.
set -euo pipefail
cd "$(dirname "$0")"

LEGACY_REF=${LEGACY_REF:-c1b8b92}
IMAGE=trmig-e2e:latest
T_START=$(date +%s)
STEP=""
REPO=$(git rev-parse --show-toplevel)
SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/trmig.XXXXXX")

compose() { docker compose -f compose.yml "$@"; }
on() { compose exec -T host "$@"; }
sh_on() { compose exec -T host sh -c "$1"; }

dump_evidence() {
  echo "---------------- evidence ----------------"
  sh_on 'systemctl --no-pager status serverwatch trinetra 2>&1 | head -40' 2>/dev/null || true
  sh_on 'journalctl --no-pager -n 40 -u serverwatch -u trinetra 2>&1' 2>/dev/null || true
  sh_on 'ls -la /usr/local/bin /usr/bin/serverwatch /usr/bin/trinetra /etc/serverwatch /etc/trinetra /var/lib/serverwatch /var/lib/trinetra /etc/systemd/system/*.service 2>&1' 2>/dev/null || true
  echo "--- mocktg: messages ---"
  compose exec -T mocktg curl -s http://localhost:8080/_messages 2>/dev/null || true
  echo
  echo "------------------------------------------"
}

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ] && [ -n "$STEP" ]; then dump_evidence; fi
  if [ "$rc" -ne 0 ] && [ -n "${KEEP_ON_FAIL:-}" ]; then
    echo "KEEP_ON_FAIL set: leaving the containers up (docker compose -f $(pwd)/compose.yml down -v)"
    rm -rf "$SCRATCH"; exit "$rc"
  fi
  echo "== cleanup =="
  compose down -v -t 2 >/dev/null 2>&1 || true
  rm -rf "$SCRATCH"
  echo "runtime: $(( $(date +%s) - T_START ))s"
  exit "$rc"
}
trap cleanup EXIT

step() { STEP="$1"; echo "== $1 =="; }
pass() { echo "PASS $STEP${1:+: $1}"; }
fail() { echo "FAIL $STEP: $*"; exit 1; }

# wait_until <timeout-seconds> <what> <command...>: poll every second.
wait_until() {
  local timeout=$1 what=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  while ! "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      fail "timed out after ${timeout}s waiting for: $what"
    fi
    sleep 1
  done
}

systemd_up() { case "$(on systemctl is-system-running 2>/dev/null | tr -d '\r')" in running|degraded) return 0 ;; esac; return 1; }
active() { [ "$(on systemctl is-active "$1" 2>/dev/null | tr -d '\r')" = active ]; }
inactive() { ! active "$1"; }
size_of() { sh_on "stat -c %s $1 2>/dev/null || echo 0" | tr -d '\r'; }
size_above() { [ "$(size_of "$1")" -gt "$2" ]; }
mocktg() { compose exec -T mocktg curl -s "http://localhost:8080$1"; }
msg_count() { mocktg /_messages | jq length; }
# stats_reply_since <n>: a message after the first n is a /stats reply (the
# status table, which starts with the overall verdict, not a boot report).
stats_reply_since() { mocktg /_messages | jq -e --argjson n "$1" '.[$n:] | any(contains("CPU") and contains("Mem") and (contains("back online") | not))' >/dev/null; }
config_is() { [ "$(on "$1" config get "$2" 2>/dev/null | tr -d '\r')" = "$3" ]; }
alert_active() { on "$1" alerts list 2>/dev/null | sed -n '/^ACTIVE/,/^HISTORY/p' | grep -q "^  $2 "; }
web_up() { [ "$(sh_on 'curl -s -o /dev/null -w %{http_code} http://127.0.0.1:8088/login')" = 200 ]; }
# enroll_begin <name>: HTTP status of a tokenless passkey enrolment for name.
enroll_begin() {
  sh_on "curl -s -o /dev/null -w %{http_code} -X POST -H 'X-Forwarded-Proto: https' -H 'X-Forwarded-Host: host.example' -H 'Content-Type: application/json' -d '{\"name\":\"$1\"}' http://127.0.0.1:8088/enroll/begin"
}

# snapshot <dir> <out>: one line per entry, path relative to dir: regular
# files as sha256 + mode + owner, directories as "dir" + mode + owner,
# symlinks by target. Sorted, so two snapshots diff cleanly.
snapshot() {
  sh_on "cd '$1' && find . | sort | while read -r f; do
    if [ -L \"\$f\" ]; then echo \"link \$(readlink \"\$f\") \$f\";
    elif [ -d \"\$f\" ]; then echo \"dir \$(stat -c '%a %U:%G' \"\$f\") \$f\";
    else echo \"\$(sha256sum < \"\$f\" | cut -c1-64) \$(stat -c '%a %U:%G' \"\$f\") \$f\"; fi; done > '$2'"
}
sha_of() { sh_on "sha256sum < $1 | cut -c1-64" | tr -d '\r'; }
enabled_state() { on systemctl is-enabled "$1" 2>/dev/null | tr -d '\r'; }

# legacy_intact: the legacy install is exactly as the snapshot taken after the
# daemon stopped, serverwatch.service is still active and enabled, the legacy
# binaries and links are untouched, and no trinetra install exists.
legacy_intact() {
  active serverwatch || fail "serverwatch.service no longer active"
  [ "$(enabled_state serverwatch)" = enabled ] || fail "serverwatch.service no longer enabled: $(enabled_state serverwatch)"
  sh_on 'test -e /etc/systemd/system/multi-user.target.wants/serverwatch.service' || fail "serverwatch wants link gone"
  [ "$(sha_of /usr/local/bin/serverwatch)" = "$SW_BIN_SHA" ] || fail "/usr/local/bin/serverwatch changed"
  [ "$(sh_on 'readlink /usr/bin/serverwatch')" = /usr/local/bin/serverwatch ] || fail "/usr/bin/serverwatch link changed"
  snapshot /var/lib/serverwatch /root/state.check
  snapshot /etc/serverwatch /root/etc.check
  sh_on 'cmp -s /root/state.before /root/state.check' || fail "/var/lib/serverwatch changed: $(sh_on 'diff /root/state.before /root/state.check')"
  sh_on 'cmp -s /root/etc.before /root/etc.check' || fail "/etc/serverwatch changed: $(sh_on 'diff /root/etc.before /root/etc.check')"
  sh_on 'test -f /etc/systemd/system/serverwatch.service && test -f /usr/local/bin/serverwatch && ! test -L /usr/local/bin/serverwatch && test -f /usr/local/bin/serverwatch-ctl && test -f /usr/local/bin/serverwatch-web' \
    || fail "legacy unit or binaries changed"
  sh_on '! test -e /etc/systemd/system/trinetra.service && ! test -e /usr/local/bin/trinetra && ! test -e /usr/bin/trinetra' \
    || fail "trinetra unit or binary appeared"
}

# ---------------------------------------------------------------------------
step "1 build + legacy install"
compose down -v -t 1 >/dev/null 2>&1 || true
mkdir -p "$SCRATCH/legacy"
git -C "$REPO" archive "$LEGACY_REF" | tar -x -C "$SCRATCH/legacy"
grep -qx 'module serverwatch' "$SCRATCH/legacy/go.mod" || fail "$LEGACY_REF is not a serverwatch-named tree"
docker build -q -f "$REPO/test/docker/migration/Dockerfile" \
  --build-context legacysrc="$SCRATCH/legacy" -t "$IMAGE" "$REPO" >/dev/null
UP=$(compose up -d 2>&1) || fail "compose up: $UP"
wait_until 60 "systemd in the host container" systemd_up
wait_until 30 "mock telegram" mocktg /_messages
OUT=$(on /opt/legacy/serverwatch install 2>&1) || fail "serverwatch install: $OUT"
echo "  $OUT"
wait_until 30 "serverwatch.service active" active serverwatch
sh_on 'test -f /etc/serverwatch/config.json && test -d /var/lib/serverwatch && test -f /etc/systemd/system/serverwatch.service && test -x /usr/local/bin/serverwatch && test -x /usr/local/bin/serverwatch-ctl && test -x /usr/local/bin/serverwatch-web && test "$(readlink /usr/bin/serverwatch)" = /usr/local/bin/serverwatch' \
  || fail "legacy install layout incomplete"
# Fleet mode came after v0.4.1: only exercise it when the legacy build has it.
HAS_FLEET=0
LEGACY_HELP=$(on /opt/legacy/serverwatch help 2>&1 || true)
if grep -q 'serverwatch fleet ' <<<"$LEGACY_HELP"; then HAS_FLEET=1; fi
pass "serverwatch ($LEGACY_REF) installed and running under systemd (fleet: $([ "$HAS_FLEET" = 1 ] && echo yes || echo 'no, skipping fleet checks'))"

# ---------------------------------------------------------------------------
step "2 legacy state"
on serverwatch config set sample_interval 5 >/dev/null
on serverwatch config set thresholds.mem_pct 1 >/dev/null   # a real, firing alert
TOKOUT=$(on serverwatch telegram set-token TESTTOKEN) || fail "set-token: $TOKOUT"
PIN=$(sed -n 's/^ *\/start \([0-9][0-9]*\).*/\1/p' <<<"$TOKOUT")
[ -n "$PIN" ] || fail "no enrolment pin in: $TOKOUT"
mocktg "/_inject?text=/start%20$PIN" >/dev/null
wait_until 30 "telegram enrolment (chat_id 999)" config_is serverwatch telegram.chat_id 999
FPR=""
if [ "$HAS_FLEET" = 1 ]; then
  INIT=$(on serverwatch fleet init --address host) || fail "fleet init: $INIT"
  FPR=$(sed -n 's/^ *CA fingerprint: \(sha256:[^ ]*\).*/\1/p' <<<"$INIT")
  [ -n "$FPR" ] || fail "no CA fingerprint in: $INIT"
fi
sh_on 'mkdir -p /etc/serverwatch/tls && echo cert > /etc/serverwatch/tls/cert.pem && echo key > /etc/serverwatch/tls/key.pem && chmod 600 /etc/serverwatch/tls/key.pem'
on serverwatch config set web.tls_cert /etc/serverwatch/tls/cert.pem >/dev/null
on serverwatch config set web.tls_key /etc/serverwatch/tls/key.pem >/dev/null
on serverwatch config set web.enabled true >/dev/null
# Stand-in for a passkey-enrolled web admin (users.json as the web plugin
# writes it); registering a real passkey needs a browser authenticator.
sh_on 'umask 077; printf "[{\"id\":\"dXNlci1hbGljZQ\",\"name\":\"alice\",\"role\":\"admin\",\"created\":1758000000,\"credentials\":[{\"id\":\"Y3JlZC0x\",\"publicKey\":\"cHVibGljLWtleQ==\",\"signCount\":3,\"transports\":[\"internal\"]}]}]\n" > /var/lib/serverwatch/users.json'
on systemctl restart serverwatch
if [ "$HAS_FLEET" = 1 ]; then
  wait_until 30 "serverwatch fleet master" sh_on 'serverwatch fleet status | grep -qx "role: master"'
fi
wait_until 30 "serverwatch-web up" web_up
[ "$(enroll_begin alice)" = 409 ] || fail "legacy web: existing user alice not seen"
[ "$(enroll_begin bob)" = 403 ] || fail "legacy web: tokenless enrolment not closed"
wait_until 60 "mem alert active" alert_active serverwatch mem
S0=$(size_of /var/lib/serverwatch/ts/raw/cpu.tsd)
wait_until 30 "cpu samples growing" size_above /var/lib/serverwatch/ts/raw/cpu.tsd "$S0"
pass "samples, mem alert, telegram enrolled (pin $PIN), fleet master ${FPR:-(no fleet in $LEGACY_REF)}, web user alice"

# ---------------------------------------------------------------------------
step "3 stop legacy + snapshot, then run the unit on a hold"
on systemctl stop serverwatch
wait_until 30 "serverwatch.service inactive" inactive serverwatch
SW_BIN_SHA=$(sha_of /usr/local/bin/serverwatch)
[ "$SW_BIN_SHA" = "$(sha_of /opt/legacy/serverwatch)" ] || fail "/usr/local/bin/serverwatch is not the legacy build"
sh_on 'cp /etc/serverwatch/config.json /root/config.before.json'
snapshot /var/lib/serverwatch /root/state.before
snapshot /etc/serverwatch /root/etc.before
CPU_BEFORE=$(size_of /var/lib/serverwatch/ts/raw/cpu.tsd)
ALERTLOG_BEFORE=$(sh_on 'wc -l < /var/lib/serverwatch/alertlog.jsonl' | tr -d '\r ')
ALERTLOG_SHA=$(sh_on 'sha256sum < /var/lib/serverwatch/alertlog.jsonl' | tr -d '\r')
N_STATE=$(sh_on 'grep -vc "^dir " /root/state.before' | tr -d '\r '); N_ETC=$(sh_on 'grep -vc "^dir " /root/etc.before' | tr -d '\r ')
[ "$N_STATE" -gt 0 ] && [ "$N_ETC" -gt 0 ] || fail "empty snapshot: $N_STATE state files, $N_ETC config files"
if [ "$HAS_FLEET" = 1 ]; then
  sh_on 'grep -qx "dir 700 root:root ./fleet/pki" /root/state.before' || fail "legacy fleet/pki is not 0700: $(sh_on 'grep pki /root/state.before')"
fi
# The unit comes back up on a no-op ExecStart: active and enabled like a live
# install, but nothing writes the data the snapshot just recorded.
sh_on 'mkdir -p /etc/systemd/system/serverwatch.service.d && printf "[Service]\nExecStart=\nExecStart=/bin/sleep infinity\n" > /etc/systemd/system/serverwatch.service.d/hold.conf && systemctl daemon-reload && systemctl start serverwatch'
wait_until 30 "serverwatch.service active (hold)" active serverwatch
[ "$(enabled_state serverwatch)" = enabled ] || fail "serverwatch.service not enabled: $(enabled_state serverwatch)"
pass "$N_STATE state files, $N_ETC config files, cpu.tsd $CPU_BEFORE bytes, $ALERTLOG_BEFORE alert log lines; serverwatch.service active+enabled on the hold"

# ---------------------------------------------------------------------------
step "4 new daemon refuses the unmigrated install"
if OUT=$(on timeout 30 /opt/trinetra/trinetra daemon 2>&1); then fail "trinetra daemon started next to the serverwatch install: $OUT"; fi
grep -qF 'found a serverwatch install at' <<<"$OUT" || fail "unexpected daemon refusal: $OUT"
grep -qF 'sudo trinetra install' <<<"$OUT" || fail "refusal does not point at trinetra install: $OUT"
sh_on '! test -e /var/lib/trinetra && ! test -e /etc/trinetra' || fail "daemon created trinetra dirs"
legacy_intact
echo "  ok: $OUT"
# A config-writing CLI command must not create /etc/trinetra either (install
# would then refuse to merge); it points at install instead.
if CLI=$(on /opt/trinetra/trinetra config set server.name other 2>&1); then fail "trinetra config set ran next to the serverwatch install: $CLI"; fi
grep -qF 'run `sudo trinetra install` first' <<<"$CLI" || fail "unexpected config set refusal: $CLI"
sh_on '! test -e /var/lib/trinetra && ! test -e /etc/trinetra' || fail "config set created trinetra dirs"
legacy_intact
echo "  ok: $CLI"
pass

# ---------------------------------------------------------------------------
step "5 refusal: legacy + trinetra data"
for newp in /var/lib/trinetra/keep.txt /etc/trinetra/config.json; do
  newd=$(dirname "$newp")
  case "$newd" in /var/lib/trinetra) other=/etc/trinetra ;; *) other=/var/lib/trinetra ;; esac
  sh_on "mkdir -p $newd && echo '{\"server\":{\"name\":\"other\"}}' > $newp"
  if OUT=$(on /opt/trinetra/trinetra install 2>&1); then fail "install with $newp present succeeded: $OUT"; fi
  grep -qF 'Nothing was changed' <<<"$OUT" || fail "refusal without 'Nothing was changed': $OUT"
  grep -qF 'refusing to merge' <<<"$OUT" || fail "unexpected refusal: $OUT"
  [ "$(sh_on "cat $newp")" = '{"server":{"name":"other"}}' ] || fail "$newp changed"
  [ "$(sh_on "cd $newd && find . -mindepth 1")" = "./$(basename "$newp")" ] || fail "$newd holds more than the planted file: $(sh_on "ls -la $newd")"
  sh_on "! test -e $other" || fail "$other was created by the refused install"
  legacy_intact
  sh_on "rm -rf $newd"
  echo "  $newp: refused, nothing changed"
done
pass

# ---------------------------------------------------------------------------
step "6 trinetra install migrates the running serverwatch.service"
active serverwatch || fail "serverwatch.service not active before install"
[ "$(enabled_state serverwatch)" = enabled ] || fail "serverwatch.service not enabled before install"
# Hold trinetra.service on a no-op ExecStart while the post-migration
# checksums are taken, so a running daemon cannot touch the files between
# install and the snapshot. Removed in step 7 before the real start.
sh_on 'mkdir -p /etc/systemd/system/trinetra.service.d && printf "[Service]\nExecStart=\nExecStart=/bin/sleep infinity\n" > /etc/systemd/system/trinetra.service.d/hold.conf'
OUT=$(on /opt/trinetra/trinetra install 2>&1) || fail "trinetra install: $OUT"
sed 's/^/  | /' <<<"$OUT"
grep -qF 'found a serverwatch install; migrating it to trinetra' <<<"$OUT" || fail "install did not migrate"
# The held unit never opens the control socket: install says so rather
# than claiming "started" (#162).
grep -qF 'the daemon is still starting' <<<"$OUT" || fail "install claimed a held daemon started: $OUT"
# dirs
sh_on '! test -e /etc/serverwatch && ! test -e /var/lib/serverwatch' || fail "old dirs still present"
sh_on 'test -d /etc/trinetra && test -d /var/lib/trinetra' || fail "new dirs missing"
echo "  ok: /etc/serverwatch, /var/lib/serverwatch gone; /etc/trinetra, /var/lib/trinetra present"
# byte-identical files (config.json is rewritten, plugins.json re-recorded by
# install; ./update and everything under it -- e.g. ./update/apply.lock --
# is legitimately created by install's preflight apply-lock take
# (installPreflight/takeApplyLock in internal/trinetra/systemd.go and
# update_apply.go) and was never present before migration, so it can't be
# byte-identical across the snapshots)
snapshot /var/lib/trinetra /root/state.after
snapshot /etc/trinetra /root/etc.after
sh_on 'grep -v " ./plugins.json$" /root/state.before > /root/sb; grep -v -e " ./plugins.json$" -e " ./migrated-from-serverwatch$" -e " ./update$" -e " ./update/" /root/state.after > /root/sa; cmp -s /root/sb /root/sa' \
  || fail "state files differ: $(sh_on 'diff /root/sb /root/sa')"
sh_on 'grep -v " ./config.json$" /root/etc.before > /root/eb; grep -v " ./config.json$" /root/etc.after > /root/ea; cmp -s /root/eb /root/ea' \
  || fail "config dir files differ: $(sh_on 'diff /root/eb /root/ea')"
if [ "$HAS_FLEET" = 1 ]; then
  sh_on 'grep -qx "dir 700 root:root ./fleet/pki" /root/state.after' || fail "fleet/pki is not 0700 after migration"
fi
N_DIRS=$(sh_on 'cat /root/sa /root/ea | grep -c "^dir "' | tr -d '\r ')
echo "  ok: $(( N_STATE - 1 )) state + $(( N_ETC - 1 )) config files byte-identical (sha256, mode, owner); $N_DIRS dirs keep mode+owner$([ "$HAS_FLEET" = 1 ] && echo ' (fleet/pki 0700)')"
# config rewrite: equal to the old config with the old dir prefixes rewritten
sh_on 'jq -S "walk(if type == \"string\" then sub(\"^/etc/serverwatch/\"; \"/etc/trinetra/\") | sub(\"^/var/lib/serverwatch/\"; \"/var/lib/trinetra/\") else . end)" /root/config.before.json > /root/cfg.want && jq -S . /etc/trinetra/config.json > /root/cfg.got && cmp -s /root/cfg.want /root/cfg.got' \
  || fail "config not rewritten as expected: $(sh_on 'diff /root/cfg.want /root/cfg.got')"
[ "$(sh_on 'jq -r .web.tls_cert /etc/trinetra/config.json')" = /etc/trinetra/tls/cert.pem ] || fail "web.tls_cert not rewritten"
[ "$(sh_on 'jq -r .web.tls_key /etc/trinetra/config.json')" = /etc/trinetra/tls/key.pem ] || fail "web.tls_key not rewritten"
sh_on '! grep -q serverwatch /etc/trinetra/config.json' || fail "config still mentions serverwatch"
echo "  ok: config equal to the old one with paths rewritten (web.tls_cert/tls_key -> /etc/trinetra/tls/)"
# marker
sh_on 'grep -Eq "^[0-9]{4}-[0-9]{2}-[0-9]{2}T" /var/lib/trinetra/migrated-from-serverwatch' || fail "migrated-from-serverwatch marker missing"
sh_on '! test -e /var/lib/trinetra/.migrating-from-serverwatch && ! test -e /etc/trinetra/.migrating-from-serverwatch' || fail "in-progress marker left behind"
echo "  ok: marker $(sh_on 'cat /var/lib/trinetra/migrated-from-serverwatch' | tr -d '\r')"
# units
sh_on '! test -e /etc/systemd/system/serverwatch.service' || fail "old unit still on disk"
[ -z "$(sh_on 'systemctl list-unit-files --no-legend serverwatch.service')" ] || fail "systemd still knows serverwatch.service"
inactive serverwatch || fail "serverwatch.service still active after install"
[ "$(on systemctl show -p LoadState --value serverwatch | tr -d '\r')" = not-found ] || fail "serverwatch.service still loaded: $(on systemctl show -p LoadState -p ActiveState serverwatch)"
[ "$(enabled_state serverwatch)" != enabled ] || fail "serverwatch.service still enabled"
sh_on '! test -e /etc/systemd/system/multi-user.target.wants/serverwatch.service' || fail "serverwatch wants link left behind"
[ "$(sh_on 'readlink /etc/systemd/system/multi-user.target.wants/trinetra.service')" = /etc/systemd/system/trinetra.service ] || fail "trinetra wants link missing"
# The product leaves an old drop-in dir in place (it may hold local
# overrides) and tells the operator to carry them over by hand.
sh_on 'test -f /etc/systemd/system/serverwatch.service.d/hold.conf' || fail "old drop-in dir was not left in place"
grep -qF '/etc/systemd/system/serverwatch.service.d holds local overrides for the old unit; it was left in place' <<<"$OUT" || fail "no note about the old drop-in dir"
echo "  ok: running serverwatch.service stopped, disabled (no wants link), unloaded; old drop-in dir left in place with a note"
sh_on 'grep -qx "ExecStart=/usr/local/bin/trinetra daemon" /etc/systemd/system/trinetra.service && grep -qx "RuntimeDirectory=trinetra" /etc/systemd/system/trinetra.service' \
  || fail "trinetra.service not written as expected: $(on cat /etc/systemd/system/trinetra.service)"
[ "$(on systemctl is-enabled trinetra | tr -d '\r')" = enabled ] || fail "trinetra.service not enabled"
active trinetra || fail "trinetra.service not started by install"
echo "  ok: trinetra.service written, enabled and started by install (running the test hold ExecStart until step 7)"
# binaries + compat link
sh_on 'test -x /usr/local/bin/trinetra && test -x /usr/local/bin/trinetra-ctl && test -x /usr/local/bin/trinetra-web && ! test -e /usr/local/bin/serverwatch-ctl && ! test -e /usr/local/bin/serverwatch-web' \
  || fail "plugin binaries not swapped"
[ "$(sh_on 'readlink /usr/local/bin/serverwatch')" = /usr/local/bin/trinetra ] || fail "/usr/local/bin/serverwatch is not a compat link to trinetra"
[ "$(sh_on 'readlink /usr/bin/serverwatch')" = /usr/local/bin/serverwatch ] || fail "/usr/bin/serverwatch is not a link to /usr/local/bin/serverwatch"
[ "$(sh_on 'readlink /usr/bin/trinetra')" = /usr/local/bin/trinetra ] || fail "/usr/bin/trinetra link missing"
DEP2=$(sh_on '/usr/bin/serverwatch config get server.name 2>&1 >/dev/null')
[ "$DEP2" = "serverwatch is now trinetra; this name will be removed in the next release" ] || fail "no deprecation notice via /usr/bin/serverwatch: $DEP2"
DEP=$(sh_on 'serverwatch config get server.name 2>&1 >/dev/null')
[ "$DEP" = "serverwatch is now trinetra; this name will be removed in the next release" ] || fail "no deprecation notice via the compat name: $DEP"
echo "  ok: /usr/bin/serverwatch -> /usr/local/bin/serverwatch -> /usr/local/bin/trinetra; invoking it prints: $DEP"
for pl in ctl web; do
  got=$(sh_on "jq -r .$pl /var/lib/trinetra/plugins.json" | sed 's/^sha256://')
  [ "$got" = "$(sha_of /usr/local/bin/trinetra-$pl)" ] || fail "plugins.json $pl=$got is not trinetra-$pl: $(on cat /var/lib/trinetra/plugins.json)"
  [ "$got" != "$(sha_of /opt/legacy/serverwatch-$pl)" ] || fail "plugins.json $pl still records serverwatch-$pl"
done
echo "  ok: plugins.json records trinetra-ctl and trinetra-web (not serverwatch-*)"
pass

# ---------------------------------------------------------------------------
step "7 trinetra daemon reads the migrated state"
sh_on 'rm -r /etc/systemd/system/trinetra.service.d && systemctl daemon-reload && systemctl restart trinetra'
wait_until 30 "trinetra.service active" active trinetra
[ "$(on systemctl show -p ExecStart --value trinetra | sed -n 's/.*argv\[\]=\([^;]*\) ;.*/\1/p' | tr -d '\r' | sed 's/ *$//')" = "/usr/local/bin/trinetra daemon" ] \
  || fail "trinetra.service ExecStart: $(on systemctl show -p ExecStart trinetra)"
wait_until 30 "trinetra status" on trinetra status
ST=$(on trinetra status)
grep -q '"cpu"' <<<"$ST" || fail "status lacks cpu: $ST"
echo "  ok: trinetra status works under systemd (unit's own ExecStart)"
wait_until 30 "samples continue in the migrated series" size_above /var/lib/trinetra/ts/raw/cpu.tsd "$CPU_BEFORE"
echo "  ok: cpu.tsd grew $CPU_BEFORE -> $(size_of /var/lib/trinetra/ts/raw/cpu.tsd) bytes (appended to the migrated file)"
config_is trinetra telegram.chat_id 999 || fail "telegram.chat_id lost"
# No boot report is expected here: the legacy daemon stopped cleanly in step
# 3 (clean_stop marker), which suppresses "back online". So the enrolment is
# proven by a /stats reply: taken after the daemon is up, the reply must be
# a new message carrying the status table and not a boot report.
M0=$(msg_count)
mocktg /_inject?text=/stats >/dev/null
wait_until 30 "reply to /stats on the enrolled chat" stats_reply_since "$M0"
echo "  ok: enrolment kept (chat_id 999); /stats answered by trinetra ($M0 -> $(msg_count) messages)"
alert_active trinetra mem || fail "mem alert not active after migration: $(on trinetra alerts list)"
[ "$(sh_on "head -n $ALERTLOG_BEFORE /var/lib/trinetra/alertlog.jsonl | sha256sum" | tr -d '\r')" = "$ALERTLOG_SHA" ] \
  || fail "alert log does not start with the legacy alert log"
HIST=$(on trinetra alerts list --since 24h)
grep -q 'fire   mem ' <<<"$HIST" || fail "legacy mem fire missing from alert history: $HIST"
echo "  ok: legacy mem alert still active, its fire event in the history"
if [ "$HAS_FLEET" = 1 ]; then
  ST=$(on trinetra fleet status)
  grep -qx "role: master" <<<"$ST" || fail "fleet role lost: $ST"
  grep -qx "CA fingerprint: $FPR" <<<"$ST" || fail "fleet CA changed (want $FPR): $ST"
  echo "  ok: fleet master, same CA $FPR"
else
  echo "  skip: fleet checks ($LEGACY_REF has no fleet mode)"
fi
wait_until 30 "trinetra-web up" web_up
[ "$(enroll_begin alice)" = 409 ] || fail "trinetra-web does not see migrated user alice"
[ "$(enroll_begin bob)" = 403 ] || fail "trinetra-web opened tokenless enrolment (users.json not read)"
echo "  ok: trinetra-web sees web user alice (409 on re-enrol, tokenless enrolment closed)"
pass

# ---------------------------------------------------------------------------
step "8 re-run install is a normal install"
OUT=$(on /opt/trinetra/trinetra install 2>&1) || fail "second install: $OUT"
grep -qF 'migrating' <<<"$OUT" && fail "second install tried to migrate again: $OUT"
grep -qF 'installed and started' <<<"$OUT" || fail "install did not wait for the daemon: $OUT"
# #162: the operator's next command, run straight after install with no
# wait, must reach the daemon over the control socket.
CLI=$(on trinetra cli status 2>&1) || fail "trinetra cli right after install: $CLI"
echo "  ok: trinetra cli status answered straight after install"
wait_until 30 "trinetra.service active" active trinetra
wait_until 30 "trinetra status after reinstall" on trinetra status
sh_on '! test -e /etc/serverwatch && ! test -e /var/lib/serverwatch' || fail "old dirs reappeared"
config_is trinetra telegram.chat_id 999 || fail "second install lost the enrolment"
pass "$(head -1 <<<"$OUT")"

STEP=""
echo "ALL PASS ($(( $(date +%s) - T_START ))s)"
