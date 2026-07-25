# server-watcher (`serverwatch`)

A single Go binary that monitors a Linux/systemd home server and reports over
**Telegram**. No AI, no cloud, no metrics dashboard — deterministic thresholds
and a rolling baseline, all logic stdlib-only. Everything configurable is set
through the `serverwatch` CLI; there is no hand-edited config file.

It runs as one systemd service with two goroutines: a sampler loop (collect
metrics, update the baseline, evaluate thresholds, write a heartbeat) and a
Telegram long-poller (commands answered in ~1s). `Restart=always` +
`WantedBy=multi-user.target` mean it survives crashes and starts at boot.

## Quick start

Build on your dev machine, ship the binary, install on the server:

```bash
cd server-watcher
make linux                                    # dist/serverwatch-linux-amd64, -arm64
scp dist/serverwatch-linux-amd64 myserver:/tmp/serverwatch

ssh myserver
sudo /tmp/serverwatch install                 # copies to /usr/local/bin, writes+enables the systemd unit
sudo serverwatch telegram set-token <token>   # only required setting; chat id is auto-captured
                                               # from your first message to the bot
```

Get the `<token>` from Telegram's **@BotFather** (`/newbot`, follow the prompts,
copy the token it gives you) before running `telegram set-token`.

Send the bot any message once so it learns your chat id, then try `/stats` or
`/help`. From here everything else is optional — defaults already work.

## CLI reference

```
serverwatch install                       copy this binary to /usr/local/bin, write + enable the
                                           systemd unit, seed config.json if absent, start the service
serverwatch uninstall [--purge]           disable + remove the unit; --purge also deletes
                                           /var/lib/serverwatch and /etc/serverwatch/config.json
serverwatch daemon                        the long-running process (run by systemd, not by hand)

serverwatch config get [key]              print effective config (defaults + overrides), or one key
serverwatch config set <key> <value>      set a key, e.g. `config set sample_interval 30`
serverwatch config unset <key>            revert a key to its default

serverwatch telegram set-token <token>    set the bot token (chat id auto-captured on first message)
serverwatch monitor list                  discovered targets with on/off state + effective threshold
serverwatch monitor enable <target>       re-enable a target, e.g. `monitor enable docker:jellyfin`
serverwatch monitor disable <target>      stop alerting on a target
serverwatch monitor threshold <t> <v>     per-target threshold override
serverwatch schedule daily HH:MM | off    daily digest time (24h, local time)
serverwatch schedule weekly dow@HH:MM | off   weekly rollup, e.g. `mon@09:00`
serverwatch quiet-hours HH-HH | off       suppress non-critical pings in a window (wraps midnight)
serverwatch healthchecks set <url> | off  healthchecks.io dead-man-switch URL (optional)

serverwatch status                        print /var/lib/serverwatch/status.json (current snapshot)
serverwatch doctor                        run discovery + permission probes, print what works
serverwatch help                          this usage text
```

Every `config`/`monitor`/`schedule`/`quiet-hours`/`healthchecks`/`telegram`
write updates `config.json` and sends `SIGHUP` to the running daemon, which
reloads live — **except `sample_interval`**, which takes effect only on the
next daemon restart (`systemctl restart serverwatch`), because the sampler
ticker is created once at startup and not reset on reload.

### Config keys

`config get`/`set`/`unset` accept: `sample_interval` (seconds, min 5, default
60), `baseline_sigma` (default 3), `quiet_hours` (`"HH-HH"` or empty),
`telegram.token`, `telegram.chat_id`, `healthchecks.url`, `schedule.daily`,
`schedule.weekly`, `thresholds.disk_pct` (90), `thresholds.temp_c` (80),
`thresholds.cpu_pct` (95), `thresholds.mem_pct` (90), `thresholds.swap_pct`
(50), `critical_overrides_quiet` (bool, default true).

### `<target>` namespacing

`monitor list/enable/disable/threshold` operate on discovered targets:
`docker:<container>`, `disk:<mount>`, `iface:<name>`, `temp:<zone-path>`,
`smart:<device>`. The scalar checks that actually fire alerts — `cpu`, `mem`,
`swap`, `temp`, and each `disk:<mount>` — support both global thresholds
(`thresholds.*`) and per-target overrides via `monitor threshold`.

## On-server layout (FHS)

```
/usr/local/bin/serverwatch                the binary
/etc/serverwatch/config.json              config + secrets (0600, root-owned)
/etc/systemd/system/serverwatch.service   the unit `install` writes
/var/lib/serverwatch/
  status.json          current snapshot — check this first
  heartbeat            last-alive unix timestamp, rewritten every sample
  samples/YYYY-MM-DD.jsonl   per-day metric samples, pruned past 30 days
  downtime.jsonl        downtime events (power_down / net_down), 30-day retention
  baseline.json         rolling per-metric mean/stddev
  alerts.json           active-alert state (fire-once + recovery dedup)
```

## Self-discovery + docker-under-sudo

On startup and periodically, `Discover` enumerates what's on the box — nothing
is hand-declared:

- **Docker** — probes access in order and remembers the working method: plain
  `docker ps` (root or `docker`-group membership), then `sudo docker ps`. Every
  container becomes a `docker:<name>` target. `doctor` and `monitor list` show
  which method worked (`socket`/`group`/`sudo`) or `unavailable` if neither did.
- **Filesystems** — real mounts from `df -PB1` (tmpfs/overlay/`/proc`/`/sys`/
  `/dev`/`/run` filtered out) become `disk:<mount>` targets.
- **Network interfaces** — every non-loopback interface in `/proc/net/dev`
  becomes an `iface:<name>` target.
- **Temperature** — every `/sys/class/thermal/thermal_zone*/temp` zone becomes
  a `temp:<path>` target.
- **SMART** — `smartctl --scan` (falling back to `sudo` the same way docker
  does) enumerates block devices as `smart:<device>` targets.

Anything requiring elevation that isn't available (no root, no sudo, no
`smartctl`) is simply omitted or marked unavailable — discovery never crashes
the daemon for a missing tool.

**What actually alerts today:** the sampler evaluates threshold + baseline
checks on `cpu`, `mem`, `swap`, `temp`, and each discovered `disk:<mount>`
(see thresholds above). Docker/interface/SMART targets are discovered, listed
in `monitor list`, and can carry per-target overrides, but container state,
interface throughput and SMART health do not yet drive alert firing — that's
future work, not implemented in this build.

## Downtime tracking

Two failure modes, tracked separately, at minute (sample-interval) resolution,
written to `downtime.jsonl`:

**A — power down / process not running (reconstructed from heartbeat).** Every
sample the daemon rewrites `heartbeat` with the current time. On startup it
reads the previous heartbeat; if `now − last_heartbeat > 2 × sample_interval`,
it records a `power_down` event (`start`/`end`/`duration_sec`) and, once a
Telegram token + chat id are set, sends a boot/recovery message with the gap
and a fresh snapshot. Needs nothing external — it works from local state alone.

**B — box up, internet down (logged live).** Each sample checks connectivity
(reach `1.1.1.1:53` / `8.8.8.8:53`). A downward flip opens a `net_down`
interval; the next successful check closes it and appends the completed event.

**C — real-time "it's down now" push (optional healthchecks.io dead-man
switch).** When `healthchecks.url` is set, every sample pings it. If the box
is off or its own internet is down, pings stop and healthchecks.io messages
you directly — the only mechanism that can alert while the box itself can't
speak. Set it with `serverwatch healthchecks set <url>`; turn it off with
`serverwatch healthchecks off`.

`/history [days]` (default 7) and `/down` render A+B over Telegram.

## Telegram

**Commands** (send as a message to the bot):

```
/stats, /status   current readings (CPU, mem, swap, load, temp, internet, disks)
/disk             filesystem usage
/net              internet reachability
/history [days]   downtime events in the last N days (default 7)
/down             alias for /history
/help             this list
```

**Proactive messages**, each independently gated by config:

- **Boot/recovery report** — sent on every daemon startup if a `power_down`
  gap was reconstructed (see downtime A above); includes the gap and a fresh
  snapshot.
- **Threshold/baseline alerts** — one message when a check crosses (⚠️, or 🚨
  if critical), one ✅ recovery message when it clears; no repeat spam while
  sustained (hysteresis, tracked in `alerts.json`).
- **Daily digest** — `serverwatch schedule daily HH:MM`: currently reports the
  downtime summary for the prior 24h. (Peak CPU/mem stats are part of the
  message format but not yet populated — they render as 0 until a later task
  wires sample lookback in.)
- **Weekly rollup** — `serverwatch schedule weekly <dow>@HH:MM` (e.g.
  `mon@09:00`): same digest content over the prior period, with the same
  peak-stats caveat.

`quiet-hours HH-HH` (wraps midnight, e.g. `23-8`) suppresses non-critical
alerts in that window; a check whose `Critical` flag is set (currently: any
`disk:<mount>` threshold breach) still gets through if
`critical_overrides_quiet` is true (the default).

## `serverwatch doctor`

Runs the same probes as the daemon and prints what's usable on this host:
docker access + method, whether `smartctl` responds (directly or via sudo),
how many thermal zones exist, and how many targets `Discover` finds in total.
Good first command to run after `install`, or when diagnosing a permission gap.

## Manage

```bash
journalctl -u serverwatch -f          # live logs (stdout/stderr go to journald)
systemctl status serverwatch          # is it running, last restart, exit code
serverwatch status                    # current readings via the CLI
cat /var/lib/serverwatch/status.json  # same data, straight off disk
```

Config changes made through the CLI take effect immediately via `SIGHUP` — no
`systemctl restart` needed unless the binary itself changed.

## Build

```bash
make test      # go test ./... — unit tests, run anywhere (incl. macOS)
make vet       # go vet ./...
make build     # local-OS binary at dist/serverwatch
make linux     # cross-compile: dist/serverwatch-linux-amd64 + dist/serverwatch-linux-arm64
make validate  # containerized end-to-end suite (test/docker/scenarios.sh) — Linux/Docker required
```

`make validate` is the Tier-3 containerized harness (discovery, downtime
reconstruction, anomaly hysteresis, docker-under-sudo) driven by a mock
Telegram server; it requires Docker and is not part of `make test`.
