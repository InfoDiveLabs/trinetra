# Roadmap and status

This chapter is a snapshot of where serverwatch stands today: what you can run
in production right now, what is taking shape on the development branch, and
what is still unfinished. It is meant to be read as a status page, so it stays
honest about the gaps rather than promising them away.

## Current stable release: v0.3.2

The current stable release is v0.3.2, and it is the version to install if you
want something that works today. It is a single daemon, the stdlib-only
`serverwatch` process, and it already carries the full monitoring stack:

- **Tiered storage.** A fast tier samples CPU, load, memory, and swap on a
  short interval to drive detection and the live picture, while slower
  collectors and one-minute aggregates feed the on-disk time-series store (see
  [Storage and the data model](09-storage-and-data-model.md)). The `tsfile`
  backend keeps history compact, per-series, corruption-tolerant, and
  crash-durable.
- **Multi-channel alerting.** Alerts, boot and recovery reports, the daily
  digest, and the weekly rollup route through a channel-agnostic dispatcher
  with severity and target filtering (see [Alerting and notification
  channels](06-alerting-and-channels.md)). Seven channels ship: Telegram, email
  over SMTP, a generic webhook, Slack, Discord, ntfy, and Gotify.
- **Extended collection.** Beyond the core metrics, the daemon can gather
  per-container CPU, memory, and network series, network throughput, a systemd
  unit inventory, process snapshots, and richer disk facts such as inodes,
  filesystem type, fill projection, and SMART attributes. These are opt-in
  through `collect.*` toggles, with a `doctor` guardrail to keep cardinality and
  disk use in check.
- **The web UI** (see [The web UI](08-web-ui.md)). This release built it with
  `-tags web` as a goroutine inside the daemon, sharing the live snapshot, the
  sample store, and the config. It offers passkey-only WebAuthn auth, RBAC, a
  live dashboard over SSE, history graphs, a web config editor, an alerts page,
  and an admin-curated public status view. The default binary stayed free of
  the web build's dependencies. Later development moves the web UI out of
  process entirely into a supervised, separate binary with no build tag; see
  the beta section below.

This is the release running on a live host behind Cloudflare and nginx, and it
is what the rest of this handbook documents as the working system.

## The v0.4.0-beta.1 preview: core plus plugins

The next line of work lives on the `develop` branch as the v0.4.0-beta.1
preview. It reshapes serverwatch from a single monolithic daemon into a lean
core with plugin binaries layered around it. Treat this as a preview: it is a
foundation you can read and experiment with, not something to put in front of a
production server yet.

The preview introduces four things:

- **A single `core.API` contract.** One internal interface describes everything
  the daemon can answer or be told to do, so that both in-process callers and
  separate processes speak to the same surface.
- **A local control socket.** The core exposes `core.API` over a unix socket in
  the machine's runtime directory. The protocol is newline-delimited JSON, one
  request or response per line, and each daemon launch mints a fresh token that
  a client must present in a handshake before the socket will answer. That keeps
  the control channel local and gated to processes that can read the token.
- **A plugin runtime.** The core hosts plugins and manages the socket-side
  machinery that lets a separate process attach and drive the daemon.
- **Plugin binaries.** Two plugins ship on top of that foundation.
  `serverwatch-ctl` dials the socket to run status, doctor, and alerts against
  a live daemon from a second process; it remains scaffolding, with no
  supervisor managing it yet. `serverwatch-web` serves the web UI out of
  process, talking to the core over the socket instead of living inside the
  daemon as a goroutine; for the beta it has grown a supervisor of its own
  that verifies, spawns, restarts, and stops it, covered in the delivered
  list below.

The intent behind the split is that the core stays small and boring while
everything richer plugs in around it without pulling weight into the default
binary.

## Delivered, at a glance

Rolled up across the stable releases and the preview foundation, the following
is done and verified:

- **Multi-channel alerting** across seven channels (Telegram, email/SMTP,
  generic webhook, Slack, Discord, ntfy, Gotify).
- **Tiered sampling** plus the `tsfile` time-series store, with downsampling and
  per-resolution retention.
- **Extended datapoint collection**: container series, network throughput,
  systemd inventory, process snapshots, and the extra disk and SMART facts.
- **The passkey web UI**: WebAuthn auth, RBAC, live dashboard, history graphs,
  config editor, alerts, and the curated public view.
- **The `core.API` foundation**, the **control socket**, and the **plugin
  runtime**, including the socket hardening and the per-launch token handshake.
- **The web supervisor**: the web UI now runs fully out of process, as a
  single `serverwatch-web` binary with no build tag. The core verifies and
  spawns it as a child process when `web.enabled` is set, restarts it under a
  capped backoff if it exits, and stops it on daemon shutdown.

### A note on Telegram enrollment

Earlier documentation described the bot as capturing the owner chat id
automatically from the first inbound message. That behavior has been replaced.
Enrollment is now explicit: you send `/start <pin>` to the bot, and the daemon
claims that chat as owner only when the pin matches. If you have seen the old
first-message capture described as a delivered feature, the current and correct
behavior is the `/start <pin>` handshake.

## In progress and not done yet

The preview is a foundation, and several pieces that make it a complete
replacement for the embedded design are still open. Being plain about them:

- **The `serverwatch-ctl` interactive TUI (in progress).** A first slice has
  landed: running `serverwatch-ctl` with no subcommand opens a Bubble Tea TUI
  showing live status and a guided "set up the web UI" flow that applies over
  the socket. The remaining management screens (channels, schedules, thresholds,
  first-run onboarding) are still to come.
- **Channel-save validation over the control socket.** Saving a channel from
  the web UI does not yet validate it end to end, so it can silently accept a
  channel that would never deliver. This gap is specific to the socket path
  the web UI reads and writes through.
- **Per-interface throughput alerting.** Throughput is collected as a series,
  but alerting on a specific interface crossing a threshold is not wired up.
- **Live event streaming over the control socket.** The `Subscribe` method
  that would push live events from the core to attached clients is not
  implemented over the socket yet. Until it lands, the web UI's dashboard
  cannot get full server-sent-event push the way a direct in-process reader
  of `core.API` could.

## Not planned, for now

A couple of directions are deliberately out of scope at this stage. Continuous
integration and delivery is intentionally deferred. Multi-host aggregation and
external metrics export, such as Prometheus or remote-write, are the kind of
thing the `SampleStore` interface was designed to allow later, but they are not
being built now.

---

[Previous: Command reference](11-command-reference.md) | [Handbook index](README.md)
