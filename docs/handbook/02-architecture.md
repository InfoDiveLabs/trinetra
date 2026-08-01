# Architecture

serverwatch is one Go binary that runs as one systemd service. There is no
agent-plus-collector split, no sidecar, no message broker, and no external
database. Everything the daemon needs, it does inside a single long-running
process started by systemd and kept alive by it. This chapter is about what
that process is made of: the goroutines it runs, the tiered sampling model that
sets its rhythm, the internal contract every consumer reads it through, and the
local control socket that lets separate plugin processes talk to it without the
daemon ever growing a dependency it does not need.

If you read only one chapter of this handbook, read this one. The pieces
introduced here (the core contract, the control socket, and the plugin model
layered on top of them) are the load-bearing structure the rest of the system
hangs off of, and the control socket in particular appears in no other
document.

## The daemon process

The whole thing runs as `serverwatch daemon`, the long-running process systemd
starts from the unit's `ExecStart`. You never launch it by hand; systemd owns
its lifecycle. Inside that one process, work is split across a small number of
goroutines, each with a clear owner and no shared mutable state beyond a couple
of carefully guarded values.

There are two goroutines that matter in every build:

| Goroutine | What it does | Cadence |
|-----------|--------------|---------|
| Sampler loop | Collects host metrics, evaluates anomaly checks, writes `status.json`, appends to the time-series store, feeds the watchdog | Ticks at `fast_interval` |
| Telegram long-poller | Answers inbound bot commands (`/stats`, `/disk`, and so on) and handles owner enrollment | Blocks on Telegram's long-poll, replies in about a second |

In the web-enabled build there is a third:

| Goroutine | What it does | Cadence |
|-----------|--------------|---------|
| Embedded web server | Serves the passkey-only dashboard, history graphs, and config editor over HTTP | Request-driven |

The sampler loop is the heart of the daemon and lives in `cmdDaemon`
(`internal/serverwatch/daemon.go`). It is the sole owner of most of the
daemon's stateful pieces: the previous CPU sample it diffs against, the network
rate calculator, the per-process CPU calculator, the SMART scan cache, and the
`merged` snapshot it rebuilds every tick. Because that state belongs to one
goroutine, almost none of it needs locking.

The Telegram poller runs in its own goroutine (`pollLoop`) and deliberately
keeps its own copy of the state it needs (its own CPU sample, for instance), so
no pointer is shared across the goroutine boundary. When an authorized command
comes in, it collects a fresh full snapshot on the spot and replies. Ownership
is claimed once, through a one-time enrollment PIN printed to the log, and only
the owner chat is ever answered.

Config is shared between goroutines through one `sync.RWMutex` and a
pointer-swap discipline: the live `*config.Config` is never mutated in place.
When a `SIGHUP` arrives (every CLI write sends one), the config is re-read from
disk and swapped in atomically, so readers always see a complete, consistent
config struct. This is why almost every setting reloads live with no restart.

The daemon is written to stay up. If config on disk is corrupt at startup, it
falls back to defaults rather than crashing (under `Restart=always` a crash
would become a crash-loop). If the time-series store fails to open, store
writes are disabled and the daemon keeps running. If the web server or the
control socket fails to start, that failure is logged and the daemon carries on
without it. The guiding rule is that enhancements are never a reason to take
the monitor down.

## The tiered sampler

The single most important design decision in the daemon is that not all checks
cost the same, so not all checks run at the same rate. Reading a few numbers
out of `/proc` is nearly free; shelling out to `df`, `docker`, `systemctl`, and
`smartctl` is not. serverwatch splits collection into two tiers plus an
independent heartbeat, and one loop drives all three.

The loop ticks at `fast_interval`. Every Nth tick, where `N =
sample_interval / fast_interval`, it also runs the slow tier. The heartbeat
runs on its own clock, checked every tick but written only when its own
interval has elapsed.

```
fast tick   fast tick   fast tick   ...   every Nth fast tick
   |           |           |                    |
 collectFast collectFast collectFast          collectFast + collectSlow
 (cpu/mem/    ...          ...                 (disk/docker/systemd/
  swap/load/                                    SMART/network)
  temp)
```

### The fast tier

The fast tier is cheap and never spawns a subprocess. It reads `/proc/stat`,
`/proc/meminfo`, `/proc/loadavg`, and the first thermal zone under
`/sys/class/thermal`, and turns them into CPU percent, memory percent, swap
percent, the three load averages, and a temperature. This is `collectFast`.

Because it is cheap, the fast tier runs on every tick and drives the things
that need to be current:

- The live status view (`status.json` is rewritten every fast tick).
- The rolling baseline that anomaly detection compares against.
- The threshold and baseline checks for `cpu`, `mem`, `swap`, and `temp`.
- A raw sample per fast-tier metric appended to the time-series store
  (`cpu`, `mem`, `swap`, `load1`, `load5`, `load15`, `temp`).

The default `fast_interval` is 5 seconds (minimum 1).

### The slow tier

The slow tier is where the expensive checks live. This is `collectSlow`, and it
runs only on slow ticks:

- `df` for per-filesystem usage, plus device, filesystem type, inode usage, and
  a linear fill-rate projection.
- `docker ps` for container up/down state, plus the opt-in `docker stats` for
  per-container CPU, memory, and network.
- `systemctl --failed` for the alerting unit list, plus the opt-in full unit
  inventory.
- `smartctl` for device health, plus the opt-in per-device attribute read; the
  scan itself is throttled to `collect.smart_interval` (default 30 minutes)
  because it is the heaviest call in the tier.
- The opt-in `/proc/net/dev` throughput read.
- The opt-in process-table snapshot.
- A connectivity dial to decide whether the box's own internet is up.

On each slow tick the loop also appends the slow-tier series (`disk:<mount>`,
and, for whichever opt-in collectors are enabled, `docker:<name>:cpu`/`:mem`,
`net:<iface>:rx`/`:tx`, and `smart:<dev>:temp`), pings `healthchecks.url` if
set, tracks internet-down intervals, and runs the store's downsample and prune
pass.

The default `sample_interval` is 60 seconds (minimum 5), and it must be an
integer multiple of `fast_interval`.

One detail worth calling out: the very first tick after startup or a config
reload (`tick == 0`) always runs the slow tier too, so the first `status.json`
and the first anomaly evaluation are already fully populated instead of waiting
up to N-1 fast ticks for the disk and docker data to show up. Between slow
ticks, the `merged` snapshot keeps the last-collected slow values, so every
`status.json` write and every anomaly evaluation sees a complete picture, even
if the slow half of it is up to `sample_interval` old.

### The heartbeat

The heartbeat is not a tier. It runs on `heartbeat_interval` (default 30
seconds, minimum 1), independent of both tiers, and does exactly one thing:
rewrite the `heartbeat` file with the current time. Its only purpose is to let
a future boot measure how long the process was down. On startup the daemon
reads the previous heartbeat, and if the gap is larger than twice the heartbeat
interval it reconstructs a `power_down` event and can send a boot/recovery
report. Writing the heartbeat more often than its own interval would buy
nothing, so it is gated by `shouldHeartbeat`.

### Live reload of the cadence

All three intervals reload live. The sampler loop reads the current config at
the top of every tick; if `fast_interval` or `sample_interval` changed, it
resets the ticker in place and recomputes N from that point, so a new cadence
takes effect on the very next tick with no restart. The one exception across
the whole config surface is `storage.*`: the time-series store is opened once
at startup and is not reopened on `SIGHUP`, so a storage backend or retention
change needs a `systemctl restart`.

## The core contract and the plugin model

Everything that reads daemon state or changes daemon config goes through one
Go interface, `core.API`, defined in `internal/core/api.go`. It is deliberately
the only boundary:

> `API` is the single boundary through which every consumer (the embedded web
> UI, the CLI, and later a socket client) reads daemon state and applies
> changes.

The interface splits cleanly into reads and writes:

```go
type API interface {
    // reads
    Snapshot() (DashboardView, error)
    Monitoring() (MonitoringView, error)
    Series(metric string, from, to int64, res Resolution) ([]SeriesPoint, error)
    Events(from, to int64) ([]DownEventView, error)
    ActiveAlerts() ([]AlertRecord, error)
    AlertHistory(since int64, limit int) ([]AlertRecord, error)
    Config() (*config.Config, error)
    Doctor() (DoctorReport, error)

    // writes
    ApplyConfig(*config.Config) error
    AckAlert(key string) error
    UnackAlert(key string) error
    TestChannel(name string) error
    Subscribe(ctx context.Context) (<-chan Event, error)
}
```

There are a couple of implementations of this interface. Inside the running
daemon there is an in-process one (`newInprocAPI`) that reads the live snapshot
and config directly and applies changes through the same reload path a `SIGHUP`
would. For the separate CLI process there is a file-backed one that reads the
same on-disk state. The point of the interface is that a consumer does not care
which one it holds.

This is what makes the plugin model possible. The daemon serves its in-process
`core.API` over a local control socket (described in the next section) so that a
separate process can dial in and call the very same methods. A plugin is just a
separate binary that connects to that socket and talks the contract.

The reason to keep plugins as separate processes is dependency hygiene, and it
is a hard rule here. The default `serverwatch` binary is 100% standard library.
Anything heavier lives in a plugin binary that carries its own dependencies and
never bloats the daemon. Concretely:

| Binary | Build | Dependencies | Role |
|--------|-------|--------------|------|
| `serverwatch` | default | stdlib only | The daemon and the CLI |
| `serverwatch-web` | `-tags web` | passkey/webauthn stack and more | Web UI, built into the daemon it links |
| `serverwatch-ctl` | in progress | its own | A richer out-of-process control client |

The build seam that enforces this is a small one. The file that starts the web
server (`daemon_web.go`) is the only file in the package allowed to import
`internal/web`, and it is compiled only under `-tags web`. The default build
compiles a no-op stub (`daemon_noweb.go`) in its place. The call site in
`cmdDaemon` is identical in both builds; only the linked half decides whether
anything starts. The control-socket code, by contrast, is deliberately untagged
and ships in both variants, because `internal/control` imports only the
standard library plus `internal/core` and `internal/config`, so serving it
never drags a third-party dependency into the default build.

A word on status: the plugins are beta and in progress. The web UI works and is
documented separately, but the out-of-process control story is still being
built out. There is no interactive `serverwatch-ctl` CLI yet, and there is no
supervisor managing plugin processes. What exists today is the contract, the
transport, and a working client library for it. Treat the plugin picture as the
direction of travel, not a finished feature.

## The control socket

The control socket is how a separate process reaches the daemon's live
`core.API`. It is the transport under the plugin model, and it is worth getting
precise, because the details are all in the code and nowhere else.

### Where it lives

The socket is a unix domain socket named `control.sock`, created inside the
daemon's runtime directory:

```
$RUNTIME_DIRECTORY/control.sock   ->   /run/serverwatch/control.sock
```

`RUNTIME_DIRECTORY` is set by systemd because the unit declares
`RuntimeDirectory=serverwatch`. That one line tells systemd to create
`/run/serverwatch` on a tmpfs before the service starts, own it as
`User=root`, export its path into the service's environment, and remove it when
the service stops. So the directory always exists with the right lifetime and
the daemon never has to create or clean it up. When you run the daemon by hand
outside systemd, `RUNTIME_DIRECTORY` is unset and the code falls back to
`/run/serverwatch`, creating the directory itself as a best effort.

### Permissions

The socket is defended two ways: file permissions and a token. The permissions
come first.

| Path | Mode | Owner |
|------|------|-------|
| `/run/serverwatch/` (parent dir) | `0700` | root |
| `/run/serverwatch/control.sock` | `0600` | root |
| `/run/serverwatch/token` | `0600` | root |

systemd creates the runtime directory `0755`, so `serveControlSocket`
explicitly chmods it down to `0700` before binding, closing the window where a
non-owner could connect between the listen and the socket's own chmod.
`net.Listen` honors the umask rather than an explicit mode, so the socket is
chmodded to `0600` right after it is created. Any stale socket left over from
an unclean shutdown is removed first, because `net.Listen` on a leftover socket
file fails with "address already in use".

The file mode is defense in depth against other local users on a multi-user
host. The actual authentication is the token.

### The token

On every launch the daemon generates a fresh token: 16 random bytes rendered as
32 hexadecimal characters. It writes that token to a sibling `token` file next
to the socket, mode `0600`, root-owned. A plugin running as the same user reads
that file to learn the token it must present. Because the token is regenerated
per launch, a token from a previous run is worthless after a restart.

Token generation and the token file are treated as enhancements, the same way
the socket itself is: if the token cannot be generated or written, the daemon
logs it and continues serving the socket with no token auth rather than
crash-looping over it.

### The wire protocol

The protocol is newline-delimited JSON. Every frame is a single JSON object on
one line, terminated by a newline so the other end can delimit frames with a
buffered reader. There are three frame shapes: a handshake `hello`, a
`request`, and a `response`.

A connection opens with a hello handshake in both directions. The client sends
a hello carrying the fixed magic string, the protocol version, and its token.
The server checks the magic and version, then constant-time compares the token
(`crypto/subtle`) against its own. If the magic or version is wrong, or the
token does not match, the server writes an error response and closes the
connection. Only on success does the server echo its own hello (with an empty
token) and start accepting requests.

```
client -> server   {"hello":"serverwatch-control","version":1,"token":"<32 hex>"}
server -> client   {"hello":"serverwatch-control","version":1}     (on success)
```

The current `ProtocolVersion` is 1. A version mismatch is rejected outright; the
handshake exists precisely so the two ends agree before any real frame flows.

After the handshake, each call is a request frame with a monotonically
increasing id, a method name, and JSON params, answered by a response frame
carrying the same id. When a call succeeds, `ok` is true and `result` holds the
method's return value; when it fails, `ok` is false and `error` carries the
message.

```
request    {"id":1,"method":"Snapshot","params":{}}
response   {"id":1,"ok":true,"result":{ ... }}
```

The connection is guarded by two read deadlines so a peer that connects and
then goes silent cannot tie up a goroutine forever: a 10 second deadline on the
opening hello, and a 5 minute idle deadline on each request frame after that.
The client side holds a mutex for the whole round trip of each call, so request
and response frames and their ids never interleave across goroutines sharing
one client, and it bounds each call with its own 30 second timeout.

### The methods

The methods exposed over the socket mirror `core.API` exactly. The reads return
the method's data; the writes return an empty result and surface only success
or an error.

| Kind | Methods |
|------|---------|
| Reads | `Snapshot`, `Monitoring`, `Series`, `Events`, `ActiveAlerts`, `AlertHistory`, `Config`, `Doctor` |
| Writes | `ApplyConfig`, `AckAlert`, `UnackAlert`, `TestChannel` |
| Not yet supported | `Subscribe` (live streaming) |

`Subscribe`, the live event stream, is not implemented over the socket yet.
Calling it returns an error saying streaming is not supported over the control
socket yet, on both the server and the client. When live streaming lands, this
is the method that will carry it.

One subtlety in `Config`/`ApplyConfig`: they carry the raw `config.Config`
value rather than a display-formatted projection, so the `omitempty` semantics
of the opt-in `collect.*` toggles survive the round trip intact.

### Failure handling

Serving the socket is non-fatal, in both build variants. `cmdDaemon` starts it
and, if binding fails (for example, a permission problem on `/run`), logs the
failure and leaves the daemon running without it. This mirrors how the web
server is treated: the control socket is an enhancement, never a reason to take
the monitor down. When the daemon stops, the stop function closes the listener
and removes both the socket and the token file.

## The systemd unit

The unit is written by `serverwatch install`, rendered by `renderUnit` in
`internal/serverwatch/systemd.go`, and installed to
`/etc/systemd/system/serverwatch.service`. There is exactly one unit; the same
unit wraps the default binary and the web-enabled binary, because `install`
always copies whichever binary is running to `/usr/local/bin/serverwatch` and
writes this same file around it. Here it is exactly as emitted:

```ini
[Unit]
Description=server-watcher host monitor
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/serverwatch daemon
Restart=always
RestartSec=5
WatchdogSec=90
User=root
RuntimeDirectory=serverwatch
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Most of these lines are ordinary, but a few are load-bearing and worth
explaining.

`After=network-online.target docker.service` and
`Wants=network-online.target` order the daemon after the network is up and
after docker, so its first slow tick has a working network and a reachable
docker socket to probe. `Wants` (rather than `Requires`) keeps the ordering
without making the network a hard dependency that could block the service from
starting.

`Type=simple` means systemd considers the service started as soon as it forks
`ExecStart`; there is no readiness protocol. This works fine alongside the
watchdog because the `WATCHDOG=1` notification is accepted from the main PID
regardless of `Type`, unlike `READY=1` which would require `Type=notify`.

`ExecStart=/usr/local/bin/serverwatch daemon` runs the daemon subcommand. This
is the process this whole chapter is about, and it is the only way the daemon is
meant to start.

`Restart=always` with `RestartSec=5` is the survive-crashes half of the
resilience story: if the process exits for any reason, systemd restarts it after
5 seconds. Paired with `WantedBy=multi-user.target` in the install section
(which is what `systemctl enable` hooks into), the daemon both starts at boot
and comes back after a crash.

`RuntimeDirectory=serverwatch` is the line that makes the control socket
possible. As covered above, it is what creates `/run/serverwatch` with the
right ownership and lifetime and exports its path into the environment, so the
socket has a home that appears before the daemon starts and vanishes when it
stops.

`WatchdogSec=90` is the line that catches a wedged daemon. systemd expects a
`WATCHDOG=1` ping from the service at least every 90 seconds; if none arrives,
it kills and restarts the unit. The sampler loop sends exactly that ping on
every fast tick, which at the default 5 second cadence is far inside the 90
second window. The pairing is deliberate: `Restart=always` catches a process
that has died, but only the watchdog catches a process that is still alive yet
stuck, because a sampler loop that has wedged stops sending pings and systemd
restarts it. The comment on `renderUnit` in the source spells this out, and it
is the reason the number is 90 and not something tighter: it has to leave
comfortable headroom above the fast interval.

`User=root` is required because the daemon reads privileged data and shells out
to privileged tools (`smartctl`, docker, and so on). `StandardOutput=journal`
and `StandardError=journal` send all logging to the journal, which is why
`journalctl -u serverwatch` is the way to read the daemon's output, including
the one-time Telegram enrollment PIN.

## Putting it together

Step back and the shape is simple. One process, started and kept alive by
systemd, runs a tiered sampler loop and a Telegram poller (and, in the web
build, a web server). The sampler sets the rhythm: cheap checks every few
seconds, expensive checks every minute, a heartbeat on its own clock, and a
watchdog ping every tick that lets systemd restart the process if the loop ever
wedges. Every consumer, in-process or out, reads and writes daemon state
through one interface, `core.API`, and the daemon serves that interface over a
token-authenticated local socket so that separate, dependency-carrying plugin
processes can drive it while the default binary stays pure standard library.
The systemd unit ties the resilience and the socket lifetime together in a
dozen lines. That is the architecture the rest of this handbook builds on.
