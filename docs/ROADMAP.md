# Roadmap & status

Tracking board: **[Home Server project](https://github.com/users/Suraj-Tiwari/projects/1)**
(cards grouped by *Area*; open follow-ups are linked issues).

**Current release:** [v0.3.2](https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.3.2) — web-UI polish from real-deployment feedback: detailed live public page + `/` landing routing, `/monitoring` detail view, full web config editor, real nav/status/availability data, quieter alerts (baseline σ-detection opt-in), docker-overlay disk filtering, cache-busted assets. Deployed to a live host behind Cloudflare/nginx.
**Previous:** [v0.3.0](https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.3.0)/[v0.3.1](https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.3.1) — embedded HTMX web UI (passkey auth, RBAC, live dashboard/history) · [v0.2.0](https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.2.0) — alerting, tiered storage, extended collection · [v0.1.0](https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.1.0) — single home server.

## Delivered (v0.3.0)

Every task shipped test-first with an independent per-task review, then a final whole-branch review;
verified by the full unit suite + `go test -race` on both the default and `-tags web` builds. See
[docs/WEB.md](WEB.md).

**Epic #56 — Web UI (HTMX + passkeys, multi-user):**
- Embedded in the daemon as a goroutine (`-tags web` build) sharing the live snapshot, SampleStore,
  and config; the default `serverwatch` binary stays **100% stdlib** (enforced by a build test).
- Passkey-only **WebAuthn** auth; server-side sessions + CSRF; RBAC (admin/viewer) + first-passkey
  bootstrap + single-use enrollment tokens; strict CSP (nonce'd scripts).
- Self-configuring serving modes: **proxy** (Cloudflare Tunnel/nginx/Caddy), **autocert**, **manual** TLS;
  startup rpID/origin validation.
- Live dashboard (SSE), history graphs (uPlot + SampleStore), web config editor (thresholds/monitors/
  schedules/channels → in-process reload + audit log), alerts page (+ack), admin user management, and an
  admin-curated **anonymous public view** (server-side panel allowlist).
- UI ported from the local HTML mockup; htmx + uPlot vendored/embedded (no CDN).

## Delivered (v0.2.0)

Every task below shipped test-first with an independent per-task review; verified by the full unit
suite + `go test -race` + the dockerized end-to-end harness.

- **Epic #33 — Multi-channel alerting:** `Notifier`/dispatcher abstraction with severity + target
  routing; seven channels (Telegram, Email/SMTP, generic webhook, Slack, Discord, ntfy, Gotify);
  `channel` CLI + automatic Telegram migration.
- **Epic #44 — Tiered sampling + efficient storage:** fast (5s) / slow (60s) tiers; `SampleStore`
  interface + `tsfile` binary backend (versioned, per-series, corruption-tolerant, crash-durable
  prune); raw + 1m resolutions, downsampling, per-resolution retention; `dump`/`migrate` CLI.
- **Epic #69 — Extended datapoint collection:** alert event log + ack/delivery; per-container
  cpu/mem series; net throughput series; systemd inventory + process snapshots (live); load5/15,
  inodes, fstype, fill projection, SMART attributes; opt-in `collect.*` toggles + `doctor` guardrail.
- **Post-release hardening:** systemd watchdog (#2), `//go:build unix` (#3), throttled SMART scans
  via `collect.smart_interval` (#4), human wording for binary-state alerts (#5).

## Delivered (v0.1.0)

Verified by the full unit suite + `go test -race` + a dockerized end-to-end harness
(`make validate`: discovery, anomaly, downtime, docker-down scenarios).

**Core engine**
- CLI-managed config store (no env), atomic 0600 writes, per-target overrides
- Side-effect interfaces (Clock/Exec/FileSource) for testability
- Metric parsers: CPU/mem/swap/load/uptime/disk/temperature from `/proc`, `df`, `sysfs`
- Rolling baseline with bias-corrected exponential-variance z-score
- Anomaly engine: static thresholds + baseline deviation, fire/recover, hysteresis, dedup
- Downtime tracking: minute heartbeat, `power_down` reconstruction on boot, live `net_down`
- On-disk store: per-day JSONL samples, 30-day retention, downtime log, `status.json`

**Discovery & network**
- Self-discovery of docker containers (socket → group → **sudo** fallback), filesystems, interfaces, temp sensors, SMART devices
- Internet reachability, public-IP change, per-interface throughput
- Alerting wired for docker-down, `systemctl --failed`, and SMART-health failures (with recovery)

**Telegram & reporting**
- Two-way bot: `/stats` `/status` `/disk` `/net` `/docker` `/services` `/history` `/down` `/help`
- Boot/recovery report, daily digest, weekly rollup, quiet hours
- Zero-config chat-id capture from the first inbound message

**Daemon, systemd & ops**
- `serverwatch install/uninstall/doctor`; self-written systemd unit (`Restart=always`, boot start)
- Sampler + long-poller goroutines; race-safe SIGHUP hot-reload; optional healthchecks.io dead-man switch

**Build, docs & release**
- `Makefile` cross-compile; README, `docs/DEPLOYMENT.md`, `docs/DESIGN.md`, MIT license
- v0.1.0 release with `linux-amd64/arm64/arm` + `darwin-amd64/arm64` binaries and checksums
- Final whole-branch review fixes: config crash-loop guard, exec/HTTP timeouts, temp-target key, schedule validation

## Planned

Everything below is tracked on the board as issues nested under epics.

### Post-v0.3.2 (real-deployment feedback)
Validated bug/UX reports from running the live host, filed as standalone issues (not yet grouped under an epic). Each was checked against the code before filing.
- [#78 Telegram bot accepts commands from any sender; zero-config chat-id is hijackable](https://github.com/Suraj-Tiwari/server-monitor/issues/78) (security, high): no inbound sender authz + trust-on-first-message capture.
- [#79 Telegram configured via web UI never delivers](https://github.com/Suraj-Tiwari/server-monitor/issues/79): chat-id auto-capture is wired only to the CLI-set global token, so a web-only setup is silently dropped.
- [#80 Web UI sidebar shows a hardcoded name, not the signed-in passkey user](https://github.com/Suraj-Tiwari/server-monitor/issues/80): name/avatar are role-keyed literals ("Suraj"/"Aditi"), never read from the real user.
- [#81 Mobile: fix clipped tables + dedicated mobile UI for data-dense sections](https://github.com/Suraj-Tiwari/server-monitor/issues/81): 5 tables clip for lack of `.tablewrap`; plus purpose-built mobile views for public/dashboard/monitoring/history.
- [#82 `sudo serverwatch` fails on RHEL/CentOS](https://github.com/Suraj-Tiwari/server-monitor/issues/82): `/usr/local/bin` is off sudo's `secure_path` on some distros; fix via a `/usr/bin` symlink at install.
- [#83 Interactive `serverwatch setup` wizard + up-front channel validation](https://github.com/Suraj-Tiwari/server-monitor/issues/83): no interactive setup exists; multi-channel config is flag-heavy and fails late.

Investigated but not filed: CPU alert on a single core at 100%. The `cpu` metric is already the aggregate of all cores normalized to 0-100 (reads only the summary `cpu ` line of `/proc/stat`), so a single saturated core cannot trip the threshold. Not a bug.

### Post-release hardening (follow-ups)
- [#1 Deploy to the home server and verify](https://github.com/Suraj-Tiwari/server-monitor/issues/1) — pending (manual, on-server)
- ~~#2 systemd watchdog~~ · ~~#3 `//go:build unix`~~ · ~~#4 Throttle SMART~~ · ~~#5 binary-state alert wording~~ — shipped in v0.2.0

### ✅ Epic [#33 Multi-channel alerting](https://github.com/Suraj-Tiwari/server-monitor/issues/33)
Channel-agnostic alerting: a `Notifier` abstraction + dispatcher with severity/type routing, pluggable channels (stdlib-only).
- #34 Notifier interface + Alert model + concurrent dispatcher
- #35 Severity levels + per-channel routing/filters
- #36 Channel config store + CLI + telegram migration
- #37 Refactor Telegram outbound into a Notifier channel
- #38 Wire dispatcher into daemon alert/digest/boot-report paths
- #39 Email (SMTP) channel
- #40 Generic webhook channel with body templating
- #41 Slack + Discord presets
- #42 ntfy / Gotify channel
- #43 `channel test` command + docs

### ✅ Epic [#44 Tiered sampling + efficient storage](https://github.com/Suraj-Tiwari/server-monitor/issues/44)
Fast tier (5s) for CPU/load/mem/swap driving detection + live status; 1-min aggregate persistence; a future-safe time-series store replacing JSON. Design: [docs/DESIGN-storage.md](DESIGN-storage.md).
- #45 Config keys (fast_interval, heartbeat_interval) + validation
- #46 Split collectors into fast/slow tiers
- #47 Per-metric baseline alpha derived from tier interval
- #48 Per-tier anomaly eval + live status.json + heartbeat cadence
- #49 In-memory fast ring buffer + 1-min aggregate
- #50 `SampleStore` interface + swappable backend
- #51 `tsfile` backend (versioned binary, per-series, corruption-tolerant)
- #52 Resolution tiers + downsampling + per-resolution retention
- #53 Migration (JSONL→tsfile) + `dump`/`migrate` CLI
- #54 Integrate `SampleStore` across daemon/handlers/digests
- #55 Tiered + storage docs & config update

### ✅ Epic [#69 Extended datapoint collection (UI data coverage)](https://github.com/Suraj-Tiwari/server-monitor/issues/69)
Collect the extra data the UI shows, cheaply — must land before the UI's stats/history tasks. Bounded time-series vs live snapshots vs event-log; opt-in, slow-tier collectors; ~tens of MB on disk thanks to tsfile.
- #70 Alert event log + ack + delivery record (powers Alerts history)
- #71 Per-container CPU/mem/net (docker stats) → series
- #72 Network throughput series (/proc/net/dev deltas)
- #73 Full systemd unit inventory (live snapshot)
- #74 Process snapshot: top-N + counts (live, no series)
- #75 Schema extras: load5/15, inodes, fs type/device, SMART attrs, fill projection
- #76 Extended-collection config toggles + cardinality/disk guardrails
- #77 Integrate into status.json + SampleStore + docs

### ✅ Epic [#56 Web UI (HTMX + passkeys, multi-user)](https://github.com/Suraj-Tiwari/server-monitor/issues/56)
HTMX UI with passkey-only auth, roles (admin/viewer/public), web-based config, live dashboard (SSE) + history graphs (uPlot), and an admin-curated public view. Dependency-isolated web build (cli-only stays stdlib); self-configuring serving modes (reverse proxy/Cloudflare Tunnel, autocert, manual TLS). _(Detailed design kept local, not in-repo.)_
- #57 Module + build variants (cli-only vs cli+web)
- #58 HTTP server skeleton (ServeMux + html/template + embedded htmx/uPlot)
- #59 Serving modes (proxy/autocert/manual) + rpID/origin validation + security headers
- #60 WebAuthn passkey registration + credential store
- #61 WebAuthn login + sessions + CSRF + logout
- #62 User store + RBAC (admin/viewer) + bootstrap + enrollment tokens
- #63 User management pages (admin)
- #64 Live dashboard via SSE
- #65 History graphs (uPlot, SampleStore)
- #66 Web config editor (+ audit log)
- #67 Public view (exposure config + admin panel picker + curated anon route)
- #68 Docs + deploy recipes + web systemd unit

## Not planned (yet)

- CI/CD (intentionally deferred).
- Multi-host aggregation, external metrics export (Prometheus/remote-write) — enabled later by the `SampleStore` interface, but out of scope now.
