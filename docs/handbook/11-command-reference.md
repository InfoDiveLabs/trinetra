# Command reference

This chapter lists every command shipped by trinetra. Section 1 covers the
`trinetra` binary, which is both the daemon and the operator CLI. Section 2
covers the two out-of-process plugin binaries, `trinetra-ctl` and
`trinetra-web`, neither of which is part of the shipped daemon binary
itself. `trinetra-ctl` is now the primary, recommended way to manage a
running trinetra day to day: it wraps the
schedule, quiet hours, healthchecks, monitor thresholds, and notification
channels in guided, validated screens, plus a first-run onboarding flow for
Telegram. `trinetra-web` is a supervised, separate binary that the daemon
manages (see [Web supervisor](02-architecture.md#web-supervisor)). Section 3
is the authoritative reference for the thin, scriptable management verbs,
gathered in one place for anyone managing trinetra without
`trinetra-ctl`.

## 1. `trinetra` (daemon + CLI)

`trinetra` dispatches a single subcommand per invocation. Run it with no
arguments, `help`, `-h`, or `--help` to print usage.

```
trinetra <command> [args]
```

Every command falls into one of two modes:

- **read-only**: reads config or on-disk state and prints; nothing is written.
- **persists + SIGHUP**: writes the change to disk, then sends a best-effort
  SIGHUP to a running daemon so it hot-reloads the new config. The SIGHUP is a
  no-op when the daemon is not running (the write to disk still happens, and the
  daemon picks it up on next start).

A few commands write other on-disk state (systemd units, the time-series store,
alert ack state) rather than the config file; those are called out in the Notes
column.

### Commands

| Command | Purpose | Mode |
| --- | --- | --- |
| `config get [key]` | Print the full effective config, or one key. | read-only |
| `config set <key> <value>` | Set one config key. | persists + SIGHUP |
| `config unset <key>` | Reset one config key to its default. | persists + SIGHUP |
| `install [--force] [--require-signed] [--state-already-at-new-path]` | Write the systemd unit and enable/start the service. `--require-signed` refuses an install unless a signed manifest (`manifest.json` + both `.sig` files) sits next to the binary. | persists (see Notes) |
| `uninstall [--purge]` | Remove the systemd unit; `--purge` also removes config and state. | persists (see Notes) |
| `daemon` | Run the sampler/notifier loop in the foreground. | long-running |
| `cli` | Front-door: verify and exec `trinetra-ctl`, the management TUI. | see Notes |
| `web` | Front-door: verify and exec `trinetra-web`, the web UI. | see Notes |
| `users` | Manage web UI accounts; runs `trinetra-web users …` (see [trinetra users](#trinetra-users)). | none |
| `status` | Print the last status snapshot. | read-only |
| `version [--json]` | Print the build-stamped version this binary was compiled with. | read-only |
| `doctor` | Print a diagnostic report (collectors, tools, targets). | read-only |
| `migrate [--force]` | Import legacy data into the time-series store. | persists (see Notes) |
| `dump --metric <id> [...]` | Export one metric's series to stdout. | read-only |
| `alerts [list] [...]` | List recent alerts. | read-only |
| `alerts ack <key>` | Acknowledge an active alert. | persists (see Notes) |
| `alerts unack <key>` | Un-acknowledge an alert. | persists (see Notes) |
| `fleet <subcommand>` | Fleet mode: make this host a master, join or leave one, manage nodes and join codes. | see [`trinetra fleet`](#trinetra-fleet) |
| `status-page <subcommand>` | Public status page: manage services and incidents. | see [`trinetra status-page`](#trinetra-status-page) |
| `update <subcommand>` | Signed self-update: check, apply, roll back, and report status. | see [`trinetra update`](#trinetra-update) |

This section covers the daemon, lifecycle, and low-level scriptable commands.
The day-to-day management verbs (`monitor`, `schedule`, `quiet-hours`,
`healthchecks`, `channel`, and Telegram onboarding) are not listed here: the
primary, recommended way to drive them is the interactive
[`trinetra-ctl`](plugins/trinetra-ctl.md) TUI, and their thin scriptable
forms for automation live in [Daemon-only config
management](#3-daemon-only-config-management).

### Notes

| Command | Detail |
| --- | --- |
| `config set`/`config unset` | `storage.*` changes are persisted and SIGHUP is still sent, but the time-series store is opened once at daemon start and is not re-opened on reload. A `storage.backend` or retention change needs a daemon restart to take effect. |
| `install` / `uninstall` | These write or remove systemd unit files and toggle the service; they do not send a config SIGHUP. `uninstall --purge` additionally deletes the config file and state directory. |
| `migrate` | Writes into the time-series store, not the config file; no SIGHUP is sent. `--force` proceeds past guards. |
| `alerts ack` / `alerts unack` | Writes the alert ack-state file and then sends a best-effort SIGHUP so a running daemon re-reads it. |
| `update apply` / `update rollback` | Write self-update state (staged files, the pending marker, the version floor) under the state directory, not the config file; no SIGHUP is sent. They restart the daemon themselves, via the launched health guard, once the new build is confirmed or rolled back. |
| `cli` / `web` | Neither writes config nor sends a SIGHUP. Each resolves and verifies the matching plugin binary next to the core binary, then hands off to it; see the front-door detail below. |

### `dump` flags

`dump` exports a single metric's series.

| Flag | Values | Default | Meaning |
| --- | --- | --- | --- |
| `--metric <id>` | e.g. `cpu`, `mem`, `disk:/` | required | Metric id to export. |
| `--since <dur>` | Go duration, e.g. `24h`, `30m` | `24h` | How far back to query. |
| `--res <res>` | `raw` \| `1m` | `raw` | Resolution to read. |
| `--format <fmt>` | `csv` \| `json` | `csv` | Output format. |

```
trinetra dump --metric cpu --since 24h --res 1m --format json
```

### `alerts` flags

`alerts list` (the default when no subcommand is given) accepts:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--since <dur>` | `24h` | How far back to list. |
| `--limit <n>` | `20` | Maximum number of alerts to print. |

```
trinetra alerts --since 24h --limit 20
trinetra alerts ack cpu:high
trinetra alerts unack cpu:high
```

### `cli` and `web` front-doors

```
trinetra cli
trinetra web
```

`cli` and `web` are front-doors: the one-command way to launch the two plugin
binaries documented in section 2, without needing to know their binary names
or where they live. `cli` execs `trinetra-ctl`; `web` execs
`trinetra-web`. Both are typically run with `sudo`, because that is how the
daemon itself runs, and launching a plugin as root means the core must first
prove it is about to exec the genuine binary it installed, not something an
attacker planted or modified. See [Architecture](02-architecture.md) for the
full trust model (absolute path resolved from the core binary's own
directory, owner and permission checks, and a SHA-256 checksum against the
manifest `trinetra install` writes).

There are three outcomes:

| Outcome | What you see |
| --- | --- |
| Verified | The plugin runs; the front-door hands off control to it. |
| Not installed | trinetra prints an install instruction: download the plugin (`trinetra-ctl` or `trinetra-web`) from the releases page next to the daemon binary and run `trinetra install`, or build it from source (`go build -o /usr/local/bin/trinetra-ctl ./cmd/trinetra-ctl`; the web plugin builds the same way, no tag: `go build -o /usr/local/bin/trinetra-web ./cmd/trinetra-web`) and then run `trinetra install` to record its checksum. Nothing is exec'd. |
| Present but unsafe | The binary exists but fails a check (wrong owner, group/world-writable, or a checksum that does not match the manifest). trinetra refuses with a warning that this may indicate tampering. Nothing is exec'd. |

If you build or hand-copy `trinetra-ctl` / `trinetra-web` into place
yourself, you must (re-)run `trinetra install` afterward so its checksum
is recorded (see [Installation and first run](03-installation.md)); until
then the front-door has nothing to verify the unrecorded binary against and
refuses to run it.

## trinetra users

Web UI accounts are stored by the web plugin, so `trinetra users …` runs
`trinetra-web users …` through the same front door as `trinetra web`. It works
with the daemon stopped (it then reads `web.*` from `/etc/trinetra/config.json`).
Every change is written to the web audit log as `cli:<your user>`.

| Command | Does |
|---------|------|
| `users list [--json]` | Name, role, passkey count, created. |
| `users invite --role admin\|responder\|viewer [--ttl 24h] [--json]` | Prints a single-use enroll link: `<web.origin>/enroll?token=…` (or `http://<web.listen>/…` with a note when `web.origin` is unset). |
| `users set-role <name> <role>` | Change a role. Refuses to demote the last admin. |
| `users remove <name>` | Remove an account and its passkeys. Refuses to remove the last admin. |

The first admin: on a web UI reachable only from this machine, the first
person to open `/enroll` may create it without a link. Otherwise run
`sudo trinetra users invite --role admin`.

## trinetra fleet

`trinetra fleet` manages fleet mode (see [Fleet
mode](02-architecture.md#fleet-mode) for the architecture, and the dedicated
[Fleet mode](13-fleet.md) chapter for a task-oriented walkthrough of
everything below: setting up a fleet, incidents, routing and policies,
silences and maintenance windows, managed config, audit, and
revoke/leave/remove). Run it with no arguments or `help` to print the usage:

```
usage:
  trinetra fleet init --address HOST[,IP] [--port 9443]   make this host the fleet master
  trinetra fleet token create [--tags a,b] [--ttl 1h] [--uses 1]
  trinetra fleet token list | token delete <id>
  trinetra fleet join <code> [--name NAME]                  join a master as a child
  trinetra fleet status | nodes [--tag T] [--state S] [--q TEXT]
  trinetra fleet node revoke|remove|rename|tag <node> [value]
  trinetra fleet incidents [--state firing]                 list incidents
  trinetra fleet incident <id>                              show one incident
  trinetra fleet ack <id>                                   acknowledge an incident
  trinetra fleet explain <key|id>                           print an alert's pipeline trail
  trinetra fleet silence add --match tag=web,rule=cpu* --for 2h [--comment C]
  trinetra fleet silence list | expire <id>
  trinetra fleet maintenance add --name N --match ... --days mon,tue --from 22:00 --to 02:00 --tz Asia/Kolkata
  trinetra fleet maintenance list | delete <id>
  trinetra fleet route test --node web1 [--tag t] --rule cpu --severity critical
  trinetra fleet alerting show | apply <file.json>
  trinetra fleet rules                                      list aggregate rules and their state
  trinetra fleet managed list | status
  trinetra fleet managed set [--tag T] [--replace] key=value [key=value ...]
  trinetra fleet managed delete <id>
  trinetra fleet leave [--purge]                            child -> solo
  trinetra fleet disable [--purge]                          master -> solo
```

The commands split into two kinds. `init`, `join`, `leave` and `disable` change
this host's role: they write the fleet keys in the config file and the
certificate files on disk, and do not signal the daemon; each prints `Restart
to apply: sudo systemctl restart trinetra`. Everything else asks the running
daemon over the control socket, so it needs the daemon up (on the master, for
every subcommand from `node` through `managed` below) and changes take effect
immediately.

| Command | Flags | What it does |
| --- | --- | --- |
| `fleet init` | `--address HOST[,IP]` (required): the names and IPs children will use to reach this host; `--port` (default `9443`) | Makes a solo host the master. Creates the fleet CA (or reuses an existing one) and a server certificate for the addresses, sets `fleet.role` to master, and prints the CA fingerprint. Refuses if the host is already a master or child. |
| `fleet token create` | `--tags a,b` (tags every node that joins with it), `--ttl` (default `1h`), `--uses` (default `1`) | Master only. Prints a join code (`swj1_...`) and the exact `fleet join` line to run on each server. |
| `fleet token list` | none | Master only. Lists unexpired join tokens: id, uses left, expiry, tags. |
| `fleet token delete <id>` | none | Master only. Deletes a join token so it can no longer be used. |
| `fleet join <code>` | `--name NAME` (default `server.name`) | Makes a solo host a child of the master in the code. Verifies the master against the CA pin in the code before sending anything, stores the node's key and certificate, and sets `fleet.role` to child. If the requested name is already taken (case-insensitively) on that master, it registers as `<name>-2`, `-3`, and so on instead, and prints a note saying so. |
| `fleet status` | none | This host's role. On a master: listen address, join URL, CA fingerprint, node count, a `replica drops` line with the cumulative counts for every node whose replica has refused points (out of order, over the 5000-series limit, or duplicates, which are harmless re-sends), and a warning line for every node whose clock is more than 30 s off. The master's log carries a warning when a node's out-of-order or over-limit count grows. On a child: node id, master, link state (`connecting`; `linked`; `catching up` when the master is reachable but unsent data is still being retried; `retrying` when the master is unreachable or refusing requests; `revoked`), last ack, outbox size, unsent records and gaps, last error. |
| `fleet nodes` | `--tag T`, `--state S` (`online`, `lagging`, `stale`, `down`, `revoked`), `--q TEXT` (search name, id or address) | Table of nodes: on a master every enrolled node plus this host as `self`, elsewhere only `self`. Columns: state, clock skew, CPU, memory, worst disk, version, last seen, tags, depends-on, short id. SKEW is the master's filtered `master time - node send time` (the sample closest to zero among the last ten); a negative value means the node's clock is ahead. |
| `fleet node revoke <node>` | none | Master only. Refuses the node's certificate from now on; its history is kept. `<node>` is a node id, an id prefix of at least 6 characters, or an exact name. |
| `fleet node remove <node>` | none | Master only. Deletes the node from the registry and from liveness tracking and resolves its open node-down alert, if any. Its certificate is refused from then on (an unknown node counts as revoked). Its replicated history stays on disk under `fleet/nodes/<id>/`. Use it for a server that is gone for good. |
| `fleet node rename <node> <name>` | none | Master only. Changes the node's display name. Node names are unique case-insensitively: renaming to a name already used by another node is refused outright (unlike a fresh join, which silently suffixes instead). Renaming a node whose name is matched by a name-based silence stops that silence from matching it -- see [Fleet alerting](06-alerting-and-channels.md#silences-and-maintenance-windows). |
| `fleet node tag <node> <a,b>` | none | Master only. Replaces the node's tags; an empty string clears them. |
| `fleet node depends <node> <dep1,dep2,tag:t>` | none | Master only. Sets the node's dependency list (another node id/name, or `tag:<t>`); an empty string clears it. When a dependency is down, this node's own node-down/reachability alerts fold into the dependency's incident instead of paging separately. See [Fleet alerting](06-alerting-and-channels.md#dependencies). |
| `fleet incidents` | `--state S` (`firing`, `acked`, `resolved`, `suppressed`), `--node N`, `--tag T`, `--limit N` (default unlimited) | Master only. Table of incidents: id, state, severity, title, nodes, opened, updated. |
| `fleet incident <id>` | none | Master only. Full detail for one incident: its alerts (with fire/resolve times and whether each was delivered locally) and its complete timeline. |
| `fleet ack <id>` | none | Master only. Acknowledges an incident (actor `cli`). Refused if it is already resolved. |
| `fleet explain <key|id>` | none | Master only. Prints the pipeline trail (record, silence, dependency, group, route, escalate, deliver, receipt) for an alert key or an incident id -- see [Fleet alerting](06-alerting-and-channels.md#the-pipeline). |
| `fleet silence add` | `--match tag=,node=,rule=,severity=` (required; `node=` matches the node's display name as a glob OR its exact id), `--for <dur>` or `--until <RFC3339>` (one required), `--comment` | Master only. Creates a silence, printing its id and end time. |
| `fleet silence list` | none | Master only. Table of silences: id, match, start, end, author, comment. |
| `fleet silence expire <id>` | none | Master only. Ends a silence immediately. |
| `fleet maintenance add` | `--name` (required), `--match` (required), `--days` (required, comma list of `mon`..`sun`), `--from`/`--to` (`HH:MM`, `--to` before `--from` crosses midnight), `--tz` (default `UTC`) | Master only. Creates a recurring maintenance-window silence. |
| `fleet maintenance list` | none | Master only. Table of maintenance windows: id, name, match, days, from, to, tz, author. |
| `fleet maintenance delete <id>` | none | Master only. Deletes a maintenance window. |
| `fleet route test` | `--node NAME` (name or id), `--tag t1,t2`, `--rule RULE`, `--severity SEV` | Master only. Dry-runs routing for a hypothetical alert: prints the matched route, every matched policy's steps (more than one when a `continue: true` route fanned out) and `repeat_every`, and whether a silence would suppress it. |
| `fleet alerting show` | none | Master only. Prints the current `AlertingConfig` (routes, policies, rules) as JSON. |
| `fleet alerting apply <file.json>` | none | Master only. Replaces the alerting config wholesale from a JSON file, unconditionally (no optimistic-concurrency check, unlike the web editor). |
| `fleet rules` | none | Master only. Table of every aggregate rule: name, state (`ok`, `firing`, `no data`, or `error: ...`), current value, since, and its expression. |
| `fleet managed list` | none | Master only. Table of managed-config fragments: id, tag (`*` for every node), version, author, values. |
| `fleet managed set [key=value ...]` | `--tag T` (default: every node), `--replace` (replace the fragment's values wholesale instead of merging) | Master only. Creates or updates the fragment for `--tag`, merging the given keys into its existing values by default. Only the 10 allowlisted keys are accepted -- see [Fleet alerting](06-alerting-and-channels.md#managed-config). |
| `fleet managed delete <id>` | none | Master only. Deletes a managed-config fragment. |
| `fleet managed status` | none | Master only. Table of every node with an applicable fragment: node, applied/desired version, whether applied, drift, error, and any cross-fragment key conflicts. |
| `fleet leave` | `--purge`: also delete this node's fleet identity and unsent outbox | Child only. Returns the host to solo. Local history is always kept. Leaving is local only: the master is not told, and it will report the node as down (and page for it) until you run the command `leave` prints, `sudo trinetra fleet node revoke <node-id>`, on the master (or `fleet node remove <node-id>` to drop it from the list as well). Any managed-config values already applied are kept as ordinary local config, no longer read-only. |
| `fleet disable` | `--purge`: also delete the CA, node registry and every node's replicated history | Master only. Returns the host to solo. Without `--purge`, running `fleet init` again reuses the same CA, so children need not re-join. `fleet disable` then `fleet init` (and a restart) is also how you re-issue the master's 2-year server certificate, which the master warns about from 90 days before it expires. |

Children must reach the master's fleet port directly, or through TCP-level
forwarding only: the master terminates the mutual TLS itself, so a
TLS-terminating reverse proxy in front of it breaks both the CA pin check and
node authentication (see [Fleet mode](02-architecture.md#fleet-mode)).

The tunable fleet keys are ordinary config keys, set with `config set` and
applied on restart unless noted otherwise: `fleet.listen` (master listen
address, default `:9443`), `fleet.outbox_max_mb` (child outbox cap, default
`512`, minimum `16`), `fleet.node_down_after` (how long the master waits
before calling a silent node down, default `2m`, minimum `30s`), and two
child-only keys that apply live with no restart, `fleet.fallback_after`
(default `2m`, minimum `5s`) and `fleet.link_down_warn_after` (default
`10m`, minimum `30s`) -- see
[Configuration](04-configuration.md#fleet-child-timing-keys) for what each
one governs. The role, address, master URL, CA pin and node id are written
only by the commands above; `config set` refuses them.

A typical enrollment:

```
# on the master
sudo trinetra fleet init --address monitor.example.com,203.0.113.7
sudo systemctl restart trinetra
sudo trinetra fleet token create --tags prod --uses 3

# on each server, with the code it printed
sudo trinetra fleet join swj1_...
sudo systemctl restart trinetra

# back on the master
sudo trinetra fleet nodes --tag prod
```

## trinetra status-page

`trinetra status-page` manages the [public status page](08-web-ui.md#public-status-page)
through the running daemon (standalone host or fleet master; a child refuses).

```
usage:
  trinetra status-page service list
  trinetra status-page service add <id> --name N [--group G] [--desc D] [--order N] [--hold 3m] --target T ...
  trinetra status-page service rm <id>
  trinetra status-page incident list [--all]
  trinetra status-page incident show <id>
  trinetra status-page incident open --title T --service ID ... --impact degraded|outage|maintenance --message M [--status S]
  trinetra status-page incident update <id> --status S --message M
  trinetra status-page incident resolve <id> [--message M]
```

| Command | Output / effect |
| --- | --- |
| `service list` | One line per service: `<id> <name> <state> <reason>`. |
| `service add` | Creates or replaces a service. `--target` is repeatable: `host`, `node:<id>`, `tag:<tag>`, `container:<name>[@node]`, `unit:<unit>[@node]`, `mount:<path>[@node]`. `--hold` is the hold-down (0 to 1h, default 3m). |
| `incident list` | Open incidents (all with `--all`) as `<id>  <status> <impact> <title>`. |
| `incident open` | Starts a manual incident on the given services. |
| `incident update` | Posts an update with status `investigating`, `identified`, `monitoring` or `resolved`; echoed to `status.echo_channels`. |
| `incident resolve` | Posts a final `resolved` update. |

```bash
sudo trinetra status-page service add api --name "API" --group Core --target tag:api --hold 30s
sudo trinetra status-page incident open --title "Elevated errors" --service api --impact degraded --message "We are looking into it."
```

## trinetra update

`trinetra update` manages signed self-update (see [Operations:
Updating](10-operations.md#updating) for the guided walkthrough, and
[Configuration: Self-update settings](04-configuration.md#self-update-settings)
for the `update.*` config keys). Run it with no arguments to print the usage:

```
usage: update status [--json] | check | apply [--version V] [--bundle DIR] [--channel C] [--force] | rollback
```

| Command | Flags | What it does |
| --- | --- | --- |
| `update status` | `--json` | Read-only, no root needed. Prints running version, channel, source, floor, any available/pending version, when the channel was last checked, the outcome of the last apply/rollback, whether release keys are compiled in, and their fingerprints. |
| `update check` | none | Fetches and verifies the configured channel's newest release pointer and manifest, and reports whether it is newer than this host's floor. Does not install anything. |
| `update apply` | `--version V` (an exact version instead of the channel's latest), `--bundle DIR` (install from a local release directory instead of the network source), `--channel C` (override `update.channel` for this one apply), `--force` (retry a version this host previously marked bad) | Root only. Fetches, verifies both signatures, checks policy (channel, floor, `min_upgrade_from`, known-bad), stages and re-verifies every file, smoke-tests the staged core binary, swaps it in, and launches the health guard (restart, poll for up to 90s, commit or roll back). See [Operations: Updating](10-operations.md#what-apply-actually-does) for the full sequence. |
| `update rollback` | none | Root only. Restores the previously installed build (the one `apply` last replaced) and runs it through the same guarded restart-and-confirm as `apply`. Refused if there is nothing to roll back to, or another update is already pending or in progress. |

`update guard [--if-pending]` also exists, but it is not a command an
operator runs directly. It always runs from the pinned guard binary
`/usr/local/lib/trinetra/guard/trinetra`: `apply`/`rollback` launch it as the
transient `trinetra-update-guard` unit right after swapping a build in, and
`trinetra-update-watchdog.timer` runs it with `--if-pending` every minute to
finish any update a killed guard, a crash mid-swap or a reboot left pending
(see [Operations: The update watchdog](10-operations.md#the-update-watchdog)).
It is listed here only so it is recognizable in a process list or the
journal, not as a documented entry point.

What `update` verifies, and why each check exists, is in
[Security](14-security.md); to verify a release by hand with standard tools
instead, see [Security: Verify a download yourself](14-security.md#verify-a-download-yourself).

### `trinetra-release` (maintainers only)

`cmd/trinetra-release` is the release-signing tool. It is not shipped to
hosts, and it is the one place outside the plugins allowed a non-stdlib
dependency (`golang.org/x/crypto`, for the passphrase-encrypted maintainer
key). Run it from a checkout with `go run ./cmd/trinetra-release <command>`.
The procedure that uses it is in
[Operations: Release keys and releasing](10-operations.md#release-keys-and-releasing-maintainers-only).

| Command | What it does |
| --- | --- |
| `keygen --role ci\|maint\|pointer --out FILE` | Generates a key pair. `ci`/`pointer` write a base64 seed (for a GitHub secret); `maint` asks for a passphrase and writes an encrypted key. Prints only the public key and its fingerprint. |
| `manifest --dir DIR --version V --channel C --min-upgrade-from V --published RFC3339 [--keys-from-binary]` | Writes `DIR/manifest.json` for exactly the nine release binaries; any missing or unexpected `trinetra*-linux-*` file is an error. `--keys-from-binary` fills `keys` with this build's compiled-in key set. |
| `sign --role ci\|pointer --in FILE --out FILE` | Signs with the seed in `TRINETRA_SIGNING_KEY`, using the role's domain prefix. |
| `pointer --channel C --version V --issued RFC3339 --out FILE` | Writes a channel pointer that expires exactly 14 days after `--issued`. |
| `latest --channel stable\|beta` | Reads `gh api repos/{owner}/{repo}/releases --paginate` on stdin and prints the highest version for the channel among releases that carry a signed manifest (`manifest.json`, `manifest.ci.sig` and `manifest.maint.sig` all present as assets); prints nothing if none qualify. |
| `verify DIR` | Runs the host verification (both signatures, then every file's size and hash) on a release directory. |
| `fingerprints` | Prints the compiled-in production key fingerprints. |
| `cosign vX.Y.Z --key FILE [--repo OWNER/REPO]` | Co-signs a draft release: verifies the CI signature, shows the manifest and any key change, asks you to retype the version and enter the passphrase, uploads `manifest.maint.sig`, re-verifies the whole draft, then publishes it. |

## 2. Plugin binaries

The two out-of-process plugin binaries now have their own dedicated pages.
Both are separate from the shipped `trinetra` daemon and dial its control
socket rather than reading state in-process; in normal use you launch them via
the `trinetra cli` / `trinetra web` front-doors (section 1 above).

- **[Plugins overview](plugins/README.md)** -- what the plugins are, how they
  install alongside the daemon, and the front-door safe-exec model.
- **[trinetra-ctl](plugins/trinetra-ctl.md)** -- the interactive
  management TUI: subcommands, socket/token resolution, and the full
  management screens and first-run onboarding.
- **[trinetra-web](plugins/trinetra-web.md)** -- the supervised web
  binary: how the daemon runs it, and its direct-invocation flags.

## 3. Daemon-only config management

`trinetra-ctl` (section 2.1) is the primary way to manage a running
trinetra, but every flow it offers has a thin, scriptable equivalent on
the core `trinetra` binary itself. This section is the authoritative
reference for those scriptable verbs, for automation, cron jobs,
configuration-management tooling, or a box you administer entirely over SSH
without `trinetra-ctl` installed. Prefer `trinetra-ctl` for day-to-day,
interactive management; reach for these verbs when you are scripting a change
or managing headless.

| Command | Purpose |
| --- | --- |
| `config get [key]` / `config set <key> <value>` / `config unset <key>` | Read or write any config key directly. `config set` is the scriptable escape hatch for anything a guided screen does not cover. |
| `monitor list` / `monitor enable\|disable <target>` / `monitor threshold <target> <value>` | Discover targets and set per-target on/off state and threshold overrides. |
| `schedule daily HH:MM` / `schedule weekly dow@HH:MM` / `schedule off` | Set or clear the digest schedule. |
| `quiet-hours HH-HH` / `quiet-hours off` | Set or clear the quiet-hours window. |
| `healthchecks set <url>` / `healthchecks off` | Set or clear the healthchecks.io ping URL. |
| `channel list\|add\|remove\|set\|test` | List, add, remove, modify, or test-notify a notification channel. |
| `telegram set-token <token>` | Store the Telegram bot token and print the enrollment PIN (#90, below). |

Every one of these persists to `/etc/trinetra/config.json` and sends a
best-effort `SIGHUP` to reload a running daemon (the persists-plus-SIGHUP
model described in section 1 and in [Advanced configuration and
management](advanced-configuration.md#persistence-and-hot-reload)).
Full flag syntax, validation, and defaults for each key live in the
[config-key reference](advanced-configuration.md#config-key-reference).

### #90: `telegram set-token` now prints the enrollment PIN

`trinetra telegram set-token <token>` used to only persist the token; you
had to go read the daemon's journal to find the one-time enrollment PIN it
generated. It now dials the control socket after saving the token and prints
the `/start <pin>` instruction directly:

```
Telegram token saved. To finish enrollment, from your Telegram account message the bot:
  /start 123456
```

If the daemon cannot be reached, for instance because it is not installed or
is still starting, `telegram set-token` still saves the token and exits
successfully; it just falls back to pointing you at the journal instead:

```
Telegram token saved. The daemon will log the enrollment PIN on start:
  journalctl -u trinetra | grep /start
```

A failed PIN fetch is never treated as `telegram set-token` failing; only the
printed message changes. See the enrollment diagram in [Installation and
first run](03-installation.md#telegram-optional) for how
this fits together with `trinetra-ctl`'s onboarding screen, which shows
the same PIN.

### #91: guided setup lives in trinetra-ctl; `config set` is the escape hatch

The guided, validated walk-through for web UI setup and first-run Telegram
onboarding lives in `trinetra-ctl` (section 2.1), not in the core CLI. The
core CLI deliberately does not grow an interactive wizard of its own:
`config set` and the dedicated verbs above remain the direct, scriptable way
to write any of the same keys, so nothing you could do before is gone, and
configuration-management tooling never has to shell out to an interactive
program to change a setting.

---

[Previous: Operations](10-operations.md) | [Handbook index](README.md) | [Next: Roadmap and status](12-roadmap-and-status.md)
