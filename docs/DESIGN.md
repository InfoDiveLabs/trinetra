# server-watcher — design spec

**Date:** 2026-07-25
**Status:** approved (design), pending implementation plan
**Author:** Suraj + Claude

## Summary

`server-watcher` is a new personal background worker for the `workers` repo. It
runs on a **Linux/systemd home server** as a single self-installing Go binary
(`serverwatch`), monitors the host, and talks to the owner over **Telegram**. It
follows the pr-watcher mold — one binary, self-installing, on-disk state, 2-way
Telegram — with two deliberate departures:

1. **No AI.** All logic is deterministic Go. No Claude calls, no tokens.
2. **CLI-managed config, not env.** Everything configurable is set through
   `serverwatch` subcommands; there is no hand-edited `.env`.

Its headline capabilities:

- Self-configures for startup (writes + enables a systemd unit).
- Self-discovers what to monitor (docker containers, systemd services,
  filesystems, network interfaces, temperature sensors, SMART disks).
- Reports stats and fires threshold + rolling-baseline anomaly alerts over
  Telegram, deterministically.
- Keeps a **30-day, minute-resolution downtime history** — both "box was off /
  process not running" (reconstructed from a heartbeat) and "box up but internet
  down" (logged live) — and can push a real-time "it's down NOW" alert via an
  optional healthchecks.io dead-man switch.

Non-goals (YAGNI): no web UI, no metrics-export/Prometheus endpoint, no
multi-host aggregation, no AI/LLM anything, no config file the user edits by hand.

## Context

The `workers` repo holds self-contained background workers. The existing
`pr-watcher` (`prwatch`) is a Go daemon run by macOS launchd with 2-way Telegram
and on-disk `status.json`; it is the stated model for future workers. `server-watcher`
adapts that model to a Linux/systemd host and to host-monitoring instead of PR
review.

Source is developed here in `server-watcher/` on macOS, cross-compiled for Linux
(`GOOS=linux`), and the resulting binary is copied to the home server where
`serverwatch install` is run.

## Architecture

A single Go binary, `serverwatch`, running as a **system-level systemd service**
(root — required for full docker/process/SMART visibility and to start at boot
without an interactive login). One daemon process, two goroutines:

- **Sampler loop** — every `sample_interval` (default 60s): collect enabled
  metrics, update rolling baselines, evaluate thresholds/anomalies, rewrite the
  minute heartbeat, ping healthchecks.io (if configured), and fire alerts on
  state changes.
- **Telegram long-poller** — `getUpdates` long-poll for commands/inline buttons,
  replies in ~1s (same pattern as prwatch's poller).

systemd `Restart=always` restarts the daemon if it dies and starts it at boot
(`WantedBy=multi-user.target`). Logs go to **journald** via stdout/stderr
(`journalctl -u serverwatch -f`); no manual logfile plumbing.

### Package layout (mirrors pr-watcher)

```
server-watcher/
  cmd/serverwatch/main.go
  internal/config/config.go        # CLI-managed config store, defaults, effective-config resolution
  internal/telegram/telegram.go    # long-poll + send (pattern shared with prwatch)
  internal/serverwatch/
    main.go        # CLI dispatch (install, uninstall, config, monitor, status, doctor, daemon, ...)
    daemon.go      # sampler loop + telegram poller, SIGHUP reload
    collect.go     # core collectors: cpu, mem, swap, load, disk usage, uptime, temp
    discover.go    # target discovery: docker, services, filesystems, interfaces, sensors, SMART
    docker.go      # docker access probe (socket/group/sudo) + container enumeration
    smart.go       # SMART scan/health (sudo-aware)
    net.go         # connectivity, public-IP change, per-interface throughput
    downtime.go    # heartbeat write, gap -> power_down reconstruction, net_down intervals
    baseline.go    # rolling per-metric stats
    anomaly.go     # thresholds + z-score + hysteresis + dedup state machine
    store.go       # jsonl samples, retention pruning, status.json, downtime.jsonl
    digest.go      # daily / weekly rollups
    notify.go      # alert formatting + recovery + dedup
    handlers.go    # telegram commands / button menu
    systemd.go     # install/uninstall: render unit, enable/disable
    status.go      # status snapshot rendering
  test/docker/     # containerized end-to-end validation harness
  README.md
```

### Interfaces (testability prerequisite)

Every side-effecting boundary sits behind an interface so logic is unit-testable
and the real implementation is swapped in production:

- `Clock` — `Now()`; enables deterministic downtime/gap and schedule tests.
- `Telegram` — send/poll; swapped for an `httptest` mock in tests.
- `Exec` / `FileSource` — run a command / read a file, returning raw text.

**Collectors parse raw text.** Each parser takes the *output* of the external
source (`df`, `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `/proc/net/dev`,
`systemctl --failed`, `docker ps`, `smartctl --scan/-H`, `/sys/class/thermal/*`)
as a string and returns structured data. Parsing and discovery are therefore
fully unit-testable against captured fixtures.

## On-server layout (FHS)

```
/usr/local/bin/serverwatch            the binary
/etc/serverwatch/config.json          config + secrets (0600, root-owned)
/etc/systemd/system/serverwatch.service
/var/lib/serverwatch/                 all runtime state
  status.json         current snapshot (check this first)
  heartbeat           last-alive unix timestamp, rewritten every sample
  samples/YYYY-MM-DD.jsonl   per-day metric samples, pruned > 30d
  downtime.jsonl      downtime events (30d retention)
  baseline.json       rolling per-metric stats
  alerts.json         active-alert state (for dedup + recovery)
```

## Config — CLI-managed

No hand-edited files. Defaults are baked into the binary; a fresh install with
only a Telegram token set already works. The config store is `config.json`,
mutated exclusively through subcommands. Any `config`/`monitor`/`schedule` write
updates the file and sends `SIGHUP` to the running daemon, which reloads live (no
restart).

```
serverwatch install                       # copy binary, write+enable systemd unit, seed config.json
serverwatch uninstall                     # disable+remove unit; state left in place unless --purge
serverwatch daemon                         # (run by systemd) the long-running process

serverwatch config get [key]              # effective config (defaults + overrides), or one key
serverwatch config set <key> <value>      # e.g. config set sample_interval 30
serverwatch config unset <key>            # revert a key to its default

serverwatch telegram set-token <token>    # secret via CLI; chat-id auto-captured on first message
serverwatch quiet-hours 23-8 | off        # suppress pings in a window; one digest when it ends
serverwatch healthchecks set <url> | off  # dead-man switch ping target (optional)
serverwatch schedule daily 09:00 | off
serverwatch schedule weekly mon@09:00 | off

serverwatch monitor list                          # discovered targets: on/off + effective threshold each
serverwatch monitor enable|disable <target>       # e.g. disable docker:jellyfin, disable disk:/boot
serverwatch monitor threshold <target> <value>    # per-target override

serverwatch status                        # current readings (same data as status.json)
serverwatch doctor                        # discovery + permission probes; prints what works
```

`<target>` is namespaced: `docker:<name>`, `service:<unit>`, `disk:<mount>`,
`iface:<name>`, `temp:<zone>`, `smart:<dev>`, plus scalar targets `cpu`, `mem`,
`swap`, `load`, `net` (connectivity). Global thresholds live under `thresholds.*`
in config; per-target overrides are stored per target and win over the global.

## Self-discovery + docker-under-sudo

On startup and periodically the daemon discovers targets itself; the user does
not declare them.

- **Docker** — probes access in order and records the working method:
  (1) direct `/var/run/docker.sock` (works as root), (2) caller in `docker`
  group, (3) fall back to `sudo docker`. Uses that method for all queries.
  Enumerates all containers; monitors each for up/down and restart-loops by
  default. Exclude with `monitor disable docker:<name>`.
- **systemd services** — watches `systemctl --failed` globally: **any** unit
  entering the failed state alerts, zero config. A user may additionally pin
  critical units (`monitor enable service:<unit>`) so a clean *stop* (not just a
  crash) also alerts.
- **Filesystems** — auto-discovers real mounts (skips tmpfs/overlay/loop/squashfs);
  monitors usage % and fill-rate projection per mount.
- **Network interfaces** — auto-discovers up interfaces (skips `lo`); tracks
  throughput. Separately watches internet reachability and public-IP change.
- **Temperature** — auto-discovers `/sys/class/thermal` zones and hwmon sensors.
- **SMART / disk health** — auto-discovers SMART-capable block devices via
  `smartctl --scan`, using `sudo` when required (same fallback logic as docker).

Anything needing elevation degrades gracefully: if neither root nor sudo works,
that collector is marked `unavailable` in `status`/`monitor list` and its absence
never crashes the daemon. `monitor list` shows every discovered target with its
on/off state and effective threshold, so discovery is transparent and overridable.

## Metrics & anomaly detection (no AI)

Collectors, each independently toggleable, pure-Go from `/proc` and `/sys` where
possible, shelling out only where needed (`systemctl`, `docker`, `smartctl`):

- **Core** — CPU utilization + load, RAM, swap, disk usage/free, uptime, temperature.
- **Network** — internet reachability, public-IP change, per-interface throughput.
- **Disk health** — SMART health status, fill-rate projection ("/ full in ~3
  days"), inode exhaustion.
- **Services** — global `systemctl --failed` watch + optional pinned units.
- **Docker** — container up/down + restart-loop detection.

**Anomaly logic:** static thresholds **plus** a rolling 7-day baseline
(mean/stddev per metric); a reading beyond `baseline_sigma` (default 3) standard
deviations from its norm is flagged even if under the static threshold.

**Hysteresis + dedup** (a small per-target state machine, in `alerts.json`): an
alert fires once when a condition crosses, stays quiet while sustained, and sends
a **recovery** message when it clears. This prevents flapping. All thresholds and
`baseline_sigma` are config keys with per-target overrides.

## Downtime tracking

Two distinct failure modes, tracked separately, both at minute resolution for 30
days, recorded to `downtime.jsonl`.

**A. Box powered off / process not running — reconstructed from the heartbeat.**
Every sample the daemon rewrites `heartbeat` with the current time. On startup it
reads the previous heartbeat; if `now − last_heartbeat > 2 × sample_interval`, it
records a `power_down` event `{type, start: last_heartbeat, end: boot_time,
duration}` and sends a **boot/recovery report** over Telegram
("back online — was down 02:14→06:47 (4h33m)"). Requires nothing external.

**B. Box up but internet dropped — logged live.** The process is alive (no
heartbeat gap), so each sample runs a connectivity check (resolve + reach a small
set of hosts). When it flips down it opens a `net_down` interval; it closes the
interval on recovery. Both are written to `downtime.jsonl`.

**C. Real-time "it's down NOW" push — optional healthchecks.io dead-man switch.**
When `healthchecks.url` is set, every sample pings it. If the box is off *or* its
internet is down, pings stop and healthchecks.io messages the owner's Telegram
directly. This is the only mechanism that can alert while the box cannot speak,
and it is fully optional.

A+B give the complete on-box 30-day record; C gives the instant external alert.
`/history [days]` and `/down` render these over Telegram.

Edge cases handled: clock skew / time going backwards (guard, don't emit a
negative-duration event), a gap exactly at the threshold, a blip shorter than the
threshold (not an event), and daemon restart without a reboot (heartbeat gap
small → no false downtime).

## Telegram

Reuses the prwatch telegram pattern: `getUpdates` long-poll, inline-button menu,
push alerts. Chat-id auto-captured on first message to the bot.

**On-demand commands:** `/stats`, `/status`, `/disk`, `/net`, `/services`,
`/docker`, `/history [days]`, `/down`, `/help` (plus an inline-button menu
mirroring these).

**Proactive messages, each independently config-gated:**

- **Boot/recovery report** — on every startup (downtime summary + fresh snapshot).
- **Daily digest** — `schedule daily HH:MM`: 24h uptime, peak CPU/RAM, disk
  trend, anomalies, downtime events.
- **Weekly rollup** — `schedule weekly <dow>@HH:MM`: 7-day trends, total
  downtime, top offenders.

`quiet-hours` suppresses non-critical pings within a window and sends one digest
when it ends (reused from prwatch). Critical alerts (e.g. disk full imminent) may
override quiet hours — controlled by a config flag.

## Testing

### Tier 1 — Unit tests (pure, fast, run anywhere incl. macOS)

- Downtime gap → `power_down` reconstruction, incl. clock-skew, short-blip,
  exactly-at-threshold, and restart-without-reboot edges.
- `net_down` interval open/close on connectivity flips.
- Rolling-baseline math + z-score; threshold + hysteresis + dedup state machine
  (fire once → quiet while sustained → recovery); 30-day retention pruning.
- Config store: get/set/unset, defaults merge, effective-config resolution,
  per-target threshold precedence.
- Every collector/discovery parser against captured fixture outputs of its
  external command (`df`, `/proc/*`, `systemctl --failed`, `docker ps`,
  `smartctl`, thermal zones).

### Tier 2 — Integration tests (real Linux surface, CI on Linux)

- Collectors against real `/proc`, `/sys`, `df`, and real `systemctl`/`docker`
  when present (skip-gracefully when absent).
- Telegram flow against an `httptest` mock server: assert exact alert/digest
  formatting and full command round-trips (poll → handler → reply).
- Config-CLI writes → `SIGHUP` → live reload observed by the daemon.

### Tier 3 — Containerized end-to-end validation

A `test/docker/` harness driven by one command (`make validate`), proving the
worker actually does its job. Uses a **mock Telegram server container** (stubs
`getUpdates`/`sendMessage`) so no real bot or network is needed; docker socket
mounted so the docker collector sees real containers.

- **Discovery:** start throwaway containers → assert `monitor list` discovers
  them and the mock receives the expected snapshot.
- **Downtime (headline):** let it beat, `docker kill` the daemon container, wait,
  restart → assert it reconstructs the exact `power_down` interval and pushes a
  boot/recovery message. Point connectivity checks at an unreachable host via
  config → assert a `net_down` interval + recovery.
- **Anomaly:** `stress` / `fallocate` to spike CPU / fill a mount → assert
  threshold + baseline-deviation alerts fire once, then a recovery when cleared
  (verifies hysteresis for real).
- **Docker-under-sudo:** run the daemon as a non-root user without docker-group
  membership but with passwordless sudo → assert it detects and falls back to
  `sudo docker` and still discovers containers.

Systemd-in-docker is heavyweight: unit generation is verified by rendering the
unit file and asserting its contents; a real `install` + `systemctl status` is an
**optional** privileged/systemd-enabled container job (documented, not on the
fast path).

### Bonus — `serverwatch doctor`

On-server command that runs discovery + permission probes (docker access method,
sudo availability, SMART, sensors) and prints what works. Doubles as ops tooling
and a real-host smoke test.

## Success criteria

1. `serverwatch install` on a fresh Linux host, with only a Telegram token set,
   yields a daemon that starts at boot, discovers targets, and reports.
2. Killing/powering the host produces an accurate reconstructed `power_down`
   event and a boot/recovery Telegram message on return.
3. Dropping internet (box up) produces a live `net_down` interval; with
   healthchecks.io configured, a real-time down alert arrives.
4. Threshold and baseline-deviation anomalies fire once with a matching recovery,
   no flapping.
5. All config is achievable via CLI alone; `monitor list` reflects discovery and
   overrides.
6. `make validate` runs the Tier-3 container suite green with no real bot/network.

## Open items deferred to the plan

- Exact `config.json` schema and key names (`thresholds.*`, `schedule.*`, etc.).
- Precise connectivity-check host list and cadence defaults.
- Fixture capture list for parser tests.
- Whether the mock Telegram server is a tiny Go binary in `test/docker/` or a
  scripted stub.
