# Introduction

serverwatch is a monitor for a single Linux home server. It watches one
systemd host, the kind of machine that lives on a shelf and runs a handful of
docker containers, and it tells you when something is wrong. By default it
reports over Telegram: you message a bot, it answers in about a second, and it
pushes alerts, boot and recovery reports, a daily digest, and a weekly rollup
to the chat you claim as owner. If you would rather look at graphs than read
messages, there is an optional web UI (a live dashboard, history charts, a
config editor, and a curated public status page); it is covered in its own
chapter (see [The web UI](08-web-ui.md)).

## The ethos

The whole tool ships as a single Go binary. The default build is standard
library only: no third-party modules, no agent, no scraping exporter, no
Prometheus, no cloud account, and no external metrics database to stand up and
babysit. It watches your machine and it stores what it needs locally, and that
is the end of the dependency list.

There is deliberately no AI anywhere in the decision path. serverwatch decides
whether to alert two ways, both of them things you can reason about. First, it
compares metrics against static thresholds you set, so a disk crossing 90
percent is a plain, predictable event. Second, it keeps a rolling baseline of
each metric and flags a reading that deviates too far from that baseline, which
catches the "this is not normal for this box" cases that a fixed number would
miss. Both are ordinary arithmetic. You can read the rule, and you can predict
when it will fire.

Configuration follows the same principle. Everything you can change is set
through the `serverwatch` CLI (see [Configuration](04-configuration.md)), and
there is no config file you are meant to hand-edit. You set the one required value, the Telegram bot token, the same way
you set anything else:

```bash
sudo serverwatch telegram set-token <token>
```

The CLI writes an atomic, private config store on your behalf, so the config on
disk always came from a command that validated it. Defaults already work, so
past the token most installs need nothing more.

## The current direction: a core with plugins

serverwatch is organized as a lean core daemon plus optional plugin binaries
layered on top. The core is the stdlib-only `serverwatch` process: it runs the
tiered sampler loop, keeps the live picture and the time-series history, and
answers Telegram. Crucially, it exposes its state and its controls over a local
control socket (see [Architecture](02-architecture.md)), a small
newline-delimited JSON protocol on a unix socket under the machine's runtime
directory. Any separate process on the same host can dial
that socket and call the same internal API the daemon uses itself, with no
extra dependencies pulled into the core.

The plugins are the processes that speak to that socket, or that link the same
internals under a build tag. The web UI is one such plugin: a separate
`serverwatch-web` binary, built with `-tags web`, that adds passkey-only auth
and the browser dashboard while keeping the default `serverwatch` binary free
of its heavier dependencies. A management CLI, `serverwatch-ctl`, is in
progress: it dials the control socket to drive a running daemon from a second
process, and it is where a richer interactive terminal experience will land.
The point of the split is that the core stays small and boring, and everything
richer plugs in around it without weighing it down.

## How this handbook is organized

The chapters that follow go deeper, one concern at a time. **Architecture**
lays out the daemon's goroutines, the control socket, and the core-plus-plugin
shape. **Installation** covers getting the binary onto the server and enabled
under systemd. **Configuration** walks the CLI-managed settings and per-target
overrides. **Monitoring** explains what is collected and how thresholds and the
rolling baseline decide when to alert. **Alerting and channels** covers Telegram
and the other notification channels. **Downtime and liveness** describes
heartbeats, power-down reconstruction, and reachability. **Web UI** covers the
optional browser interface and its serving modes. **Storage and data model**
explains the time-series store, `status.json`, and retention. **Operations**
collects the day-to-day running and troubleshooting. **Command reference**
lists every CLI subcommand, and **Roadmap** closes with status and planned work.

---

[Handbook index](README.md) | [Next: Architecture](02-architecture.md)
