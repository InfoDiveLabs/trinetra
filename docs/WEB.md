# Web UI

serverwatch ships an optional, embedded web UI — passkey (WebAuthn) login, a
live dashboard, history graphs, a config/channels/users editor, and a
curated public status page — as an alternative to (not a replacement for)
Telegram. It's compiled into a **separate binary** and is entirely opt-in.

## cli-only vs cli+web build

There are two build variants, both from the same `./cmd/serverwatch` package:

| Binary | Build command | Contents |
|--------|----------------|----------|
| `serverwatch` | `go build ./cmd/serverwatch` (or `make build`/`make linux`) | The daemon + CLI + Telegram, 100% Go stdlib. Never imports `internal/web`; has no WebAuthn/autocert dependency at all. This is the default and the one the release matrix treats as primary. |
| `serverwatch-web` | `go build -tags web ./cmd/serverwatch` (or `make web`) | Everything above, **plus** the embedded web server (`internal/web`, `github.com/go-webauthn/webauthn`, `golang.org/x/crypto/acme/autocert`). |

Both are the exact same daemon otherwise — same config file, same systemd
unit shape, same CLI commands. The `web` build tag only adds code; it never
changes existing behavior. Config keys under `web.*`/`public.*` exist in
**both** binaries' `config.json` schema (so `serverwatch config set web.enabled
true` always works), but the plain `serverwatch` binary never reads them —
setting them has no effect until you install `serverwatch-web` instead.

If you don't want the web UI, keep using the plain `serverwatch` binary; it
carries no additional attack surface or dependency for it.

## Enabling it

1. Install/run the `serverwatch-web` binary (see **systemd** below for the
   on-server path — installing this binary is exactly what makes
   `web.enabled` take effect).
2. Turn the server on and pick a serving mode:

   ```bash
   sudo serverwatch config set web.enabled true
   sudo serverwatch config set web.mode proxy        # proxy | autocert | manual (see below)
   sudo systemctl restart serverwatch                 # web.* isn't SIGHUP-reloadable; needs a restart
   ```

Every `web.*`/`public.*` key is managed the same way as any other config key
(`serverwatch config get|set|unset`) — there is no separate web-specific CLI
or hand-edited file.

### `web.*` keys

| Key | Default | Meaning |
|-----|---------|---------|
| `web.enabled` | `false` | Turns the embedded server on. Opt-in even in the `serverwatch-web` binary. |
| `web.listen` | `127.0.0.1:8088` | `host:port` the server binds (validated as a `host:port` pair). Loopback by default — front it with a reverse proxy for LAN/WAN access (see **proxy mode**). |
| `web.mode` | `proxy` | One of `proxy` \| `autocert` \| `manual` — see **Serving modes** below. |
| `web.rp_id` | `""` | WebAuthn relying-party ID: the public hostname passkeys are scoped to (no scheme/port). Required in `autocert`/`manual` mode; optional in `proxy` mode. |
| `web.origin` | `""` | Full public origin (`https://host[:port]`) passkey ceremonies validate the browser's reported origin against. Its host **must equal** `web.rp_id` exactly. Required in `autocert`/`manual` mode. |
| `web.autocert_domains` | `""` | Comma-separated hostname allowlist for Let's Encrypt issuance. Required (non-empty) in `autocert` mode. |
| `web.tls_cert` / `web.tls_key` | `""` | PEM cert/key file paths. Both required (non-empty) in `manual` mode. |
| `web.session_ttl` | `24h` | Duration string (`time.ParseDuration` syntax) a signed-in session stays valid. |

`web.rp_id`/`web.origin`/`web.mode`/`web.listen` are all validated **at
startup** — an invalid or incomplete combination (e.g. `manual` mode with no
`tls_cert`, or `rp_id` not matching `origin`'s host) makes the web server
refuse to start; the daemon itself keeps running (Telegram/monitoring
unaffected), it just logs the failure and skips the web listener. Fix the
config and `systemctl restart serverwatch`.

### `public.*` keys (the curated anonymous view)

| Key | Default | Meaning |
|-----|---------|---------|
| `public.enabled` | `false` | Turns on `GET /public`. When `false` the route 404s (it doesn't even reveal that a public page could exist). |
| `public.panels` | `""` (empty) | Comma-separated allowlist of panel ids to expose, e.g. `cpu,mem,disk:/,uptime`. Only ids the server recognizes are ever accepted — `cpu`, `mem`, `swap`, `load`, `temp`, `uptime`, `services`, `containers`, `net`, or `disk:<mount>`. |

```bash
sudo serverwatch config set public.enabled true
sudo serverwatch config set public.panels cpu,mem,disk:/,uptime
```

Nothing is exposed anonymously until **both** are set — an admin must
explicitly turn the page on and curate exactly which metrics it shows. The
same allowlist is enforced server-side (an admin can also manage it from
`/settings/public` once signed in), so a metric absent from `public.panels`
can never leak onto `/public` regardless of what else is visible elsewhere
in the UI.

## Serving modes

`web.mode` picks how the embedded server binds and terminates (if at all)
TLS. Pick based on how you're already exposing services on this host.

### `proxy` (default) — front it with Cloudflare Tunnel / nginx / Caddy

The server binds **plain HTTP** on `web.listen` (default
`127.0.0.1:8088`) and expects a local reverse proxy to terminate TLS and
forward the original request's scheme/host via `X-Forwarded-Proto`/
`X-Forwarded-Host`. Those headers are only trusted when `web.listen` is
loopback-bound — a wildcard/public bind in this mode would let any client on
the network spoof its own origin, so keep `web.listen` on `127.0.0.1` (or
`localhost`) whenever you use a local proxy.

`web.rp_id`/`web.origin` may be left empty in this mode: the server derives
them per-request from the trusted forwarded headers instead. Setting them
explicitly still works and is validated the same way (host must match) if
you do.

**Cloudflare Tunnel** (no public inbound port needed at all):

```bash
sudo serverwatch config set web.mode proxy
sudo serverwatch config set web.listen 127.0.0.1:8088
sudo systemctl restart serverwatch

cloudflared tunnel create serverwatch
cloudflared tunnel route dns serverwatch monitor.example.com
```

`~/.cloudflared/config.yml`:

```yaml
tunnel: serverwatch
credentials-file: /root/.cloudflared/<tunnel-id>.json
ingress:
  - hostname: monitor.example.com
    service: http://127.0.0.1:8088
  - service: http_status:404
```

```bash
sudo cloudflared service install
sudo systemctl restart cloudflared
```

Then set `web.rp_id monitor.example.com` / `web.origin
https://monitor.example.com` (or leave both empty and let proxy mode derive
them from Cloudflare's forwarded headers).

**nginx**:

```nginx
server {
    listen 443 ssl;
    server_name monitor.example.com;

    ssl_certificate     /etc/letsencrypt/live/monitor.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/monitor.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8088;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
    }
}
```

(Get the cert with `certbot --nginx -d monitor.example.com`, or point nginx
at your own PKI if you already run one.)

**Caddy** (simplest — Caddy gets its own cert automatically and sets the
forwarded headers by default):

```
monitor.example.com {
    reverse_proxy 127.0.0.1:8088
}
```

For any of the three, set the rp_id/origin to match the public hostname:

```bash
sudo serverwatch config set web.rp_id monitor.example.com
sudo serverwatch config set web.origin https://monitor.example.com
sudo systemctl restart serverwatch
```

### `autocert` — built-in Let's Encrypt

The server terminates TLS itself and obtains/renews a Let's Encrypt
certificate automatically via `golang.org/x/crypto/acme/autocert`. Requires:

- `web.listen` reachable for HTTPS (e.g. `:443`, run as root or with
  `CAP_NET_BIND_SERVICE`).
- Port **80** free and reachable from the public internet — autocert's
  HTTP-01 challenge listener binds it unconditionally in this mode.
- `web.autocert_domains` — the exact public hostname(s), comma-separated.
- `web.rp_id`/`web.origin` set and matching (required, not derived, in this
  mode).

```bash
sudo serverwatch config set web.mode autocert
sudo serverwatch config set web.listen :443
sudo serverwatch config set web.autocert_domains monitor.example.com
sudo serverwatch config set web.rp_id monitor.example.com
sudo serverwatch config set web.origin https://monitor.example.com
sudo systemctl restart serverwatch
```

Certificates are cached under `/var/lib/serverwatch/autocert/` (renewed
automatically; no certbot/cron needed).

### `manual` — your own TLS cert/key

For an existing internal CA, a wildcard cert you already manage, etc.:

```bash
sudo serverwatch config set web.mode manual
sudo serverwatch config set web.listen :443
sudo serverwatch config set web.tls_cert /etc/serverwatch/tls/fullchain.pem
sudo serverwatch config set web.tls_key  /etc/serverwatch/tls/privkey.pem
sudo serverwatch config set web.rp_id monitor.example.com
sudo serverwatch config set web.origin https://monitor.example.com
sudo systemctl restart serverwatch
```

`web.tls_cert`/`web.tls_key` must both be set (PEM files, readable by the
service user — root, by default) and `web.rp_id`/`web.origin` are required
and validated the same as in `autocert` mode.

## Passkey enrollment & first-run bootstrap

There is no password login — accounts authenticate with a **passkey**
(WebAuthn: a platform authenticator like Touch ID/Windows Hello, or a
hardware key). Getting the first account is a one-time bootstrap:

1. With `web.enabled=true` and no accounts yet, visit `/enroll` (no token
   needed) and register a passkey. **The very first passkey registered
   becomes the admin account.** Two people racing to bootstrap at once still
   only produces one admin — the decision is made atomically at write time,
   and the loser sees "enrollment is closed."
2. Once any account exists, `/enroll` requires a token: open tokenless
   enrollment is only valid for that very first account.
3. An admin invites everyone else from **`/users`**: issue a single-use
   enrollment token (choosing the new account's role and the token's TTL),
   share the resulting `/enroll?token=...` link, and the invitee registers
   their own passkey against it. The token is burned on first use regardless
   of outcome, and expires on its own even if never used.
4. Admins can also change an existing account's role, remove accounts, and
   revoke individual passkeys from `/users` — with a standing guard that
   refuses to demote or remove the **last** remaining admin, so the instance
   can never end up adminless.

### Roles

| Role | Can do |
|------|--------|
| `admin` | Everything a viewer can, plus `/config`, `/channels`, `/users`, `/settings/public`, and alert acknowledgement. |
| `viewer` | Read-only: dashboard, `/history`, `/alerts` (view only, no ack). |
| *(anonymous)* | Only `/public`, and only if `public.enabled` is true — a curated, admin-picked subset of metrics, no login, no other route reachable. |

## systemd

The `install` command always copies **whichever binary is currently
running** to `/usr/local/bin/serverwatch` and writes/enables the unit around
it (see `internal/serverwatch/systemd.go`). There's no separate "web" unit —
installing the `serverwatch-web` binary and setting `web.enabled=true` *is*
the whole path to a web-capable service:

```bash
sudo /tmp/serverwatch-web install       # installs itself as /usr/local/bin/serverwatch
sudo serverwatch config set web.enabled true
sudo serverwatch config set web.mode proxy   # or autocert/manual, see above
sudo systemctl restart serverwatch
```

Upgrading later from a plain `serverwatch` install to a web-capable one (or
back) is the same `install` step with the other binary — it overwrites the
binary in place, config and history are untouched, and `web.enabled` simply
starts (or stops) taking effect on the next restart.

## Data sources

The dashboard, history graphs, and public view all read from the same data
the CLI/Telegram already use — the live snapshot (`status.json`) and the
`SampleStore` time-series (see `docs/DEPLOYMENT.md`'s **Storage &
retention**). The web UI adds no new collectors and no new on-disk series;
it's a second consumer of the exact same data.
