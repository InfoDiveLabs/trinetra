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

## Planned / follow-ups

Open issues on the board:

- [#1 Deploy to the home server and verify](https://github.com/Suraj-Tiwari/server-monitor/issues/1)
- [#2 systemd watchdog (WatchdogSec + sd_notify)](https://github.com/Suraj-Tiwari/server-monitor/issues/2)
- [#3 `//go:build unix` tag for signal_unix.go](https://github.com/Suraj-Tiwari/server-monitor/issues/3)
- [#4 Throttle SMART health checks](https://github.com/Suraj-Tiwari/server-monitor/issues/4)
- [#5 Improve binary-state alert wording](https://github.com/Suraj-Tiwari/server-monitor/issues/5)

## Not planned (yet)

- CI/CD (intentionally deferred).
- Multi-host aggregation, web UI, metrics export (Prometheus). Out of scope for a single-host worker.
