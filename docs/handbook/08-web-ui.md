# The web UI

serverwatch is a Telegram tool first, but it ships an optional browser
interface for people who would rather look at graphs than read chat messages.
The web UI is entirely opt-in and passkey only. It is an alternative to
Telegram, not a replacement for it: the same daemon can push alerts to a chat
and serve a dashboard at the same time, and neither depends on the other.

What the web UI gives you is a small, self-contained console for one server:

- A live dashboard that updates in place as new samples land, no page reload.
- History graphs that plot the time-series the daemon already keeps (see
  [Storage and the data model](09-storage-and-data-model.md)).
- A web-based editor for your configuration, notification channels, and user
  accounts, so you can change settings without touching the CLI.
- An alerts page listing recent alerts, with acknowledgement for admins.
- A curated public status page you can expose anonymously, showing only the
  panels you explicitly allow.

Everything the web UI shows comes from the same data the CLI and Telegram
already use: the live snapshot and the sampled time-series. The UI adds no new
collectors and writes no new on-disk series. It is a second reader of data the
daemon is already producing.

## How the web UI runs

There is one `serverwatch-web` binary, and it is a plain, separate program
with no build tag:

```bash
go build -o /usr/local/bin/serverwatch-web ./cmd/serverwatch-web
```

`internal/web` is an ordinary, untagged package like any other in the
codebase. What keeps its dependencies (the WebAuthn stack and the rest) out of
the daemon is not a build tag; it is simply that the daemon package never
imports `internal/web`. `serverwatch install` builds and ships all three
binaries (`serverwatch`, `serverwatch-ctl`, `serverwatch-web`) and records
their checksums in the root-only install manifest (see [Installation and
first run](03-installation.md)).

`serverwatch-web` does not embed the daemon and does not read its state
directly. Like `serverwatch-ctl`, it dials the daemon's [control
socket](02-architecture.md#the-control-socket), borrows the socket client as
its data source, and serves the UI from its own process. Live push over SSE
degrades the same way it would for any control-socket consumer: `Subscribe`,
the streaming method, is not implemented over the socket yet (see the
[roadmap chapter](12-roadmap-and-status.md)), so the dashboard falls back to
polling instead of a live push where that matters.

There are two ways `serverwatch-web` gets started:

- **Supervised, via `web.enabled`.** This is the path for anything you run
  day to day. Set `web.enabled true` and the daemon itself verifies and
  spawns `serverwatch-web` as a child process, passing it the control socket
  path and a per-launch token. If the child exits, the daemon restarts it
  under a capped backoff; if the daemon shuts down, it stops the child too.
  You never run or babysit a second process by hand. See [Web
  supervisor](02-architecture.md#web-supervisor) for the full lifecycle and
  its diagram.

  ```bash
  sudo serverwatch config set web.enabled true
  sudo systemctl restart serverwatch
  ```

  Toggling `web.enabled` takes effect on the next restart: the supervisor
  decides once, at daemon startup, whether to spawn the child, and the
  `web.*` keys are not reloaded on SIGHUP, so `systemctl restart serverwatch`
  is what actually starts or stops it. There is no separate "web" unit;
  installing `serverwatch-web` and setting `web.enabled true` is the entire
  path to a web-serving service.

- **Manual, via `serverwatch web`.** This front-door subcommand runs the same
  trust checks the supervisor uses, then execs `serverwatch-web` directly in
  the foreground. Reach for this when you want to run the web UI yourself,
  for example while testing on a box where `web.enabled` is off.

  ```bash
  sudo serverwatch web
  ```

Either way, before `serverwatch-web` can run at all, the core has to be able
to verify it: an absolute path next to the core binary's own directory, root
ownership with no group/world write bit, and a SHA-256 match against the
install manifest. If you build or hand-copy `serverwatch-web` into place
yourself, (re-)run `serverwatch install` afterward so its checksum is
recorded; until then, both the supervisor and the `serverwatch web`
front-door refuse to run it and log why. See [The front-door safe-exec trust
model](02-architecture.md#the-front-door-safe-exec-trust-model) for the full
checks.

A subtle but useful detail: the `web.*` and `public.*` config keys exist in
`serverwatch`'s config schema regardless of whether `serverwatch-web` is
installed, so `serverwatch config set web.enabled true` always succeeds even
before the plugin binary is present. It simply has nothing to supervise until
`serverwatch-web` exists next to the core binary and passes verification.

## Authentication and roles

There is no password login anywhere in the web UI. Accounts authenticate with
WebAuthn passkeys only: a platform authenticator like Touch ID or Windows
Hello, or a hardware security key. Sessions are kept server-side, and state-
changing requests are protected with CSRF tokens.

Getting the first account is a one-time bootstrap. With `web.enabled true` and
no accounts yet, you visit `/enroll` with no token and register a passkey. The
very first passkey to register becomes the admin account. This decision is
atomic: if two people race to bootstrap at the same instant, exactly one wins
and becomes admin, and the other is told enrollment is closed. There is never a
window where the instance has two admins from the race or none at all.

Once any account exists, tokenless `/enroll` stops working. Every subsequent
user joins through a single-use enrollment token. An admin issues a token from
the `/users` page (choosing the new account's role and the token's lifetime),
shares the resulting `/enroll?token=...` link, and the invitee registers their
own passkey against it. The token is burned on first use whatever the outcome,
and it expires on its own even if it is never used. Admins can also change an
existing account's role, remove accounts, and revoke individual passkeys, with
a standing guard that refuses to demote or remove the last remaining admin, so
the instance can never lock itself out of administration.

There are three roles:

| Role | What it can do |
|------|----------------|
| `admin` | Everything a viewer can, plus `/config`, `/channels`, `/users`, `/settings/public`, and acknowledging alerts. |
| `viewer` | Read-only: the dashboard, history graphs, and the alerts page (view only, no ack). |
| anonymous | Only `/public`, and only when `public.enabled` is true. A curated, admin-picked subset of metrics, no login, no other route reachable. |

## The pages

Signed in, the UI is a handful of routes.

- **Live dashboard.** The landing page for any signed-in user. It shows the
  current state of the machine and updates in place over server-sent events
  (SSE) as fresh samples arrive, so you do not reload to see new numbers. It
  includes a 24 hour availability strip built from real per-request data.
- **History graphs.** Time-series charts of the metrics the daemon keeps,
  rendered client-side with uPlot. Same series the CLI and Telegram read; the
  page is just another view onto them.
- **`/config`.** An admin editor for the daemon's configuration. Writes go
  through the same validate-persist-apply path the CLI uses, so a change saved
  here is a change the daemon has validated.
- **`/channels`.** An admin editor for notification channels, with a test
  action to send through a channel and confirm it works.
- **`/users`.** The admin account console: issue enrollment tokens, set roles,
  remove accounts, and revoke passkeys.
- **`/settings/public`.** The admin control for the anonymous status page:
  toggle it on and pick which panels it exposes.
- **`/alerts`.** Recent alerts. Viewers can read it; admins can acknowledge
  from it.
- **`/public`.** The anonymous status page, described in its own section below.
  It is the only route an unauthenticated visitor can reach, and only when it
  is enabled.

## Serving modes

`web.mode` decides how the `serverwatch-web` server binds and how (or
whether) it terminates TLS. Pick the mode that matches how you already expose services on
the host. Whatever you choose, a web failure never takes down monitoring: the
web configuration is validated at startup, and an invalid or incomplete
combination makes the web listener refuse to start while the daemon keeps
running. Telegram and monitoring are unaffected; the daemon logs the failure,
skips the listener, and you fix the config and restart.

### `proxy` (default)

The server binds plain HTTP on `web.listen` (default `127.0.0.1:8088`) and
expects a local reverse proxy, Cloudflare Tunnel, nginx, or Caddy, to terminate
TLS and forward the original scheme and host. When the listener is loopback
bound, the server derives its WebAuthn relying-party ID and origin per request
from the trusted `X-Forwarded-*` headers, so you can leave `web.rp_id` and
`web.origin` empty. Those headers are only trusted on a loopback bind: a
public bind in this mode would let any client on the network spoof its own
origin, so keep `web.listen` on `127.0.0.1` when you front it with a proxy.

```bash
sudo serverwatch config set web.mode proxy
sudo serverwatch config set web.listen 127.0.0.1:8088
sudo systemctl restart serverwatch
```

You can set `web.rp_id` and `web.origin` explicitly to the public hostname if
you prefer; they are validated the same way (the origin's host must equal
`web.rp_id`).

### `autocert`

The server terminates TLS itself and obtains and renews a Let's Encrypt
certificate automatically. This needs `web.listen` reachable for HTTPS, port 80
free and reachable for the ACME HTTP-01 challenge, `web.autocert_domains` set to
the public hostnames, and `web.rp_id` / `web.origin` set and matching (they are
required here, not derived).

```bash
sudo serverwatch config set web.mode autocert
sudo serverwatch config set web.listen :443
sudo serverwatch config set web.autocert_domains monitor.example.com
sudo serverwatch config set web.rp_id monitor.example.com
sudo serverwatch config set web.origin https://monitor.example.com
sudo systemctl restart serverwatch
```

### `manual`

You supply your own certificate and key, for an internal CA or a wildcard you
already manage. Both `web.tls_cert` and `web.tls_key` must point at readable PEM
files, and `web.rp_id` / `web.origin` are required and matching as in autocert.

```bash
sudo serverwatch config set web.mode manual
sudo serverwatch config set web.listen :443
sudo serverwatch config set web.tls_cert /etc/serverwatch/tls/fullchain.pem
sudo serverwatch config set web.tls_key  /etc/serverwatch/tls/privkey.pem
sudo serverwatch config set web.rp_id monitor.example.com
sudo serverwatch config set web.origin https://monitor.example.com
sudo systemctl restart serverwatch
```

### The `web.*` keys

| Key | Default | Meaning |
|-----|---------|---------|
| `web.enabled` | `false` | Turns on daemon supervision of `serverwatch-web`. Opt-in even when the binary is installed. |
| `web.listen` | `127.0.0.1:8088` | The `host:port` the server binds. Loopback by default; front it with a proxy for LAN or WAN access. |
| `web.mode` | `proxy` | One of `proxy`, `autocert`, or `manual`. |
| `web.rp_id` | `""` | WebAuthn relying-party ID: the public hostname passkeys are scoped to, no scheme or port. Required in autocert and manual; optional (derived) in proxy. |
| `web.origin` | `""` | Full public origin, `https://host[:port]`. Its host must equal `web.rp_id`. Required in autocert and manual. |
| `web.autocert_domains` | `""` | Comma-separated hostname allowlist for Let's Encrypt. Required in autocert mode. |
| `web.tls_cert` | `""` | PEM certificate path. Required in manual mode. |
| `web.tls_key` | `""` | PEM key path. Required in manual mode. |
| `web.session_ttl` | `24h` | How long a signed-in session stays valid (a `time.ParseDuration` string). |

`web.rp_id`, `web.origin`, `web.mode`, and `web.listen` are validated at
startup. An invalid or incomplete combination (manual mode with no cert, or an
`rp_id` that does not match the origin's host) makes the web server refuse to
start, without touching the rest of the daemon.

## The public status page

The public page is the only anonymous surface in the web UI, and it is off by
default. It is meant for a status page you can hand to anyone without giving
them a login, showing only the handful of metrics you decide are safe to share.

Two settings gate it, and nothing is exposed until both are set. `public.enabled`
turns on the `GET /public` route (while false, the route 404s and does not even
hint that a public page could exist). `public.panels` is an allowlist of exactly
which panels appear, enforced server-side, so a metric absent from the list can
never leak onto `/public` no matter what is visible elsewhere in the UI.

```bash
sudo serverwatch config set public.enabled true
sudo serverwatch config set public.panels availability,cpu,mem,disk:/,uptime
```

An admin can also curate the same allowlist from `/settings/public` once signed
in. The recognized panel ids are:

| Panel id | Shows |
|----------|-------|
| `availability` | A 24 hour up/down strip. |
| `cpu` | Live CPU percentage. |
| `mem` | Live memory percentage. |
| `swap` | Live swap percentage. |
| `load` | Load average (1m/5m/15m). |
| `temp` | Hottest temperature sensor. |
| `uptime` | Online/offline status. |
| `services` | Count of services up. |
| `containers` | Count of containers. |
| `net` | Network throughput. |
| `disk:<mount>` | Usage for a specific mount, e.g. `disk:/`. |

The `availability` panel is worth calling out on its own, because it is not a
scalar tile like the others: it renders a whole 24 hour up/down strip rather
than a single number, and it must be listed in `public.panels` by name for that
strip to appear. Older copies of the reference documentation omit it from the
allowlist; the panel is real and allowlistable, and the table above is the
authoritative list.

---

[Previous: Downtime and liveness](07-downtime-and-liveness.md) | [Handbook index](README.md) | [Next: Storage and the data model](09-storage-and-data-model.md)
