# serverwatch-web

`serverwatch-web` is the out-of-process web plugin. It dials the control
socket and runs the dashboard as a separate process. It is a plain binary with
no build tag, built alongside `serverwatch` and `serverwatch-ctl`.

For everything the web UI serves -- authentication and passkeys, the
dashboard, the serving modes, and the public page -- see the [Web UI
chapter](../08-web-ui.md). This page covers only how the binary runs and its
direct invocation.

## Installing

`serverwatch-web` installs alongside the daemon. The recommended path is to
download the `serverwatch-web-<arch>` asset from the [releases
page](https://github.com/Suraj-Tiwari/server-monitor/releases) into the same
directory as the `serverwatch` binary (renamed to `serverwatch-web`, dropping
the arch suffix), then run `sudo serverwatch install`: it copies the plugin
into `/usr/local/bin` next to the daemon and records its checksum in the
root-only install manifest. With the binary in place, the daemon supervises it
whenever `web.enabled` is set, or you can launch it with `serverwatch web`. If
you already installed the daemon, download the plugin next to `serverwatch` and
re-run `serverwatch install`. Building from source
(`go build -o serverwatch-web ./cmd/serverwatch-web`) is the secondary option.
See [Installation and first run](../03-installation.md) and
[Plugins](README.md) for the shared install and safe-exec model.

## How it runs

`serverwatch-web` is a plain, separate binary that the daemon supervises. When
`web.enabled` is set, the daemon:

- **verifies** the plugin with the same front-door safe-exec check used for
  `serverwatch web` (resolved next to the core binary, owner and permission
  checks, SHA-256 match against the install manifest);
- **spawns** it as a child process, passing the control socket path and a
  per-launch token via the `SERVERWATCH_CONTROL_SOCKET` and
  `SERVERWATCH_CONTROL_TOKEN` environment variables;
- **restarts** it under a capped backoff if it exits (starting at 1 second,
  doubling to a 30 second cap, reset once the child stays up past 60 seconds),
  re-running the verification check on every respawn;
- **stops** it on daemon shutdown rather than leaving it orphaned.

`web.enabled` is checked once at startup and is not reloaded on SIGHUP, so
toggling it takes effect on the next `systemctl restart serverwatch`. For the
full detail see [Web supervisor](../02-architecture.md#web-supervisor).

You can also launch it directly via the `serverwatch web` front-door, or by
running the binary itself (useful when scripting or working from a
non-standard install location).

## Direct invocation

```
serverwatch-web [-socket PATH] [-token TOKEN] [-token-file PATH] \
  [-state-dir DIR] [-alert-log PATH] [-alert-state PATH]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-socket` | `$SERVERWATCH_CONTROL_SOCKET`, else `$RUNTIME_DIRECTORY/control.sock`, else `/run/serverwatch/control.sock` | Control socket to dial. |
| `-token` | `$SERVERWATCH_CONTROL_TOKEN`, else read from `-token-file` | Control socket auth token, given directly. |
| `-token-file` | sibling `token` file next to the socket | File to read the token from when `-token` is not set. |
| `-state-dir` | `/var/lib/serverwatch` | Web UI state directory (sessions, enrollment tokens). |
| `-alert-log` | `<state-dir>/alertlog.jsonl` | Path to the alert log. |
| `-alert-state` | `<state-dir>/alerts.json` | Path to the alert ack-state file. |

The socket and token resolution order matches
[serverwatch-ctl](serverwatch-ctl.md): flag, then environment, then the
systemd/by-hand default. As with the daemon, a missing token file means
no-auth when no token was supplied directly.

---

[Plugins overview](README.md) | [Handbook index](../README.md)
