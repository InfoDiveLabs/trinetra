#!/usr/bin/env bash
# Multi-container end-to-end test for serverwatch fleet mode.
#
# Runs the real serverwatch binary and real daemons in separate containers on
# one Docker network (compose.yml): a master, two children, a solo host that
# never joins, and the mock Telegram API. It drives everything through the
# CLI, the way an operator would, and asserts on what the daemons actually
# did: fleet status/nodes output, alert logs, messages the master sent to the
# mock Telegram, and byte-level comparison of each child's time-series files
# with the master's replica of them (tscmp/).
#
# Prints "PASS <step>" / "FAIL <step>: <why>" and exits non-zero on the first
# failure, after dumping the daemons' logs. Containers, network and volumes
# are always removed on exit. See README.md next to this file.
set -euo pipefail
cd "$(dirname "$0")"

FAST=5               # fast_interval (default) in seconds
DOWN_AFTER=30        # master fleet.node_down_after (the minimum)
NET=swfleet_fleetnet # network name pinned in compose.yml
T_START=$(date +%s)
STEP=""

compose() { docker compose -f compose.yml "$@"; }
on() { local svc=$1; shift; compose exec -T "$svc" "$@"; }
ctr() { compose ps -q "$1"; } # container id of a service

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/swfleet.XXXXXX")

dump_evidence() {
  echo "---------------- evidence ----------------"
  for s in master child1 child2 solo; do
    echo "--- $s: tail of /var/log/sw.log ---"
    on "$s" sh -c 'tail -n 40 /var/log/sw.log 2>/dev/null || echo "(no log)"' 2>/dev/null || echo "(container unavailable)"
  done
  echo "--- master: alertlog.jsonl ---"
  on master sh -c 'cat /var/lib/serverwatch/alertlog.jsonl 2>/dev/null' 2>/dev/null || true
  echo "--- master: fleet status / nodes ---"
  on master serverwatch fleet status 2>&1 || true
  on master serverwatch fleet nodes 2>&1 || true
  echo "--- master: replica ingest.state and raw cpu sizes per node ---"
  on master sh -c 'for d in /var/lib/serverwatch/fleet/nodes/*/; do echo "$d: $(cat "$d/ingest.state" 2>/dev/null) cpu.tsd=$(stat -c %s "$d/ts/raw/cpu.tsd" 2>/dev/null)"; done' 2>/dev/null || true
  for s in child1 child2; do
    echo "--- $s: fleet status ---"
    on "$s" serverwatch fleet status 2>&1 || true
    on "$s" sh -c 'echo "outbox: $(ls /var/lib/serverwatch/outbox 2>/dev/null | tr "\n" " ") cursor=$(cat /var/lib/serverwatch/outbox/cursor 2>/dev/null)"; echo "local cpu.tsd=$(stat -c %s /var/lib/serverwatch/ts/raw/cpu.tsd 2>/dev/null)"' 2>/dev/null || true
  done
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

id_of() { eval "echo \$ID_$1"; }
now_in() { on "$1" date +%s | tr -d '\r'; } # clock as the containers see it

start_daemon() {
  compose exec -d "$1" sh -c 'serverwatch daemon >>/var/log/sw.log 2>&1'
  wait_until 30 "$1 daemon control socket" on "$1" serverwatch fleet status
}

stop_daemon() {
  on "$1" sh -c 'pkill -f "^serverwatch daemon" || true'
  wait_until 30 "$1 daemon to exit" on "$1" sh -c '! pgrep -f "^serverwatch daemon"'
}

# Fleet alert lines in the master's alert log for key/kind.
alert_count() { # <svc> <key> <kind>
  on "$1" sh -c "grep -F '\"key\":\"$2\"' /var/lib/serverwatch/alertlog.jsonl 2>/dev/null | grep -cF '\"kind\":\"$3\"' || true" | tr -d '\r'
}
alert_seen() { [ "$(alert_count "$@")" -ge 1 ]; }
alert_more_than() { [ "$(alert_count "$1" "$2" "$3")" -gt "$4" ]; }

node_state() { # <name> -> STATE column from the master's fleet nodes
  on master serverwatch fleet nodes | awk -v n="$1" '$1==n {print $2}'
}
node_is() { [ "$(node_state "$1")" = "$2" ]; }
link_state() { on "$1" serverwatch fleet status | sed -n 's/^link: \([a-z][a-z ]*[a-z]\)[ ,(].*/\1/p'; } # e.g. linked, catching up
link_is() { [ "$(link_state "$1")" = "$2" ]; }
unsent() { on "$1" serverwatch fleet status | sed -n 's/^outbox: .* MB, \([0-9]*\) unsent.*/\1/p'; }
unsent_is_zero() { [ "$(unsent "$1")" = "0" ]; }
mocktg_has() { on mocktg curl -s http://localhost:8080/_messages | grep -qF "$1"; }

# fidelity <child-svc> <node-id> <metric> [tscmp flags...]: the master's
# replica must be a byte prefix of the child's series, trailing it by at most
# two fast intervals of records. The replica is copied out first so it can
# never be newer than the child copy. A lag-only failure (exit 2) is retried
# a few times because a batch may be in flight at the instant of the copy;
# any integrity failure (exit 1) fails immediately.
fidelity() {
  local svc=$1 id=$2 metric=$3; shift 3
  local out rc=0 try
  for try in 1 2 3 4 5; do
    cpq "master:/var/lib/serverwatch/fleet/nodes/$id/ts/raw/$metric.tsd" "$SCRATCH/replica.tsd"
    cpq "$svc:/var/lib/serverwatch/ts/raw/$metric.tsd" "$SCRATCH/child.tsd"
    cpq "$SCRATCH/replica.tsd" master:/tmp/replica.tsd
    cpq "$SCRATCH/child.tsd" master:/tmp/child.tsd
    rc=0
    out=$(on master tscmp -child /tmp/child.tsd -replica /tmp/replica.tsd -maxlag 2 "$@") || rc=$?
    [ "$rc" -eq 2 ] || break
    sleep 2
  done
  [ "$rc" -eq 0 ] || fail "$svc $metric: $out"
  echo "  $svc $metric: $out"
}

cpq() { compose cp "$@" >/dev/null 2>&1 || fail "docker compose cp $*"; }

file_size() { on "$1" stat -c %s "$2" | tr -d '\r'; }

# ---------------------------------------------------------------------------
step "1 bring-up"
compose down -v -t 1 >/dev/null 2>&1 || true
compose build --quiet
UP=$(compose up -d 2>&1) || fail "compose up: $UP"
wait_until 30 "mock telegram" on mocktg curl -sf http://localhost:8080/_messages
# tscmp is part of the image's /src copy; build it once on the master.
on master sh -c 'cd /src && go build -o /usr/local/bin/tscmp ./test/docker/fleet/tscmp'

# Master: telegram against the mock, fast node-down, then a normal solo start
# followed by `fleet init` and a restart, as an operator would do it.
on master serverwatch telegram set-token TESTTOKEN >/dev/null
on master serverwatch config set telegram.chat_id 999 >/dev/null
on master serverwatch config set fleet.node_down_after ${DOWN_AFTER}s >/dev/null
start_daemon master
start_daemon solo
INIT=$(on master serverwatch fleet init --address master) || fail "fleet init: $INIT"
FPR=$(sed -n 's/^ *CA fingerprint: \(sha256:[^ ]*\).*/\1/p' <<<"$INIT")
[ -n "$FPR" ] || fail "no CA fingerprint in fleet init output: $INIT"
stop_daemon master
start_daemon master
ST=$(on master serverwatch fleet status)
grep -qx "role: master" <<<"$ST" || fail "master fleet status: $ST"
grep -qx "CA fingerprint: $FPR" <<<"$ST" || fail "fleet status fingerprint differs from init's ($FPR): $ST"
grep -q "listening: .*:9443" <<<"$ST" || fail "master not listening on 9443: $ST"
pass "role master, CA $FPR"

# ---------------------------------------------------------------------------
step "2 enrol"
TOK=$(on master serverwatch fleet token create --uses 2 --tags lab) || fail "token create: $TOK"
CODE=$(grep -o 'swj1_[A-Za-z0-9_=-]*' <<<"$TOK" | head -1)
[ -n "$CODE" ] || fail "no swj1_ code in: $TOK"
for c in child1 child2; do
  J=$(on "$c" serverwatch fleet join "$CODE" --name "$c") || fail "$c join: $J"
  id=$(sed -n 's/^Joined fleet master https:\/\/master:9443 as node \([0-9a-f]*\) .*/\1/p' <<<"$J")
  [ -n "$id" ] || fail "$c: no node id in join output: $J"
  eval "ID_$c=$id"
  start_daemon "$c"
done
T_CHILDREN=$(date +%s)
wait_until 60 "child1 online on master" node_is child1 online
wait_until 60 "child2 online on master" node_is child2 online
NODES=$(on master serverwatch fleet nodes)
echo "$NODES"
awk '$1=="master" && $NF=="self"' <<<"$NODES" | grep -q . || fail "master does not list itself as self"
for c in child1 child2; do
  awk -v n="$c" '$1==n' <<<"$NODES" | grep -qw lab || fail "$c lacks tag lab"
done
pass "child1=$ID_child1 child2=$ID_child2 online"

# ---------------------------------------------------------------------------
step "3 replication fidelity"
# Let at least 60 s of samples accumulate (a wait, not an assertion).
while [ $(( $(date +%s) - T_CHILDREN )) -lt 60 ]; do sleep 2; done
for c in child1 child2; do
  for m in cpu mem; do fidelity "$c" "$(id_of "$c")" "$m" -min 10; done
done
pass

# ---------------------------------------------------------------------------
step "4 partition + store-and-forward"
C1=$ID_child1
DOWN_BEFORE=$(alert_count master "fleet:node:$C1:down" fire)
P_FROM=$(now_in master)
docker network disconnect "$NET" "$(ctr child1)"
wait_until 75 "master down alert for child1" alert_more_than master "fleet:node:$C1:down" fire "$DOWN_BEFORE"
echo "  master raised fleet:node:$C1:down after $(( $(now_in master) - P_FROM ))s"
wait_until 85 "child1 link retrying" link_is child1 retrying
U1=$(unsent child1)
while [ $(( $(now_in master) - P_FROM )) -lt 90 ]; do sleep 2; done
U2=$(unsent child1)
[ "$U2" -gt "$U1" ] || fail "child1 outbox did not grow while partitioned ($U1 -> $U2 unsent)"
echo "  child1 link retrying, outbox grew $U1 -> $U2 unsent"
P_TO=$(now_in master)
docker network connect "$NET" "$(ctr child1)"
wait_until 60 "master recover alert for child1" alert_seen master "fleet:node:$C1:down" recover
wait_until 60 "child1 outbox drained" unsent_is_zero child1
wait_until 30 "child1 link linked" link_is child1 linked
mocktg_has "child1 is down" || fail "mock telegram got no child1 down message"
mocktg_has "child1 is back" || fail "mock telegram got no child1 recover message"
on mocktg curl -s http://localhost:8080/_messages | tr ',' '\n' | grep -F child1 | sed 's/^/  telegram: /'
for m in cpu mem; do
  fidelity child1 "$C1" "$m" -from "$P_FROM" -to "$P_TO" -maxgap $(( 2 * FAST ))
done
# Requests that sat in the partition arrive with an old send time; the
# master's skew filter must not read that delay as a slow clock.
SKEW_LOG=$(on master sh -c 'grep -F "clock is" /var/log/sw.log || true' | tr -d '\r')
[ -z "$SKEW_LOG" ] || fail "partition delay reported as clock skew: $SKEW_LOG"
SKEW_C1=$(on master serverwatch fleet nodes | awk '$1=="child1" {print $3}')
case "$SKEW_C1" in
  0s|[+-][0-9]s|[+-][12][0-9]s|[+-]30s) ;;
  *) fail "child1 skew after the partition is $SKEW_C1 (want within 30s)" ;;
esac
echo "  no clock-skew warning after the partition (child1 skew $SKEW_C1)"
pass "partitioned $(( P_TO - P_FROM ))s, no hole in the replica"

# ---------------------------------------------------------------------------
step "5 master restart"
FIRES_BEFORE=$(on master sh -c "grep -F '\"key\":\"fleet:' /var/lib/serverwatch/alertlog.jsonl | grep -cF '\"kind\":\"fire\"' || true" | tr -d '\r')
# A restart whose outage outlasts node_down_after (a bare `compose restart`
# is back in ~1 s), so the master's start-up grace period is what keeps it
# from paging nodes whose last contact is older than node_down_after, and the
# children have to spool through the outage.
OUTAGE=$(( DOWN_AFTER + 10 ))
R_FROM=$(now_in master)
compose stop -t 5 master >/dev/null 2>&1
sleep "$OUTAGE"
compose start master >/dev/null 2>&1
start_daemon master
T_RESTART=$(now_in master)
wait_until 90 "child1 online after master restart" node_is child1 online
wait_until 90 "child2 online after master restart" node_is child2 online
echo "  both children online $(( $(now_in master) - T_RESTART ))s after the master daemon started"
# No node-down storm: nodes that keep shipping must not be paged. Watch past
# node_down_after so a wrongly-expired grace period would show.
while [ $(( $(now_in master) - T_RESTART )) -lt $(( DOWN_AFTER + 5 )) ]; do sleep 2; done
FIRES_AFTER=$(on master sh -c "grep -F '\"key\":\"fleet:' /var/lib/serverwatch/alertlog.jsonl | grep -cF '\"kind\":\"fire\"' || true" | tr -d '\r')
[ "$FIRES_AFTER" -eq "$FIRES_BEFORE" ] || fail "fleet alerts fired after master restart ($FIRES_BEFORE -> $FIRES_AFTER): $(on master tail -n 3 /var/lib/serverwatch/alertlog.jsonl)"
# The live update reconnects at once, but the data shipper may be asleep in
# its retry backoff (up to ~61 s after a 40 s outage), so give the spooled
# backlog time to drain before comparing replicas.
for c in child1 child2; do
  wait_until 90 "$c outbox drained after master restart" unsent_is_zero "$c"
done
echo "  both outboxes drained $(( $(now_in master) - T_RESTART ))s after the master daemon started"
for c in child1 child2; do
  node_is "$c" online || fail "$c is $(node_state "$c") after restart"
  # The children kept sampling while the master was away: no hole there either.
  for m in cpu mem; do fidelity "$c" "$(id_of "$c")" "$m" -from "$R_FROM" -to "$T_RESTART" -maxgap $(( 2 * FAST )); done
done
pass "no fleet alerts, both online, replicas intact across the $(( T_RESTART - R_FROM ))s outage"

# ---------------------------------------------------------------------------
step "6 revoke"
C2=$ID_child2
R=$(on master serverwatch fleet node revoke child2) || fail "revoke: $R"
grep -q "^Revoked $C2" <<<"$R" || fail "unexpected revoke output: $R"
wait_until 60 "child2 link revoked" link_is child2 revoked
wait_until 30 "child2 local revoked alert" alert_seen child2 "fleet:link:revoked" fire
wait_until 30 "master shows child2 revoked" node_is child2 revoked
REP=/var/lib/serverwatch/fleet/nodes/$C2/ts/raw/cpu.tsd
LOC=/var/lib/serverwatch/ts/raw/cpu.tsd
S1=$(file_size master "$REP"); L1=$(file_size child2 "$LOC")
sleep $(( 4 * FAST ))  # stability window: the replica must not grow
S2=$(file_size master "$REP"); L2=$(file_size child2 "$LOC")
[ "$S2" -eq "$S1" ] || fail "master replica of child2 kept growing after revoke ($S1 -> $S2 bytes)"
[ "$L2" -gt "$L1" ] || fail "child2 stopped sampling locally after revoke ($L1 -> $L2 bytes)"
pass "child2 link revoked, local alert raised, replica frozen at $S1 bytes while local store grows"

# ---------------------------------------------------------------------------
step "7 leave + remove"
L=$(on child1 serverwatch fleet leave --purge) || fail "leave: $L"
grep -q "fleet node revoke $C1" <<<"$L" || fail "leave did not print the master-side command: $L"
stop_daemon child1
start_daemon child1
ST=$(on child1 serverwatch fleet status)
grep -qx "role: solo" <<<"$ST" || fail "child1 after leave: $ST"
on child1 sh -c '! test -e /var/lib/serverwatch/fleet-child && ! test -e /var/lib/serverwatch/outbox' \
  || fail "child1 still has fleet-child/ or outbox/: $(on child1 ls /var/lib/serverwatch)"
RM=$(on master serverwatch fleet node remove child1) || fail "remove: $RM"
grep -q "^Removed $C1" <<<"$RM" || fail "unexpected remove output: $RM"
T_RM=$(now_in master)
DOWN_AT_RM=$(alert_count master "fleet:node:$C1:down" fire)
on master serverwatch fleet nodes | awk '$1=="child1"' | grep -q . && fail "master still lists child1: $(on master serverwatch fleet nodes)"
while [ $(( $(now_in master) - T_RM )) -lt $(( DOWN_AFTER + 15 )) ]; do sleep 2; done
[ "$(alert_count master "fleet:node:$C1:down" fire)" -eq "$DOWN_AT_RM" ] || fail "master paged removed child1 as down"
on master serverwatch fleet nodes | awk '$1=="child1"' | grep -q . && fail "child1 reappeared in fleet nodes"
# child2 has been revoked (and silent) for longer than node_down_after by now.
[ "$(alert_count master "fleet:node:$C2:down" fire)" -eq 0 ] || fail "master paged revoked child2 as down"
pass "child1 solo with identity purged; removed on master, no down page for it (or revoked child2) after $(( DOWN_AFTER + 15 ))s"

# ---------------------------------------------------------------------------
step "8 solo regression"
on solo sh -c '! test -e /var/lib/serverwatch/fleet && ! test -e /var/lib/serverwatch/fleet-child && ! test -e /var/lib/serverwatch/outbox' \
  || fail "solo has fleet dirs: $(on solo ls /var/lib/serverwatch)"
# 9443 = 0x24E3; state 0A = LISTEN.
on solo sh -c '! awk "\$4==\"0A\"" /proc/net/tcp /proc/net/tcp6 | grep -qi ":24E3 "' \
  || fail "something listens on 9443 on solo"
ST=$(on solo serverwatch fleet status)
grep -qx "role: solo" <<<"$ST" || fail "solo fleet status: $ST"
wait_until 30 "solo serverwatch status" on solo serverwatch status
SS=$(on solo serverwatch status)
grep -qi "cpu" <<<"$SS" || fail "solo status lacks CPU: $SS"
on solo test -s /var/lib/serverwatch/ts/raw/cpu.tsd || fail "solo is not recording samples"
pass "no fleet dirs, no :9443 listener, role solo, status works"

# ---------------------------------------------------------------------------
step "9 security spot checks"
NODES_BEFORE=$(on master serverwatch fleet nodes | tail -n +2 | wc -l | tr -d ' ')
CODE_HTTP=$(on solo curl -sk -X POST -o /dev/null -w '%{http_code}' https://master:9443/fleet/v1/ingest)
[ "$CODE_HTTP" = "401" ] || fail "ingest without client cert returned $CODE_HTTP, want 401"
echo "  POST /fleet/v1/ingest without client cert -> 401"
if OUT=$(on solo serverwatch fleet join swj1_garbage --name bogus 2>&1); then fail "garbage join code accepted: $OUT"; fi
echo "  garbage code refused: $OUT"
if OUT=$(on solo serverwatch fleet join "$CODE" --name reused 2>&1); then fail "spent join code accepted: $OUT"; fi
echo "  spent code refused: $OUT"
ST=$(on solo serverwatch fleet status)
grep -qx "role: solo" <<<"$ST" || fail "solo changed role after refused joins: $ST"
NODES_AFTER=$(on master serverwatch fleet nodes | tail -n +2 | wc -l | tr -d ' ')
[ "$NODES_AFTER" -eq "$NODES_BEFORE" ] || fail "node count changed $NODES_BEFORE -> $NODES_AFTER"
on master serverwatch fleet nodes | awk '$1=="bogus" || $1=="reused"' | grep -q . && fail "refused join registered a node"
pass "401 without cert; garbage and spent codes refused, no node registered"

STEP=""
echo "ALL PASS ($(( $(date +%s) - T_START ))s)"
