# Fleet mode Docker end-to-end test

`run.sh` proves fleet mode across real, separate hosts: each role runs the
real `trinetra` binary and daemon in its own container on one Docker
network, driven only through the CLI.

```
make fleet-e2e          # or: bash test/docker/fleet/run.sh
```

Needs Docker with Compose v2+. Takes about 6 minutes (most of it is waiting
for real sample intervals and the 30 s node-down timer). Containers, the
network and volumes are removed on exit, pass or fail.

## Topology (`compose.yml`, image from `test/docker/Dockerfile`)

| Service | Role |
|---|---|
| `master` | `fleet init --address master`; Telegram pointed at the mock; `fleet.node_down_after 30s` |
| `child1`, `child2` | join the master with one two-use join code tagged `lab` |
| `solo` | plain install, never touches fleet |
| `mocktg` | mock Telegram API; records what the master sends |

No systemd: `run.sh` starts each daemon with `docker compose exec -d` and sets
`RUNTIME_DIRECTORY=/run/trinetra` so the CLI finds the control socket.
Daemon output goes to `/var/log/sw.log` in each container. No container is
privileged and none mounts the Docker socket.

## Scenarios

1. **Bring-up**: master starts solo, `fleet init`, restart; `fleet status` shows role master and the CA fingerprint `init` printed.
2. **Enrol**: `fleet token create --uses 2 --tags lab`, both children join and start; `fleet nodes` shows self plus both children `online` with tag `lab`.
3. **Replication fidelity**: after 60 s, each child's `ts/raw/{cpu,mem}.tsd` is compared with the master's replica under `fleet/nodes/<id>/ts/raw/` by `tscmp/`: same header, the replica a byte-for-byte prefix of the child's file, trailing by at most 2 records, strictly increasing timestamps.
4. **Partition + store-and-forward**: `docker network disconnect` child1 for 90 s. Master logs and sends (to the mock) the `fleet:node:<id>:down` alert, child1's link goes `retrying` with a growing outbox. After reconnecting: recover alert, outbox drains to 0 unsent, fidelity holds, the replica has no gap wider than 2 fast intervals across the partition, and the master logs no clock-skew warning (child1's SKEW stays within 30 s): requests delayed by the partition must not read as a slow clock.
5. **Master restart**: stop the master container for 40 s (longer than `node_down_after`), start it and its daemon again. No fleet alert fires in the 35 s after (past `node_down_after`), both children `online`, both outboxes drain (the shipper may sit in its up-to-60 s retry backoff first), fidelity holds with no hole across the master's outage.
6. **Revoke**: `fleet node revoke child2`; child2's link shows `revoked` and it logs `fleet:link:revoked` locally; the master's replica of child2 stops growing while child2's local store keeps growing.
7. **Leave + remove**: `fleet leave --purge` on child1 and restart: role solo, `fleet-child/` and `outbox/` gone. `fleet node remove child1` on the master: gone from `fleet nodes`, and no down alert for it (nor for revoked child2) after 45 s.
8. **Solo regression**: no `fleet/`, `fleet-child/` or `outbox/` directories, nothing listening on 9443, `fleet status` says solo, `trinetra status` works.
9. **Security spot checks**: `POST /fleet/v1/ingest` from `solo` without a client certificate is refused with 401; a garbage join code and the spent code are both refused and register no node.

Output is one `PASS <step>` / `FAIL <step>: <why>` line per scenario; the
first failure exits non-zero after dumping every daemon's log tail, the
master's alert log, `fleet nodes`/`fleet status` and the mock's messages.
