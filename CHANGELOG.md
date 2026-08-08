# Changelog

All notable changes to serverwatch are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [semantic versioning](https://semver.org/spec/v2.0.0.html). Dates are
YYYY-MM-DD. Preview builds are cut as `vX.Y.Z-beta.N` tags on the `develop`
branch; stable releases are tagged on `main`.

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

[0.4.0]: https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.4.0
[0.3.2]: https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.3.2
[0.3.1]: https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.3.1
[0.3.0]: https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.3.0
[0.2.0]: https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.2.0
[0.1.0]: https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.1.0
