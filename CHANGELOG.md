# Changelog

All notable changes to Trinetra (formerly serverwatch) are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [semantic versioning](https://semver.org/spec/v2.0.0.html). Dates are
YYYY-MM-DD. Preview builds are cut as `vX.Y.Z-beta.N` tags on the `develop`
branch; stable releases are tagged on `main`.

## [Unreleased]

## [0.5.0] - 2026-09-30

The first release since the serverwatch rename, and the biggest yet: fleet
mode (master/child, phases 1-3 -- replication and local fallback, a full
alerting/incidents/routing engine, and a fleet web UI), signed releases with
a maintainer-verified, self-rolling-back update path, and a round of web UI
polish, all documented in a new [security chapter](docs/handbook/14-security.md).
Releases are Linux-only from this version on; v0.4.1 was the last to also
ship macOS (darwin) binaries. The license also changes, see below.

### Added

- **Fleet mode (master/child), phase 1.** A master enrolls children with a
  one-line join code that pins its CA (no trust-on-first-use); children then
  talk to it over mutual TLS with 90-day client certificates that renew
  themselves, and can be revoked. Each child spools a copy of its samples, down
  events and alert log into a durable, capped outbox and ships it to the
  master, which keeps a per-node replica. A master outage loses nothing: the
  backlog drains in order when it returns, and if the outbox cap was hit the
  dropped range is rebuilt from the child's local store before newer data is
  sent. The master raises node-down alerts (folded into one fleet-connectivity
  alert when most of the fleet drops at once); a child warns locally when its
  link has been down for ten minutes.
- **`trinetra fleet` commands:** `init`, `join`, `leave`, `disable`,
  `status`, `nodes`, `node revoke|rename|tag`, and `token create|list|delete`.
  See the [command reference](docs/handbook/11-command-reference.md#trinetra-fleet).
- **Config keys** `fleet.listen` (default `:9443`), `fleet.outbox_max_mb`
  (default `512`) and `fleet.node_down_after` (default `2m`). The role and
  identity keys are managed by `trinetra fleet` and refused by `config set`.
- **Control socket:** requests take an optional `node` to read a remote node's
  replica through the same methods, plus new `Fleet.*` methods for fleet
  management. Both are backward compatible: requests without `node` behave as
  before.
- **Fleet mode, phases 2 and 3: fleet alerting and the fleet web UI.** The
  master now decides delivery for the whole fleet instead of just relaying
  node-down alerts. A child holding a valid lease routes its firing alerts to
  the master instead of delivering them itself, and falls back to local
  delivery (prefixed "via local fallback") if no receipt arrives within
  `fleet.fallback_after` or the lease expires -- at-least-once, deduplicated
  by `(node, alert key, fired_at)`, never doubled up. On the master, every
  alert runs through a full pipeline -- silence, dependency fold, grouping,
  routing, escalation, delivery, receipt -- all recorded and explainable with
  `fleet explain`. New: ordered routes with matchers and `continue` fan-out
  to several escalation policies at once; multi-step escalation policies with
  `repeat_every`; silences and recurring maintenance windows (matched by tag,
  node name-glob-or-id, rule, and severity, and pushed to children so local
  fallback honours them too); incident grouping and dependency folding;
  fixed-grammar aggregate rules (`count`, `avg`/`max`/`min`, `online`,
  `absent`) evaluated fleet-wide on the master; and managed config, a closed
  10-key allowlist a master can push to children by tag, read-only locally
  and re-imposed on every apply. The master's Telegram messages gain Ack and
  Silence-1h inline buttons on incident fire notifications. New `trinetra
  fleet` subcommands: `incidents`, `incident`, `ack`, `explain`, `silence
  add|list|expire`, `maintenance add|list|delete`, `route test`, `alerting
  show|apply`, `rules`, `managed list|set|delete|status`, and `node depends`.
  New config keys `fleet.fallback_after` (default `2m`) and
  `fleet.link_down_warn_after` (default `10m`), both child-only and
  live-applied. See [Fleet
  alerting](docs/handbook/06-alerting-and-channels.md#fleet-alerting).
- **The fleet web UI.** Every existing page is now also reachable per node
  under `/n/{id}/...`, with a replica banner and a stale-data indicator for
  remote pages, a top-bar node switcher, and a Ctrl/Cmd-K fuzzy palette
  (recent nodes, and a "web1 history"-style page-type jump). `/fleet` gains a
  health strip, a heatmap, top-N panels, a sortable/filterable live node
  table, and a compare view (up to 10 nodes, or an aggregate, one metric
  overlaid). New admin pages: `/fleet/admin` (tokens, node rename/tags/
  dependencies/revoke/remove, link health), `/fleet/incidents` (list and
  timeline, with ack/silence), `/fleet/alerting` (routes/policies/rules
  editor plus a route tester), `/fleet/silences` (silences and maintenance
  windows, times shown in the master's own local zone), `/fleet/managed`
  (managed-config fragments and per-node drift), and `/fleet/audit` (every
  fleet mutation, who and when). Remote-node actions (ack/unack, container
  logs) work whenever that node is currently connected, and are disabled
  with a reason when it isn't. See [The web
  UI](docs/handbook/08-web-ui.md#fleet).
- **Signed self-update.** `trinetra update status|check|apply|rollback`
  fetches, independently verifies (CI signature + maintainer co-signature
  over a manifest of exact file hashes), stages, smoke-tests and swaps in a
  new release, then launches a guarded restart that confirms the new build
  is healthy within 90s or automatically rolls back and marks the version
  bad. `trinetra install --require-signed` runs the same signature check for
  the initial install. New config keys `update.channel` (default `stable`),
  `update.source` (default `github`), `update.github_token`, and
  `update.check_interval` (default `24h`); the daemon checks on that cadence
  and alerts when an update becomes available, commits, or rolls back.
  Releases are Linux-only: from this release on, macOS (darwin) binaries
  are dropped (v0.4.1, as serverwatch, was the last release to ship them).
  Maintainer tooling (`cmd/trinetra-release`) and the key ceremony/release
  process are documented in [Operations: Release keys and releasing](docs/handbook/10-operations.md#release-keys-and-releasing-maintainers-only).
  See [Operations: Updating](docs/handbook/10-operations.md#updating).

### Changed

- **Renamed to Trinetra.** serverwatch is now **Trinetra** ("Sees what you
  can't."): module `github.com/InfoDiveLabs/trinetra`, binaries `trinetra`,
  `trinetra-ctl`, `trinetra-web`, paths `/etc/trinetra`, `/var/lib/trinetra`,
  `/run/trinetra`, and the `trinetra.service` unit. The web UI, TUI, and
  notifications carry the new Trinetra visual identity. The GitHub repo moves
  to `InfoDiveLabs/trinetra` (the old `Suraj-Tiwari/server-monitor` URLs
  redirect).
  - **Upgrade:** download `trinetra` and the plugins you use
    (`trinetra-ctl`, `trinetra-web`) into one directory, then run the one
    command you already know, `sudo trinetra install`. On a host with an existing serverwatch install it detects it
    and migrates in place before the normal install runs: it stops and
    disables `serverwatch.service`, moves `/etc/serverwatch` →
    `/etc/trinetra` and `/var/lib/serverwatch` → `/var/lib/trinetra` (an
    atomic rename, or a byte-verified copy when the two are on different
    filesystems, so nothing is deleted before its replacement is proven in
    place), rewrites any config paths that pointed inside the old
    directories, then removes the old unit and plugin binaries (a drop-in
    override dir for the old unit, if any, is left in place with a note) and
    replaces `/usr/local/bin/serverwatch` with a compat symlink to
    `trinetra`; `/usr/bin/serverwatch` is deliberately left pointing at it
    rather than redirected or removed, so `sudo serverwatch ...` still
    resolves via `secure_path` on distros that omit `/usr/local/bin`. Both
    compat names are kept for one release, with a deprecation notice on use.
    It refuses rather
    than merges if both a serverwatch install and existing trinetra data are
    present, or a legacy directory is unexpectedly empty (likely an
    unmounted volume); `--state-already-at-new-path` adopts a state volume
    you moved yourself, `--force` proceeds past a `serverwatch.service`
    systemd could not confirm was stopped or a `serverwatch daemon` still
    running outside it (found through its pid file). An old plugin with no
    `trinetra-ctl`/`trinetra-web` counterpart next to `trinetra` is named in
    a `WARNING:` line of the summary. Until install has run, `trinetra
    daemon` and the config- and state-writing CLI commands refuse on a host
    that has only a serverwatch install, instead of starting empty. A
    `/var/lib/trinetra/migrated-from-serverwatch` marker records the
    migration; the whole thing is idempotent and resumable, and every stop
    point explains how to finish or roll back by hand. See [Upgrading from a
    serverwatch install](docs/handbook/03-installation.md#upgrading-from-a-serverwatch-install).
  - **Kept on purpose:** the web cookie names `sw_session`/`sw_enroll`/`sw_login`
    (renaming them would log every user out); the WebAuthn RP ID/origin
    handling (config-driven, bound into existing passkeys); config JSON keys;
    `plugins.json` key names; the `control.sock`/`token` file names inside the
    runtime dir; the `/usr/local/bin/serverwatch` → `trinetra` compat symlink
    and the `/usr/bin/serverwatch` link that keeps pointing at it (both
    removed in the next release); the
    `SERVERWATCH_CONTROL_SOCKET`/`TOKEN` environment fallback (also removed in
    the next release); and the literal `"serverwatch-control"` control-socket
    handshake magic string. Fleet certificates already issued under a
    serverwatch install keep their old subject names: the CA's Organization
    `"serverwatch fleet"` and the master certificate's CN `"serverwatch fleet
    master"` (cosmetic, nothing verifies them, left as is). Newly issued ones
    use `"trinetra fleet"` and `"trinetra fleet master"`.

### License

- **License change:** from this release on, Trinetra is licensed under the
  Functional Source License 1.1, ALv2 Future License (FSL-1.1-ALv2),
  © 2026 InfoDive Labs Pvt Ltd: free to use, self-host and modify for any purpose
  except a competing commercial product or service, and each release becomes
  Apache-2.0 two years after it is published. Releases up to v0.4.1 (published as
  serverwatch) stay MIT.

### Unchanged

- Solo installs (no `fleet.role`, the default) run no fleet code, open no
  listener and create no fleet directories.

## [0.4.1] - 2026-08-20

The stable cut of the `0.4.1-beta.1`…`beta.3` line, tested on the live host
since 2026-08-02. Everything below is the beta content promoted unchanged;
see the beta entries for full detail. Highlights:

- **Server identity (#99).** Configurable `server.name` shown across web, ctl,
  and alert titles; host inventory (CPU/RAM/disks/OS/uptime) on a new web Host
  page, `serverwatch-ctl host`, and `core.API.HostInfo`; local + opt-in public
  IP; CPU/mem alerts name the top process and container. (#100, #101, #102, #103)
- **Security hardening.** Fail-closed control socket (#96) and web user store
  (#105), bounded Telegram enrollment PIN brute force (#93), DOM XSS sink
  removed (#94), rate-limited ceremony begins (#95), opt-in outbound SSRF guard
  (#97), documented plugin-copy trust assumption (#98).
- **Reliability.** Web dashboard no longer freezes on a desynced socket client
  (#105); daemon restarts are no longer recorded as host downtime (#116);
  collection is fail-visible with per-collector health and alerts (#110); Swarm
  services keyed by service, not task (#118); storage maintenance no longer
  stalls history reads (#113).
- **Operability.** Build-time version stamps with mismatch detection in the
  panel and `serverwatch-ctl version` (#107); series-cardinality guardrail in
  `doctor` (#112); setup UX fixes (#106); container logs in the drawer;
  paginated downtime list.

## [0.4.1-beta.3] - 2026-08-08

Five tracker issues, all with core / web / ctl parity: versions in the panel,
setup UX fixes, and three reliability fixes for how the daemon collects and
keys data.

### Added

- **Versions in the panel (#107).** Each binary is now stamped with a build-time
  version (git-derived, with a `dev` fallback for plain `go build`). The web
  sidebar shows the core daemon's version (over the control socket) and the web
  plugin's own version, with a "version mismatch" marker when they differ after
  a partial upgrade. `serverwatch-ctl version` prints both from the terminal.
- **Monitoring-failure alerts (#110).** A slow-tier collector (docker, disk,
  services, smart) that fails or times out for three consecutive cycles now
  raises a `collector:<name>` alert and recovers on the next success.

### Fixed

- **Collection is fail-visible, not silent (#110).** A failed or timed-out
  collection command no longer publishes missing data or flips a healthy target
  to gone: the daemon carries the last-known values forward (marked stale) and
  records the failure. Per-collector health (consecutive failures, last success,
  last error) is in `status.json`, shown as a warning banner on the web
  dashboard, and printed by `serverwatch-ctl status`.
- **Docker Swarm services are keyed by service, not task (#118).** On a Swarm
  node, containers are keyed by their stable service name instead of the
  ephemeral `<service>.<slot>.<taskid>` task name (tasks summed). A rolling
  deploy no longer creates new per-task series or fires false down/recover
  churn, and per-service history stays continuous across redeploys. Plain-docker
  hosts are unchanged.
- **Series-cardinality guardrail (#112).** `serverwatch doctor` now warns when
  the time-series count is abnormally high (a healthy host is in the low
  hundreds), catching an accumulation before it degrades the daemon. Stale
  series already age out past retention.
- **Setup UX (#106).** The proxy-mode web wizard now offers an optional domain
  step (deriving rp_id/origin, or leaving them to forwarded headers and
  documenting that on the confirm screen), so an operator whose reverse proxy
  does not forward `X-Forwarded-*` headers can set them without dropping to
  `config set`. `serverwatch install` no longer nudges you to set a Telegram
  token when one is already configured.

## [0.4.1-beta.2] - 2026-08-08

This preview folds together everything since `0.4.1-beta.1`: a security-hardening
pass, the server-identity epic (#99), and the round of fixes from testing beta.1
on the live host. Every capability lands in core, web, and `serverwatch-ctl`
together.

### Fixed (from beta testing)

- **Multi-socket CPUs now report their socket count.** `parseCPUInfo` summed
  cores across sockets but never counted the sockets, so a dual-socket box read
  as one CPU. The host view now shows `2× <model> (2 sockets / 32 cores / 64
  threads)`.
- **The disk inventory shows only real disks.** Docker `overlay` layers,
  `tmpfs`, snap `squashfs`/loop mounts, and the other pseudo filesystems are
  filtered out; local block devices, LVM volumes, and network mounts
  (NFS/CIFS) are kept.
- **Container logs are viewable.** The dashboard drawer's "View logs" action is
  wired to a new `core.API.ContainerLogs` (a validated `docker logs --tail`
  snapshot over the control socket), also exposed as `serverwatch-ctl logs
  <container> [--tail N]`. The container name is validated against the live
  container list before shelling out.
- **Per-metric history is drawn in the drawer.** Rows backed by a stored series
  (disk mounts) now render a real 6h sparkline from `/api/series` instead of the
  "not available yet" placeholder; rows with no stored series omit the chart
  rather than faking one.
- **Host info on the dashboard.** A compact host strip (name, OS, CPU, RAM,
  uptime, local IP) sits at the top of the dashboard on desktop, linking through
  to the full Host page.
- **Identity setup in the web UI.** The admin `/config` page now has an Identity
  panel to set `server.name` and toggle the `collect.public_ip` opt-in.
- **The downtime list on the history page is paginated.** It shows the most
  recent 8 incidents with a "Show all" expander instead of an unbounded wall of
  rows.

### Server identity (#99)

- **Configurable `server.name`.** A new `server.name` config key names the
  server; it resolves through the configured name, then the OS hostname, then
  `serverwatch`. The web UI brand shows it in place of the old hardcoded
  `MONITOR.HOME.LAN`, `serverwatch-ctl` shows it in the Home header and config
  editor, and every alert title is prefixed `[name]` so a multi-server inbox is
  legible. (#101)
- **Host inventory.** A stdlib-only collector reports CPU model / cores /
  threads, total RAM, kernel, OS release, uptime, and per-disk model / type /
  size / filesystem. It is served over a dedicated `core.API.HostInfo()` method
  (control socket included), rendered on a new web **Host** page (viewer-gated),
  and available as `serverwatch-ctl host` (with `--json`). (#100)
- **Host IP addresses.** The inventory reports the local IP always and the
  public IP only when the opt-in `collect.public_ip` key is set (default false,
  since resolving it makes an outbound request). Both are shown on the web Host
  page and the ctl subcommand. (#102)
- **CPU and memory alerts name the culprit.** A `cpu`/`mem` breach message now
  carries a ` (top: <proc> N%, container <name> N%)` suffix built from the
  process and container data already on the snapshot, so an alert reads
  `cpu = 96.0 >= threshold 95.0 (top: ffmpeg 82%, container web 30%)` instead of
  a bare number. It degrades to no suffix when that data is absent. (#103)

### Security hardening & correctness

Working through the Fable 5 security review (fail-closed auth paths, an XSS
sink, brute-force and lock amplification bounds, opt-in SSRF blocking), fixing
downtime accounting so a daemon restart is no longer mistaken for a host outage,
and stopping a maintenance pass from stalling web history reads.

#### Added

- **Opt-in outbound SSRF guard for channel and healthchecks URLs.** The new
  `notify.block_private_targets` config key (default false) makes the daemon
  refuse to dial loopback, link-local (including the `169.254.169.254` cloud
  metadata endpoint), and private (RFC1918 / ULA) targets when set. The check
  runs against the resolved IP, so a hostname that points inward is blocked too.
  (#97)
- **Tunable Telegram enrollment brute-force bound.** `telegram.enroll_max_attempts`
  (default 5) and `telegram.enroll_cooldown` (default 60s) control when the
  enrollment PIN cools down and rotates. (#93)
- **`serverwatch downtime purge`** clears bogus downtime events, e.g. the short
  fabricated `power_down` events an old crash loop wrote. (#116)

#### Fixed

- **A daemon restart is no longer recorded as a host `power_down`.** The
  reconstruction now reads the host boot time from `/proc/stat` and records
  downtime only when the host actually rebooted during the gap; a monitor
  restart (crash loop, deploy, `systemctl restart`) records nothing, so a
  restart storm can no longer fabricate hours of downtime and tank the uptime
  percentage. Overlapping and adjacent outages are coalesced into one incident
  and their union, not a double-counted sum. (#116)
- **A storage maintenance pass no longer stalls web history reads.** `Prune` and
  `Downsample` take the store lock per file instead of holding it for the whole
  multi-second pass, so `Query` reads interleave between files. (#113)
- **Removed dead mockup UI from the container drawer:** the non-functional
  `Restart` action and the placeholder sparkline "charts" that never loaded real
  data are gone, replaced by honest not-yet-available states. (#114)

#### Security

- **The control socket fails closed** when its per-launch auth token cannot be
  generated or written: it now runs without the socket rather than serving it
  with no authentication. (#96)
- **The web UI fails closed on an unreadable user store.** An unreadable
  `users.json` was treated as an empty first-run store, which could open a
  tokenless-admin bootstrap window; enrollment now refuses when the store cannot
  be read. (#105)
- **The Telegram enrollment PIN is bounded against brute force:** after a run of
  wrong `/start` guesses it cools down and rotates to a fresh value, so the
  six-digit space cannot be walked. (#93)
- **The detail drawer is built with `textContent`, not `innerHTML` string
  concatenation,** removing a CSP-mitigated DOM XSS sink where `data-*` values
  were concatenated into live markup. (#94)
- **Unauthenticated ceremony begins are rate-limited** (`/enroll/begin` and
  `/login/begin`, 15 per 10s per client) so an anonymous caller cannot hammer
  the shared ceremony-store lock. (#95)
- **Documented the install-time plugin-copy trust assumption:** only run
  `serverwatch install` from a directory you control, since it adopts the plugin
  binaries beside it. (#98)

## [0.4.1-beta.1] - 2026-08-02

A reliability release for the core-plus-plugin line: the web plugin is made
truly channel-only, and a socket-client defect that could freeze the dashboard
is fixed.

### Fixed

- **The web dashboard no longer freezes into an all-zero board until a core
  restart.** The control-socket client held one long-lived connection with no
  reconnect: on a read timeout or a response-id mismatch it returned the error
  but kept the connection, which is then permanently frame-misaligned (a late
  response is read by the next call and mismatches its id, desyncing every call
  after). Because `serverwatch-web` holds one client for its whole lifetime, a
  single slow daemon response wedged every `Snapshot` and the dashboard
  rendered the zero-value view (0 cores, 0%, Offline) while alerts and Telegram
  kept working. The client now poisons the connection on any transport failure
  and transparently re-dials on the next call, with a bounded reconnect
  handshake. (#105)

### Changed

- **The web plugin no longer reads or writes daemon-owned state on disk.**
  Active alerts, alert history, and alert acks now go through the control
  socket (`core.API.ActiveAlerts` / `AlertHistory` / `AckAlert`) instead of
  decoding `alerts.json` / `alertlog.jsonl` directly, and the ack handler no
  longer writes `alerts.json` itself (it had been a second writer racing the
  daemon). This also removes the disk shortcut that masked the socket-desync
  bug above, so the dashboard now fails coherently rather than half-failing
  into all-zeros. The web keeps ownership of its own auth material (users,
  sessions, enrollment tokens); the core has nothing to do with auth.
- `core.AlertRecord` gains a `DeliveredTo` field so the alerts page's Delivered
  column keeps its per-channel names when read over the socket.

### Internal

- The docker validate harness builds under Go 1.24 (matching `go.mod`) rather
  than the stale 1.22 base image.

## [0.4.0] - 2026-08-02

The core-plus-plugin release. serverwatch is reshaped from a single monolithic
daemon into a lean, stdlib-only `serverwatch` core with plugin binaries layered
around it over a local control socket. The core stays small while the web UI
and management tooling move out of process.

### Added

- **Control socket and `core.API`.** The core exposes one internal `core.API`
  contract over a unix socket in the runtime directory, newline-delimited JSON,
  one request or response per line. Each daemon launch mints a fresh token that
  a client must present in a handshake before the socket answers, keeping the
  channel local and gated to processes that can read the token.
- **`serverwatch-web` out of process.** The passkey web UI now runs as its own
  binary with no build tag, talking to the core over the socket. The core
  verifies, spawns, restarts (capped backoff), and stops it as a child process
  when `web.enabled` is set. The default `serverwatch` binary is stdlib-only,
  enforced by a dependency-graph test.
- **`serverwatch-ctl`, the primary management client.** A separate interactive
  binary that dials the socket, with a styled live-status home dashboard
  (colour-coded CPU/MEM/SWAP meters, a live CPU sparkline, an alerts panel, a
  disks panel, a 24h availability strip, and a network/inventory line), guided
  screens for schedule, quiet hours, healthchecks, monitor thresholds, and
  channels, an all-settings screen over every remaining config key, a guided
  web-setup wizard functional in every serving mode, first-run Telegram
  onboarding, a `?` help overlay, and breadcrumbs.
- **Scriptable `serverwatch-ctl` subcommands.** `status`, `doctor`, and
  `alerts` gain `--json` output; `config get <key>` / `config set <key>
  <value>` reach every flat config key through the same validated setter the
  TUI uses (applied live); `channels test <name>` sends a live test
  notification.
- **Live event streaming over the control socket.** `core.API.Subscribe` runs
  end to end: an in-process event bus that the sampler loop and every dispatched
  alert publish onto, a dedicated socket connection streaming those events, and
  `serverwatch-web` subscribing to push live dashboard updates.
- **Front-door install and safe-exec.** `serverwatch install` records each
  plugin's checksum in a root-only manifest; the `serverwatch cli` and
  `serverwatch web` front-doors verify a plugin (owner, permissions, checksum)
  against that manifest before exec'ing it.
- **Mobile web UI.** The dashboard is fully responsive: a bottom tab bar with a
  "More" sheet, card-list tables, and layouts gated to narrow viewports.
- **Enrollment PIN over the socket (#90).** `serverwatch telegram set-token`
  prints the `/start <pin>` instruction directly to the terminal after saving
  the token; `serverwatch-ctl`'s onboarding surfaces the same PIN.
- **Optimized production release channel.** `make release-prod` builds stripped,
  trimmed binaries (`-s -w -trimpath`) for the stable line, alongside the
  unstripped `make release` used for beta/dev builds.

### Changed

- The web UI no longer builds with `-tags web` inside the daemon; it is a
  separate supervised process. Guided setup (web UI, Telegram onboarding) is
  owned by `serverwatch-ctl`; the core CLI keeps only thin, scriptable verbs.
- `serverwatch install` now restarts an already-running service on an in-place
  upgrade (enable + restart) instead of `enable --now`, which only started a
  stopped service.
- Documentation is ctl-first throughout, with download-first install
  instructions and dedicated plugin pages.

### Fixed

- `serverwatch-ctl` now treats `SERVERWATCH_CONTROL_TOKEN` as the token value
  (as the front-doors and web supervisor set it), not a file path, fixing an
  "unexpected server hello" handshake failure for `serverwatch cli`/`web`.
- Telegram command authorization is enforced against the enrolled owner chat
  (security hardening).
- Mobile web UI: the header no longer forces horizontal page scroll (dropped
  the fixed-width heartbeat, title flexes/truncates); the active-alerts card no
  longer widens the page on long unbreakable alert keys; the monitoring tab
  strip scrolls within itself instead of overflowing.

### Security

- Per-launch control-socket token with a constant-time compare, `0600` socket
  in a `0700` runtime directory, and checksum-manifest verification before any
  plugin is exec'd. A security review was run over the web UI and control paths,
  with findings triaged and tracked.

## [0.3.2] - 2026-07-27

- Interval validation, a live container sidebar, a reworked public status page
  (allowlist-filtered SSE, anonymous-safe), cache-busting asset versioning, and
  opt-in baseline alerts.

## [0.3.1] - 2026-07-26

- Web UI security hardening (TOCTOU, CSP, account-takeover fixes) and Telegram
  message-size and markdown fixes.

## [0.3.0] - 2026-07-26

- The passkey web UI: WebAuthn auth, RBAC, a live dashboard over SSE, history
  graphs, a web config editor, an alerts page, and a curated public status view,
  built behind `-tags web` so the default binary stayed dependency-free.

## [0.2.0] - 2026-07-26

- Multi-channel alerting (email/SMTP, generic webhook, Slack, Discord, ntfy,
  Gotify alongside Telegram) and tiered sampling with the `tsfile` time-series
  store.

## [0.1.0] - 2026-07-25

- Initial release: the stdlib-only `serverwatch` daemon with core metric
  collection, threshold and anomaly detection, and Telegram alerting.

[Unreleased]: https://github.com/InfoDiveLabs/trinetra/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.5.0
[0.4.1]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.4.1
[0.4.0]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.4.0
[0.3.2]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.3.2
[0.3.1]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.3.1
[0.3.0]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.3.0
[0.2.0]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.2.0
[0.1.0]: https://github.com/InfoDiveLabs/trinetra/releases/tag/v0.1.0
