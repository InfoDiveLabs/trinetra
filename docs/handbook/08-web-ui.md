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

## Two deployment shapes that share one name

This is the single most important thing to get straight before you install
anything. There are two different binaries, and today they are both named
`serverwatch-web`. They are built from different packages, they run in
different ways, and only one of them is a finished, shipping feature. When you
read "serverwatch-web" in a release, a Makefile, or a shell prompt, you have to
know which of the two is meant.

| | (a) In-process embedded web | (b) Out-of-process web plugin |
|---|---|---|
| Status | Shipped, working path | Beta / preview only |
| Package | `./cmd/serverwatch` with `-tags web` | `./cmd/serverwatch-web` with `-tags web` |
| Built by the Makefile? | Yes, `make web` | No |
| How it runs | A goroutine inside the daemon | A separate process next to the daemon |
| How it reads state | Directly, in-process | Dials the daemon's control socket |
| Live push (SSE) | Full | Degraded |

Both compilations produce a file literally called `serverwatch-web`, which is
why the distinction is easy to miss. The rest of this section spells out each
one.

### (a) In-process embedded web: the shipping path

This is the one you almost certainly want. The web server is compiled into the
daemon under the `web` build tag and runs as a goroutine inside the same
process. There is no second binary to run, no socket to bridge, and nothing new
to keep alive: if the daemon is up, the web UI is up.

Build it with the Makefile:

```bash
make web
```

That target runs:

```bash
go build -tags web -o dist/serverwatch-web ./cmd/serverwatch
```

Note what that command is: it builds the ordinary `./cmd/serverwatch` package,
the whole daemon and CLI, and the `web` tag adds the embedded web server on
top. The output is named `dist/serverwatch-web` to mark that this is the
web-capable build, but it is the same daemon in every other respect. It reads
the same config file, installs the same systemd unit, and answers the same CLI
commands. The plain `serverwatch` binary (from `make build`) never links the
web code at all and carries none of its dependencies.

Install that binary the same way you install any serverwatch build (the
`install` command copies whichever binary is currently running to
`/usr/local/bin/serverwatch` and wires up the unit), then turn the server on:

```bash
sudo serverwatch config set web.enabled true
sudo systemctl restart serverwatch
```

Toggling `web.enabled` takes effect on the next restart. The `web.*` keys (see
[Configuration](04-configuration.md)) are not reloaded on SIGHUP, so a
`systemctl restart serverwatch` is what actually starts or stops the listener. Installing the web-capable binary and setting
`web.enabled true` is the entire path to a web-serving service; there is no
separate "web" unit.

A subtle but useful detail: the `web.*` and `public.*` config keys exist in
both binaries' config schema, so `serverwatch config set web.enabled true`
always succeeds even on a plain `serverwatch` install. The plain binary simply
never reads those keys. Setting them has no effect until the web-capable binary
is the one running.

### (b) Out-of-process web plugin: beta preview

The second `serverwatch-web` lives in `./cmd/serverwatch-web`. It is a distinct
program that does not embed the daemon. Instead it dials the daemon's control
socket, borrows the socket client as its data source, and serves the exact same
UI as its own process alongside the daemon. It is the first step of the
plugin-over-socket direction described in the [Architecture
chapter](02-architecture.md).

It is a preview, and you should treat it as one:

- It is not built by the Makefile. You build it by hand with
  `go build -tags web ./cmd/serverwatch-web`.
- There is no supervisor. Nothing in the core daemon spawns it, restarts it, or
  keeps it alive; you run and babysit it yourself.
- Live push is degraded. The socket transport does not yet support the
  streaming Subscribe method, so server-sent events over the plugin cannot push
  the way the in-process build does.

Because it reads through the control socket, the plugin needs to find the
socket and its auth token. It resolves those from flags, then the
`SERVERWATCH_CONTROL_SOCKET` / `SERVERWATCH_CONTROL_TOKEN` environment
variables, then the default runtime paths (`/run/serverwatch/control.sock` and
the sibling `token` file). A few local, on-disk paths that the socket cannot
provide (the state directory for sessions, and the alert log and alert-state
files) come from its own flags, defaulting under `/var/lib/serverwatch`.

Use the out-of-process plugin for preview and experimentation. For anything you
rely on, use the in-process embedded build (a).

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

`web.mode` decides how the embedded server binds and how (or whether) it
terminates TLS. Pick the mode that matches how you already expose services on
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
| `web.enabled` | `false` | Turns the embedded server on. Opt-in even in the web-capable binary. |
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
