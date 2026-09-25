<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/trinetra-wordmark-ash.png">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/trinetra-wordmark-ink.png">
    <img alt="Trinetra" src="docs/assets/trinetra-wordmark-ink.png" width="320">
  </picture>
</p>

<p align="center"><strong>Sees what you can't.</strong></p>

**A home server monitor that fits in your head.** One small Go daemon watches a
Linux/systemd box, decides when something is wrong using rules you can actually
read, and tells you over Telegram. No agent, no cloud, no Prometheus, no
external metrics database. The core is standard library only.

![status: stable](https://img.shields.io/badge/status-stable-brightgreen)
![version: v0.4.1](https://img.shields.io/badge/version-v0.4.1-blue)
![core: stdlib only](https://img.shields.io/badge/core-stdlib%20only-00ADD8)
![Go 1.22](https://img.shields.io/badge/Go-1.22-00ADD8)
![platform: Linux + systemd](https://img.shields.io/badge/platform-Linux%20%2B%20systemd-333)
![license: MIT](https://img.shields.io/badge/license-MIT-green)

```bash
sudo trinetra install                      # one systemd service
sudo trinetra telegram set-token <token>   # the only required setting
# then, from your phone:  /start <pin>  ->  /stats
```

Upgrading an existing `serverwatch` install? Same command — see
[Upgrading from serverwatch](#upgrading-from-serverwatch) below.

---

## Why Trinetra

Most monitoring stacks are built for fleets: a scraper, a time-series database,
a dashboard service, an alertmanager, and a pile of YAML to wire them together.
That is a lot of moving parts to babysit for one machine on a shelf.
Trinetra is the opposite bet.

- **One binary, one service.** Drop it on the box, run `install`, set a token.
  It runs as a single systemd unit and stores what it needs locally.
- **No AI in the decision path.** It alerts two ways you can reason about:
  static thresholds you set, and a rolling per-metric baseline that flags "this
  is not normal for this box." Both are ordinary arithmetic. You can read the
  rule and predict when it fires.
- **The core is stdlib only.** No third-party modules in the default
  `trinetra` daemon: no scraping exporter, no cloud account, nothing to keep
  patched but Go itself. Heavier features live in optional plugin binaries, not
  in the core.
- **Configured by command, not by hand.** Every setting goes through the CLI,
  which writes an atomic, validated, private config store. There is no file you
  are meant to hand-edit, so the config on disk always came from a command that
  checked it.
- **Answers in about a second.** Message the Telegram bot and it replies fast,
  with live stats, history, and controls.

## What it watches

| Area | What you get |
|------|--------------|
| **Live metrics** | CPU, memory, swap, load, temperature on a fast tier that drives detection and live status |
| **Slow tier** | Disk usage and SMART, docker containers, systemd services, network throughput |
| **Alerting** | Static thresholds plus a rolling baseline, with boot and recovery reports, a daily digest, and a weekly rollup |
| **Channels** | Telegram by default, plus email, webhook, Slack, Discord, ntfy, and Gotify |
| **Downtime** | Heartbeats, power-down reconstruction across reboots, and reachability checks |
| **History** | A compact binary time-series store with tiered retention, queryable from the CLI and the web UI |
| **Web UI** | An optional live dashboard, history charts, a config editor, and a curated public status page |
| **Fleet mode** | One Trinetra master can collect the history of many children, while every host keeps monitoring and alerting on its own |

See [Monitoring](docs/handbook/05-monitoring.md),
[Alerting and channels](docs/handbook/06-alerting-and-channels.md), and
[Downtime and liveness](docs/handbook/07-downtime-and-liveness.md) for the
details.

## Architecture at a glance

Trinetra is a lean **core** daemon with optional **plugins** around it. The
core runs the sampler, keeps the live picture and the history, answers Telegram,
and exposes its whole internal API over a local control socket (newline-JSON on
a unix socket, token-authenticated). Plugins are separate processes that dial
that socket, so the core stays small and their dependencies never touch it.

```mermaid
flowchart TD
    subgraph core["trinetra (core daemon, stdlib only)"]
        sampler["tiered sampler<br/>fast + slow"]
        detect["detection<br/>thresholds + baseline"]
        store["time-series store<br/>+ live status"]
        tg["Telegram bot"]
        sock["control socket<br/>(unix, token auth)"]
    end
    ctl["trinetra-ctl<br/>management TUI"] -->|dials| sock
    web["trinetra-web<br/>web UI + passkey auth"] -->|dials| sock
    core -->|supervises + verifies| web
    user["you"] -->|Telegram| tg
    user -->|browser| web
```

Two front-door subcommands launch the plugins for you, so you never need to know
their binary names:

```bash
sudo trinetra cli   # launches trinetra-ctl, the management TUI
sudo trinetra web   # launches trinetra-web, the web UI
```

Because those run as root, the core never execs a plugin blindly. It first
proves the binary next to it is the exact one it installed: an absolute path
from the core's own directory (never `$PATH`), a root-owner and non-writable
check on the file and its directory, and a SHA-256 match against a root-only
manifest recorded at install. Any mismatch is refused, not run. The full trust
model is in [Architecture](docs/handbook/02-architecture.md).

## Quick start

```bash
# On the server, pick your arch: linux-amd64 / linux-arm64 / linux-arm (older Pis)
cd /tmp && arch=linux-amd64
for b in trinetra trinetra-ctl trinetra-web; do
  curl -fsSL -o "$b" "https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/$b-$arch"
done
chmod +x trinetra trinetra-ctl trinetra-web

sudo ./trinetra install     # installs the daemon AND both plugins, enables the systemd service
sudo trinetra cli           # guided first-run setup: bot token, enrollment PIN, web UI
```

`trinetra install` copies all three binaries to `/usr/local/bin` and starts
the service; `trinetra cli` opens the interactive
[trinetra-ctl](docs/handbook/plugins/trinetra-ctl.md#managing-with-trinetra-ctl)
TUI, which walks you through the Telegram bot token, the `/start <pin>`
enrollment, and (optionally) the web UI. Prefer the command line? The one
required setting is the bot token, which prints the enrollment PIN right in the
terminal:

```bash
sudo trinetra telegram set-token <token>   # token from @BotFather
```

From there, `/stats` or `/help` in Telegram, or `sudo trinetra web` for the
browser dashboard. The two plugins are optional: drop them from the download
loop if you only want the Telegram daemon. Full steps are in [Installation and
first run](docs/handbook/03-installation.md).

## Upgrading from serverwatch

Trinetra is the rename of what used to be called serverwatch: same daemon,
same data, new name. If this host already runs a `serverwatch` install, the
exact same install command migrates it in place — there is no separate
migration tool and nothing to run first:

```bash
sudo trinetra install
```

That one command stops and disables `serverwatch.service`, moves
`/etc/serverwatch` → `/etc/trinetra` and `/var/lib/serverwatch` →
`/var/lib/trinetra` (an atomic rename, or a verified copy when the two are on
different filesystems), rewrites any config paths that pointed inside the old
directories, then runs the normal install and removes the old unit and plugin
binaries (a drop-in override directory for the old unit, if you had one, is
left in place with a note instead of being deleted). Nothing is deleted
before its replacement is proven in place, so config, history, alert state,
Telegram enrollment, web users/passkeys, and fleet identity/PKI all carry over
untouched. `/usr/local/bin/serverwatch` becomes a compat symlink to
`trinetra`, and `/usr/bin/serverwatch` is deliberately left pointing at it
rather than redirected or removed, so `/usr/bin/serverwatch` →
`/usr/local/bin/serverwatch` → `/usr/local/bin/trinetra` and `sudo
serverwatch ...` keeps resolving on distros whose `secure_path` omits
`/usr/local/bin`. Both compat names are kept for one release (with a
one-line deprecation notice on use) and removed in the next. A marker at
`/var/lib/trinetra/migrated-from-serverwatch` records when a host was
migrated.

It refuses rather than guesses: if both a `serverwatch` install and existing
`trinetra` data are present, or a legacy directory exists but is unexpectedly
empty (usually an unmounted volume), install stops without touching anything
and tells you exactly what to check. `--state-already-at-new-path` adopts a
state volume you moved yourself ahead of time, and `--force` proceeds past a
`serverwatch.service` systemd could not confirm was stopped. The whole
migration is idempotent and resumable, and every stop point explains both how
to finish it and how to roll back by hand.

Full behavior, every refusal case, and the manual rollback steps are in
[Upgrading from a serverwatch install](docs/handbook/03-installation.md#upgrading-from-a-serverwatch-install).

## Documentation

Everything is in the handbook, one concern per chapter.

**[Read the handbook](docs/handbook/README.md)**

| | |
|---|---|
| [Introduction](docs/handbook/01-introduction.md) | What it is and the ethos |
| [Architecture](docs/handbook/02-architecture.md) | Daemon internals, the control socket, core + plugins, the safe-exec trust model |
| [Installation](docs/handbook/03-installation.md) | Getting it onto the server, enabled, and upgrading from serverwatch |
| [Configuration](docs/handbook/04-configuration.md) | The CLI-managed settings and per-target overrides |
| [Monitoring](docs/handbook/05-monitoring.md) | What is collected and how alerting decides |
| [Alerting and channels](docs/handbook/06-alerting-and-channels.md) | Telegram and the other channels |
| [Downtime and liveness](docs/handbook/07-downtime-and-liveness.md) | Heartbeats and power-down reconstruction |
| [Web UI](docs/handbook/08-web-ui.md) | The browser interface and its serving modes |
| [Storage and data model](docs/handbook/09-storage-and-data-model.md) | The time-series store and retention |
| [Operations](docs/handbook/10-operations.md) | Day-to-day running and troubleshooting |
| [Command reference](docs/handbook/11-command-reference.md) | Every CLI subcommand |
| [Roadmap and status](docs/handbook/12-roadmap-and-status.md) | Where it is and what is planned |

## Status

The current stable release is **v0.4.1**, which hardens the core-plus-plugin
architecture introduced in v0.4.0 with the server-identity feature set, a
security-hardening pass, and a round of live-host reliability fixes. See the
[changelog](CHANGELOG.md) for the Trinetra rename and fleet mode, both in
progress on top of it. The handbook marks any feature that is still
experimental where it appears; see [Roadmap and status](docs/handbook/12-roadmap-and-status.md)
for the current line.

## License

MIT.
