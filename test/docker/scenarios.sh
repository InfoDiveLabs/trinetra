#!/usr/bin/env bash
# End-to-end validation harness for serverwatch.
#
# Brings up the daemon plus a mock Telegram server in Docker, configures the
# daemon entirely via its CLI (no env, no real bot), then drives real scenarios
# and asserts on the messages the daemon actually sent to the mock.
set -euo pipefail
cd "$(dirname "$0")"

VICTIM=sw-victim

compose() { docker compose -f docker-compose.yml "$@"; }

# Dump the messages the daemon has sent to the mock (a JSON array of strings).
fetch_messages() { compose exec -T mocktg sh -c 'curl -s http://localhost:8080/_messages'; }

# Inject an inbound update (as if a user messaged the bot).
inject() { compose exec -T mocktg sh -c "curl -s 'http://localhost:8080/_inject?text=$1'" >/dev/null; }

assert_contains() {
  local haystack="$1" needle="$2"
  if ! grep -qF "$needle" <<<"$haystack"; then
    echo "ASSERT FAIL: expected to find '$needle' in:"; echo "$haystack"; exit 1
  fi
  echo "ok: found '$needle'"
}

# Poll the mock's recorded sends until $needle appears or we time out.
wait_for_message() {
  local needle="$1" timeout="${2:-40}" i=0 msgs=""
  while [ "$i" -lt "$timeout" ]; do
    msgs=$(fetch_messages || true)
    if grep -qF "$needle" <<<"$msgs"; then
      echo "ok: found '$needle' (after ${i}s)"
      return 0
    fi
    sleep 2; i=$((i+2))
  done
  echo "ASSERT FAIL: '$needle' not found within ${timeout}s. recorded messages:"
  echo "$msgs"
  echo "--- daemon logs (sw) ---"; compose exec -T sw sh -c 'cat /var/lib/serverwatch/*.log 2>/dev/null' || true
  echo "--- compose ps ---"; compose ps || true
  exit 1
}

cleanup() {
  echo "== cleanup =="
  docker rm -f "$VICTIM" >/dev/null 2>&1 || true
  compose down -v || true
}
trap cleanup EXIT

# Fresh start.
docker rm -f "$VICTIM" >/dev/null 2>&1 || true
compose down -v >/dev/null 2>&1 || true

echo "== build & up =="
compose up -d --build

# Wait for the mock to answer.
for i in $(seq 1 30); do
  if compose exec -T mocktg sh -c 'curl -sf http://localhost:8080/_messages >/dev/null 2>&1'; then break; fi
  sleep 1
done

echo "== configure daemon via CLI (proves config-by-CLI) =="
compose exec -T sw serverwatch telegram set-token TESTTOKEN
compose exec -T sw serverwatch config set telegram.chat_id 999
compose exec -T sw serverwatch config set sample_interval 5

# ---------------------------------------------------------------------------
# Scenario 1: discovery
# ---------------------------------------------------------------------------
echo "== scenario 1: discovery (doctor) =="
OUT=$(compose exec -T sw serverwatch doctor)
echo "$OUT"
assert_contains "$OUT" "docker: available=true"
assert_contains "$OUT" "targets discovered:"
N=$(sed -n 's/^targets discovered: \([0-9]*\).*/\1/p' <<<"$OUT")
if [ -z "${N:-}" ] || [ "$N" -lt 1 ]; then
  echo "ASSERT FAIL: expected >=1 discovered targets, got '${N:-}'"; exit 1
fi
echo "ok: discovered $N targets"

# ---------------------------------------------------------------------------
# Start the daemon.
# ---------------------------------------------------------------------------
echo "== start daemon =="
compose exec -d sw serverwatch daemon
sleep 8   # let it beat + sample a couple of times

# ---------------------------------------------------------------------------
# Scenario 2: on-demand command (/stats -> reply containing CPU)
# ---------------------------------------------------------------------------
echo "== scenario 2: on-demand /stats =="
inject "/stats"
wait_for_message "CPU" 30

# ---------------------------------------------------------------------------
# Scenario 3: anomaly (CPU spike -> ALERT)
# ---------------------------------------------------------------------------
echo "== scenario 3: CPU anomaly =="
compose exec -d sw stress-ng --cpu 0 --timeout 45s
wait_for_message "ALERT" 45

# ---------------------------------------------------------------------------
# Scenario 5 (docker-down): throwaway container discovered, stopped -> alert
# ---------------------------------------------------------------------------
echo "== scenario 5: docker-down alert =="
docker run -d --name "$VICTIM" busybox sleep 3600 >/dev/null
sleep 8   # let a sample discover it while it is running
docker stop "$VICTIM" >/dev/null
wait_for_message "docker:$VICTIM" 45
# and it should be visible via the /docker command
inject "/docker"
wait_for_message "$VICTIM" 30

# ---------------------------------------------------------------------------
# Scenario 4 (downtime): kill daemon, wait > 2*interval, restart -> boot report
# ---------------------------------------------------------------------------
echo "== scenario 4: downtime / boot report =="
compose exec -T sw pkill -f 'serverwatch daemon' || true
sleep 25   # gap > 2*sample_interval (2*5s)
compose exec -d sw serverwatch daemon
wait_for_message "back online" 30

echo "ALL SCENARIOS PASSED"
