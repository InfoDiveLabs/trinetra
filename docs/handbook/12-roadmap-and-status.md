# Roadmap and status

This chapter is a snapshot of where serverwatch stands today: what you can run
in production right now, and what is still unfinished. It is meant to be read
as a status page, so it stays honest about the gaps rather than promising them
away.

## Current stable release: v0.4.0

The current stable release is v0.4.0, and it is the version to install if you
want something that works today. It reshapes serverwatch from the single
monolithic daemon of the v0.3.x line into a lean, stdlib-only `serverwatch`
core with plugin binaries layered around it over a local control socket. The
core stays small and boring while everything richer plugs in around it without
pulling weight into the default binary.

The core daemon carries the full monitoring stack the v0.3.x line established:

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

On top of that core, v0.4.0 ships the core-plus-plugin architecture the rest of
this handbook describes as current:

- **A single `core.API` contract.** One internal interface describes everything
  the daemon can answer or be told to do, so that both in-process callers and
  separate processes speak to the same surface.
- **A local control socket.** The core exposes `core.API` over a unix socket in
  the machine's runtime directory. The protocol is newline-delimited JSON, one
  request or response per line, and each daemon launch mints a fresh token that
  a client must present in a handshake before the socket will answer. That keeps
  the control channel local and gated to processes that can read the token.
- **`serverwatch-web` out of process.** The passkey web UI (WebAuthn auth, RBAC,
  a live dashboard over SSE, history graphs, a web config editor, an alerts
  page, and an admin-curated public status view) now runs as its own binary
  with no build tag, talking to the core over the socket instead of living
  inside the daemon as a goroutine. The core verifies and spawns it as a child
  process when `web.enabled` is set, restarts it under a capped backoff if it
  exits, and stops it on daemon shutdown. The web UI is also fully responsive on
  mobile. See [The web UI](08-web-ui.md).
- **`serverwatch-ctl`, the primary management client.** A separate interactive
  binary that dials the socket. It is the recommended way to manage a running
  serverwatch day to day: a styled live-status home dashboard, guided screens
  for schedule, quiet hours, healthchecks, monitor thresholds, and channels
  (an enabled channel is validated over the socket before it can be saved), an
  all-settings screen that reaches every remaining config key, a guided
  web-setup wizard functional in every serving mode, and a first-run Telegram
  onboarding flow. It also exposes scriptable subcommands (`status`, `doctor`,
  `alerts`, with `--json`; `config get`/`config set`; `channels test`) for
  automation and headless use. Every config key is reachable through it, so
  there is nothing you must drop to the daemon's own flags for. See [Managing
  with serverwatch-ctl](plugins/serverwatch-ctl.md).
- **Live event streaming over the control socket.** `core.API.Subscribe` is
  implemented end to end: the daemon runs an in-process event bus that the
  sampler loop and every dispatched alert publish onto, the control socket
  dedicates a connection to streaming those events to a subscriber, and
  `serverwatch-web` subscribes over that stream to push dashboard updates
  instead of only polling for them. See [The live event
  stream](02-architecture.md#the-live-event-stream) and [Live dashboard
  updates](08-web-ui.md#live-dashboard-updates).
- **A front-door install and safe-exec model.** `serverwatch install` records
  each plugin's checksum in a root-only manifest; the `serverwatch cli` and
  `serverwatch web` front-doors verify a plugin against that manifest (owner,
  permissions, checksum) before exec'ing it, and installing over a running
  daemon restarts it onto the new binary. See [Plugins](plugins/README.md).

This line runs on a live host behind Cloudflare and nginx.

## Delivered, at a glance

Rolled up across the stable releases, the following is done and verified:

- **Multi-channel alerting** across seven channels (Telegram, email/SMTP,
  generic webhook, Slack, Discord, ntfy, Gotify).
- **Tiered sampling** plus the `tsfile` time-series store, with downsampling and
  per-resolution retention.
- **Extended datapoint collection**: container series, network throughput,
  systemd inventory, process snapshots, and the extra disk and SMART facts.
- **The passkey web UI**: WebAuthn auth, RBAC, live dashboard, history graphs,
  config editor, alerts, and the curated public view, now served out of process
  and responsive on mobile.
- **The `core.API` foundation**, the **control socket**, and the **plugin
  runtime**, including the socket hardening and the per-launch token handshake.
- **The web supervisor**: the web UI runs fully out of process as a single
  `serverwatch-web` binary with no build tag, spawned, restarted under a capped
  backoff, and stopped by the core.
- **Live event streaming over the control socket** (`core.API.Subscribe`), from
  the daemon's event bus through the socket to `serverwatch-web`'s live
  dashboard.
- **`serverwatch-ctl` as a complete management client**: a styled live-status
  home dashboard (system meters, a live CPU sparkline, an alerts panel, a disks
  panel, and a 24h availability strip), guided screens for schedule, quiet
  hours, healthchecks, monitor thresholds, and channels, an all-settings screen
  over every config key, the guided web-setup wizard, first-run Telegram
  onboarding, a `?` help overlay, and scriptable subcommands (`--json` output,
  `config get`/`config set`, `channels test`). The thin, scriptable core CLI
  verbs it wraps are unchanged and still work standalone, collected for
  automation/no-ctl use in [Daemon-only config
  management](11-command-reference.md#3-daemon-only-config-management).
- **The enrollment PIN over the socket (#90)**: `serverwatch telegram
  set-token` prints the `/start <pin>` instruction directly to the terminal
  right after saving the token, and `serverwatch-ctl`'s onboarding screen
  surfaces the same PIN. See [Installation and first
  run](03-installation.md#5-connect-telegram-and-enroll-as-owner).
- **Guided setup ownership (#91)**: the guided, validated walk-through for web
  UI setup and first-run Telegram onboarding lives in `serverwatch-ctl`; the
  core CLI does not grow an interactive wizard of its own. `config set` and the
  dedicated verbs remain the scriptable escape hatch for anything a guided
  screen does not cover.
- **The front-door install and safe-exec model**, with checksum-manifest
  verification before a plugin is exec'd and restart-on-upgrade of a running
  daemon.

### A note on Telegram enrollment

Earlier documentation described the bot as capturing the owner chat id
automatically from the first inbound message. That behavior has been replaced.
Enrollment is now explicit: you send `/start <pin>` to the bot, and the daemon
claims that chat as owner only when the pin matches. If you have seen the old
first-message capture described as a delivered feature, the current and correct
behavior is the `/start <pin>` handshake.

## In progress and not done yet

Several pieces that would round out the architecture are still open. Being
plain about them:

- **Channel-save validation over the control socket, for the web UI.** Saving
  a channel from the web UI does not yet validate it end to end, so it can
  silently accept a channel that would never deliver. This gap is specific to
  the socket path the web UI reads and writes through; `serverwatch-ctl`'s
  channels screen already validates an enabled channel before saving it (see
  the delivered list above), and the web UI is the one path left to close
  this on.
- **Per-interface throughput alerting.** Throughput is collected as a series,
  but alerting on a specific interface crossing a threshold is not wired up.
- **Fleet mode (master/child).** Phase 1 (enrollment, store-and-forward
  telemetry, replicas, node-down alerts, `serverwatch fleet` CLI) is in; the
  fleet web UI, alert routing/escalation/silences, and managed config follow.
  See [Fleet mode](02-architecture.md#fleet-mode).

## Not planned, for now

A couple of directions are deliberately out of scope at this stage. Continuous
integration and delivery is intentionally deferred. External metrics export,
such as Prometheus or remote-write, is the kind of thing the `SampleStore`
interface was designed to allow later, but it is not being built now.

---

[Previous: Command reference](11-command-reference.md) | [Handbook index](README.md)
