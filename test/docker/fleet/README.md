# Fleet mode Docker end-to-end test

`run.sh` proves fleet mode across real, separate hosts: each role runs the
real `trinetra` binary and daemon in its own container on one Docker
network, driven only through the CLI.

```
make fleet-e2e          # or: bash test/docker/fleet/run.sh
```

Needs Docker with Compose v2+. Takes about 15-18 minutes (most of it is
waiting for real sample intervals, the 30 s node-down timer (the product's
minimum), the 90 s `fleet.fallback_after` timer, the master's 30 s
incident-grouping delay, and the aggregate rule's 1-minute `for` window --
also the product's minimum). Waits for something to happen are bounded
polls; the fixed waits left are the windows in which something must NOT
happen (no alert storm, no duplicate message, a frozen replica) and a few
settle periods. Containers, the
network and volumes are removed on exit, pass or fail.

## Topology (`compose.yml`, image from `test/docker/Dockerfile`)

| Service | Role |
|---|---|
| `master` | `fleet init --address master`; Telegram pointed at the mock; `fleet.node_down_after 30s` |
| `child1`, `child2` | join the master with one two-use join code tagged `lab`; each gets its own Telegram bot token and `fleet.fallback_after 30s` (steps 10+) |
| `solo` | plain install, never touches fleet |
| `mocktg` | mock Telegram API; records what the master (and, for local-fallback delivery, a child) sends |

No systemd: `run.sh` starts each daemon with `docker compose exec -d` and sets
`RUNTIME_DIRECTORY=/run/trinetra` so the CLI finds the control socket.
Daemon output goes to `/var/log/sw.log` in each container. No container is
privileged and none mounts the Docker socket.

`test/docker/mocktg` scopes its `getUpdates` queues by bot token (see its own
doc comment): each child's poller only ever sees updates meant for it, so
step 15's callback injection (aimed at the master's token) can never be
stolen by a child's poller.

## Scenarios

1. **Bring-up**: master starts solo, `fleet init`, restart; `fleet status` shows role master and the CA fingerprint `init` printed.
2. **Enrol**: `fleet token create --uses 2 --tags lab`, both children join and start; `fleet nodes` shows self plus both children `online` with tag `lab`.
3. **Replication fidelity**: once the master's replica holds 10 records per series (about 50 s), each child's `ts/raw/{cpu,mem}.tsd` is compared with the master's replica under `fleet/nodes/<id>/ts/raw/` by `tscmp/`: same header, the replica a byte-for-byte prefix of the child's file, trailing by at most 2 records, strictly increasing timestamps.
4. **Partition + store-and-forward**: `docker network disconnect` child1 until the master has logged and sent (to the mock) the `fleet:node:<id>:down` alert and child1's link has gone `retrying` with a growing outbox (about a minute). After reconnecting: recover alert, outbox drains to 0 unsent, fidelity holds, the replica has no gap wider than 2 fast intervals across the partition, and the master logs no clock-skew warning (child1's SKEW stays within 30 s): requests delayed by the partition must not read as a slow clock.
5. **Master restart**: stop the master container for 40 s (longer than `node_down_after`), start it and its daemon again. No fleet alert fires in the 35 s after (past `node_down_after`), both children `online`, both outboxes drain (the shipper may sit in its up-to-60 s retry backoff first), fidelity holds with no hole across the master's outage.
6. **Revoke**: `fleet node revoke child2`; child2's link shows `revoked` and it logs `fleet:link:revoked` locally; the master's replica of child2 stops growing while child2's local store keeps growing.
7. **Leave + remove**: `fleet leave --purge` on child1 and restart: role solo, `fleet-child/` and `outbox/` gone. `fleet node remove child1` on the master: gone from `fleet nodes`, and no down alert for it (nor for revoked child2) after 45 s.
8. **Solo regression**: no `fleet/`, `fleet-child/` or `outbox/` directories, nothing listening on 9443, `fleet status` says solo, `trinetra status` works.
9. **Security spot checks**: `POST /fleet/v1/ingest` from `solo` without a client certificate is refused with 401; a garbage join code and the spent code are both refused and register no node.
10. **Handoff**: child1's `thresholds.mem_pct` is lowered until its own usage breaches it. While linked, mocktg gets exactly one message, prefixed `child1: `, sent by the master; child1 sends nothing itself. This incident is left open (needed, still holding its Telegram buttons, by step 15). child1 is then partitioned and a second, distinct alert (`thresholds.swap_pct` -- `cpu` is avoided: it reads noisy/bursty near a low threshold and was observed flapping fire/recover against the real daemon, which would break the exact-count assertions here) fires immediately (well inside the lease's ~90s remaining validity, so it is genuinely routed-then-falls-back rather than delivered instantly with no lease at all): after `fleet.fallback_after` (60s -- comfortably clear of incident-grouping's own 30s `groupWait` gate on the master's side, or the two race and the child usually wins, see run.sh's comment on `FALLBACK_AFTER`), exactly one message arrives prefixed `via local fallback: master unreachable`, from child1 itself, and the master never sends its own copy. On reconnect the master records the incident as delivered locally (`fleet incident <id>` shows `[delivered locally]`) and sends no second message.
11. **Silence**: `fleet silence add --match node=child2 --for 10m`. Firing child2's `mem` while linked is suppressed (no message; `fleet explain <id>` shows it). The alert is recovered (raising the threshold) and fired again immediately after partitioning child2 (same still-valid-lease timing as step 10, since only the routed-then-falls-back path consults a pushed silence): still no message, either during the partition or after reconnecting, because the pushed silence covers it too. The silence is then expired so it doesn't shadow later steps.
12. **Routing**: a second Telegram channel (`tgsecond`, chat `222`) is added on the master, and `fleet alerting apply` installs a route sending child2's `mem` alerts to a policy naming only that channel. child2's mem is recovered and fired a third time (still avoiding `cpu`, same flapping reason as step 10). `fleet route test --node child2 --rule mem` shows the new route/policy, and the alert is delivered exactly once, with exactly one message recorded against chat `222`.
13. **Managed config**: `fleet managed set --tag lab thresholds.mem_pct=<N>` pushes a fragment covering both children. Each child's `config get thresholds.mem_pct` reflects it, `config set` on that key is refused ("managed by the fleet master (fragment ...)"), and `fleet managed status` shows both applied with no drift.
14. **Aggregate rule**: `fleet alerting apply` adds the rule `online(tag:lab) < 2 for 1m` (the grammar's minimum `for`). Partitioning child2 drops the tagged-online count to 1; `fleet rules` shows it firing once sustained, then `ok` again after child2 reconnects.
15. **Telegram buttons**: the master's fire notification for child1's mem incident (from step 10) carries an inline keyboard (`/_markups`) with `Ack` (`ack:<id>`) and `Silence 1h` (`sil1h:<id>`), both mocktg-recorded. Injecting `ack:<id>` from a foreign chat is answered "not authorized" and acks nothing; injecting it from the enrolled chat acks the incident (`fleet incident <id>` shows `acked by: telegram`) and is answered "acked".
16. **Web smoke**: `trinetra-web` (and `trinetra-ctl`, for a complete manifest) are built into the image; `plugins.json` (`{"ctl":...,"web":...}`) is written to the state dir from their real SHA-256 sums; `web.enabled`/`web.listen 0.0.0.0:8088` are set and the master daemon restarted (the web supervisor only reads `web.enabled` at startup) so the verified `trinetra-web` plugin launches. From `solo`, every fleet page (`/fleet`, `/fleet/incidents`, `/fleet/alerting`, `/fleet/silences`, `/fleet/managed`, `/fleet/audit`, `/n/<child1-id>/monitoring`, `/api/fleet/nodes`) returns 302 to `/login` without a session.

The B6 review regression ("an acked incident that gains a new grouped member
still delivers the member's update") is covered as a Go test instead of a
Docker scenario -- `TestGroupingAckedIncidentStillDeliversNewMemberUpdate` in
`internal/trinetra/fleet_grouping_test.go` -- since reliably forcing incident
grouping (two nodes racing into the same group-by bucket) across real
containers within group_wait/group_interval would need clock control this
harness doesn't have, where the existing engine-fixture tests already do.

Output is one `PASS <step>` / `FAIL <step>: <why>` line per scenario; the
first failure exits non-zero after dumping every daemon's log tail, the
master's alert log, `fleet nodes`/`fleet status` and the mock's messages.
