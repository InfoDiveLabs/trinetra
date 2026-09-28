#!/usr/bin/env bash
# Multi-container end-to-end test for trinetra fleet mode.
#
# Runs the real trinetra binary and real daemons in separate containers on
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
FALLBACK_AFTER=90    # child fleet.fallback_after -- MUST clear groupWait (fleet_engine.go's
                     # 30s incident-grouping delivery gate: even a lone, ungrouped incident's
                     # first notification waits for it) by a wide margin, or the master's
                     # grouped delivery and the child's own local-fallback timer race, and
                     # the child usually wins (its clock starts the instant it fires; the
                     # master's is gated behind a 5s masterTickInterval poll of groupWait
                     # PLUS actual dispatch PLUS the receipt frame's own round trip back to
                     # the child). Setting this equal to (or too near) groupWait was diagnosed
                     # live against the real daemon two different ways: at 30-60s, Submit's
                     # own hadLeaseBefore/deliveredLocally decision was fine (delivery was
                     # simply never attempted before the child gave up); even at 60s, one busy
                     # run (several other incidents in flight at once, competing for the
                     # keyed dispatcher) still delivered successfully but the receipt lagged
                     # past the fallback deadline, so the child ALSO fired its own local
                     # fallback for the exact same (node, key, fired_at) -- a genuine second
                     # message, not a counting bug in this script. 90s leaves real headroom
                     # above both.
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
  on master sh -c 'cat /var/lib/trinetra/alertlog.jsonl 2>/dev/null' 2>/dev/null || true
  echo "--- master: fleet status / nodes ---"
  on master trinetra fleet status 2>&1 || true
  on master trinetra fleet nodes 2>&1 || true
  echo "--- master: replica ingest.state and raw cpu sizes per node ---"
  on master sh -c 'for d in /var/lib/trinetra/fleet/nodes/*/; do echo "$d: $(cat "$d/ingest.state" 2>/dev/null) cpu.tsd=$(stat -c %s "$d/ts/raw/cpu.tsd" 2>/dev/null)"; done' 2>/dev/null || true
  for s in child1 child2; do
    echo "--- $s: fleet status ---"
    on "$s" trinetra fleet status 2>&1 || true
    on "$s" sh -c 'echo "outbox: $(ls /var/lib/trinetra/outbox 2>/dev/null | tr "\n" " ") cursor=$(cat /var/lib/trinetra/outbox/cursor 2>/dev/null)"; echo "local cpu.tsd=$(stat -c %s /var/lib/trinetra/ts/raw/cpu.tsd 2>/dev/null)"' 2>/dev/null || true
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
  compose exec -d "$1" sh -c 'trinetra daemon >>/var/log/sw.log 2>&1'
  wait_until 30 "$1 daemon control socket" on "$1" trinetra fleet status
}

stop_daemon() {
  on "$1" sh -c 'pkill -f "^trinetra daemon" || true'
  wait_until 30 "$1 daemon to exit" on "$1" sh -c '! pgrep -f "^trinetra daemon"'
}

# Fleet alert lines in the master's alert log for key/kind.
alert_count() { # <svc> <key> <kind>
  on "$1" sh -c "grep -F '\"key\":\"$2\"' /var/lib/trinetra/alertlog.jsonl 2>/dev/null | grep -cF '\"kind\":\"$3\"' || true" | tr -d '\r'
}
alert_seen() { [ "$(alert_count "$@")" -ge 1 ]; }
alert_more_than() { [ "$(alert_count "$1" "$2" "$3")" -gt "$4" ]; }

node_state() { # <name> -> STATE column from the master's fleet nodes
  on master trinetra fleet nodes | awk -v n="$1" '$1==n {print $2}'
}
node_is() { [ "$(node_state "$1")" = "$2" ]; }
link_state() { on "$1" trinetra fleet status | sed -n 's/^link: \([a-z][a-z ]*[a-z]\)[ ,(].*/\1/p'; } # e.g. linked, catching up
link_is() { [ "$(link_state "$1")" = "$2" ]; }
unsent() { on "$1" trinetra fleet status | sed -n 's/^outbox: .* MB, \([0-9]*\) unsent.*/\1/p'; }
unsent_is_zero() { [ "$(unsent "$1")" = "0" ]; }
mocktg_has() { on mocktg curl -s http://localhost:8080/_messages | grep -qF "$1"; }
mocktg_has_answer() { on mocktg curl -s http://localhost:8080/_answers | grep -qF "$1"; }
# msg_count <substring>: how many times substring occurs across every
# sendMessage text mocktg has recorded so far (steps 10-15 assert on the
# DELTA around a specific action, since mocktg accumulates for the whole run).
# grep -o exits 1 on zero matches, which -- under this script's `set -eo
# pipefail` -- would otherwise silently kill the whole run the first time a
# BASE_* count is legitimately 0 (no "FAIL" line, just a bare exit: exactly
# the trap alert_count already dodges with its own inline `|| true`).
# `{ ...; }` groups the grep so `|| true` absorbs only its exit code, not
# wc/tr's.
msg_count() { on mocktg curl -s http://localhost:8080/_messages | { grep -o -F "$1" || true; } | wc -l | tr -d ' \r'; }
chat_count() { on mocktg curl -s http://localhost:8080/_chats | { grep -o -F "\"$1\"" || true; } | wc -l | tr -d ' \r'; }
incident_acked() { on master trinetra fleet incident "$1" | grep -q "^acked by: "; }
# The following compare against a BASE_* snapshot a step takes just before
# triggering the action being waited on (set as a plain global right before
# each wait_until call; bash resolves it at call time, not definition time).
mem_delivered_once() { [ "$(msg_count "child1: mem =")" -gt "$BASE_MEM" ]; }
fallback_delivered_once() { [ "$(msg_count "via local fallback: master unreachable")" -gt "$BASE_FALLBACK" ]; }
mem_routed_once() { [ "$(msg_count "child2: mem =")" -gt "$BASE_C2MEM3" ]; }
rule_state() { on master trinetra fleet rules | awk '$1=="lab-online"{print $2}'; }
rule_is_firing() { [ "$(rule_state)" = "firing" ]; }
rule_is_ok() { [ "$(rule_state)" = "ok" ]; }
child_cfg_is() { [ "$(on "$1" trinetra config get "$2")" = "$3" ]; } # <svc> <key> <want>
managed_applied_no_drift() { on master trinetra fleet managed status | awk -v n="$1" '$1==n && $4=="true" && $5=="-"' | grep -q .; }

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
    cpq "master:/var/lib/trinetra/fleet/nodes/$id/ts/raw/$metric.tsd" "$SCRATCH/replica.tsd"
    cpq "$svc:/var/lib/trinetra/ts/raw/$metric.tsd" "$SCRATCH/child.tsd"
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
on master trinetra telegram set-token TESTTOKEN >/dev/null
on master trinetra config set telegram.chat_id 999 >/dev/null
on master trinetra config set fleet.node_down_after ${DOWN_AFTER}s >/dev/null
start_daemon master
start_daemon solo
INIT=$(on master trinetra fleet init --address master) || fail "fleet init: $INIT"
FPR=$(sed -n 's/^ *CA fingerprint: \(sha256:[^ ]*\).*/\1/p' <<<"$INIT")
[ -n "$FPR" ] || fail "no CA fingerprint in fleet init output: $INIT"
stop_daemon master
start_daemon master
ST=$(on master trinetra fleet status)
grep -qx "role: master" <<<"$ST" || fail "master fleet status: $ST"
grep -qx "CA fingerprint: $FPR" <<<"$ST" || fail "fleet status fingerprint differs from init's ($FPR): $ST"
grep -q "listening: .*:9443" <<<"$ST" || fail "master not listening on 9443: $ST"
pass "role master, CA $FPR"

# ---------------------------------------------------------------------------
step "2 enrol"
TOK=$(on master trinetra fleet token create --uses 2 --tags lab) || fail "token create: $TOK"
CODE=$(grep -o 'swj1_[A-Za-z0-9_=-]*' <<<"$TOK" | head -1)
[ -n "$CODE" ] || fail "no swj1_ code in: $TOK"
for c in child1 child2; do
  J=$(on "$c" trinetra fleet join "$CODE" --name "$c") || fail "$c join: $J"
  id=$(sed -n 's/^Joined fleet master https:\/\/master:9443 as node \([0-9a-f]*\) .*/\1/p' <<<"$J")
  [ -n "$id" ] || fail "$c: no node id in join output: $J"
  eval "ID_$c=$id"
  # Each child gets its own working Telegram bot (steps 10/11/12/14 need real
  # outbound delivery to prove local-fallback/silence/routing rather than
  # just "no telegram configured"): a distinct token keeps its getUpdates
  # poller in its own mocktg queue, never stealing an update injected for
  # the master (step 15) -- see test/docker/mocktg's per-token queues.
  on "$c" trinetra telegram set-token "TESTTOKEN_$c" >/dev/null
  on "$c" trinetra config set telegram.chat_id 999 >/dev/null
  on "$c" trinetra config set fleet.fallback_after ${FALLBACK_AFTER}s >/dev/null
  start_daemon "$c"
done
T_CHILDREN=$(date +%s)
wait_until 60 "child1 online on master" node_is child1 online
wait_until 60 "child2 online on master" node_is child2 online
NODES=$(on master trinetra fleet nodes)
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
SKEW_C1=$(on master trinetra fleet nodes | awk '$1=="child1" {print $3}')
case "$SKEW_C1" in
  0s|[+-][0-9]s|[+-][12][0-9]s|[+-]30s) ;;
  *) fail "child1 skew after the partition is $SKEW_C1 (want within 30s)" ;;
esac
echo "  no clock-skew warning after the partition (child1 skew $SKEW_C1)"
pass "partitioned $(( P_TO - P_FROM ))s, no hole in the replica"

# ---------------------------------------------------------------------------
step "5 master restart"
FIRES_BEFORE=$(on master sh -c "grep -F '\"key\":\"fleet:' /var/lib/trinetra/alertlog.jsonl | grep -cF '\"kind\":\"fire\"' || true" | tr -d '\r')
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
FIRES_AFTER=$(on master sh -c "grep -F '\"key\":\"fleet:' /var/lib/trinetra/alertlog.jsonl | grep -cF '\"kind\":\"fire\"' || true" | tr -d '\r')
[ "$FIRES_AFTER" -eq "$FIRES_BEFORE" ] || fail "fleet alerts fired after master restart ($FIRES_BEFORE -> $FIRES_AFTER): $(on master tail -n 3 /var/lib/trinetra/alertlog.jsonl)"
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
R=$(on master trinetra fleet node revoke child2) || fail "revoke: $R"
grep -q "^Revoked $C2" <<<"$R" || fail "unexpected revoke output: $R"
wait_until 60 "child2 link revoked" link_is child2 revoked
wait_until 30 "child2 local revoked alert" alert_seen child2 "fleet:link:revoked" fire
wait_until 30 "master shows child2 revoked" node_is child2 revoked
REP=/var/lib/trinetra/fleet/nodes/$C2/ts/raw/cpu.tsd
LOC=/var/lib/trinetra/ts/raw/cpu.tsd
S1=$(file_size master "$REP"); L1=$(file_size child2 "$LOC")
sleep $(( 4 * FAST ))  # stability window: the replica must not grow
S2=$(file_size master "$REP"); L2=$(file_size child2 "$LOC")
[ "$S2" -eq "$S1" ] || fail "master replica of child2 kept growing after revoke ($S1 -> $S2 bytes)"
[ "$L2" -gt "$L1" ] || fail "child2 stopped sampling locally after revoke ($L1 -> $L2 bytes)"
pass "child2 link revoked, local alert raised, replica frozen at $S1 bytes while local store grows"

# ---------------------------------------------------------------------------
step "7 leave + remove"
L=$(on child1 trinetra fleet leave --purge) || fail "leave: $L"
grep -q "fleet node revoke $C1" <<<"$L" || fail "leave did not print the master-side command: $L"
stop_daemon child1
start_daemon child1
ST=$(on child1 trinetra fleet status)
grep -qx "role: solo" <<<"$ST" || fail "child1 after leave: $ST"
on child1 sh -c '! test -e /var/lib/trinetra/fleet-child && ! test -e /var/lib/trinetra/outbox' \
  || fail "child1 still has fleet-child/ or outbox/: $(on child1 ls /var/lib/trinetra)"
RM=$(on master trinetra fleet node remove child1) || fail "remove: $RM"
grep -q "^Removed $C1" <<<"$RM" || fail "unexpected remove output: $RM"
T_RM=$(now_in master)
DOWN_AT_RM=$(alert_count master "fleet:node:$C1:down" fire)
on master trinetra fleet nodes | awk '$1=="child1"' | grep -q . && fail "master still lists child1: $(on master trinetra fleet nodes)"
while [ $(( $(now_in master) - T_RM )) -lt $(( DOWN_AFTER + 15 )) ]; do sleep 2; done
[ "$(alert_count master "fleet:node:$C1:down" fire)" -eq "$DOWN_AT_RM" ] || fail "master paged removed child1 as down"
on master trinetra fleet nodes | awk '$1=="child1"' | grep -q . && fail "child1 reappeared in fleet nodes"
# child2 has been revoked (and silent) for longer than node_down_after by now.
[ "$(alert_count master "fleet:node:$C2:down" fire)" -eq 0 ] || fail "master paged revoked child2 as down"
pass "child1 solo with identity purged; removed on master, no down page for it (or revoked child2) after $(( DOWN_AFTER + 15 ))s"

# ---------------------------------------------------------------------------
step "8 solo regression"
on solo sh -c '! test -e /var/lib/trinetra/fleet && ! test -e /var/lib/trinetra/fleet-child && ! test -e /var/lib/trinetra/outbox' \
  || fail "solo has fleet dirs: $(on solo ls /var/lib/trinetra)"
# 9443 = 0x24E3; state 0A = LISTEN.
on solo sh -c '! awk "\$4==\"0A\"" /proc/net/tcp /proc/net/tcp6 | grep -qi ":24E3 "' \
  || fail "something listens on 9443 on solo"
ST=$(on solo trinetra fleet status)
grep -qx "role: solo" <<<"$ST" || fail "solo fleet status: $ST"
wait_until 30 "solo trinetra status" on solo trinetra status
SS=$(on solo trinetra status)
grep -qi "cpu" <<<"$SS" || fail "solo status lacks CPU: $SS"
on solo test -s /var/lib/trinetra/ts/raw/cpu.tsd || fail "solo is not recording samples"
pass "no fleet dirs, no :9443 listener, role solo, status works"

# ---------------------------------------------------------------------------
step "9 security spot checks"
NODES_BEFORE=$(on master trinetra fleet nodes | tail -n +2 | wc -l | tr -d ' ')
CODE_HTTP=$(on solo curl -sk -X POST -o /dev/null -w '%{http_code}' https://master:9443/fleet/v1/ingest)
[ "$CODE_HTTP" = "401" ] || fail "ingest without client cert returned $CODE_HTTP, want 401"
echo "  POST /fleet/v1/ingest without client cert -> 401"
if OUT=$(on solo trinetra fleet join swj1_garbage --name bogus 2>&1); then fail "garbage join code accepted: $OUT"; fi
echo "  garbage code refused: $OUT"
if OUT=$(on solo trinetra fleet join "$CODE" --name reused 2>&1); then fail "spent join code accepted: $OUT"; fi
echo "  spent code refused: $OUT"
ST=$(on solo trinetra fleet status)
grep -qx "role: solo" <<<"$ST" || fail "solo changed role after refused joins: $ST"
NODES_AFTER=$(on master trinetra fleet nodes | tail -n +2 | wc -l | tr -d ' ')
[ "$NODES_AFTER" -eq "$NODES_BEFORE" ] || fail "node count changed $NODES_BEFORE -> $NODES_AFTER"
on master trinetra fleet nodes | awk '$1=="bogus" || $1=="reused"' | grep -q . && fail "refused join registered a node"
pass "401 without cert; garbage and spent codes refused, no node registered"

# ---------------------------------------------------------------------------
# Steps 6-7 revoked child2 and left+removed child1 -- neither is a working,
# online, lab-tagged fleet member any more (child1 is plain solo; child2 is
# still fleet-child but blocked). Steps 10+ need both back, so re-enrol them
# fresh here, exactly as an operator recovering a fleet would: purge
# child2's stale local identity (child1's was already purged in step 7),
# drop its now-orphaned revoked registry entry (fleet node remove allows
# this for a revoked or down node), and join both in with a new token.
step "10 handoff"
on child2 trinetra fleet leave --purge >/dev/null 2>&1 || true
RM2=$(on master trinetra fleet node remove child2) || fail "remove child2 (re-enrol): $RM2"
grep -q "^Removed $ID_child2" <<<"$RM2" || fail "unexpected remove output for child2: $RM2"
stop_daemon child1
stop_daemon child2
TOK2=$(on master trinetra fleet token create --uses 2 --tags lab) || fail "token create (re-enrol): $TOK2"
CODE2=$(grep -o 'swj1_[A-Za-z0-9_=-]*' <<<"$TOK2" | head -1)
[ -n "$CODE2" ] || fail "no swj1_ code in: $TOK2"
for c in child1 child2; do
  J=$(on "$c" trinetra fleet join "$CODE2" --name "$c") || fail "$c re-join: $J"
  id=$(sed -n 's/^Joined fleet master https:\/\/master:9443 as node \([0-9a-f]*\) .*/\1/p' <<<"$J")
  [ -n "$id" ] || fail "$c: no node id in re-join output: $J"
  eval "ID_$c=$id"
  # Belt and suspenders: leave --purge only removes fleet-child/outbox, so
  # these ordinary config keys likely survived from step 2, but set them
  # again in case a future change ever widens the purge.
  on "$c" trinetra telegram set-token "TESTTOKEN_$c" >/dev/null
  on "$c" trinetra config set telegram.chat_id 999 >/dev/null
  on "$c" trinetra config set fleet.fallback_after ${FALLBACK_AFTER}s >/dev/null
  start_daemon "$c"
done
wait_until 60 "child1 online on master (re-enrol)" node_is child1 online
wait_until 60 "child2 online on master (re-enrol)" node_is child2 online
# "online" (ingest/heartbeat-driven) can go true slightly before the
# separate master<->child stream has delivered this node's first lease
# (fleet_engine.go's PushLeaseNow on Hub.OnConnect): an alert fired in that
# window sees hadLeaseBefore==false and the master silently treats it as
# already delivered (no dispatch, no receipt -- see Submit's doc comment),
# so it only ever reaches Telegram via the child's OWN fallback_after
# timer, never "from the master". Waiting past one lease push cycle here
# avoids racing that window before step 10 fires anything.
sleep $(( DOWN_AFTER + 5 ))
echo "  re-enrolled child1=$ID_child1 child2=$ID_child2 (fresh identities, tag lab)"

BASE_MEM=$(msg_count "child1: mem =")
on child1 trinetra config set thresholds.mem_pct 1 >/dev/null
wait_until 240 "master delivered child1's mem alert" mem_delivered_once # generous: this host observed dispatcher/liveness jitter under load, occasionally pushing delivery past 100s
sleep $(( 3 * FAST ))
AFTER_LINKED=$(msg_count "child1: mem =")
[ "$AFTER_LINKED" -eq $(( BASE_MEM + 1 )) ] || fail "expected exactly one child1 mem message while linked (master -> mocktg), got $(( AFTER_LINKED - BASE_MEM ))"
INC1=$(on master trinetra fleet incidents --node "$ID_child1" --state firing | awk '$0 ~ /mem/ {print $1; exit}')
[ -n "$INC1" ] || fail "master never recorded a firing mem incident for child1: $(on master trinetra fleet incidents --node "$ID_child1")"
echo "  child1 mem incident $INC1 delivered once, from the master, prefixed with its node name"
# INC1 is left OPEN (never recovered): step 15 needs a still-firing,
# master-delivered incident to ack (only a master-delivered fire ever
# carries Ack/Silence-1h buttons -- deliverFallback never sets any).

# Fire a SECOND alert right after disconnecting, not after waiting for the
# "retrying" display state: the child's lease handoff only prefixes a
# delivery "via local fallback" when the alert was successfully routed to
# the master FIRST (Route() sees a still-valid lease) and only later times
# out waiting for a receipt (fleet_lease.go's handoff.Tick, gated on
# fleet.fallback_after). A lease is valid up to 90s past its last push
# (leaseInterval 30s + leaseValidFor 90s) and renews every 30s while
# linked, so firing immediately -- rather than after however long
# "retrying" takes to display -- keeps this comfortably inside that window
# instead of risking the lease going stale first (which would skip
# routing/fallback entirely and deliver instantly with no prefix).
#
# swap, not cpu, not a second use of mem: mem is already active (and must
# stay that way for INC1/step 15) and cpu reads noisy/bursty near a low
# threshold (observed flapping fire/recover repeatedly in an earlier run of
# this harness against the real daemon, which blows the "exactly one"
# counts below); swap sits at a small but stable non-zero percentage.
BASE_FALLBACK=$(msg_count "via local fallback: master unreachable")
docker network disconnect "$NET" "$(ctr child1)"
on child1 trinetra config set thresholds.swap_pct 0.05 >/dev/null
wait_until 85 "child1 link retrying (handoff)" link_is child1 retrying
wait_until $(( FALLBACK_AFTER + 60 )) "child1 delivered its swap alert via local fallback" fallback_delivered_once
sleep $(( 3 * FAST ))
AFTER_FALLBACK=$(msg_count "via local fallback: master unreachable")
[ "$AFTER_FALLBACK" -eq $(( BASE_FALLBACK + 1 )) ] || fail "expected exactly one local-fallback message, got $(( AFTER_FALLBACK - BASE_FALLBACK ))"
mocktg_has "via local fallback: master unreachable" || fail "mock telegram never got a local-fallback message"
[ "$(msg_count "child1: swap =")" -eq 0 ] || fail "the master delivered child1's swap alert even though child1 was partitioned"
echo "  child1 delivered its own swap alert locally after ${FALLBACK_AFTER}s of no receipt, prefixed 'via local fallback: master unreachable'"

docker network connect "$NET" "$(ctr child1)"
wait_until 60 "child1 link linked after handoff" link_is child1 linked
wait_until 60 "child1 outbox drained after handoff" unsent_is_zero child1
sleep $(( 3 * FAST ))
[ "$(msg_count "via local fallback: master unreachable")" -eq "$AFTER_FALLBACK" ] || fail "the master re-sent the already-fallback-delivered swap alert after reconnecting"
[ "$(msg_count "child1: swap =")" -eq 0 ] || fail "the master sent a duplicate child1 swap message after reconnecting"
SWAP_INC=$(on master trinetra fleet incidents --node "$ID_child1" --state firing | awk '$0 ~ /swap/ {print $1; exit}')
[ -n "$SWAP_INC" ] || fail "the master never recorded child1's fallback-delivered swap incident after reconnecting"
[ "$SWAP_INC" != "$INC1" ] || fail "the swap fire reused the mem incident $INC1 instead of opening its own"
on master trinetra fleet incident "$SWAP_INC" | grep -q "delivered locally" \
  || fail "master's swap incident for child1 is not marked delivered locally: $(on master trinetra fleet incident "$SWAP_INC")"
pass "linked: one message from the master; partitioned: one local-fallback message after ${FALLBACK_AFTER}s; reconnect: recorded, no duplicate"

# ---------------------------------------------------------------------------
step "11 silence"
# Only mem is touched on child2 throughout steps 11/12 (see step 10's note
# on why cpu is avoided) -- the recover/refire dance below gets a second,
# distinctly-timestamped mem fire without needing a second metric.
SIL=$(on master trinetra fleet silence add --match node=child2 --for 10m --comment "e2e step 11") || fail "silence add: $SIL"
SIL_ID=$(sed -n 's/^Created silence \([^,]*\),.*/\1/p' <<<"$SIL")
[ -n "$SIL_ID" ] || fail "no silence id in: $SIL"
sleep $(( 2 * FAST )) # let the silence reach child2 over the stream before it's partitioned
BASE_C2MEM=$(msg_count "child2: mem =")
on child2 trinetra config set thresholds.mem_pct 1 >/dev/null
sleep $(( 6 * FAST ))
[ "$(msg_count "child2: mem =")" -eq "$BASE_C2MEM" ] || fail "child2's mem alert was delivered by the master despite the active silence"
INC2=$(on master trinetra fleet incidents --node "$ID_child2" | awk '$0 ~ /mem/ {print $1; exit}')
[ -n "$INC2" ] || fail "master never recorded child2's suppressed mem incident: $(on master trinetra fleet incidents --node "$ID_child2")"
on master trinetra fleet explain "$INC2" | grep -q suppressed \
  || fail "fleet explain $INC2 does not show a suppression: $(on master trinetra fleet explain "$INC2")"
echo "  silence $SIL_ID suppressed child2's mem alert while linked (fleet explain $INC2 shows it)"

# Recover it (raise the threshold back above real usage) so "fire again"
# below is a genuinely new, distinctly-timestamped alert, not a no-op re-eval
# of the still-active one -- the daemon only emits fire/recover on a state
# TRANSITION, never every tick.
on child2 trinetra config set thresholds.mem_pct 90 >/dev/null
sleep $(( 4 * FAST ))

# "Fire again" right after partitioning, immediately (same reasoning as step
# 10: stays inside the still-valid lease window, so this is genuinely
# routed-then-fallen-back, the one path that consults the pushed silence --
# see deliverFallback/pushedSilences in fleet_lease.go/fleet_silences.go).
BASE_FALLBACK2=$(msg_count "via local fallback")
docker network disconnect "$NET" "$(ctr child2)"
on child2 trinetra config set thresholds.mem_pct 1 >/dev/null
wait_until 85 "child2 link retrying (silence)" link_is child2 retrying
sleep $(( FALLBACK_AFTER + 20 )) # give the fallback timer a chance to fire (and be suppressed)
[ "$(msg_count "via local fallback")" -eq "$BASE_FALLBACK2" ] || fail "child2 delivered its silenced mem alert locally despite the pushed silence"
[ "$(msg_count "child2: mem =")" -eq "$BASE_C2MEM" ] || fail "child2's mem alert leaked a message while partitioned and silenced"
docker network connect "$NET" "$(ctr child2)"
wait_until 60 "child2 link linked after silence test" link_is child2 linked
wait_until 60 "child2 outbox drained after silence test" unsent_is_zero child2
[ "$(msg_count "via local fallback")" -eq "$BASE_FALLBACK2" ] || fail "the silenced mem alert was delivered after reconnecting"
[ "$(msg_count "child2: mem =")" -eq "$BASE_C2MEM" ] || fail "the master delivered child2's silenced mem alert after reconnecting"
# Expire it now (it was created --for 10m, which would otherwise still be
# active and covering ALL of child2's alerts, no rule filter, straight
# through step 14): step 12 needs a real, undelivered-by-silence mem alert
# on child2 next.
EXP=$(on master trinetra fleet silence expire "$SIL_ID") || fail "silence expire: $EXP"
grep -q "^Expired silence $SIL_ID\.$" <<<"$EXP" || fail "unexpected silence expire output: $EXP"
# Settle: docker network disconnect/connect on this same container twice now
# (step 10 did it to child1, this step to child2) was observed, live against
# this host, to sometimes leave the veth/bridge path re-established but not
# fully stable for another 1-2 minutes -- the master would eventually mark
# the node down again with no docker-level disconnect in sight, well after
# link_is already reported "linked". Give it real time to settle before
# step 12 depends on prompt master<->child2 delivery again.
sleep 45
pass "silence $SIL_ID suppressed child2's mem alert both while linked and while partitioned and falling back"

# ---------------------------------------------------------------------------
step "12 routing"
# child2's mem is still active (threshold 1, never raised back after step
# 11's partition fire) -- recover it so the routed fire below is genuinely
# new. cpu is avoided here too, same flapping reason as step 10.
on child2 trinetra config set thresholds.mem_pct 90 >/dev/null
sleep $(( 4 * FAST ))

on master trinetra channel add tgsecond --type telegram --set chat_id=222 >/dev/null || fail "channel add tgsecond failed"
cat >"$SCRATCH/alerting-12.json" <<'JSON'
{
  "version": 0,
  "routes": [
    {"name": "child2-mem-to-second", "matchers": [{"node": "child2", "rule": "mem"}], "policy": "second"}
  ],
  "policies": [
    {"name": "default", "steps": [{"after": "0s", "channels": ["*"]}], "send_resolved": true},
    {"name": "second", "steps": [{"after": "0s", "channels": ["tgsecond"]}], "send_resolved": true}
  ],
  "default_policy": "default"
}
JSON
cpq "$SCRATCH/alerting-12.json" master:/tmp/alerting-12.json
APPLY=$(on master trinetra fleet alerting apply /tmp/alerting-12.json) || fail "alerting apply: $APPLY"
RT=$(on master trinetra fleet route test --node child2 --rule mem --severity warning) || fail "route test: $RT"
grep -q "^route: child2-mem-to-second$" <<<"$RT" || fail "route test did not select the new route: $RT"
grep -q "^policy: second$" <<<"$RT" || fail "route test did not select policy 'second': $RT"
echo "  fleet route test shows: $(tr '\n' ' ' <<<"$RT")"

BASE_C2MEM3=$(msg_count "child2: mem =")
on child2 trinetra config set thresholds.mem_pct 1 >/dev/null
wait_until 240 "child2's mem alert routed to the second channel" mem_routed_once # generous: this host observed dispatcher/liveness jitter under load, occasionally pushing delivery past 100s
sleep $(( 3 * FAST ))
AFTER_C2MEM3=$(msg_count "child2: mem =")
[ "$AFTER_C2MEM3" -eq $(( BASE_C2MEM3 + 1 )) ] || fail "child2's routed mem alert was delivered $(( AFTER_C2MEM3 - BASE_C2MEM3 )) times, want exactly 1 (only the 'second' policy, not '*', so only chat 222)"
[ "$(chat_count 222)" -eq 1 ] || fail "expected exactly one sendMessage recorded against chat 222, got $(chat_count 222): $(on mocktg curl -s http://localhost:8080/_chats)"
pass "policy 'second' (chat 222) confirmed by fleet route test, and child2's mem alert landed there and only there"

# ---------------------------------------------------------------------------
step "13 managed config"
NN=7
MSET=$(on master trinetra fleet managed set --tag lab thresholds.mem_pct=$NN) || fail "managed set: $MSET"
FRAG_ID=$(sed -n 's/^Saved managed-config fragment \([^ ]*\).*/\1/p' <<<"$MSET")
[ -n "$FRAG_ID" ] || fail "no fragment id in: $MSET"
wait_until 30 "child1 config reflects the pushed thresholds.mem_pct" child_cfg_is child1 thresholds.mem_pct "$NN"
wait_until 30 "child2 config reflects the pushed thresholds.mem_pct" child_cfg_is child2 thresholds.mem_pct "$NN"
if OUT=$(on child1 trinetra config set thresholds.mem_pct 50 2>&1); then
  fail "child1 accepted a local override of a managed key: $OUT"
fi
grep -q "managed by the fleet master" <<<"$OUT" || fail "unexpected refusal wording: $OUT"
grep -qF "$FRAG_ID" <<<"$OUT" || fail "refusal did not name the owning fragment: $OUT"
echo "  child1 config set refused: $OUT"
wait_until 30 "fleet managed status shows child1 applied, no drift" managed_applied_no_drift "$ID_child1"
wait_until 30 "fleet managed status shows child2 applied, no drift" managed_applied_no_drift "$ID_child2"
pass "managed thresholds.mem_pct=$NN pushed to tag lab (fragment $FRAG_ID); child set refused; status shows applied with no drift"

# ---------------------------------------------------------------------------
step "14 aggregate rule"
cat >"$SCRATCH/alerting-14.json" <<'JSON'
{
  "version": 0,
  "routes": [
    {"name": "child2-mem-to-second", "matchers": [{"node": "child2", "rule": "mem"}], "policy": "second"}
  ],
  "policies": [
    {"name": "default", "steps": [{"after": "0s", "channels": ["*"]}], "send_resolved": true},
    {"name": "second", "steps": [{"after": "0s", "channels": ["tgsecond"]}], "send_resolved": true}
  ],
  "default_policy": "default",
  "rules": [
    {"name": "lab-online", "expr": "online(tag:lab) < 2 for 1m", "severity": "critical"}
  ]
}
JSON
cpq "$SCRATCH/alerting-14.json" master:/tmp/alerting-14.json
APPLY=$(on master trinetra fleet alerting apply /tmp/alerting-14.json) || fail "alerting apply (rule): $APPLY"
on master trinetra fleet rules | grep -q "^lab-online" || fail "fleet rules does not list lab-online: $(on master trinetra fleet rules)"

docker network disconnect "$NET" "$(ctr child2)"
wait_until 85 "child2 link retrying (rule)" link_is child2 retrying
wait_until 240 "rule lab-online fires (online(tag:lab) < 2 for 1m)" rule_is_firing
docker network connect "$NET" "$(ctr child2)"
wait_until 60 "child2 link linked after rule test" link_is child2 linked
wait_until 60 "child2 outbox drained after rule test" unsent_is_zero child2
wait_until 90 "rule lab-online recovers" rule_is_ok
pass "online(tag:lab) < 2 for 1m fired while child2 was partitioned, and recovered after reconnect"

# ---------------------------------------------------------------------------
step "15 telegram buttons"
MARKUPS=$(on mocktg curl -s http://localhost:8080/_markups)
grep -qF "ack:$INC1" <<<"$MARKUPS" || fail "no Ack button recorded for incident $INC1: $MARKUPS"
grep -qF "sil1h:$INC1" <<<"$MARKUPS" || fail "no Silence-1h button recorded for incident $INC1: $MARKUPS"
echo "  incident $INC1's fire message carries Ack/Silence-1h buttons"

# Foreign chat (555, never enrolled): answered "not authorized", nothing changes.
on mocktg curl -s "http://localhost:8080/_inject_callback?data=ack%3A$INC1&chat=555&id=cb-foreign&token=TESTTOKEN" >/dev/null
wait_until 20 "foreign callback answered 'not authorized'" mocktg_has_answer "cb-foreign:not authorized"
incident_acked "$INC1" && fail "a callback from a foreign chat acked incident $INC1: $(on master trinetra fleet incident "$INC1")"
echo "  callback from foreign chat 555 answered 'not authorized'; incident $INC1 untouched"

# Enrolled chat (999): ack succeeds.
on mocktg curl -s "http://localhost:8080/_inject_callback?data=ack%3A$INC1&chat=999&id=cb-owner&token=TESTTOKEN" >/dev/null
wait_until 20 "owner callback answered 'acked'" mocktg_has_answer "cb-owner:acked"
wait_until 20 "incident $INC1 shows acked" incident_acked "$INC1"
on master trinetra fleet incident "$INC1" | grep -q "^acked by: telegram$" \
  || fail "incident $INC1 not acked by 'telegram': $(on master trinetra fleet incident "$INC1")"
pass "Ack button from the enrolled chat acked incident $INC1; the same callback from a foreign chat was refused"

# ---------------------------------------------------------------------------
step "16 web smoke"
CTLSHA=$(on master sha256sum /usr/local/bin/trinetra-ctl | awk '{print $1}' | tr -d '\r')
WEBSHA=$(on master sha256sum /usr/local/bin/trinetra-web | awk '{print $1}' | tr -d '\r')
[ -n "$CTLSHA" ] && [ -n "$WEBSHA" ] || fail "could not compute plugin checksums (ctl=$CTLSHA web=$WEBSHA)"
printf '{"ctl":"%s","web":"%s"}' "$CTLSHA" "$WEBSHA" >"$SCRATCH/plugins.json"
cpq "$SCRATCH/plugins.json" master:/var/lib/trinetra/plugins.json
on master trinetra config set web.enabled true >/dev/null
on master trinetra config set web.listen 0.0.0.0:8088 >/dev/null
# web.enabled is only read at daemon startup (shouldStartWeb(cfgAtStart, ...)
# in web_supervisor.go), so the master's daemon needs a restart to pick it
# up and spawn the verified trinetra-web plugin.
stop_daemon master
start_daemon master
wait_until 30 "trinetra-web listening on master:8088" on solo curl -sf -o /dev/null http://master:8088/login
for path in /fleet /fleet/incidents /fleet/alerting /fleet/silences /fleet/managed /fleet/audit "/n/$ID_child1/monitoring" /api/fleet/nodes; do
  CODE=$(on solo curl -s -o /dev/null -w '%{http_code}' "http://master:8088${path}")
  [ "$CODE" = "302" ] || fail "GET $path without a session returned $CODE, want 302 to /login"
done
echo "  every fleet page 302'd an unauthenticated request to /login: /fleet /fleet/incidents /fleet/alerting /fleet/silences /fleet/managed /fleet/audit /n/<id>/monitoring /api/fleet/nodes"
pass "trinetra-web verified and launched via plugins.json; unauthenticated fleet pages redirect to /login"

STEP=""
echo "ALL PASS ($(( $(date +%s) - T_START ))s)"
