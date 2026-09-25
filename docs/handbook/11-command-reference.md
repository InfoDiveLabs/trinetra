# Command reference

This chapter lists every command shipped by serverwatch. Section 1 covers the
`serverwatch` binary, which is both the daemon and the operator CLI. Section 2
covers the two out-of-process plugin binaries, `serverwatch-ctl` and
`serverwatch-web`, neither of which is part of the shipped daemon binary
itself. `serverwatch-ctl` is now the primary, recommended way to manage a
running serverwatch day to day: it wraps the
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
| `fleet <subcommand>` | Fleet mode: make this host a master, join or leave one, manage nodes and join codes. | see [`serverwatch fleet`](#serverwatch-fleet) |

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
| Not installed | serverwatch prints an install instruction: download the plugin (`serverwatch-ctl` or `serverwatch-web`) from the releases page next to the daemon binary and run `serverwatch install`, or build it from source (`go build -o /usr/local/bin/serverwatch-ctl ./cmd/serverwatch-ctl`; the web plugin builds the same way, no tag: `go build -o /usr/local/bin/serverwatch-web ./cmd/serverwatch-web`) and then run `serverwatch install` to record its checksum. Nothing is exec'd. |
| Present but unsafe | The binary exists but fails a check (wrong owner, group/world-writable, or a checksum that does not match the manifest). serverwatch refuses with a warning that this may indicate tampering. Nothing is exec'd. |

If you build or hand-copy `serverwatch-ctl` / `serverwatch-web` into place
yourself, you must (re-)run `serverwatch install` afterward so its checksum
is recorded (see [Installation and first run](03-installation.md)); until
then the front-door has nothing to verify the unrecorded binary against and
refuses to run it.

## serverwatch fleet

`serverwatch fleet` manages fleet mode (see [Fleet
mode](02-architecture.md#fleet-mode)). Run it with no arguments or `help` to
print the usage:

```
usage:
  serverwatch fleet init --address HOST[,IP] [--port 9443]   make this host the fleet master
  serverwatch fleet token create [--tags a,b] [--ttl 1h] [--uses 1]
  serverwatch fleet token list | token delete <id>
  serverwatch fleet join <code> [--name NAME]                  join a master as a child
  serverwatch fleet status | nodes [--tag T] [--state S] [--q TEXT]
  serverwatch fleet node revoke|remove|rename|tag <node> [value]
  serverwatch fleet leave [--purge]                            child -> solo
  serverwatch fleet disable [--purge]                          master -> solo
```

The commands split into two kinds. `init`, `join`, `leave` and `disable` change
this host's role: they write the fleet keys in the config file and the
certificate files on disk, and do not signal the daemon; each prints `Restart
to apply: sudo systemctl restart serverwatch`. Everything else asks the running
daemon over the control socket, so it needs the daemon up (on the master, for
the node and token commands) and changes take effect immediately.

| Command | Flags | What it does |
| --- | --- | --- |
| `fleet init` | `--address HOST[,IP]` (required): the names and IPs children will use to reach this host; `--port` (default `9443`) | Makes a solo host the master. Creates the fleet CA (or reuses an existing one) and a server certificate for the addresses, sets `fleet.role` to master, and prints the CA fingerprint. Refuses if the host is already a master or child. |
| `fleet token create` | `--tags a,b` (tags every node that joins with it), `--ttl` (default `1h`), `--uses` (default `1`) | Master only. Prints a join code (`swj1_...`) and the exact `fleet join` line to run on each server. |
| `fleet token list` | none | Master only. Lists unexpired join tokens: id, uses left, expiry, tags. |
| `fleet token delete <id>` | none | Master only. Deletes a join token so it can no longer be used. |
| `fleet join <code>` | `--name NAME` (default `server.name`) | Makes a solo host a child of the master in the code. Verifies the master against the CA pin in the code before sending anything, stores the node's key and certificate, and sets `fleet.role` to child. |
| `fleet status` | none | This host's role. On a master: listen address, join URL, CA fingerprint, node count, and a warning line for every node whose replica has dropped points (out of order, or over the 5000-series limit) or whose clock is more than 30 s off. On a child: node id, master, link state, last ack, outbox size, unsent records and gaps, last error. |
| `fleet nodes` | `--tag T`, `--state S` (`online`, `lagging`, `stale`, `down`, `revoked`), `--q TEXT` (search name, id or address) | Table of nodes: on a master every enrolled node plus this host as `self`, elsewhere only `self`. Columns: state, clock skew, CPU, memory, worst disk, version, last seen, tags, short id. SKEW is the master's smoothed `master time - node send time`; a negative value means the node's clock is ahead. |
| `fleet node revoke <node>` | none | Master only. Refuses the node's certificate from now on; its history is kept. `<node>` is a node id, an id prefix of at least 6 characters, or an exact name. |
| `fleet node remove <node>` | none | Master only. Deletes the node from the registry and from liveness tracking and resolves its open node-down alert, if any. Its certificate is refused from then on (an unknown node counts as revoked). Its replicated history stays on disk under `fleet/nodes/<id>/`. Use it for a server that is gone for good. |
| `fleet node rename <node> <name>` | none | Master only. Changes the node's display name. |
| `fleet node tag <node> <a,b>` | none | Master only. Replaces the node's tags; an empty string clears them. |
| `fleet leave` | `--purge`: also delete this node's fleet identity and unsent outbox | Child only. Returns the host to solo. Local history is always kept. Leaving is local only: the master is not told, and it will report the node as down (and page for it) until you run the command `leave` prints, `sudo serverwatch fleet node revoke <node-id>`, on the master (or `fleet node remove <node-id>` to drop it from the list as well). |
| `fleet disable` | `--purge`: also delete the CA, node registry and every node's replicated history | Master only. Returns the host to solo. Without `--purge`, running `fleet init` again reuses the same CA, so children need not re-join. `fleet disable` then `fleet init` (and a restart) is also how you re-issue the master's 2-year server certificate, which the master warns about from 90 days before it expires. |

Children must reach the master's fleet port directly, or through TCP-level
forwarding only: the master terminates the mutual TLS itself, so a
TLS-terminating reverse proxy in front of it breaks both the CA pin check and
node authentication (see [Fleet mode](02-architecture.md#fleet-mode)).

The tunable fleet keys are ordinary config keys, set with `config set` and
applied on restart: `fleet.listen` (master listen address, default `:9443`),
`fleet.outbox_max_mb` (child outbox cap, default `512`, minimum `16`) and
`fleet.node_down_after` (how long the master waits before calling a silent node
down, default `2m`, minimum `30s`). The role, address, master URL, CA pin and
node id are written only by the commands above; `config set` refuses them.

A typical enrollment:

```
# on the master
sudo serverwatch fleet init --address monitor.example.com,203.0.113.7
sudo systemctl restart serverwatch
sudo serverwatch fleet token create --tags prod --uses 3

# on each server, with the code it printed
sudo serverwatch fleet join swj1_...
sudo systemctl restart serverwatch

# back on the master
sudo serverwatch fleet nodes --tag prod
```

## 2. Plugin binaries

The two out-of-process plugin binaries now have their own dedicated pages.
Both are separate from the shipped `serverwatch` daemon and dial its control
socket rather than reading state in-process; in normal use you launch them via
the `serverwatch cli` / `serverwatch web` front-doors (section 1 above).

- **[Plugins overview](plugins/README.md)** -- what the plugins are, how they
  install alongside the daemon, and the front-door safe-exec model.
- **[serverwatch-ctl](plugins/serverwatch-ctl.md)** -- the interactive
  management TUI: subcommands, socket/token resolution, and the full
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
model described in section 1 and in [Advanced configuration and
management](advanced-configuration.md#persistence-and-hot-reload)).
Full flag syntax, validation, and defaults for each key live in the
[config-key reference](advanced-configuration.md#config-key-reference).

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
