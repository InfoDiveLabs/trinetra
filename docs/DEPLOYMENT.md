# Deployment guide

Step-by-step for getting `serverwatch` running on a Linux/systemd home server and
verifying it works. For the architecture and design rationale, see
[DESIGN.md](DESIGN.md); for the time-series storage engine specifically, see
[DESIGN-storage.md](DESIGN-storage.md).

## 0. Prerequisites

- A Linux host with **systemd** (Ubuntu/Debian/Raspberry Pi OS, etc.).
- Root (the service runs as root so it can read every container/process, SMART
  data, and start at boot without a login).
- A Telegram bot token from **@BotFather** (`/newbot` → copy the token).
- Optional: Docker (auto-discovered), `smartmontools` for SMART health
  (`apt install smartmontools`), and a [healthchecks.io](https://healthchecks.io)
  check URL for the real-time dead-man switch.

## 1. Get the binary

**Prebuilt** — download the right arch from the
[Releases](https://github.com/Suraj-Tiwari/server-monitor/releases) page:

| Host | Asset |
|------|-------|
| x86-64 server / NUC | `serverwatch-linux-amd64` |
| Raspberry Pi 3/4/5 (64-bit OS) | `serverwatch-linux-arm64` |
| Older 32-bit Pi / ARMv7 | `serverwatch-linux-arm` |

```bash
curl -fsSL -o /tmp/serverwatch \
  https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/serverwatch-linux-arm64
chmod +x /tmp/serverwatch
```

Verify the checksum against `checksums.txt` on the release:

```bash
sha256sum /tmp/serverwatch     # compare to the matching line in checksums.txt
```

**Or build from source** (Go 1.22+):

```bash
git clone git@github.com:Suraj-Tiwari/server-monitor.git
cd server-monitor
make linux                     # produces dist/serverwatch-linux-amd64 and -arm64
scp dist/serverwatch-linux-arm64 myserver:/tmp/serverwatch
```

## 2. Install as a systemd service

```bash
sudo /tmp/serverwatch install
```

This copies the binary to `/usr/local/bin/serverwatch`, writes
`/etc/systemd/system/serverwatch.service` (`Restart=always`,
`WantedBy=multi-user.target`), seeds `/etc/serverwatch/config.json` (mode 0600) if
absent, and runs `systemctl daemon-reload` + `enable --now`. The daemon is now
running and will start on every boot.

## 3. Connect Telegram

```bash
sudo serverwatch telegram set-token <token-from-BotFather>
```

Then **send your bot any message** (e.g. `hi`). The daemon captures your chat id
from that first message automatically — no need to set it by hand. Confirm:

```bash
# in Telegram:
/stats      # current CPU/mem/swap/load/temp + disks + internet
/help       # full command list
```

Commands: `/stats` `/status` `/disk` `/net` `/docker` `/services` `/history [days]`
`/down` `/help`.

## 4. Verify discovery and permissions

```bash
sudo serverwatch doctor
```

Prints what the host exposes: docker access method (socket / group / sudo /
unavailable), whether `smartctl` works, thermal-zone count, and the total number
of discovered targets. Anything it can't reach (no docker, no SMART) is reported
as unavailable and simply isn't monitored — it never crashes the daemon.

It also reports the on/off state of every opt-in extended collector
(`container_stats` / `net_throughput` / `services` / `processes` /
`smart_attrs` — see **Storage & retention** below for what each feeds) and
the configured time-series store's current series count plus on-disk size,
e.g. `time-series: 14 series, 2.3 MB on disk (raw+1m)` (or `unavailable` if
the store failed to open) — a quick cardinality/disk-cost sanity check,
especially worth re-running after toggling a `collect.*` key on a small
device.

List every discovered target and its on/off + threshold:

```bash
sudo serverwatch monitor list
```

## 5. Tune what you care about (all optional)

Everything is CLI-managed; changes hot-reload via SIGHUP — including the
sampling cadence (no restart needed, except `storage.*` — see note):

```bash
sudo serverwatch config set thresholds.disk_pct 85     # alert when any fs >= 85%
sudo serverwatch config set thresholds.temp_c 75
sudo serverwatch monitor disable docker:some-noisy-container
sudo serverwatch monitor threshold disk:/boot 70       # per-target override
sudo serverwatch schedule daily 09:00                  # daily digest at 9am
sudo serverwatch schedule weekly mon@09:00             # weekly rollup Monday 9am
sudo serverwatch quiet-hours 23-8                       # suppress non-critical pings overnight
sudo serverwatch config get                             # show the effective config
```

The daemon samples in two tiers (see **Storage & retention** below for
detail):

```bash
sudo serverwatch config set fast_interval 10           # slow down the cheap CPU/mem/swap/load/temp tier
sudo serverwatch config set sample_interval 120        # slow down the pricier disk/docker/systemd/SMART tier
                                                        # (must stay a multiple of fast_interval)
sudo serverwatch config set heartbeat_interval 60       # liveness heartbeat cadence
```

`fast_interval`, `sample_interval`, and `heartbeat_interval` all take effect
on the next tick via SIGHUP — no restart needed.

> `storage.*` keys (backend, retention) are the exception: the running
> time-series store isn't reopened on SIGHUP, so a change only takes effect
> after `sudo systemctl restart serverwatch`.

### Storage & retention

serverwatch's on-disk/live state comes in three shapes — knowing which one a
datapoint lives in tells you whether it's queryable history (`dump`), a live
read of "right now" (`status.json`), or a discrete event record:

**1. Time-series** (`/var/lib/serverwatch/ts/`, the tsfile store) — numeric
metrics on a fixed cadence, queryable via `serverwatch dump --metric <id>`
(see **Exporting/graphing a series** below). The complete series list:
`cpu`, `mem`, `swap`, `load1`, `load5`, `load15`, `temp` (fast tier);
`disk:<mount>` (slow tier, one per filesystem); `docker:<name>:cpu`/`:mem`
(slow tier, opt-in `collect.container_stats`); `net:<iface>:rx`/`:tx` (slow
tier, opt-in `collect.net_throughput`); `smart:<dev>:temp` (slow tier,
opt-in `collect.smart_attrs`). See [DESIGN-storage.md](DESIGN-storage.md)
for the on-disk format and rationale.

**2. Live snapshot** (`status.json`, rewritten every `fast_interval`) — the
current value of every series above, plus fields that are refreshed each
slow tick but never persisted as history: the full systemd unit inventory
(opt-in `collect.services`), a process-table overview of counts + top-N by
CPU/mem (opt-in `collect.processes`), per-container cpu/mem/net stats
(opt-in `collect.container_stats` — note per-container **net** is
snapshot-only, unlike cpu/mem which also feed the series above), live
network rates, per-mount disk detail (device/fstype/inode%/fill-rate
projection, always collected), and SMART health + attributes (attributes
opt-in via `collect.smart_attrs`). Container up/down state, systemd/SMART
health state, the unit list, and the process table are never stored as
series — only `status.json` carries them. See **Data model** in
[README.md](../README.md) for the full field-by-field breakdown.

**3. Event log** — `alertlog.jsonl` (every alert fire/recover + per-channel
delivery outcome, read via `serverwatch alerts`, pruned to ~30d) and
`ts/events.tsd` (downtime events — `power_down`/`net_down`, see **Downtime
tracking** below — event-shaped but stored in the tsfile store since it
shares its retention/downsample machinery).

Config keys:

```bash
sudo serverwatch config get storage.backend            # tsfile (default) | memory
sudo serverwatch config set storage.raw_retention 72h  # default 48h
sudo serverwatch config set storage.rollup_retention 1440h  # default 720h (30d)
```

- `storage.backend` — `tsfile` (default; durable, on-disk) or `memory`
  (non-persistent; mainly for tests).
- `storage.raw_retention` — how long raw-resolution samples are kept before
  being pruned (all raw metrics, fast- and slow-tier alike).
- `storage.rollup_retention` — how long 1-minute rollups and downtime events
  are kept.

The extended slow-tier collectors above are each independently toggleable
and default to **on** (opt-out, not opt-in):

```bash
sudo serverwatch config set collect.container_stats false  # skip `docker stats` (cpu/mem/net)
sudo serverwatch config set collect.net_throughput false   # skip /proc/net/dev rx/tx rates
sudo serverwatch config set collect.services false         # skip the full systemd unit inventory
sudo serverwatch config set collect.processes false        # skip the process-table overview
sudo serverwatch config set collect.smart_attrs false      # skip `smartctl -A` (temp/wear/realloc)
sudo serverwatch config set collect.smart_interval 600      # SMART scan throttle, seconds (default 1800 = 30 min)
```

`collect.smart_interval` (seconds, default `1800` = 30 min) throttles the SMART
scan (`smartctl --scan`/`-H`/`-A`) independently of `sample_interval` — SMART
health rarely changes and the scan is the heaviest slow-tier call. Cached
results still flow to `status.json` and the recovery sweep every slow tick
between scans; set it as low as `sample_interval` to scan every slow tick.

Turning one off is useful on a small device, or a host with hundreds of
short-lived processes/containers, where the default collection is more than
you need. The cheaper always-on checks each opt-in collector sits next to
(container list, `systemctl --failed`, SMART `--scan`/`-H` health) keep
running regardless — only the heavier per-entity detail is gated. Run
`serverwatch doctor` (step 4) after changing any of these to confirm the new
state and see its effect on series count/disk usage.

**Upgrading from a pre-storage-epic install:** older installs wrote
`samples/YYYY-MM-DD.jsonl` + `downtime.jsonl` instead of `ts/`. Run once,
after upgrading the binary:

```bash
sudo serverwatch migrate         # one-shot import into the time-series store
```

It imports every legacy record, then archives the old files to `*.migrated`
(never deletes them). It's idempotent — a marker file makes a second run a
no-op — so it's safe to include in an upgrade script; pass `--force` to force
a re-import.

**Exporting/graphing a series:**

```bash
serverwatch dump --metric cpu --since 24h --format csv > cpu.csv
serverwatch dump --metric disk:/ --since 30d --res 1m --format json
```

`--res raw|1m` picks the resolution explicitly; `--format csv|json` picks the
output shape. Useful for feeding an external plotting tool without standing
up a full metrics stack.

## 6. Alert history & acknowledgement

Every alert notification (fire and recover) is recorded to `alertlog.jsonl`
with its per-channel delivery outcome (see **Event log** in **Storage &
retention** above). Review it with:

```bash
serverwatch alerts                              # same as `alerts list`
serverwatch alerts list --since 12h --limit 50   # narrower window, more history
serverwatch alerts ack disk:/                    # acknowledge an active alert
serverwatch alerts unack disk:/                  # clear the acknowledgement
```

`alerts`/`alerts list` prints two sections: **ACTIVE ALERTS** (key, time
since it fired, reason, and `[acked ... ago]` once acknowledged) and
**HISTORY** — up to `--limit` (default `20`) most-recent-first entries from
the last `--since` (default `24h`), each showing timestamp/kind
(`fire`/`recover`)/key/severity/title plus a per-channel delivery line (`ok`
or `FAILED: <err>`). Acknowledging an active alert doesn't silence a future
re-fire, it just annotates the currently-active one; `ack`/`unack` also
best-effort SIGHUPs the running daemon so the change is picked up
immediately.

## 7. Notification channels (optional)

Telegram (step 3) is the original always-on channel. On top of it, serverwatch
can fan every threshold/baseline alert, the boot/recovery report, and the
daily/weekly digest out to any number of additional channels — email, generic
webhooks, Slack, Discord, [ntfy](https://ntfy.sh), and
[Gotify](https://gotify.net) — each independently enabled and routed. The
healthchecks.io dead-man switch (next step) is separate and not part of this
fan-out.

### The model

Every outbound alert goes to a Dispatcher, which sends it to every
**enabled** channel whose route matches:

- `min_severity` — the lowest severity (`info` | `warning` | `critical`) the
  channel accepts. Default `info` (everything gets through).
- `include_kinds` / `exclude_kinds` — comma-separated target kinds to
  restrict or block a channel to. Valid kinds are `cpu`, `mem`, `swap`,
  `temp`, `disk`, `docker`, `service`, `smart` (the part of a target id
  before `:`, e.g. `disk:/` → `disk`). The boot report and digests carry no
  kind, so they still reach a channel unless `include_kinds` is set.
- `critical_overrides_quiet` — per-channel: if true, `critical` alerts still
  reach this channel during quiet hours (step 5's `quiet-hours` window).

A pre-existing `telegram.token` auto-migrates into a real `telegram`-typed
channel the first time any `channel` subcommand runs, so an existing
Telegram-only install needs no changes — it just shows up in `channel list`.

### Supported channel types

| Type | Required settings | Optional settings |
|------|--------------------|--------------------|
| `telegram` | `chat_id` (unless already migrated from legacy config) | `token` (falls back to legacy `telegram.token`) |
| `email` | `host`, `from`, `to` (comma-separated) | `port` (default `587`), `username`, `password`, `starttls` (default `true`) |
| `webhook` | `url` | `method` (default `POST`), `content_type` (default `application/json`), `template` (a Go `text/template` body; defaults to a generic `{"text": "..."}` payload) |
| `slack` | `url` (Slack incoming-webhook URL) | — (fixed Slack-formatted body) |
| `discord` | `url` (Discord webhook URL) | — (fixed Discord-formatted body, truncated to Discord's 2000-char limit) |
| `ntfy` | `topic` | `server` (default `https://ntfy.sh`), `token` (for protected topics) |
| `gotify` | `server`, `token` (Gotify application token) | — |

All settings above are per-channel key/value pairs, written with `channel set
<name> setting.<key> <value>` (or `--set key=value` at `channel add` time).

### Managing channels

```bash
sudo serverwatch channel add ops-email --type email
sudo serverwatch channel set ops-email setting.host smtp.fastmail.com
sudo serverwatch channel set ops-email setting.from serverwatch@home.lan
sudo serverwatch channel set ops-email setting.to ops@home.lan
sudo serverwatch channel set ops-email min_severity critical    # only page email for criticals
sudo serverwatch channel test ops-email                          # send a synthetic test alert now
sudo serverwatch channel list                                    # every channel + its routing
```

Other useful `channel set` keys: `enabled true|false`, `include_kinds
disk,docker`, `exclude_kinds smart`, `critical_overrides_quiet true|false`.
`channel remove <name>` deletes a channel. Every `channel add`/`set`/`remove`
writes `config.json` and signals the running daemon (`SIGHUP`), same as
`config set` — no restart needed.

## 8. Real-time "server is down NOW" alert (optional)

The daemon reconstructs downtime from its own heartbeat and reports it on the
next boot ("back online, was down 02:14→06:47"). For an *instant* alert while the
box is actually offline, point it at a [healthchecks.io](https://healthchecks.io)
dead-man switch — when the box (or its internet) drops, healthchecks pings your
Telegram directly:

```bash
sudo serverwatch healthchecks set https://hc-ping.com/<your-uuid>
```

## 9. Managing the service

```bash
systemctl status serverwatch            # is it running?
journalctl -u serverwatch -f            # live logs
serverwatch status                      # current snapshot (also /var/lib/serverwatch/status.json)
sudo serverwatch config get             # effective config
sudo systemctl restart serverwatch      # after a storage.* config change or a binary upgrade
```

## 10. Upgrading

Download/build a newer binary and re-run install (it overwrites the binary and
reloads the unit; your config and time-series history are preserved):

```bash
sudo /tmp/serverwatch install
```

Upgrading from a pre-storage-epic install (no `ts/` directory yet)? Run
`serverwatch migrate` once afterward to pull the old JSONL history into the
new store — see **Storage & retention** in step 5.

## 11. Uninstalling

```bash
sudo serverwatch uninstall            # stop + remove the unit (keeps config + history)
sudo serverwatch uninstall --purge    # also delete /etc/serverwatch and /var/lib/serverwatch
```

## On-disk layout

```
/usr/local/bin/serverwatch                  the binary
/etc/serverwatch/config.json                config + secrets (0600)
/etc/systemd/system/serverwatch.service     the unit
/var/lib/serverwatch/
  status.json           current snapshot
  heartbeat             last-alive timestamp (drives downtime reconstruction)
  ts/raw/<metric>.tsd    raw samples, binary (storage.raw_retention, default 48h)
  ts/1m/<metric>.tsd     1-minute rollups (storage.rollup_retention, default 720h/30d)
  ts/events.tsd          power_down + net_down events (rollup_retention window)
  baseline.json          rolling per-metric mean/variance
  alerts.json            active-alert state (dedup + recovery)
  alertlog.jsonl         alert notification history (fire/recover + per-channel
                         delivery outcome), pruned to ~30d — see `serverwatch alerts`
```

Pre-upgrade installs instead have `samples/*.jsonl` + `downtime.jsonl`; run
`serverwatch migrate` to archive them (to `*.migrated`) and import their data
into `ts/` — see **Storage & retention** in step 5.

## Troubleshooting

- **No Telegram replies:** confirm the token (`serverwatch config get`), and that
  you messaged the bot at least once so it captured the chat id. Check
  `journalctl -u serverwatch -f` for API errors.
- **Docker not monitored:** run `serverwatch doctor`. If it says docker
  unavailable, either add the service user to the `docker` group or ensure
  passwordless `sudo docker` works — the daemon falls back to `sudo` automatically
  when the socket isn't directly reachable (it runs as root by default, so the
  socket is normally reachable).
- **SMART unavailable:** `apt install smartmontools`. Some disks/USB bridges don't
  expose SMART; those are simply skipped.
- **Alerts too noisy/quiet:** adjust `thresholds.*`, `baseline_sigma`, or disable
  specific targets with `monitor disable <target>`.
- **A notification channel isn't delivering:** run `serverwatch channel test
  <name>` first — it builds the channel and sends one synthetic alert,
  reporting a clear success/failure independent of routing. If that succeeds
  but real alerts still don't arrive, check `serverwatch channel list` for
  `enabled=false` or a `min_severity`/`include_kinds`/`exclude_kinds` that's
  filtering them out.
