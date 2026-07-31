# Core + plugin foundation (design)

Date: 2026-08-01
Status: proposed
Scope: foundation only. The agent/hub cluster is a later epic that builds on this seam.

## Goal

Carve today's single-binary monitor into a **sensor-only core** plus **core-supervised plugins**, preserving current behavior, with the core exposing its capabilities over a **local control socket**. The control contract is deliberately the same shape a future agent/hub cluster needs, so the cluster work becomes additive (swap the local socket for a network transport) rather than a rewrite.

Explicit non-goals for this epic: the network protocol, multi-node, the hub, push vs pull, store-and-forward. Those come later and reuse this contract.

## Decisions (locked with the maintainer)

1. **Core role = sensor only.** The core collects, detects (edge anomaly/baseline), persists (tsfile + status.json + heartbeat + watchdog), emits alert events, and serves a control API. Delivery and UI are not hard-wired code paths in the detection loop.
2. **Delivery is a consumer, embedded by default.** On a single host the notifier runs as an in-process consumer of the core's event bus, so a home user runs exactly one service and never loses alerts. It is written as a consumer so a future hub can host it instead.
3. **Transport = unix socket + newline-delimited JSON.** `/run/serverwatch.sock`, mode 0600, root-owned. Stdlib `net` + `encoding/json` only. No gRPC, no new dependencies in the core.
4. **Process model = core-supervised plugins (go-plugin style).** The core owns plugin lifecycle and the trust boundary: it launches plugin binaries, hands each a per-launch handshake token, monitors them, and restarts the long-running ones.
5. **Core stays 100% stdlib.** Plugins carry their own dependencies (web: webauthn/htmx; ctl: Bubble Tea). Enforced by a build test, as the web split is today.

## Architecture

```
core: serverwatch (stdlib daemon)
  collectors (fast/slow) -> anomaly/baseline -> downtime
  SampleStore/tsfile, status.json, heartbeat, systemd watchdog
  event bus (fire / recover / digest / boot)
  config store + SIGHUP reload
  control server (unix socket, newline JSON)
  plugin supervisor
  embedded consumers by default: notifier (delivery), healthchecks, telegram-inbound (optional)

plugin: serverwatch-web   client of the control socket; supervised child when web.enabled
plugin: serverwatch-ctl   client; core-mediated launch for interactive management
```

## The Core contract

One Go interface, `core.API`, is the single seam. Two implementations exist behind it: an in-process one (S1) and a socket-client one (S2). Consumers depend only on the interface.

```
Snapshot() (Snapshot, error)
History(series string, since, until int64, res Resolution) ([]Point, error)
Events(since int64) ([]Event, error)
Alerts() ([]AlertRecord, error)
Config() (*config.Config, error)
ApplyConfig(*config.Config) error        // validate + persist + reload
Discover() ([]Target, error)
TestChannel(name string) error
Doctor() (DoctorReport, error)
Subscribe(ctx) (<-chan Event, error)     // delivery and other consumers
```

Everything web, ctl, and the notifier need is here. Today's `web.Deps` collapses into this interface.

## Control protocol (over the socket)

- **Framing:** one JSON object per line. Request `{id, method, params}`; response `{id, ok, result|error}`. A subscribed connection also receives `{event: ...}` lines.
- **Auth:** connecting requires the socket itself (0600, root-owned). Additionally the core hands each spawned plugin a one-time token via env/inherited fd, presented in a `hello` frame. An operator running `ctl` directly reads a root-only token file.
- **Versioning:** the `hello` exchange carries a protocol version; a mismatch fails fast with a clear message rather than misbehaving.

## Plugin supervision (go-plugin style)

- The core spawns a plugin binary whose path is pinned and checksum-verified, passing the socket path plus a one-time token via env.
- **Web plugin:** long-running and supervised: started when `web.enabled`, restarted on crash with backoff, stopped on disable or shutdown.
- **CLI manager:** interactive. The user runs `serverwatch manage`, which the core execs into the ctl binary, handing over the tty plus socket plus token, so launch and trust stay core-mediated. Direct `serverwatch-ctl` invocation falls back to dialing the socket with the root-only token file.
- **Health:** a periodic ping over the socket; an unresponsive supervised child (web) is restarted under a backoff with a restart-storm cap.

## Alerting inversion (event bus)

Today the detection loop calls dispatch directly. New model: detection publishes `AlertEvent`s (fire / recover / digest / boot) to an in-process bus; a notifier consumer subscribes and delivers to channels. Outbound behavior is unchanged, but delivery is now a swappable subscriber. Later, an agent forwards these events to the hub and the hub's notifier consumes them.

## Build and packaging

- `cmd/serverwatch`      core daemon (stdlib)
- `cmd/serverwatch-web`  web plugin (webauthn/htmx)
- `cmd/serverwatch-ctl`  cli plugin (Bubble Tea)
- `go.mod` gains Bubble Tea; the core binary links none of it (verified by the same kind of build test that keeps the default binary web-dependency-free today).
- Makefile builds all three; a release ships the core plus plugins per arch.

## Sequencing (incremental, each stage shippable and green)

- **S1. Core contract, in-process.** Define `core.API`; route in-process consumers through it (`web.Deps` becomes an in-process `core.API`; the CLI `status`/`dump`/`alerts`/`doctor` go through it). No process change. Pure refactor.
- **S2. Control socket.** Add the socket server, the newline-JSON codec, the socket-client `core.API`, and `hello`/auth/version. Now the interface has two implementations.
- **S3. Split the CLI out.** `serverwatch-ctl` plus the supervisor and handshake. Query and config commands run over the socket. First plugin: proves the protocol end to end cheaply.
- **S4. Split the web out.** `serverwatch-web` handlers use the socket-client `core.API`; the live dashboard/SSE and the `DashboardView`/`MonitoringView`/history serialize across the socket; retire the in-process `web.Deps` wiring. Heaviest stage.
- **S5. Alerting inversion.** Detection publishes to the event bus; the notifier becomes an embedded consumer; the telegram-inbound bot becomes a consumer/client.

Deferred to the next epic: network transport (agent to hub), multi-node, push plus store-and-forward buffering, cross-node aggregation, the Bubble Tea rich-UX build-out (issue #83, built on this seam), and the mobile UI (issue #81, independent of this epic).

## Security

- Socket is 0600, root-owned, never bound to the network.
- Per-launch one-time token for spawned plugins; a root-only token file for operator-launched `ctl`.
- Plugin binary path is pinned and checksum-verified before exec.
- The core validates every `ApplyConfig` input, reusing the existing config validators.

## Testing

- TDD throughout; `go test -race`; the default build plus the plugin builds.
- Protocol: codec round-trip, `hello`/version-mismatch, auth-reject, request/response plus event-stream conformance.
- Supervisor: spawn/handshake/restart-on-crash/stop; refusal on checksum mismatch.
- Parity: web and ctl behavior over the socket matches the in-process baseline (golden tests captured at S1).

## Risks

- The web out-of-process move (S4) carries real SSE/serialization cost and latency; it is the heaviest, highest-risk stage and lands last.
- Protocol versioning needs discipline across core and plugins.
- Supervision edge cases (zombie children, restart storms) are bounded by backoff plus a restart cap.
- The interactive-CLI-under-supervision nuance is handled by core-mediated exec plus the token-file fallback.

## Impact on current work

- The shipped fixes for #78, #79, #80, #82 (PRs #85 to #88) are on today's code and remain valid.
- Issue #83 (interactive manager) becomes the `ctl` plugin, built on this seam after S3.
- Issue #81 (mobile) is web-UI only and independent of this epic.
