# server-watcher (`serverwatch`)

A single Go binary that monitors a Linux/systemd home server and reports over
**Telegram**, with an optional web UI. No AI, no cloud, no external metrics
database: deterministic static thresholds plus a rolling baseline, standard
library only. Everything is configured through the `serverwatch` CLI; there is
no hand-edited config file.

It runs as one systemd service with a tiered sampler (a fast tier for
CPU/mem/swap/load/temp that drives detection and live status, and a slow tier
for disk/docker/systemd/SMART/network), a Telegram bot that answers commands in
about a second, and a compact binary time-series store for history.

## Documentation

Everything lives in the handbook:

**[Read the handbook](docs/handbook/README.md)**

Start with the [Introduction](docs/handbook/01-introduction.md) and
[Installation and first run](docs/handbook/03-installation.md).

## Quick start

```bash
# On the server, pick the arch: amd64 / arm64 / arm (older Pis)
curl -fsSL -o /tmp/serverwatch \
  https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/serverwatch-linux-amd64
chmod +x /tmp/serverwatch
sudo /tmp/serverwatch install                 # copies to /usr/local/bin (+/usr/bin symlink), writes and enables the systemd unit
sudo serverwatch telegram set-token <token>   # token from @BotFather; the only required setting
# The daemon logs a one-time enrollment PIN. Read it, then from YOUR Telegram
# account message the bot:  /start <pin>       (see: journalctl -u serverwatch | grep /start)
```

From there, `/stats` or `/help` in Telegram, and see the
[handbook](docs/handbook/README.md) for configuration, the web UI, alerting,
and operations.

## Beta: the core and plugins

A core-plus-plugin architecture (a control socket, and separate
`serverwatch-ctl` and `serverwatch-web` binaries) is in preview on the
`develop` branch. It is marked beta throughout the handbook; the production
path today is the single daemon with the optional in-process web UI. See
[Roadmap and status](docs/handbook/12-roadmap-and-status.md).

## License

MIT.
