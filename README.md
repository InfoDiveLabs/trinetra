# server-watcher (`serverwatch`)

A single Go binary that monitors a Linux/systemd home server and reports over
**Telegram**. No AI, no cloud, no metrics dashboard — deterministic thresholds
and a rolling baseline, all logic stdlib-only. Everything configurable is set
through the `serverwatch` CLI; there is no hand-edited config file.

It runs as one systemd service with two goroutines: a tiered sampler loop —
a **fast tier** (CPU/mem/swap/load/temp every `fast_interval`, default 5s)
that drives live status and anomaly detection, plus a **slow tier**
(disk/docker/systemd/SMART/network every `sample_interval`, default 60s) for
the pricier checks — and a Telegram long-poller (commands answered in ~1s).
`Restart=always` + `WantedBy=multi-user.target` mean it survives crashes and
starts at boot. Sample data (the scalar host metrics, plus per-disk usage,
per-container docker cpu/mem, per-interface network throughput, and
per-device SMART temperature) is persisted to a compact binary time-series
store; the fuller live picture — service list, process table, per-container
network I/O, disk detail, SMART attributes — lives in `status.json` instead
(see **Data model** below, On-server layout, and
[docs/DESIGN-storage.md](docs/DESIGN-storage.md)).

## Quick start

**Option A — prebuilt binary** (from the [Releases](https://github.com/Suraj-Tiwari/server-monitor/releases) page):

```bash
# on the server (pick the arch: amd64 / arm64 / arm for older Pis)
curl -fsSL -o /tmp/serverwatch \
  https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/serverwatch-linux-amd64
chmod +x /tmp/serverwatch
sudo /tmp/serverwatch install                 # copies to /usr/local/bin, writes+enables the systemd unit
sudo serverwatch telegram set-token <token>   # only required setting; chat id is auto-captured
                                               # from your first message to the bot
```

**Option B — build from source** and ship the binary:

```bash
make linux                                    # dist/serverwatch-linux-amd64, -arm64
scp dist/serverwatch-linux-amd64 myserver:/tmp/serverwatch
ssh myserver
sudo /tmp/serverwatch install
sudo serverwatch telegram set-token <token>
```

Get the `<token>` from Telegram's **@BotFather** (`/newbot`, follow the prompts,
copy the token it gives you) before running `telegram set-token`.

Send the bot any message once so it learns your chat id, then try `/stats` or
`/help`. From here everything else is optional — defaults already work.

See **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** for the full install/verify/manage
guide, **[docs/DESIGN.md](docs/DESIGN.md)** for the architecture, and
**[docs/ROADMAP.md](docs/ROADMAP.md)** for status and planned work.

### Optional: web UI

A separate `serverwatch-web` binary (`make web`) adds an embedded, passkey-only
web UI — live dashboard, history graphs, config/channels/users editor, and a
curated public status page — as an alternative to Telegram. It's opt-in and
built from a `-tags web` build; the default `serverwatch` binary above stays
100% stdlib with no extra dependencies. See **[docs/WEB.md](docs/WEB.md)** for
enabling it, the three serving modes (Cloudflare Tunnel/nginx/Caddy, autocert,
manual TLS), and passkey enrollment.

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

serverwatch channel list                  list configured notification channels + their routing
serverwatch channel add <name> --type <t> [--set k=v ...]   add a channel (see Notification channels)
serverwatch channel set <name> <key> <value>   update a channel's routing or setting.<key>
serverwatch channel remove <name>         delete a channel
serverwatch channel test <name>           send a synthetic test alert through one channel

serverwatch status                        print /var/lib/serverwatch/status.json (current snapshot)
serverwatch doctor                        run discovery + permission probes, print what works
serverwatch migrate [--force]             one-shot import of legacy JSONL samples/downtime into the
                                           configured time-series store; re-run is a no-op unless
                                           --force (see On-server layout below)
serverwatch dump --metric <id> [--since 24h] [--res raw|1m] [--format csv|json]
                                           export one metric's series (e.g. `cpu`, `disk:/`) for
                                           humans or graphing tools
serverwatch alerts [list] [--since 24h] [--limit 20]
                                           active alerts + recent alert-log history (see Alert
                                           history below)
serverwatch alerts ack <key>              acknowledge an active alert, e.g. `alerts ack disk:/`
serverwatch alerts unack <key>            clear an acknowledgement
serverwatch help                          this usage text
```

Every `config`/`monitor`/`schedule`/`quiet-hours`/`healthchecks`/`telegram`
write updates `config.json` and sends `SIGHUP` to the running daemon, which
reloads live — including `fast_interval`/`sample_interval` (the sampler
ticker is reset in place, so a new interval takes effect on the very next
tick) and `heartbeat_interval`, no restart needed. `storage.*` keys are the
exception: the running time-series store isn't reopened on `SIGHUP`, so a
`storage.backend`/retention change only takes effect after
`systemctl restart serverwatch`.

### Config keys

`config get`/`set`/`unset` accept:

- `fast_interval` (seconds, min 1, default 5) — the fast tier's cadence.
- `sample_interval` (seconds, min 5, default 60) — the slow tier's cadence;
  must be an integer multiple of `fast_interval` (and `fast_interval` is
  rejected if lowering it would break that multiple — adjust
  `sample_interval` first).
- `heartbeat_interval` (seconds, min 1, default 30) — liveness heartbeat
  cadence, independent of both tiers.
- `baseline_alerts` (bool, default **false**) — opt-in for baseline (z-score)
  deviation alerting on cpu/mem/swap/temp/disk; threshold alerting on those
  same metrics is always on regardless of this setting. Defaults off because
  a metric with a low, unstable mean (e.g. an idling cpu%) can read many
  standard deviations from its own baseline on a completely normal wobble,
  firing/recovering every tick even with `baseline_sigma`/`baseline_min_pct`'s
  gates below — enable it only if you specifically want deviation-based
  alerts on top of the threshold ones.
- `baseline_sigma` (default 3), `quiet_hours` (`"HH-HH"` or empty),
  `telegram.token`, `telegram.chat_id`, `healthchecks.url`, `schedule.daily`,
  `schedule.weekly`, `thresholds.disk_pct` (90), `thresholds.temp_c` (80),
  `thresholds.cpu_pct` (95), `thresholds.mem_pct` (90),
  `thresholds.swap_pct` (50), `critical_overrides_quiet` (bool, default
  true).
- `storage.backend` (`tsfile` or `memory`, default `tsfile`) — the
  time-series backend (see [docs/DESIGN-storage.md](docs/DESIGN-storage.md)).
- `storage.raw_retention` (duration string, default `48h`) — how long raw
  samples are kept.
- `storage.rollup_retention` (duration string, default `720h`/30d) — how
  long 1-minute rollups and downtime events are kept.
- `collect.container_stats` (bool, default `true`) — slow-tier per-container
  `docker stats` (cpu%/mem/net). Feeds the `docker:<name>:cpu`/`:mem` series
  plus `container_stats` (incl. net rx/tx, snapshot-only) in `status.json`.
- `collect.net_throughput` (bool, default `true`) — slow-tier per-interface
  `/proc/net/dev` throughput. Feeds the `net:<iface>:rx`/`:tx` series plus
  `net_rates` in `status.json`.
- `collect.services` (bool, default `true`) — slow-tier full systemd unit
  inventory (`units` in `status.json`; snapshot-only, no series — the
  `systemctl --failed` alerting collection always runs regardless).
- `collect.processes` (bool, default `true`) — slow-tier process-table
  overview (`processes` in `status.json`: counts + top-N by CPU/mem;
  snapshot-only, no series).
- `collect.smart_attrs` (bool, default `true`) — slow-tier per-device
  `smartctl -A` attribute reads (`smart_attrs` in `status.json`, feeds the
  `smart:<dev>:temp` series; the cheaper `--scan`/`-H` health check always
  runs regardless).
- `collect.smart_interval` (int seconds, default `1800` — 30 min) — throttles
  the SMART scan (`smartctl --scan`/`-H`/`-A`); cached results still flow to
  `status.json` every slow tick between scans. Set as low as `sample_interval`
  to scan every slow tick.

  All five `collect.*` toggles above are opt-**out**: unset/absent means enabled.
  Turn one off with, e.g., `serverwatch config set collect.processes false`
  — useful on a small device, or a host with hundreds of short-lived
  processes/containers, where the default collection is more than you need.
  `serverwatch doctor` prints the current on/off state of all five plus the
  resulting series count and disk usage.

### Sampling tiers

The sampler loop ticks at `fast_interval`; every Nth tick (N =
`sample_interval / fast_interval`) it also runs the slow tier:

- **Fast tier** — cheap, no subprocess: `/proc` reads for CPU/mem/swap/load
  plus the thermal-zone temperature. Drives `status.json`, the rolling
  baseline, and the cpu/mem/swap/temp anomaly checks. Each fast tick appends
  a raw sample per metric (`cpu`, `mem`, `swap`, `load1`, `load5`, `load15`,
  `temp`) to the time-series store.
- **Slow tier** — the pricier checks: `df` for disk usage (plus device/
  fstype/inode detail), `docker ps` (plus the opt-in `docker stats`),
  `systemctl --failed` (plus the opt-in full unit inventory), `smartctl`
  (plus the opt-in `-A` attribute read), the opt-in `/proc/net/dev`
  throughput read, the opt-in process-table snapshot, and the
  internet-connectivity dial. Drives the disk/docker/service/smart anomaly
  checks and their recovery sweeps, and pings `healthchecks.url` if set.
  Each slow tick also appends `disk:<mount>` samples, and — for whichever of
  the opt-in collectors above are enabled — `docker:<name>:cpu`/`:mem`,
  `net:<iface>:rx`/`:tx`, and `smart:<dev>:temp` samples too. Docker
  container up/down state and systemd/SMART health stay alert-only; the
  full unit list, process table, and per-container network I/O are
  snapshot-only (`status.json`, never a series) — see **Data model** below
  for the complete breakdown.
- **Heartbeat** — on its own `heartbeat_interval` cadence, independent of
  both tiers: rewrites `heartbeat` so a future boot can measure how long the
  process was down (see Downtime tracking below).

### `<target>` namespacing

`monitor list/enable/disable/threshold` operate on discovered targets:
`docker:<container>`, `disk:<mount>`, `iface:<name>`, `temp:<zone-path>`,
`smart:<device>`. The scalar checks that actually fire alerts — `cpu`, `mem`,
`swap`, `temp`, and each `disk:<mount>` — support both global thresholds
(`thresholds.*`) and per-target overrides via `monitor threshold`.

## Data model

serverwatch's on-disk/live state comes in three distinct shapes. Knowing
which one a datapoint lives in tells you whether it's queryable history
(`dump`), a live read of "right now" (`status.json`), or a discrete
thing-that-happened record:

### 1. Time-series (`ts/`, the tsfile store)

Numeric metrics on a fixed cadence, downsampled + pruned per
`storage.raw_retention`/`rollup_retention`, queried via `serverwatch dump
--metric <id>`. The complete list of series ids:

- `cpu`, `mem`, `swap`, `load1`, `load5`, `load15`, `temp` — fast tier,
  every `fast_interval`.
- `disk:<mount>` — slow tier, one per discovered filesystem.
- `docker:<name>:cpu`, `docker:<name>:mem` — slow tier, one pair per running
  container (opt-in, `collect.container_stats`, default on).
- `net:<iface>:rx`, `net:<iface>:tx` — slow tier, bytes/sec per interface
  (opt-in, `collect.net_throughput`, default on; empty until the second
  slow tick, since a rate needs a prior sample to diff against).
- `smart:<dev>:temp` — slow tier, one per SMART device that reports a
  temperature attribute (opt-in, `collect.smart_attrs`, default on).

That's all of them. Per-container network I/O, the service list, and the
process table are deliberately **not** series (see #2 below) — unbounded
per-container/per-unit/per-process cardinality is exactly what this design
avoids; only bounded, low-cardinality numeric metrics get a series.

### 2. Live snapshot (`status.json`)

One JSON document — the full in-memory `Snapshot`
(`internal/serverwatch/status.go`) — rewritten every `fast_interval` tick.
`serverwatch status` and `cat /var/lib/serverwatch/status.json` both just
read this file. Alongside the current value of every series above, it
carries fields that are refreshed each slow tick but never persisted as
history:

- **Services** — `units`: the full systemd unit inventory (name/load/
  active/sub/description for every unit; opt-in, `collect.services`,
  default on). `failed_units` — the alerting-only list of units currently
  in `failed` state — is separate and always collected regardless.
- **Processes** — `processes`: total/running/sleeping/zombie counts plus a
  bounded top-N (by CPU, falling back to mem on the very first tick) of
  pid/name/state/cpu%/mem/threads (opt-in, `collect.processes`, default
  on).
- **Container state + stats** — `containers` (name → state, always
  collected once docker is reachable) and `container_stats` (opt-in,
  `collect.container_stats`): per-container cpu%/mem **and** net rx/tx MB.
  The net figures are snapshot-only — not fed into a series, unlike cpu/mem
  above.
- **Network rates** — `net_rates`: the same per-interface rx/tx bytes/sec
  that also feeds the `net:<iface>:rx`/`:tx` series.
- **Disk detail** — `disk_detail` (always collected, no toggle): per-mount
  device path, filesystem type, inode-usage%, free/total bytes, and — once
  enough `disk:<mount>` history exists — a linear fill-rate projection
  (days until full).
- **SMART health + attributes** — `smart_health` (device → PASSED/FAILED/
  UNKNOWN, always collected) and `smart_attrs` (opt-in,
  `collect.smart_attrs`): per-device temperature/wear%/reallocated-sectors.

The nested per-entity types (container stats, interface rates, disk detail,
SMART attributes, unit info, the process snapshot) don't carry explicit
`json` struct tags, so their keys in the raw `status.json` are the Go field
names verbatim (e.g. a container's cpu% is `container_stats.<name>.CPUPct`,
not `cpu_pct`) — see `internal/serverwatch/status.go`, `docker.go`, `net.go`,
`proc.go`, and `discover.go` if you're consuming the file directly rather
than through the CLI or Telegram.

### 3. Event log (append-only, JSONL)

Discrete records of things that happened, not sampled values:

- **`alertlog.jsonl`** — every alert notification the daemon has dispatched
  (fire and recover), with the per-channel delivery outcome, written by
  every anomaly transition, the boot report, and the daily/weekly digests.
  Pruned to ~30 days on each slow tick. Read via `serverwatch alerts` (see
  Alert history below) rather than parsed directly.
- **`ts/events.tsd`** — downtime events (`power_down`, `net_down`; see
  Downtime tracking below). Also event-shaped, but lives inside the tsfile
  store rather than a separate JSONL file since it shares that store's
  retention/downsample machinery.

## On-server layout (FHS)

```
/usr/local/bin/serverwatch                the binary
/etc/serverwatch/config.json              config + secrets (0600, root-owned)
/etc/systemd/system/serverwatch.service   the unit `install` writes
/var/lib/serverwatch/
  status.json           current snapshot — check this first
  heartbeat             last-alive unix timestamp, rewritten every heartbeat_interval
  ts/raw/<metric>.tsd    raw samples per metric, binary (storage.raw_retention, default 48h)
  ts/1m/<metric>.tsd     1-minute min/avg/max rollups (storage.rollup_retention, default 720h/30d)
  ts/events.tsd          downtime events (power_down / net_down), same rollup_retention window
  baseline.json          rolling per-metric mean/stddev
  alerts.json            active-alert state (fire-once + recovery dedup)
  alertlog.jsonl         alert notification history (fire/recover + per-channel
                         delivery outcome), pruned to ~30d
```

`<metric>` is the metric id (`cpu`, `mem`, `swap`, `load1`, `temp`,
`disk:/mount`, ...) with any non-filename-safe byte percent-encoded, e.g.
`disk:/` → `disk%3A%2F.tsd`. Use `serverwatch dump --metric <id>` rather than
reading these files directly.

**Upgrading from the old JSONL store:** a pre-upgrade install has
`samples/YYYY-MM-DD.jsonl` + `downtime.jsonl` instead of `ts/`. Run
`serverwatch migrate` once to import that history into the time-series store
above; it archives the legacy files to `*.migrated` (never deletes them) and
is safe to leave un-run — a fresh `ts/` directory is created and used
regardless, `migrate` only back-fills old history. See
[docs/DESIGN-storage.md](docs/DESIGN-storage.md) for the on-disk format.

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

**What actually alerts today:** the sampler always evaluates threshold checks
on `cpu`, `mem`, `swap`, `temp`, and each discovered `disk:<mount>` (see
thresholds above); baseline (z-score) deviation checks on those same metrics
are opt-in via `baseline_alerts` (default off — see Config keys above), plus
three binary checks: Docker container up/down
(`docker:<name>`, running=ok/anything else=bad), failed systemd units
(`service:<unit>`, from `systemctl --failed`, recovers once the unit is no
longer listed), and SMART health (`smart:<device>`, `FAILED`=bad). All of
these carry the same fire/recover + hysteresis behavior as the scalar checks.
Interface targets (`iface:<name>`) are discovered and listed in `monitor list`;
throughput is collected into the `net:<iface>:rx`/`:tx` series and
`status.json`'s `net_rates` (see Data model above) but doesn't yet drive
alert firing — that's future work.

### Alert history (`alerts`)

`serverwatch alerts` (bare, or `alerts list`) prints two sections: currently
**ACTIVE ALERTS** (key, time since it fired, reason, and `[acked ... ago]` if
acknowledged) and recent **HISTORY** from `alertlog.jsonl` — timestamp,
kind (`fire`/`recover`), key, severity, title, and a per-channel delivery
line (`ok` or `FAILED: <err>`) underneath each. `--since <dur>` (default
`24h`) and `--limit <n>` (default `20`, most-recent-first) bound the history
shown. `serverwatch alerts ack <key>` / `alerts unack <key>` acknowledge or
clear an acknowledgement on an active alert (e.g. `alerts ack disk:/`) —
acknowledging doesn't silence a future re-fire, it just annotates the
current active one — and best-effort SIGHUPs the running daemon so it picks
up the change promptly.

## Downtime tracking

Two failure modes, tracked separately, stored as events in the time-series
store (`ts/events.tsd`, retained for `storage.rollup_retention`, default
720h/30d):

**A — power down / process not running (reconstructed from heartbeat).** Every
`heartbeat_interval` the daemon rewrites `heartbeat` with the current time. On
startup it reads the previous heartbeat; if
`now − last_heartbeat > 2 × heartbeat_interval`, it records a `power_down`
event (`start`/`end`/`duration_sec`) and, once a Telegram token + chat id are
set, sends a boot/recovery message with the gap and a fresh snapshot. Needs
nothing external — it works from local state alone.

**B — box up, internet down (logged live).** Each slow-tier tick
(`sample_interval`) checks connectivity (reach `1.1.1.1:53` / `8.8.8.8:53`). A
downward flip opens a `net_down` interval; the next successful check closes it
and appends the completed event.

**C — real-time "it's down now" push (optional healthchecks.io dead-man
switch).** When `healthchecks.url` is set, every slow-tier tick pings it. If
the box is off or its own internet is down, pings stop and healthchecks.io
messages you directly — the only mechanism that can alert while the box
itself can't speak. Set it with `serverwatch healthchecks set <url>`; turn it
off with `serverwatch healthchecks off`.

`/history [days]` (default 7) and `/down` render A+B over Telegram.

## Telegram

**Commands** (send as a message to the bot):

```
/stats, /status   current readings (CPU, mem, swap, load, temp, internet, disks)
/disk             filesystem usage
/net              internet reachability
/history [days]   downtime events in the last N days (default 7)
/down             alias for /history
/docker           container states
/services         failed systemd units
/help             this list
```

**Proactive messages**, each independently gated by config:

- **Boot/recovery report** — sent on every daemon startup if a `power_down`
  gap was reconstructed (see downtime A above); includes the gap and a fresh
  snapshot.
- **Threshold/baseline alerts** — one message when a check crosses (⚠️, or 🚨
  if critical), one ✅ recovery message when it clears; no repeat spam while
  sustained (hysteresis, tracked in `alerts.json`).
- **Daily digest** — `serverwatch schedule daily HH:MM`: peak CPU/mem over the
  prior 24h (from the real sample history, not a placeholder) plus the
  downtime summary for that window.
- **Weekly rollup** — `serverwatch schedule weekly <dow>@HH:MM` (e.g.
  `mon@09:00`): same content, computed over the prior 7 days.

`quiet-hours HH-HH` (wraps midnight, e.g. `23-8`) suppresses non-critical
alerts in that window; a check whose `Critical` flag is set (currently: any
`disk:<mount>` threshold breach) still gets through if
`critical_overrides_quiet` is true (the default).

## Notification channels

Telegram (Quick start above) is the original always-on channel, but every
threshold/baseline alert, the boot/recovery report, and the daily/weekly
digest now fan out through a **Dispatcher** to any number of independently
configured channels — email, generic webhooks, Slack, Discord,
[ntfy](https://ntfy.sh), and [Gotify](https://gotify.net) — each with its own
enable flag and routing. healthchecks.io (see Downtime tracking above) stays
separate: it's a dead-man switch, not part of this fan-out.

A channel receives an alert only if it's **enabled** *and* its route allows
it:

- `min_severity` — lowest severity (`info` | `warning` | `critical`) it
  accepts; empty/default is `info` (everything).
- `include_kinds` / `exclude_kinds` — comma-separated target kinds to
  restrict or block delivery to. The kinds an alert can carry are `cpu`,
  `mem`, `swap`, `temp`, `disk`, `docker`, `service`, `smart` (taken from the
  part of the target id before `:`, e.g. `disk:/` → `disk`). The boot report
  and daily/weekly digest carry no kind, so they still reach a channel unless
  `include_kinds` is set (an empty `include_kinds` means "all kinds").
- `critical_overrides_quiet` — per-channel: if true, `critical` alerts still
  reach this channel during quiet hours.

A pre-existing `telegram.token` (from `telegram set-token`) auto-migrates
into a real `telegram`-typed channel the moment any `channel` subcommand
runs, so upgrading from a Telegram-only setup needs no action — it shows up
in `channel list` like any other channel, and the legacy `telegram.token`/
`telegram.chat_id` keys keep working as a fallback.

| Type | Settings (`channel set <name> setting.<key> <value>`) |
|------|--------------------------------------------------------|
| `telegram` | `chat_id` (required unless already migrated); `token` (falls back to legacy `telegram.token`) |
| `email` | `host`, `from`, `to` (comma-separated) required; `port` (587), `username`, `password`, `starttls` (`true`) optional |
| `webhook` | `url` required; `method` (`POST`), `content_type` (`application/json`), `template` (a `text/template` body; defaults to a generic `{"text": "..."}` payload) optional |
| `slack` | `url` (Slack incoming-webhook URL) required — fixed Slack-formatted body |
| `discord` | `url` (Discord webhook URL) required — fixed Discord-formatted body, truncated to Discord's 2000-char limit |
| `ntfy` | `topic` required; `server` (`https://ntfy.sh`), `token` (for protected topics) optional |
| `gotify` | `server`, `token` (Gotify application token) required |

```bash
serverwatch channel add ops-email --type email
serverwatch channel set ops-email setting.host smtp.fastmail.com
serverwatch channel set ops-email setting.from serverwatch@home.lan
serverwatch channel set ops-email setting.to ops@home.lan
serverwatch channel set ops-email min_severity critical   # only page email for criticals
serverwatch channel test ops-email                         # send a synthetic test alert now
serverwatch channel list                                   # see every channel + its routing
```

Other `channel set` keys: `enabled true|false`, `include_kinds disk,docker`,
`exclude_kinds smart`, `critical_overrides_quiet true|false`. `channel
remove <name>` deletes a channel. Every `channel add`/`set`/`remove` writes
`config.json` and signals the running daemon the same way `config set` does
— no restart needed.

## `serverwatch doctor`

Runs the same probes as the daemon and prints what's usable on this host:
docker access + method, whether `smartctl` responds (directly or via sudo),
how many thermal zones exist, how many targets `Discover` finds in total, the
on/off state of every opt-in extended collector (`container_stats`/
`net_throughput`/`services`/`processes`/`smart_attrs` — see `collect.*` in
Config keys above), and the configured time-series store's current series
count plus on-disk size (`N series, X.X MB on disk (raw+1m)`, or
`unavailable` if the store failed to open) — a quick cardinality/disk-cost
sanity check, especially useful before/after toggling `collect.*` on a small
device. Good first command to run after `install`, or when diagnosing a
permission gap.

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
make web       # local-OS binary at dist/serverwatch-web (-tags web, see docs/WEB.md)
make web-cross # full release matrix: serverwatch + serverwatch-web for
               # linux amd64/arm64/arm + darwin amd64/arm64
make release   # web-cross, plus checksums.txt over every dist/ artifact
make validate  # containerized end-to-end suite (test/docker/scenarios.sh) — Linux/Docker required
```

`make validate` is the Tier-3 containerized harness (discovery, downtime
reconstruction, anomaly hysteresis, docker-under-sudo) driven by a mock
Telegram server; it requires Docker and is not part of `make test`.
