# Roadmap & status

Tracking board: **[Home Server project](https://github.com/users/Suraj-Tiwari/projects/1)**
(cards grouped by *Area*; open follow-ups are linked issues).

**Current release:** [v0.1.0](https://github.com/Suraj-Tiwari/server-monitor/releases/tag/v0.1.0) — feature-complete for a single home server.

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

### Post-release hardening (follow-ups)
- [#1 Deploy to the home server and verify](https://github.com/Suraj-Tiwari/server-monitor/issues/1)
- [#2 systemd watchdog (WatchdogSec + sd_notify)](https://github.com/Suraj-Tiwari/server-monitor/issues/2)
- [#3 `//go:build unix` tag for signal_unix.go](https://github.com/Suraj-Tiwari/server-monitor/issues/3)
- [#4 Throttle SMART health checks](https://github.com/Suraj-Tiwari/server-monitor/issues/4)
- [#5 Improve binary-state alert wording](https://github.com/Suraj-Tiwari/server-monitor/issues/5)

### Epic [#33 Multi-channel alerting](https://github.com/Suraj-Tiwari/server-monitor/issues/33)
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

### Epic [#44 Tiered sampling + efficient storage](https://github.com/Suraj-Tiwari/server-monitor/issues/44)
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

### Epic [#69 Extended datapoint collection (UI data coverage)](https://github.com/Suraj-Tiwari/server-monitor/issues/69)
Collect the extra data the UI shows, cheaply — must land before the UI's stats/history tasks. Bounded time-series vs live snapshots vs event-log; opt-in, slow-tier collectors; ~tens of MB on disk thanks to tsfile.
- #70 Alert event log + ack + delivery record (powers Alerts history)
- #71 Per-container CPU/mem/net (docker stats) → series
- #72 Network throughput series (/proc/net/dev deltas)
- #73 Full systemd unit inventory (live snapshot)
- #74 Process snapshot: top-N + counts (live, no series)
- #75 Schema extras: load5/15, inodes, fs type/device, SMART attrs, fill projection
- #76 Extended-collection config toggles + cardinality/disk guardrails
- #77 Integrate into status.json + SampleStore + docs

### Epic [#56 Web UI (HTMX + passkeys, multi-user)](https://github.com/Suraj-Tiwari/server-monitor/issues/56)
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
