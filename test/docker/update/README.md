# Signed self-update Docker end-to-end test

`run.sh` proves Trinetra's signed self-update path end to end: real
`trinetra`/`trinetra-release` binaries, real ed25519 signature verification,
a real (fake-GitHub-backed) network fetch, a real guarded restart-and-health-check,
and a real rollback -- against pre-built, pre-signed release fixtures rather
than the real GitHub API or the real (currently unset) production release
keys.

```
make update-e2e          # or: bash test/docker/update/run.sh
```

Needs Docker with Compose v2+. Budget up to 30 minutes: scenarios 2 and 6
each wait for a Telegram notification that only goes out on the self-update
daemon loop's real 5-minute cadence (`updateLoopInterval`,
`internal/trinetra/update_daemon.go`) -- not a harness shortcut, the same
cadence a real host runs on. Containers, the network and volumes are removed
on exit, pass or fail.

## Why this needs its own build tag and its own fakes

Every binary in this harness is built with `-tags trinetra_testkeys`
(`internal/update/keys_testkeys.go`): `ProductionKeys()` then trusts the
deterministic `update.TestKeySet()` (CI seed 1, maintainer seed 2, channel-pointer
seed 3) instead of this repo's real production keys, which start out empty
(`internal/update/keys.go`) until the real key ceremony
(docs/handbook/10-operations.md, "Release keys and releasing") has run. That
same build tag is also what gates three test-only environment hooks
(`internal/trinetra/update_e2e_hooks_testkeys.go`; a default build's own
`update_e2e_hooks.go` hard-codes them off, pinned by
`TestE2EHooksIgnoredInReleaseBuild`):

- `TRINETRA_E2E_GITHUB_BASE_URL` points `update.source=github` at `relsrv`
  (below) instead of the real GitHub API.
- `TRINETRA_E2E_RESTART_CMD` replaces the self-update guard's
  `systemctl restart trinetra`.
- `TRINETRA_E2E_GUARD_CMD` replaces `realLaunchGuard`'s
  `systemd-run ... trinetra update guard`.

The host container has no real systemd at all: `/usr/local/bin/systemctl` is
a small shell shim (`fake-systemctl.sh`) that `trinetra install` drives for
`daemon-reload`/`enable`/`restart`, and the two env hooks above point the
guard at the same non-systemd restart/launch scripts
(`e2e-restart.sh`/`e2e-guard.sh`), both `setsid`-detached so they survive the
CLI process that started them.

## Topology (`compose.yml`, image from `Dockerfile`)

| Service | Role |
|---|---|
| `host` | runs the real `trinetra` CLI/daemon, no real systemd |
| `relsrv` | fake GitHub REST API (`relsrv/main.go`), serving `/releases` -- built at image time by `build-fixtures.sh` -- and requiring `Authorization: Bearer e2etoken` on every request |
| `mocktg` | mock Telegram API (`test/docker/mocktg`, shared with the other harnesses) |

Own compose project name (`swupdate`) and own network name (`swupdate_net`):
a name shared with another harness caused a real cross-harness test failure
before.

## Fixtures (`build-fixtures.sh`, run inside the image build)

| Release | Signed as | Used by |
|---|---|---|
| `v0.5.0` | good | scenario 1 (installed from `/releases/v0.5.0` as a local bundle) |
| `v0.5.1` | good | scenario 2 (the beta channel pointer names it) |
| `v0.5.2` | good signatures, but the core binary was built with `e2eCrashOnStart=1` (`internal/trinetra/daemon.go`) and exits immediately instead of starting the daemon | scenario 6 (rollback) |
| `v0.5.3-badci` | maintainer signature good, CI signature from an untrusted test key | scenario 3 |
| `v0.5.4-badmaint` | CI signature good, no `manifest.maint.sig` at all | scenario 4 |
| `v0.5.5-tampered` | good signatures over the original bytes, then one byte flipped in the binary afterwards | scenario 5 |
| `v0.5.6` | good | scenario 7 |
| `channels/beta` | signed pointer naming `v0.5.1` | scenario 2 |

Every "good" release's 9-file manifest (3 binaries x 3 linux architectures --
`cmd/trinetra-release manifest`'s exact requirement) is built by compiling
`trinetra`/`trinetra-ctl`/`trinetra-web` once for the image's native `GOARCH`
and copying that same binary to the other two architecture names: only the
native-arch file is ever actually installed or executed here.

## Scenarios

1. **Install `0.5.0` `--require-signed` from a signed bundle**: `trinetra install --require-signed`, run straight out of `/releases/v0.5.0`, verifies the bundle's manifest and both signatures, installs, and raises the version floor. Floor and running both end up `0.5.0`.
2. **Check and apply**: `trinetra update check` reports `0.5.1` available on the beta channel; `trinetra update apply` fetches, stages, smoke-tests, swaps in, and the guard confirms it healthy. Running and floor both become `0.5.1`, and mocktg eventually gets the `updated 0.5.0 → 0.5.1` notification.
3. **Bad CI signature**: `apply --version 0.5.3-badci` is refused before anything is staged; running stays `0.5.1`.
4. **Missing maintainer signature**: `apply --version 0.5.4-badmaint` is refused the same way.
5. **Tampered binary**: `apply --version 0.5.5-tampered` is refused on the SHA-256 mismatch during staging; the installed `/usr/local/bin/trinetra` is byte-for-byte unchanged.
6. **Crash-on-start rollback**: `apply --version 0.5.2` stages and swaps in cleanly (only `trinetra daemon` itself crashes, not `version --json`), but the guard's restarted daemon never comes up healthy, so it rolls back to `0.5.1` within 90s, marks `0.5.2` bad in `state.json`, and a critical "rolled back" alert reaches mocktg. A second `apply --version 0.5.2` without `--force` is refused.
7. **Guard killed mid health-check**: `apply --version 0.5.6` stages and restarts the daemon onto it; the harness waits for the new daemon PID (proving the guard already restarted it) and then kills the guard process before it can commit. The pending update is left stuck. The harness then restarts the daemon itself (simulating the next real start), whose `resumePendingOnStart` hook launches a fresh guard that finds the already-healthy `0.5.6` build and commits it, leaving no pending update.
8. **Downgrade refused**: `apply --version 0.5.0` is refused (floor is now well above `0.5.0`); running and floor are unchanged.
9. **No token leakage**: `trinetra config get` (both the full dump and the single `update.github_token` key) and `trinetra dump --metric cpu` never print the raw `e2etoken` value -- `update.github_token` is a secret key, always shown as `(set)`.

Output is one `PASS <n> <name>` / `FAIL <n> <name>: <why>` line per scenario;
the first failure exits non-zero after dumping the daemon and guard log
tails, `trinetra update status --json`, the raw `state.json`, and mocktg's
recorded messages.
