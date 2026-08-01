# Command reference

This chapter lists every command shipped by serverwatch. Section 1 covers the
`serverwatch` binary, which is both the daemon and the operator CLI. Section 2
covers the two out-of-process plugin binaries, `serverwatch-ctl` and
`serverwatch-web`, neither of which is part of the shipped daemon binary
itself. `serverwatch-ctl` is BETA / preview, and it is now the primary,
recommended way to manage a running serverwatch day to day: it wraps the
schedule, quiet hours, healthchecks, monitor thresholds, and notification
channels in guided, validated screens, plus a first-run onboarding flow for
Telegram. `serverwatch-web` is a supervised, separate binary that the daemon
manages (see [Web supervisor](02-architecture.md#web-supervisor)). Section 3
is the authoritative reference for the thin, scriptable management verbs,
gathered in one place for anyone managing serverwatch without
`serverwatch-ctl`.

## 1. `serverwatch` (daemon + CLI)

`serverwatch` dispatches a single subcommand per invocation. Run it with no
arguments, `help`, `-h`, or `--help` to print usage.

```
serverwatch <command> [args]
```

Every command falls into one of two modes:

- **read-only**: reads config or on-disk state and prints; nothing is written.
- **persists + SIGHUP**: writes the change to disk, then sends a best-effort
  SIGHUP to a running daemon so it hot-reloads the new config. The SIGHUP is a
  no-op when the daemon is not running (the write to disk still happens, and the
  daemon picks it up on next start).

A few commands write other on-disk state (systemd units, the time-series store,
alert ack state) rather than the config file; those are called out in the Notes
column.

### Commands

| Command | Purpose | Mode |
| --- | --- | --- |
| `config get [key]` | Print the full effective config, or one key. | read-only |
| `config set <key> <value>` | Set one config key. | persists + SIGHUP |
| `config unset <key>` | Reset one config key to its default. | persists + SIGHUP |
| `install` | Write the systemd unit and enable/start the service. | persists (see Notes) |
| `uninstall [--purge]` | Remove the systemd unit; `--purge` also removes config and state. | persists (see Notes) |
| `daemon` | Run the sampler/notifier loop in the foreground. | long-running |
| `cli` | Front-door: verify and exec `serverwatch-ctl`, the management TUI. | see Notes |
| `web` | Front-door: verify and exec `serverwatch-web`, the web UI. | see Notes |
| `status` | Print the last status snapshot. | read-only |
| `doctor` | Print a diagnostic report (collectors, tools, targets). | read-only |
| `migrate [--force]` | Import legacy data into the time-series store. | persists (see Notes) |
| `dump --metric <id> [...]` | Export one metric's series to stdout. | read-only |
| `alerts [list] [...]` | List recent alerts. | read-only |
| `alerts ack <key>` | Acknowledge an active alert. | persists (see Notes) |
| `alerts unack <key>` | Un-acknowledge an alert. | persists (see Notes) |

This section covers the daemon, lifecycle, and low-level scriptable commands.
The day-to-day management verbs (`monitor`, `schedule`, `quiet-hours`,
`healthchecks`, `channel`, and Telegram onboarding) are not listed here: the
primary, recommended way to drive them is the interactive
[`serverwatch-ctl`](plugins/serverwatch-ctl.md) TUI, and their thin scriptable
forms for automation live in [Daemon-only config
management](#3-daemon-only-config-management).

### Notes

| Command | Detail |
| --- | --- |
| `config set`/`config unset` | `storage.*` changes are persisted and SIGHUP is still sent, but the time-series store is opened once at daemon start and is not re-opened on reload. A `storage.backend` or retention change needs a daemon restart to take effect. |
| `install` / `uninstall` | These write or remove systemd unit files and toggle the service; they do not send a config SIGHUP. `uninstall --purge` additionally deletes the config file and state directory. |
| `migrate` | Writes into the time-series store, not the config file; no SIGHUP is sent. `--force` proceeds past guards. |
| `alerts ack` / `alerts unack` | Writes the alert ack-state file and then sends a best-effort SIGHUP so a running daemon re-reads it. |
| `cli` / `web` | Neither writes config nor sends a SIGHUP. Each resolves and verifies the matching plugin binary next to the core binary, then hands off to it; see the front-door detail below. |

### `dump` flags

`dump` exports a single metric's series.

| Flag | Values | Default | Meaning |
| --- | --- | --- | --- |
| `--metric <id>` | e.g. `cpu`, `mem`, `disk:/` | required | Metric id to export. |
| `--since <dur>` | Go duration, e.g. `24h`, `30m` | `24h` | How far back to query. |
| `--res <res>` | `raw` \| `1m` | `raw` | Resolution to read. |
| `--format <fmt>` | `csv` \| `json` | `csv` | Output format. |

```
serverwatch dump --metric cpu --since 24h --res 1m --format json
```

### `alerts` flags

`alerts list` (the default when no subcommand is given) accepts:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--since <dur>` | `24h` | How far back to list. |
| `--limit <n>` | `20` | Maximum number of alerts to print. |

```
serverwatch alerts --since 24h --limit 20
serverwatch alerts ack cpu:high
serverwatch alerts unack cpu:high
```

### `cli` and `web` front-doors

```
serverwatch cli
serverwatch web
```

`cli` and `web` are front-doors: the one-command way to launch the two plugin
binaries documented in section 2, without needing to know their binary names
or where they live. `cli` execs `serverwatch-ctl`; `web` execs
`serverwatch-web`. Both are typically run with `sudo`, because that is how the
daemon itself runs, and launching a plugin as root means the core must first
prove it is about to exec the genuine binary it installed, not something an
attacker planted or modified. See [Architecture](02-architecture.md) for the
full trust model (absolute path resolved from the core binary's own
directory, owner and permission checks, and a SHA-256 checksum against the
manifest `serverwatch install` writes).

There are three outcomes:

| Outcome | What you see |
| --- | --- |
| Verified | The plugin runs; the front-door hands off control to it. |
| Not installed | serverwatch prints an install/build instruction, e.g. `go build -o /usr/local/bin/serverwatch-ctl ./cmd/serverwatch-ctl` (the web plugin builds the same way, no tag: `go build -o /usr/local/bin/serverwatch-web ./cmd/serverwatch-web`), followed by a reminder to run `serverwatch install` to record its checksum. Nothing is exec'd. |
| Present but unsafe | The binary exists but fails a check (wrong owner, group/world-writable, or a checksum that does not match the manifest). serverwatch refuses with a warning that this may indicate tampering. Nothing is exec'd. |

If you build or hand-copy `serverwatch-ctl` / `serverwatch-web` into place
yourself, you must (re-)run `serverwatch install` afterward so its checksum
is recorded (see [Installation and first run](03-installation.md)); until
then the front-door has nothing to verify the unrecorded binary against and
refuses to run it.

## 2. Plugin binaries

The two out-of-process plugin binaries now have their own dedicated pages.
Both are separate from the shipped `serverwatch` daemon and dial its control
socket rather than reading state in-process; in normal use you launch them via
the `serverwatch cli` / `serverwatch web` front-doors (section 1 above).

- **[Plugins overview](plugins/README.md)** -- what the plugins are, how they
  install alongside the daemon, and the front-door safe-exec model.
- **[serverwatch-ctl](plugins/serverwatch-ctl.md)** -- the interactive
  management TUI (beta): subcommands, socket/token resolution, and the full
  management screens and first-run onboarding.
- **[serverwatch-web](plugins/serverwatch-web.md)** -- the supervised web
  binary: how the daemon runs it, and its direct-invocation flags.

## 3. Daemon-only config management

`serverwatch-ctl` (section 2.1) is the primary way to manage a running
serverwatch, but every flow it offers has a thin, scriptable equivalent on
the core `serverwatch` binary itself. This section is the authoritative
reference for those scriptable verbs, for automation, cron jobs,
configuration-management tooling, or a box you administer entirely over SSH
without `serverwatch-ctl` installed. Prefer `serverwatch-ctl` for day-to-day,
interactive management; reach for these verbs when you are scripting a change
or managing headless.

| Command | Purpose |
| --- | --- |
| `config get [key]` / `config set <key> <value>` / `config unset <key>` | Read or write any config key directly. `config set` is the scriptable escape hatch for anything a guided screen does not cover. |
| `monitor list` / `monitor enable\|disable <target>` / `monitor threshold <target> <value>` | Discover targets and set per-target on/off state and threshold overrides. |
| `schedule daily HH:MM` / `schedule weekly dow@HH:MM` / `schedule off` | Set or clear the digest schedule. |
| `quiet-hours HH-HH` / `quiet-hours off` | Set or clear the quiet-hours window. |
| `healthchecks set <url>` / `healthchecks off` | Set or clear the healthchecks.io ping URL. |
| `channel list\|add\|remove\|set\|test` | List, add, remove, modify, or test-notify a notification channel. |
| `telegram set-token <token>` | Store the Telegram bot token and print the enrollment PIN (#90, below). |

Every one of these persists to `/etc/serverwatch/config.json` and sends a
best-effort `SIGHUP` to reload a running daemon (the persists-plus-SIGHUP
model described in section 1 and in [Configuration](04-configuration.md)).
Full flag syntax, validation, and defaults for each key live in the
[Configuration chapter's key
reference](04-configuration.md#config-key-reference).

### #90: `telegram set-token` now prints the enrollment PIN

`serverwatch telegram set-token <token>` used to only persist the token; you
had to go read the daemon's journal to find the one-time enrollment PIN it
generated. It now dials the control socket after saving the token and prints
the `/start <pin>` instruction directly:

```
Telegram token saved. To finish enrollment, from your Telegram account message the bot:
  /start 123456
```

If the daemon cannot be reached, for instance because it is not installed or
is still starting, `telegram set-token` still saves the token and exits
successfully; it just falls back to pointing you at the journal instead:

```
Telegram token saved. The daemon will log the enrollment PIN on start:
  journalctl -u serverwatch | grep /start
```

A failed PIN fetch is never treated as `telegram set-token` failing; only the
printed message changes. See the enrollment diagram in [Installation and
first run](03-installation.md#5-connect-telegram-and-enroll-as-owner) for how
this fits together with `serverwatch-ctl`'s onboarding screen, which shows
the same PIN.

### #91: guided setup lives in serverwatch-ctl; `config set` is the escape hatch

The guided, validated walk-through for web UI setup and first-run Telegram
onboarding lives in `serverwatch-ctl` (section 2.1), not in the core CLI. The
core CLI deliberately does not grow an interactive wizard of its own:
`config set` and the dedicated verbs above remain the direct, scriptable way
to write any of the same keys, so nothing you could do before is gone, and
configuration-management tooling never has to shell out to an interactive
program to change a setting.

---

[Previous: Operations](10-operations.md) | [Handbook index](README.md) | [Next: Roadmap and status](12-roadmap-and-status.md)
