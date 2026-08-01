# Command reference

This chapter lists every command shipped by ServerWatch. Section 1 covers the
`serverwatch` binary, which is both the daemon and the operator CLI. Section 2
covers the two out-of-process plugin binaries, `serverwatch-ctl` and
`serverwatch-web`, neither of which is part of the shipped daemon binary
itself. `serverwatch-ctl` is BETA / preview, and it is now the primary,
recommended way to manage a running serverwatch day to day: it wraps the
schedule, quiet hours, healthchecks, monitor thresholds, and notification
channels in guided, validated screens, plus a first-run onboarding flow for
Telegram. `serverwatch-web` is a supervised, separate binary that the daemon
manages (see [Web supervisor](02-architecture.md#web-supervisor)). Section 3
collects the thin, scriptable verbs from section 1 into a single
automation-focused reference for anyone managing serverwatch without
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
| `telegram set-token <token>` | Store the Telegram bot token. | persists + SIGHUP |
| `monitor list` | List discovered monitor targets and their on/off state. | read-only |
| `monitor enable\|disable <target>` | Turn a monitor target on or off. | persists + SIGHUP |
| `monitor threshold <target> <value>` | Set a target's alert threshold. | persists + SIGHUP |
| `schedule daily HH:MM` | Set the daily digest time. | persists + SIGHUP |
| `schedule weekly dow@HH:MM` | Set the weekly digest day and time. | persists + SIGHUP |
| `schedule off` | Disable the scheduled digest (daily or weekly). | persists + SIGHUP |
| `quiet-hours <HH-HH>` | Set a quiet-hours window that suppresses alerts. | persists + SIGHUP |
| `quiet-hours off` | Clear the quiet-hours window. | persists + SIGHUP |
| `healthchecks set <url>` | Set the healthchecks.io ping URL. | persists + SIGHUP |
| `healthchecks off` | Clear the healthchecks ping URL. | persists + SIGHUP |
| `channel list` | List configured notification channels. | read-only |
| `channel add ...` | Add a notification channel. | persists + SIGHUP |
| `channel remove ...` | Remove a notification channel. | persists + SIGHUP |
| `channel set ...` | Modify a notification channel. | persists + SIGHUP |
| `channel test ...` | Send a test notification to a channel. | read-only (see Notes) |
| `status` | Print the last status snapshot. | read-only |
| `doctor` | Print a diagnostic report (collectors, tools, targets). | read-only |
| `migrate [--force]` | Import legacy data into the time-series store. | persists (see Notes) |
| `dump --metric <id> [...]` | Export one metric's series to stdout. | read-only |
| `alerts [list] [...]` | List recent alerts. | read-only |
| `alerts ack <key>` | Acknowledge an active alert. | persists (see Notes) |
| `alerts unack <key>` | Un-acknowledge an alert. | persists (see Notes) |

### Notes

| Command | Detail |
| --- | --- |
| `config set`/`config unset` | `storage.*` changes are persisted and SIGHUP is still sent, but the time-series store is opened once at daemon start and is not re-opened on reload. A `storage.backend` or retention change needs a daemon restart to take effect. |
| `install` / `uninstall` | These write or remove systemd unit files and toggle the service; they do not send a config SIGHUP. `uninstall --purge` additionally deletes the config file and state directory. |
| `channel test` | Sends a live test notification through the channel; it does not write config, so it is read-only with respect to config. |
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

The following two binaries are separate from the shipped `serverwatch` daemon.
Both connect to a running daemon over its control socket (see
[Architecture](02-architecture.md)) rather than reading state in-process. In
normal use you do not invoke either binary directly; run `serverwatch cli` /
`serverwatch web` instead (section 1 above), which safely locates and execs
them. The direct invocations below still apply once launched, and remain
useful when scripting or working from a non-standard install location.

`serverwatch-ctl` is preview-quality: it is built and wired up by hand, and
nothing supervises its process. `serverwatch-web` is a supervised, separate
binary with no build tag: the daemon verifies and spawns it as a child
process when `web.enabled` is set, restarting it with capped backoff if it
exits and stopping it on daemon shutdown; see [Web
supervisor](02-architecture.md#web-supervisor).

### 2.1 `serverwatch-ctl` (BETA)

`serverwatch-ctl` is a separate client binary that dials the daemon's control
socket. Run it with a subcommand (`status`, `doctor`, `alerts`) for a one-shot,
non-interactive read that mirrors the daemon-side output. Run it with no
subcommand to launch the interactive Bubble Tea TUI, which is now the primary
way to manage a running serverwatch.

```
serverwatch-ctl [--socket PATH] [--token PATH] <command>
```

| Command | Purpose |
| --- | --- |
| `status` | Print the current dashboard snapshot. |
| `doctor` | Print the daemon's diagnostic report. |
| `alerts` | Print the currently active alerts. |

#### Managing with serverwatch-ctl

Every screen the interactive TUI offers follows the same shape: fetch the
current `Config` over the control socket, mutate it with a validated
`config.Set` setter, and commit it in one atomic `ApplyConfig` call. Nothing
is written straight to disk by `serverwatch-ctl` itself, and a screen can
never leave the daemon with a half-applied change. Where a screen needs to
know what the host actually looks like, it asks the daemon rather than
probing locally: the Monitor thresholds screen lists targets from the
daemon's own `MonitorTargets`, and an enabled channel is checked with
`ValidateChannel` before it is ever saved.

**The Home screen.** Launching `serverwatch-ctl` with no subcommand opens
Home, which shows live status. From Home:

| Key | Action |
| --- | --- |
| `m` | Open the management menu (below). |
| `s` | Launch the guided web setup wizard, applying `web.*` over the socket. |
| `r` | Force an immediate status refresh. |
| `q` / `esc` | Quit. |

If Telegram is not yet configured, or is configured but not yet enrolled,
Home opens straight into first-run onboarding instead (below), rather than
showing a dashboard with nothing to alert you.

**The management menu.** Pressing `m` from Home opens a menu of config-backed
flows: **schedule**, **quiet hours**, **healthchecks**, **monitor
thresholds**, and **channels**. Move with the up/down arrows or `j`/`k`, open
the highlighted row with `enter`, and back out with `esc`.

- **Schedule.** Choose `off`, `daily`, or `weekly`. `daily` prompts for an
  `HH:MM` time; `weekly` prompts for `dow@HH:MM`, for example `mon@09:00`.
  The screen pre-fills whatever is currently set, so accepting the current
  mode re-applies the existing value rather than an empty one.
- **Quiet hours.** A single `HH-HH` window, or `off` to clear it.
- **Healthchecks.** A single healthchecks.io ping URL, or `off` to clear it.
- **Monitor thresholds.** A live list of every target the daemon has
  discovered (fetched from the daemon, not probed locally). `enter`/`space`
  toggles the target under the cursor on or off, `t` opens a threshold-edit
  input for it, and each change applies immediately, one `ApplyConfig` per
  toggle or edit.
- **Channels.** A list of every configured notification channel. `a` adds a
  new one, walking through its type (telegram, email, webhook, slack,
  discord, ntfy, or gotify) and that type's fields; `e` edits the channel
  under the cursor; `d`/`x` removes it; `t` sends it a live test notification.
  Before an **enabled** channel is saved, the screen validates it over the
  socket and refuses to persist it if validation fails, so a channel that
  would silently fail to deliver can never be saved while turned on. A
  disabled channel skips that gate, since there is nothing it can misdeliver
  while off, which is how you stage a channel's settings before switching it
  on.

> To manage serverwatch without `serverwatch-ctl`, for scripting, automation,
> or a headless box, see [Daemon-only config
> management](#3-daemon-only-config-management) below.

**First-run onboarding.** The first time `serverwatch-ctl` runs against a
daemon whose Telegram bot has no token, or has a token but is not yet
enrolled, Home opens into a guided flow instead of the dashboard:

1. **Bot token.** Paste the token from @BotFather. `enter` saves it; `esc`
   skips onboarding for now, since the token can always be set later from the
   Channels screen or `serverwatch telegram set-token`.
2. **Enrollment PIN.** Once the token is saved, the screen fetches the
   daemon's current enrollment PIN over the socket and shows it with the
   `/start <pin>` instruction, the exact PIN `serverwatch telegram set-token`
   itself now prints (see #90 in [Daemon-only config
   management](#3-daemon-only-config-management)). It then polls every two
   seconds until the daemon reports the chat enrolled.

See [Installation and first run](03-installation.md#5-connect-telegram-and-enroll-as-owner)
for the enrollment flow diagram, which covers both this screen and
`telegram set-token`.

Socket and token resolution, highest priority first:

| Source | Socket | Token |
| --- | --- | --- |
| Flag | `--socket PATH` | `--token PATH` |
| Environment | `SERVERWATCH_CONTROL_SOCKET` | `SERVERWATCH_CONTROL_TOKEN` |
| Default | `$RUNTIME_DIRECTORY/control.sock`, else `/run/serverwatch/control.sock` | the sibling `token` file next to the resolved socket |

A missing token file is treated as no-auth (the daemon serves without a token
when it could not generate one). Any other token read error is fatal.

### 2.2 `serverwatch-web`

`serverwatch-web` is the out-of-process web plugin. See the [Web UI
chapter](08-web-ui.md) for what it serves. It dials the control socket and
runs the dashboard as a separate process. It is a plain binary with no build
tag, built by the Makefile alongside `serverwatch` and `serverwatch-ctl`, and
the daemon supervises it (verify, spawn, restart with backoff, stop on
shutdown) whenever `web.enabled` is set; see [Web
supervisor](02-architecture.md#web-supervisor).

```
serverwatch-web [-socket PATH] [-token TOKEN] [-token-file PATH] \
  [-state-dir DIR] [-alert-log PATH] [-alert-state PATH]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-socket` | `$SERVERWATCH_CONTROL_SOCKET`, else `$RUNTIME_DIRECTORY/control.sock`, else `/run/serverwatch/control.sock` | Control socket to dial. |
| `-token` | `$SERVERWATCH_CONTROL_TOKEN`, else read from `-token-file` | Control socket auth token, given directly. |
| `-token-file` | sibling `token` file next to the socket | File to read the token from when `-token` is not set. |
| `-state-dir` | `/var/lib/serverwatch` | Web UI state directory (sessions, enrollment tokens). |
| `-alert-log` | `<state-dir>/alertlog.jsonl` | Path to the alert log. |
| `-alert-state` | `<state-dir>/alerts.json` | Path to the alert ack-state file. |

The socket and token resolution order matches `serverwatch-ctl`: flag, then
environment, then the systemd/by-hand default. As with the daemon, a missing
token file means no-auth when no token was supplied directly.

## 3. Daemon-only config management

`serverwatch-ctl` (section 2.1) is the primary way to manage a running
serverwatch, but every flow it offers has a thin, scriptable equivalent on
the core `serverwatch` binary itself. Nothing here is new: these are the same
commands already documented in section 1's table, gathered in one place under
the lens that matters for automation, cron jobs, configuration-management
tooling, or a box you administer entirely over SSH without
`serverwatch-ctl` installed. Prefer `serverwatch-ctl` for day-to-day,
interactive management; reach for these verbs when you are scripting a
change or managing headless.

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
best-effort `SIGHUP` to reload a running daemon, exactly as described in
section 1's Notes column and in [Configuration](04-configuration.md). Full
flag syntax, validation, and defaults for each key live in the [Configuration
chapter's key reference](04-configuration.md#config-key-reference); section 1
above is the authoritative command-by-command list.

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
