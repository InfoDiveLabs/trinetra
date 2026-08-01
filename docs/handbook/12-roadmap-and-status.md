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
  `serverwatch-ctl` dials the socket to run status, doctor, and alerts
  against a live daemon from a second process, and is now the primary,
  interactive way to manage one: schedule, quiet hours, healthchecks,
  monitor thresholds, channels, an all-settings screen that reaches every
  remaining config key, and first-run onboarding all live there. It is a
  complete, supported management tool: every config key is reachable through
  its screens and its web-setup wizard is functional in every serving mode.
  No supervisor manages the `serverwatch-ctl` process itself the way the web
  plugin is supervised, but that is by design for a client you run by hand
  when you want it, not a background service. `serverwatch-web` serves the web UI out of process,
  talking to the core over the socket instead of living inside the daemon as
  a goroutine; for the beta it has grown a supervisor of its own that
  verifies, spawns, restarts, and stops it, covered in the delivered list
  below.

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
- **Live event streaming over the control socket.** `core.API.Subscribe` is
  implemented end to end: the daemon runs an in-process event bus that the
  sampler loop and every dispatched alert publish onto, the control socket
  dedicates a connection to streaming those events to a subscriber, and
  `serverwatch-web` subscribes over that stream to push dashboard updates
  instead of only polling for them. See [The live event
  stream](02-architecture.md#the-live-event-stream) and [Live dashboard
  updates](08-web-ui.md#live-dashboard-updates) for the details.
- **The `serverwatch-ctl` interactive management screens**: schedule, quiet
  hours, healthchecks, and monitor thresholds all now have guided screens,
  alongside a channels screen (list, add, edit, remove, test, with an
  enabled channel validated over the socket before it can be saved) and a
  first-run onboarding flow that captures the Telegram bot token and walks
  through enrollment. `serverwatch-ctl` is the primary, recommended way to
  manage a running serverwatch; see [Managing with
  serverwatch-ctl](plugins/serverwatch-ctl.md). The
  thin, scriptable core CLI verbs it wraps are unchanged and still work
  standalone, collected for automation/no-ctl use in [Daemon-only config
  management](11-command-reference.md#3-daemon-only-config-management).
- **The enrollment PIN over the socket (#90)**: `serverwatch telegram
  set-token` now prints the `/start <pin>` instruction directly to the
  terminal right after saving the token, instead of requiring a trip to the
  journal. It falls back to pointing at the journal only when the daemon
  cannot be reached. `serverwatch-ctl`'s onboarding screen surfaces the same
  PIN. See [Installation and first
  run](03-installation.md#5-connect-telegram-and-enroll-as-owner) for the
  enrollment flow diagram.
- **Guided setup ownership (#91)**: the guided, validated walk-through for
  web UI setup and first-run Telegram onboarding lives in `serverwatch-ctl`;
  the core CLI does not grow an interactive wizard of its own. `config set`
  and the dedicated verbs remain the scriptable escape hatch for anything a
  guided screen does not cover.

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

- **Channel-save validation over the control socket, for the web UI.** Saving
  a channel from the web UI does not yet validate it end to end, so it can
  silently accept a channel that would never deliver. This gap is specific to
  the socket path the web UI reads and writes through; `serverwatch-ctl`'s
  channels screen already validates an enabled channel before saving it (see
  the delivered list above), and the web UI is the one path left to close
  this on.
- **Per-interface throughput alerting.** Throughput is collected as a series,
  but alerting on a specific interface crossing a threshold is not wired up.

## Not planned, for now

A couple of directions are deliberately out of scope at this stage. Continuous
integration and delivery is intentionally deferred. Multi-host aggregation and
external metrics export, such as Prometheus or remote-write, are the kind of
thing the `SampleStore` interface was designed to allow later, but they are not
being built now.

---

[Previous: Command reference](11-command-reference.md) | [Handbook index](README.md)
