# Roadmap and status

This chapter is a snapshot of where trinetra stands today: what you can run
in production right now, and what is still unfinished. It is meant to be read
as a status page, so it stays honest about the gaps rather than promising them
away.

## Current stable release: v0.5.0

The current stable release is v0.5.0, and it is the version to install if you
want something that works today. It is the first release under the Trinetra
name (existing serverwatch installs migrate automatically on install) and adds
fleet mode, fleet-wide alerting and the fleet web UI (see [Fleet](13-fleet.md)),
and signed releases with safe self-update (see [Security](14-security.md)).
Releases are Linux-only from v0.5.0 on. The full list is in the
[changelog](../../CHANGELOG.md).

v0.5.0 builds on the v0.4.x line, which reshaped trinetra from the single
monolithic daemon of the v0.3.x line into a lean, stdlib-only `trinetra`
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

On top of that core, the v0.4.x line introduced the core-plus-plugin architecture the rest of
this handbook describes as current:

- **A single `core.API` contract.** One internal interface describes everything
  the daemon can answer or be told to do, so that both in-process callers and
  separate processes speak to the same surface.
- **A local control socket.** The core exposes `core.API` over a unix socket in
  the machine's runtime directory. The protocol is newline-delimited JSON, one
  request or response per line, and each daemon launch mints a fresh token that
  a client must present in a handshake before the socket will answer. That keeps
  the control channel local and gated to processes that can read the token.
- **`trinetra-web` out of process.** The passkey web UI (WebAuthn auth, RBAC,
  a live dashboard over SSE, history graphs, a web config editor, an alerts
  page, and an admin-curated public status view) now runs as its own binary
  with no build tag, talking to the core over the socket instead of living
  inside the daemon as a goroutine. The core verifies and spawns it as a child
  process when `web.enabled` is set, restarts it under a capped backoff if it
  exits, and stops it on daemon shutdown. The web UI is also fully responsive on
  mobile. See [The web UI](08-web-ui.md).
- **`trinetra-ctl`, the primary management client.** A separate interactive
  binary that dials the socket. It is the recommended way to manage a running
  trinetra day to day: a styled live-status home dashboard, guided screens
  for schedule, quiet hours, healthchecks, monitor thresholds, and channels
  (an enabled channel is validated over the socket before it can be saved), an
  all-settings screen that reaches every remaining config key, a guided
  web-setup wizard functional in every serving mode, and a first-run Telegram
  onboarding flow. It also exposes scriptable subcommands (`status`, `doctor`,
  `alerts`, with `--json`; `config get`/`config set`; `channels test`) for
  automation and headless use. Every config key is reachable through it, so
  there is nothing you must drop to the daemon's own flags for. See [Managing
  with trinetra-ctl](plugins/trinetra-ctl.md).
- **Live event streaming over the control socket.** `core.API.Subscribe` is
  implemented end to end: the daemon runs an in-process event bus that the
  sampler loop and every dispatched alert publish onto, the control socket
  dedicates a connection to streaming those events to a subscriber, and
  `trinetra-web` subscribes over that stream to push dashboard updates
  instead of only polling for them. See [The live event
  stream](02-architecture.md#the-live-event-stream) and [Live dashboard
  updates](08-web-ui.md#live-dashboard-updates).
- **A front-door install and safe-exec model.** `trinetra install` records
  each plugin's checksum in a root-only manifest; the `trinetra cli` and
  `trinetra web` front-doors verify a plugin against that manifest (owner,
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
  `trinetra-web` binary with no build tag, spawned, restarted under a capped
  backoff, and stopped by the core.
- **Live event streaming over the control socket** (`core.API.Subscribe`), from
  the daemon's event bus through the socket to `trinetra-web`'s live
  dashboard.
- **`trinetra-ctl` as a complete management client**: a styled live-status
  home dashboard (system meters, a live CPU sparkline, an alerts panel, a disks
  panel, and a 24h availability strip), guided screens for schedule, quiet
  hours, healthchecks, monitor thresholds, and channels, an all-settings screen
  over every config key, the guided web-setup wizard, first-run Telegram
  onboarding, a `?` help overlay, and scriptable subcommands (`--json` output,
  `config get`/`config set`, `channels test`). The thin, scriptable core CLI
  verbs it wraps are unchanged and still work standalone, collected for
  automation/no-ctl use in [Daemon-only config
  management](11-command-reference.md#3-daemon-only-config-management).
- **The enrollment PIN over the socket (#90)**: `trinetra telegram
  set-token` prints the `/start <pin>` instruction directly to the terminal
  right after saving the token, and `trinetra-ctl`'s onboarding screen
  surfaces the same PIN. See [Installation and first
  run](03-installation.md#5-connect-telegram-and-enroll-as-owner).
- **Guided setup ownership (#91)**: the guided, validated walk-through for web
  UI setup and first-run Telegram onboarding lives in `trinetra-ctl`; the
  core CLI does not grow an interactive wizard of its own. `config set` and the
  dedicated verbs remain the scriptable escape hatch for anything a guided
  screen does not cover.
- **The front-door install and safe-exec model**, with checksum-manifest
  verification before a plugin is exec'd and restart-on-upgrade of a running
  daemon.
- **The public status page.** Admin-defined services mapped to a host, fleet
  nodes, tags, containers, units or mounts; hold-down status evaluation, 90 day
  bars, automatic and hand-posted incidents with updates, an Atom feed and a
  JSON API, plus a `responder` web role and the `trinetra status-page` CLI.
  See [Public status page](08-web-ui.md#public-status-page).
- **Bounded-retry alert delivery.** The async notifier queue behind every
  channel (solo and fleet fallback alike) retries a failing channel with
  backoff instead of delivering once and giving up, without ever
  double-delivering to a channel that already succeeded. See [Alert
  history](06-alerting-and-channels.md#alert-history).

### A note on Telegram enrollment

Earlier documentation described the bot as capturing the owner chat id
automatically from the first inbound message. That behavior has been replaced.
Enrollment is now explicit: you send `/start <pin>` to the bot, and the daemon
claims that chat as owner only when the pin matches. If you have seen the old
first-message capture described as a delivered feature, the current and correct
behavior is the `/start <pin>` handshake.

### Signed releases and safe self-update

Done on the development line and heading for the next release: every release
is signed 2-of-2 (CI plus an offline maintainer co-signature) over a manifest
of exact file hashes; hosts verify it themselves against keys compiled into
the running binary, keep a version floor against downgrades, watch signed
channel pointers for a withheld update, and apply updates under a guard that
rolls back automatically if the new build is not healthy (`trinetra update`,
`install --require-signed`, the web UI's admin Updates page). The production
keys are in place. See [Security](14-security.md) and
[Operations: Updating](10-operations.md#updating).

Follow-ups, before or at the first public release:

- **Reproducible-rebuild check in `trinetra-release cosign`**, so the
  maintainer's co-signature also proves the CI binaries match the tagged
  source, not only that they are the ones the maintainer was shown.
- **A required reviewer on the `release` environment** once the repository
  is public (GitHub does not offer it for the private repository today; the
  offline co-signature is the approval gate meanwhile).
- **Automatic fleet-wide rollout** (the second half of the design): applying
  updates by channel with maintenance windows, the master caching builds for
  every architecture and serving them to children over mTLS, staged canary and
  batch rollout with health reporting, pause and pin, and a web Updates page
  for the fleet. The master stays transport only: every child still verifies
  every release itself.

## In progress and not done yet

Several pieces that would round out the architecture are still open. Being
plain about them:

- **Channel-save validation over the control socket, for the web UI.** Saving
  a channel from the web UI does not yet validate it end to end, so it can
  silently accept a channel that would never deliver. This gap is specific to
  the socket path the web UI reads and writes through; `trinetra-ctl`'s
  channels screen already validates an enabled channel before saving it (see
  the delivered list above), and the web UI is the one path left to close
  this on.
- **Per-interface throughput alerting.** Throughput is collected as a series,
  but alerting on a specific interface crossing a threshold is not wired up.
- **Fleet mode (master/child).** Phases 1 through 3 are done: enrollment,
  store-and-forward telemetry and replicas, master/child alert handoff with
  lease/receipt/local-fallback, fleet-wide routing/escalation/silences/
  maintenance windows/grouping/dependencies/aggregate rules, managed config,
  the full fleet web UI (node scoping, switcher, `/fleet` overview with
  heatmap/top-N/compare, admin pages, incidents, alerting admin with the
  route tester, silences, managed config, audit log), and the complete
  `trinetra fleet` CLI. See the [Fleet mode](13-fleet.md) chapter for a
  guided walkthrough, [Fleet mode](02-architecture.md#fleet-mode) for the
  architecture, [Fleet alerting](06-alerting-and-channels.md#fleet-alerting)
  for the alerting pipeline, [The web UI](08-web-ui.md#fleet) for the fleet
  pages, and the [command reference](11-command-reference.md#trinetra-fleet)
  for every `fleet` subcommand.

  Phase 4 (ctl + Telegram + public status) is only partially started: the
  master's Telegram channel carries **Ack** and **Silence 1h** inline
  buttons on its own incident fire messages, authorized per enrolled chat
  (see [Fleet alerting](06-alerting-and-channels.md#telegram-buttons)), but
  the rest of phase 4 is still open --
  `trinetra-ctl` has no fleet status/nodes/link-health screens yet, and the
  Telegram bot has no `/fleet`, `/node <name>`, or `/incidents` text
  commands.

  A few edges are known and parked rather than silently absent:

  - A master with `storage.backend=memory` excludes its own node from
    `disk`-metric aggregate rules (that backend keeps no queryable 1-minute
    series for the master's own disk).
  - The escalation dispatcher's `Stop()` has a narrow enqueue/wait-group race
    where it can report "drained" one tick early; safety is intact regardless,
    because any leftover delivery is resurrected on the next start.
  - Telegram inline-button authorization is per enrolled chat, not per
    Telegram user id: anyone in that chat can tap Ack/Silence 1h, same as
    anyone in it can already run text commands.

## Not planned, for now

A couple of directions are deliberately out of scope at this stage. General
continuous integration on every push is intentionally deferred; the one
pipeline that exists is the signed release workflow, which runs the full test
suite on every release tag. External metrics export,
such as Prometheus or remote-write, is the kind of thing the `SampleStore`
interface was designed to allow later, but it is not being built now.

---

[Previous: Command reference](11-command-reference.md) | [Handbook index](README.md) | [Next: Fleet mode](13-fleet.md)
