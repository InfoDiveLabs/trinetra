# Introduction

trinetra is a monitor for a single Linux home server. It watches one
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

There is deliberately no AI anywhere in the decision path. trinetra decides
whether to alert two ways, both of them things you can reason about. First, it
compares metrics against static thresholds you set, so a disk crossing 90
percent is a plain, predictable event. Second, it keeps a rolling baseline of
each metric and flags a reading that deviates too far from that baseline, which
catches the "this is not normal for this box" cases that a fixed number would
miss. Both are ordinary arithmetic. You can read the rule, and you can predict
when it will fire.

Configuration follows the same principle. Everything you can change is set
through the `trinetra` CLI (see [Configuration](04-configuration.md)), most
conveniently through the guided `trinetra-ctl` TUI (see [Managing with
trinetra-ctl](plugins/trinetra-ctl.md#managing-with-trinetra-ctl)),
which drives the same settings; there is no config file you are meant to
hand-edit. The scriptable core CLI remains the automation path: you set the
one required value, the Telegram bot token, the same way you set anything
else:

```bash
sudo trinetra telegram set-token <token>
```

The CLI writes an atomic, private config store on your behalf, so the config on
disk always came from a command that validated it. Defaults already work, so
past the token most installs need nothing more.

## The current direction: a core with plugins

trinetra is organized as a lean core daemon plus optional plugin binaries
layered on top. The core is the stdlib-only `trinetra` process: it runs the
tiered sampler loop, keeps the live picture and the time-series history, and
answers Telegram. Crucially, it exposes its state and its controls over a local
control socket (see [Architecture](02-architecture.md)), a small
newline-delimited JSON protocol on a unix socket under the machine's runtime
directory. Any separate process on the same host can dial
that socket and call the same internal API the daemon uses itself, with no
extra dependencies pulled into the core.

The plugins are the processes that speak to that socket. The web UI is one
such plugin: a separate `trinetra-web` binary, with no build tag, that the
core supervises. It adds passkey-only auth and the browser dashboard, and the
core stays free of its heavier dependencies simply because it does not import
them, not because of a build tag. `trinetra-ctl` is the other plugin, and
it is the primary, complete way to manage a running daemon day to day: it
dials the same control socket from a second process and gives you guided
screens for the schedule, quiet hours, healthchecks, monitor thresholds, and
notification channels, an "All settings" screen that reaches every remaining
config key, first-run Telegram onboarding, and a guided web-setup wizard, all
applied over the socket with the same validation the daemon uses. The point of
the split is that the core stays small and boring, and everything richer
plugs in around it without weighing it down.

You do not need to know either plugin binary's name to use it. The core
exposes two front-door subcommands that launch them for you:

```bash
sudo trinetra cli   # launches trinetra-ctl, the management TUI
sudo trinetra web   # launches trinetra-web, the web UI
```

Because these commands are typically run as root, the core does not exec the
plugin blindly: it first proves the binary next to it is the exact one it
installed, then hands off. See [Architecture](02-architecture.md) for the
trust model and [Command reference](11-command-reference.md) for the full
command details.

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
