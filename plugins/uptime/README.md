# Uptime

HTTP(S), TCP, TLS certificate expiry and DNS checks for Trinetra, with latency graphs and alerts.

Permissions: `net.outbound`, `metrics.publish`, `alerts.raise`.

## Checks

One per line in the `checks` setting:

```
<kind> <target> [name=<n>] [interval=<d>] [timeout=<d>] [keyword=<text>] [expect=<code>] [record=<A|AAAA|CNAME|TXT>] [value=<expected>]
```

| Kind | Target | Passes when |
|---|---|---|
| `http` | `http(s)://...` | status is 2xx (or `expect`), `keyword` is in the first 1 MiB of the body |
| `tcp` | `host:port` | the connection opens |
| `tls` | `host:port` (port defaults to 443) | the certificate chain verifies for the host and has not expired |
| `dns` | host name | the record exists and, with `value=`, matches it |

Quote values containing spaces: `keyword="all systems go"`. `interval` is at least 10s, `timeout` at most 60s.

## Series and alerts

- `check.<name>.up`, `check.<name>.latency_ms`, `cert.<name>.days_left` (tls, and https for http checks).
- `check.<name>.down` is raised critical after `down_after_runs` consecutive failures and resolved on the first success.
- `cert.<name>.expiring` is raised warning when the certificate has fewer than `cert_warn_days` days left and resolved on renewal.

## Build

```
make plugins PLUGINS=uptime   # host platform, into dist/plugins/uptime
PLUGIN_MATRIX=1 make plugins PLUGINS=uptime   # linux amd64/arm64/arm
```
