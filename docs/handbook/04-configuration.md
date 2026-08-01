# Configuration

The default way to configure serverwatch is the interactive `serverwatch-ctl`
TUI: guided, validated screens for the schedule, quiet hours, healthchecks,
monitor thresholds, and notification channels, plus first-run Telegram
onboarding. Start it with `sudo serverwatch cli`. Everything you configure day
to day has a screen there, and each change is validated and applied over the
control socket in one step.

This chapter opens with that default path, then documents the underlying
configuration model and the full config-key reference under
[Advanced: direct configuration](#advanced-direct-configuration), for
automation, cron, or a headless box managed without `serverwatch-ctl`.

## Managing configuration with serverwatch-ctl

Run `sudo serverwatch cli` and press `m` from the Home screen to open the
management menu. Each entry is a guided screen that fetches the current config,
lets you edit it with validation, and applies it atomically over the control
socket:

| Setting | serverwatch-ctl screen |
| --- | --- |
| Digest schedule (daily / weekly / off) | Schedule |
| Quiet-hours window | Quiet hours |
| Healthchecks ping URL | Healthchecks |
| Per-target enable/disable and thresholds | Monitor thresholds |
| Notification channels (add / edit / remove / test) | Channels |
| Web UI setup (mode, listen, domain, origin) | the `s` guided web-setup wizard from Home |
| Telegram bot token and enrollment | first-run onboarding |

The Channels screen validates an enabled channel over the socket before it
saves, so a channel that would fail to deliver can never be persisted while
turned on. For the full screen-by-screen walkthrough, see [Managing with
serverwatch-ctl](plugins/serverwatch-ctl.md#managing-with-serverwatch-ctl).

For automation, scripting, or a box without `serverwatch-ctl`, every one of
these settings is also a scriptable `serverwatch` CLI verb, documented in
[Advanced: direct configuration](#advanced-direct-configuration) below and
collected in [Daemon-only config
management](11-command-reference.md#3-daemon-only-config-management).

## Advanced: direct configuration

The rest of this chapter is the low-level reference behind those screens: the
config file, the scriptable CLI verbs, and every config key. You need it for
automation or a headless box; day to day, the screens above set all of this
for you.

Every setting in serverwatch lives in a single JSON file at
`/etc/serverwatch/config.json`, but you never open that file in an editor. Both
the `serverwatch-ctl` screens and the CLI verbs below write it for you through
the same validated path. There is no environment variable layer and no
hand-edited file: the JSON on disk is just where those tools persist what you
told them. This keeps validation in one place (a bad value is rejected at set
time, not discovered at runtime) and keeps the file's permissions and format
under the tool's control.

The general-purpose entry points are three verbs on `config`:

```bash
serverwatch config get [key]        # print every effective key, or just one
serverwatch config set <key> <val>  # set a key, e.g. config set sample_interval 30
serverwatch config unset <key>      # revert a key to its baked-in default
```

`config get` with no key prints the full effective configuration: the baked-in
defaults with your overrides applied on top, so what you see is what the daemon
actually runs with. `config unset` copies the default value back into the key,
so it is the reliable way to undo a change without remembering what the default
was.

On top of those generic verbs, a handful of dedicated subcommands wrap the keys
you touch most often, with friendlier syntax and their own validation:

```bash
serverwatch telegram set-token <token>          # writes telegram.token
serverwatch monitor enable|disable|threshold    # per-target overrides (see below)
serverwatch schedule daily HH:MM | off          # writes schedule.daily
serverwatch schedule weekly dow@HH:MM | off      # writes schedule.weekly
serverwatch quiet-hours HH-HH | off             # writes quiet_hours
serverwatch healthchecks set <url> | off        # writes healthchecks.url
serverwatch channel add|list|remove|set|test    # notification channels
```

These are equivalent to the matching `config set` calls, just with a verb-shaped
interface. Use whichever reads better for the task. Every one of them, plus
`config get/set/unset` itself, also has a guided, validated screen in
`serverwatch-ctl`; see [Daemon-only config
management](11-command-reference.md#3-daemon-only-config-management) for the
automation-focused view of this same command set.

### Persistence and hot reload

Every write, whether through `config set` or a dedicated verb, does two things.
First it persists the change with an atomic write: the new JSON is written to a
temporary sibling file, tightened to mode `0600` (the file holds your Telegram
token in plaintext, so it stays root-readable only), then renamed over the live
file. A rename on the same filesystem is atomic, so a reader or a crash mid-write
always sees either the complete old file or the complete new one, never a
truncated one. The file is owned by root and lives at
`/etc/serverwatch/config.json`.

Second, the CLI sends `SIGHUP` to the running daemon, which reloads the config
live on its next tick. Interval changes such as `fast_interval`,
`sample_interval`, and `heartbeat_interval` reset the sampler ticker in place, so
a new cadence takes effect on the very next tick with no restart. Thresholds,
schedules, quiet hours, channels, and the rest all hot-reload the same way.

There is one exception. The `storage.*` keys (`storage.backend`,
`storage.raw_retention`, `storage.rollup_retention`) are read only when the
time-series store is opened at startup. The running store is not reopened on
`SIGHUP`, so a storage change is persisted immediately but does not take effect
until you restart the service:

```bash
sudo systemctl restart serverwatch
```

### Per-target overrides are not `config set`

Some state is keyed per discovered target rather than being a single global
value: whether a specific target is alerting at all, and a per-target threshold
that overrides the global one. These are managed through `serverwatch monitor`,
not `config set`, and are stored under `targets.<id>` in the config file rather
than as a flat key:

```bash
serverwatch monitor list                       # discovered targets + on/off + effective threshold
serverwatch monitor disable docker:jellyfin    # stop alerting on one target
serverwatch monitor enable docker:jellyfin     # re-enable it
serverwatch monitor threshold disk:/ 95        # per-target threshold override
```

A target id is namespaced by kind, for example `docker:<container>`,
`disk:<mount>`, `iface:<name>`, `temp:<zone-path>`, or `smart:<device>`.
Per-target overrides are not settable with `config set <key>` and will not
appear as flat keys in `config get`; use the `monitor` verb for them.

In `serverwatch-ctl`, this is the **Monitor thresholds** screen: a live list
of every discovered target where `enter`/`space` toggles a target on or off
and `t` opens a threshold-edit input for it, each applying immediately.

## Config key reference

Everything below is a key accepted by `config get`, `config set`, and
`config unset`. Booleans accept the usual `true`/`false` (and the other forms
Go's `strconv.ParseBool` accepts). Durations are strings in Go's
`time.ParseDuration` syntax, for example `48h` or `30m`, and must be positive.

### Intervals

The sampler runs two tiers plus an independent heartbeat (see [Monitoring: what
gets collected](05-monitoring.md)). The slow tier fires every Nth fast tick, so
`sample_interval` must be an exact multiple of `fast_interval`.

| Key | Default | Validation |
|-----|---------|------------|
| `fast_interval` | `5` | Integer seconds, minimum 1. Rejected if lowering it would leave `sample_interval` no longer a multiple of it (adjust `sample_interval` first). |
| `sample_interval` | `60` | Integer seconds, minimum 5, and must be an exact integer multiple of `fast_interval`. |
| `heartbeat_interval` | `30` | Integer seconds, minimum 1. Independent of both tiers. |

```bash
serverwatch config set fast_interval 5
serverwatch config set sample_interval 60
```

### Baseline and anomaly

The rolling baseline drives z-score (deviation) anomaly detection. Deviation
alerting is off by default; only threshold alerting runs unless you opt in.

| Key | Default | Validation |
|-----|---------|------------|
| `baseline_sigma` | `3` | Float (number of standard deviations for a deviation alert). |
| `baseline_min_pct` | `0.15` | Float, must be at least 0. The minimum relative deviation (fraction of the baseline mean, so `0.15` means 15%) a value must also clear, alongside `baseline_sigma`, before a baseline anomaly fires. Suppresses flapping on noisy-but-stable metrics. |
| `baseline_alerts` | `false` | Boolean. Opt-in gate for baseline (z-score) deviation alerting on cpu/mem/swap/temp/disk. Threshold alerting on those same metrics is always on regardless of this setting. Off by default because a metric with a low, unstable mean can read many sigma from its own baseline on a normal wobble and fire every tick. |

```bash
serverwatch config set baseline_alerts true    # opt in to deviation alerts
serverwatch config set baseline_min_pct 0.2
```

### Thresholds

Global threshold ceilings. When a metric crosses its threshold, an alert fires.
Each scalar check (`cpu`, `mem`, `swap`, `temp`, and each `disk:<mount>`) also
supports a per-target override via `serverwatch monitor threshold`.

| Key | Default | Validation |
|-----|---------|------------|
| `thresholds.disk_pct` | `90` | Float percent. |
| `thresholds.temp_c` | `80` | Float degrees Celsius. |
| `thresholds.cpu_pct` | `95` | Float percent. |
| `thresholds.mem_pct` | `90` | Float percent. |
| `thresholds.swap_pct` | `50` | Float percent. |

```bash
serverwatch config set thresholds.disk_pct 85
```

### Quiet hours

Suppress non-critical pings during a window that may wrap midnight.

| Key | Default | Validation |
|-----|---------|------------|
| `quiet_hours` | empty | `"HH-HH"` with each hour 0 to 23, or empty to disable. For example `23-8`. |
| `critical_overrides_quiet` | `true` | Boolean. When true, alerts flagged critical (currently any `disk:<mount>` threshold breach) still get through during quiet hours. |

```bash
serverwatch quiet-hours 23-8          # or: serverwatch config set quiet_hours 23-8
serverwatch quiet-hours off           # clears it
```

In `serverwatch-ctl`, this is the **Quiet hours** screen in the management
menu (`m`): a single `HH-HH` window, or `off` to clear it. The command above
is the scriptable equivalent for automation.

### Telegram

The original always-on notification channel (see [Alerting and notification
channels](06-alerting-and-channels.md)). A token is the one required setting for
a fresh install.

| Key | Default | Validation |
|-----|---------|------------|
| `telegram.token` | empty | None (free-form string). Prefer `serverwatch telegram set-token <token>`. |
| `telegram.chat_id` | empty | None. Normally set by enrolling the owner chat via `/start <pin>`. |

```bash
serverwatch telegram set-token 123456:ABC-DEF   # or: config set telegram.token ...
```

`telegram set-token` saves the token, reloads the daemon, and then prints the
`/start <pin>` enrollment instruction straight to the terminal (falling back
to a `journalctl` pointer if the daemon cannot be reached yet). In
`serverwatch-ctl`, the same setup runs as the **first-run onboarding** flow
(token entry, then the same `/start <pin>` shown and polled for you); once
onboarding is complete, the token can be changed later from the **Channels**
screen. See
[Installation and first run](03-installation.md#5-connect-telegram-and-enroll-as-owner)
for the full enrollment flow and diagram.

### Healthchecks

Optional healthchecks.io dead-man switch. When set, every slow tick pings the
URL; if the box or its internet goes down, pings stop and healthchecks.io alerts
you directly.

| Key | Default | Validation |
|-----|---------|------------|
| `healthchecks.url` | empty | None (free-form string). |

```bash
serverwatch healthchecks set https://hc-ping.com/your-uuid   # config set healthchecks.url ...
serverwatch healthchecks off
```

In `serverwatch-ctl`, this is the **Healthchecks** screen in the management
menu: a single ping URL, or `off` to clear it.

### Schedule

Digest delivery times, in 24-hour local time.

| Key | Default | Validation |
|-----|---------|------------|
| `schedule.daily` | empty | `"HH:MM"` (hour 00 to 23, minute 00 to 59), or empty to disable. |
| `schedule.weekly` | empty | `"dow@HH:MM"` where `dow` is one of `sun`, `mon`, `tue`, `wed`, `thu`, `fri`, `sat`, or empty to disable. For example `mon@09:00`. |

```bash
serverwatch schedule daily 08:00       # config set schedule.daily 08:00
serverwatch schedule weekly mon@09:00
serverwatch schedule daily off
```

In `serverwatch-ctl`, this is the **Schedule** screen in the management
menu: choose `off`, `daily`, or `weekly`, pre-filled with whatever is
currently set.

### Storage

The time-series backend and its retention windows (detailed in [Storage and the
data model](09-storage-and-data-model.md)). These keys take effect only after
`systemctl restart serverwatch`; a `SIGHUP` reload does not reopen the running
store.

| Key | Default | Validation |
|-----|---------|------------|
| `storage.backend` | `tsfile` | One of `tsfile` or `memory`. |
| `storage.raw_retention` | `48h` | Duration string, must be positive. How long raw samples are kept. |
| `storage.rollup_retention` | `720h` | Duration string, must be positive. How long 1-minute rollups and downtime events are kept (720h is 30 days). |

```bash
serverwatch config set storage.raw_retention 72h
sudo systemctl restart serverwatch     # required for storage.* changes
```

### Collection toggles

The extended collectors are opt-out: unset or absent means enabled. Turn one off
on a small device, or on a host with hundreds of short-lived
processes/containers, where the default collection is more than you need.
`serverwatch doctor` prints the current on/off state of all five plus the
resulting series count and disk usage.

| Key | Default | Validation |
|-----|---------|------------|
| `collect.container_stats` | `true` | Boolean. Slow-tier per-container `docker stats` (cpu/mem/net). |
| `collect.net_throughput` | `true` | Boolean. Slow-tier per-interface `/proc/net/dev` throughput. |
| `collect.services` | `true` | Boolean. Slow-tier full systemd unit inventory. The `systemctl --failed` alerting collection always runs regardless. |
| `collect.processes` | `true` | Boolean. Slow-tier process-table overview (counts plus top-N by CPU/mem). |
| `collect.smart_attrs` | `true` | Boolean. Slow-tier per-device `smartctl -A` attribute reads. The cheaper `--scan`/`-H` health check always runs regardless. |
| `collect.smart_interval` | `1800` | Integer seconds, minimum 1. Throttles the SMART scan (30 minutes by default). Set as low as `sample_interval` to scan every slow tick. |

```bash
serverwatch config set collect.processes false     # stop collecting the process table
serverwatch config set collect.smart_interval 3600
```

### Web

Settings for the web UI, read by the separate `serverwatch-web` binary (no
build tag) when the daemon supervises it or when it is launched with
`serverwatch web`. The default `serverwatch` binary never reads them, but the
keys are still manageable so config is portable across binaries. These are
covered in detail in the [Web UI chapter](08-web-ui.md); the reference is
repeated here for completeness.

| Key | Default | Validation |
|-----|---------|------------|
| `web.enabled` | `false` | Boolean. Turns on daemon supervision of `serverwatch-web`. Opt-in even when the binary is installed. |
| `web.listen` | `127.0.0.1:8088` | `host:port` (parsed with `net.SplitHostPort`). Localhost-only by default; front it with a reverse proxy for LAN/WAN. |
| `web.mode` | `proxy` | One of `proxy`, `autocert`, or `manual`. |
| `web.rp_id` | empty | None. WebAuthn relying party ID (public hostname, no scheme/port). Required in autocert/manual modes; derived per-request in proxy mode. |
| `web.origin` | empty | None. Full public origin (`https://host[:port]`) passkey ceremonies validate against; its host must match `web.rp_id`. Required in autocert/manual modes. |
| `web.autocert_domains` | empty | None. Comma-separated hostname allowlist for autocert. Required (non-empty) in autocert mode. |
| `web.tls_cert` | empty | None. PEM cert path. Required in manual mode. |
| `web.tls_key` | empty | None. PEM key path. Required in manual mode. |
| `web.session_ttl` | `24h` | Duration string, must be positive. How long a signed-in web session stays valid. |

### Public view

The admin-curated anonymous status page. Both fields default to off/empty:
nothing is exposed anonymously until an admin enables it and curates which panels
are visible. When `public.enabled` is false, the route returns 404 rather than
revealing that a public page exists.

| Key | Default | Validation |
|-----|---------|------------|
| `public.enabled` | `false` | Boolean. Toggles `GET /public`. |
| `public.panels` | empty | Comma-separated allowlist. Each entry must be one of `availability`, `cpu`, `mem`, `swap`, `load`, `temp`, `uptime`, `services`, `containers`, `net`, or `disk:<mount>` (with a non-empty mount suffix). Any other value is rejected and the whole list is refused. |

```bash
serverwatch config set public.enabled true
serverwatch config set public.panels availability,cpu,mem,uptime,disk:/
```

The `public.panels` list is the server-side-enforced source of truth for what
the anonymous page can render. A metric absent from this list can never appear
on `/public` even though it exists elsewhere, so curating this list is the whole
of the public page's exposure surface.

---

[Previous: Installation and first run](03-installation.md) | [Handbook index](README.md) | [Next: Monitoring: what gets collected](05-monitoring.md)
