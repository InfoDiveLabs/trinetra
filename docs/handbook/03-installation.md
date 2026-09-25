# Installation and first run

This chapter walks you from a bare Linux host to a running `trinetra`
service that answers you over Telegram. It is written as a runbook: follow the
numbered steps in order and you will end up with a monitored server. Each step
also explains what happens under the hood, so you understand what you just did
rather than only that it worked.

By the end you will have installed the binary, registered it as a systemd
service that survives reboots, confirmed what the host exposes, and claimed the
bot as its owner from your own Telegram account.

## 1. Prerequisites

Before you start, make sure the host and your accounts are ready.

**A Linux host with systemd.** This covers Ubuntu, Debian, Raspberry Pi OS,
Fedora, RHEL, CentOS, and almost every mainstream distribution. The service is
managed entirely through systemd, so a host without it is not supported.

**Root access.** The service runs as `root`. This is deliberate, not lazy:
root is what lets the daemon read every container and process on the box, pull
SMART health data straight off the disks, and start automatically at boot with
no interactive login. You will run the install command with `sudo`.

**A Telegram bot token.** Message **@BotFather** on Telegram, send `/newbot`,
follow the prompts to name your bot, and copy the token it hands back. It looks
like `123456789:AAExampleTokenStringFromBotFather`. Keep it handy; you will set
it in step 5. This is the one setting that is genuinely required. Everything
else has a working default.

The rest are optional and are auto-discovered when present:

- **Docker.** If the daemon can reach Docker, every container becomes a
  monitored target automatically. Nothing to configure.
- **smartmontools.** Install it (`apt install smartmontools`, or the
  equivalent for your distro) to unlock SMART disk-health reporting. Without
  it, SMART is simply skipped.
- **A healthchecks.io check URL.** This drives the real-time dead-man switch
  described in a [later chapter](07-downtime-and-liveness.md), the one mechanism
  that can alert you while the
  box itself is offline. Optional, and set later with a single command.

Anything in that optional list that is missing is reported as unavailable and
left unmonitored. A missing tool never stops the daemon from running.

## 2. Get the binaries

The default, recommended path is to download the prebuilt release assets: the
releases page ships compiled `trinetra`, `trinetra-ctl`, and
`trinetra-web` binaries, so you do not need a Go toolchain on the host.
Download the three you want into one directory and `trinetra install` in
step 3 picks up and installs all of them in a single command. Building from
source is a secondary option, covered below, for when you want to compile it
yourself.

### Option A: prebuilt release asset

The releases page publishes one asset per architecture. Choose the one that
matches your host:

| Host | Asset |
|------|-------|
| x86-64 server or NUC | `trinetra-linux-amd64` |
| Raspberry Pi 3/4/5 on a 64-bit OS | `trinetra-linux-arm64` |
| Older 32-bit Pi or ARMv7 | `trinetra-linux-arm` |

Download the three assets for your architecture into one directory and make
them executable. The example below grabs the arm64 builds; swap the `arch`
value (`linux-amd64` / `linux-arm64` / `linux-arm`) for your host. Each file
drops its arch suffix so `trinetra install` finds the plugins by name.

```bash
cd /tmp && arch=linux-arm64
for b in trinetra trinetra-ctl trinetra-web; do
  curl -fsSL -o "$b" "https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/$b-$arch"
done
chmod +x trinetra trinetra-ctl trinetra-web
```

Verify what you downloaded before trusting it. Each release includes a
`checksums.txt` file; compute the SHA-256 of each binary and confirm it matches
the line for that asset:

```bash
sha256sum trinetra trinetra-ctl trinetra-web
```

Compare each printed hash against the matching line in `checksums.txt`. If any
differ, do not install; re-download and try again.

The two plugins are optional. Drop `trinetra-ctl` / `trinetra-web` from
the loop if you only want the Telegram daemon; `trinetra install` (step 3)
installs whichever of the three it finds beside the daemon binary. You can
always add a plugin later by downloading it next to `trinetra` and running
`trinetra install` again.

### Option B: build from source

If you would rather build it yourself, clone the repository and cross-compile
the Linux binaries. This needs Go 1.22 or newer.

```bash
git clone git@github.com:Suraj-Tiwari/server-monitor.git
cd server-monitor
make linux
```

`make linux` produces `dist/trinetra-linux-amd64` and
`dist/trinetra-linux-arm64`. Copy the one you need to the server:

```bash
scp dist/trinetra-linux-arm64 myserver:/tmp/trinetra
```

The two plugin binaries build the same way, with no build tag, from their own
`./cmd` package:

```bash
GOOS=linux GOARCH=arm64 go build -o dist/trinetra-ctl-linux-arm64 ./cmd/trinetra-ctl
GOOS=linux GOARCH=arm64 go build -o dist/trinetra-web-linux-arm64 ./cmd/trinetra-web
```

(`make cross` builds this whole matrix, plus `trinetra`, for every
supported platform in one pass.) Copy whichever of them you want next to
`/tmp/trinetra` on the server; `trinetra install` picks up whatever it
finds beside the SOURCE binary it is installing and installs it too (see
step 3 below).

Either way, you now have an executable at `/tmp/trinetra` on the host, ready
to install.

## 3. Install as a systemd service

One command turns that loose binary into a managed, boot-persistent service:

```bash
sudo /tmp/trinetra install
```

That single command does seven things. It is worth knowing each one, because
this is the moment your host goes from "has a binary in /tmp" to "runs a
monitored service."

1. **Copies the binary to `/usr/local/bin/trinetra`.** This is the real,
   permanent home of the executable. The systemd unit points at this absolute
   path.

2. **Symlinks it into `/usr/bin/trinetra`.** This is a small but important
   detail. On some distributions, notably RHEL and CentOS-family hosts, sudo's
   `secure_path` does not include `/usr/local/bin`. Without the symlink,
   `sudo trinetra ...` would fail with "command not found" on those hosts
   even though the service itself runs fine. The symlink puts the command on a
   directory that is on sudo's `secure_path` everywhere, so the `sudo
   trinetra` shortcut always resolves. The symlink is created only if
   nothing already lives at that path, so it never clobbers a distro-provided
   binary, and it is non-fatal: if the link cannot be made, the binary and unit
   are already in place and only the shortcut is affected.

3. **Copies any plugin binaries it finds next to the source binary.** This is
   what makes install a true one-step process: if `trinetra-ctl` and/or
   `trinetra-web` are sitting in the same directory as the `trinetra`
   binary you ran install from (step 2), install copies each one it finds into
   `/usr/local/bin` alongside the daemon, mode `0755`. A plugin that is not
   present there is simply skipped, not an error, and a copy hiccup on one
   plugin is non-fatal and does not stop the daemon itself from installing.
   You never have to copy the plugin binaries into place by hand; just
   download or build them next to `trinetra` before running install.

   > **Trust the directory you install from.** Because install adopts whatever
   > `trinetra-ctl` / `trinetra-web` sit beside the `trinetra` binary
   > and records *their* checksums as the trust anchor (step 4), it trusts the
   > contents of that directory. Only run `sudo trinetra install` from a
   > directory you control and whose binaries you verified (for example the
   > release assets you checksummed against `checksums.txt` in step 2). Do not
   > run it from a world-writable or shared location like `/tmp` where another
   > user could have dropped a look-alike `trinetra-ctl`/`trinetra-web`
   > beside your binary. This is operator responsibility: install runs as root
   > and executes with root's trust in that directory. (Once installed, the
   > front-door still verifies each plugin against the recorded manifest on
   > every run, so this window is only at install time.)

4. **Records the plugin checksum manifest.** `install` scans the directory it
   just copied the binary (and any plugins) into for the companion plugin
   binaries, `trinetra-ctl` and `trinetra-web`, and writes the SHA-256 of
   any it finds to `/var/lib/trinetra/plugins.json`, mode `0600`,
   root-only. This manifest is the trust anchor the safe front-door commands
   (`trinetra cli` / `trinetra web`, see
   [Architecture](02-architecture.md) and
   [Command reference](11-command-reference.md)) check before they will exec
   either plugin. A hiccup writing the manifest is non-fatal to the rest of
   install. If you build or hand-copy `trinetra-ctl` or `trinetra-web`
   directly into `/usr/local/bin` yourself, bypassing step 3 above, you must
   (re-)run `trinetra install` afterward so its checksum gets recorded; the
   front-door refuses to run a plugin binary that is not in the manifest.

5. **Writes and enables the systemd unit** at
   `/etc/systemd/system/trinetra.service`, then runs `systemctl
   daemon-reload` followed by `systemctl enable --now trinetra`. The unit it
   writes looks like this:

   ```ini
   [Unit]
   Description=Trinetra — self-hosted server & fleet monitor
   After=network-online.target docker.service
   Wants=network-online.target

   [Service]
   Type=simple
   ExecStart=/usr/local/bin/trinetra daemon
   Restart=always
   RestartSec=5
   WatchdogSec=90
   User=root
   RuntimeDirectory=trinetra
   StandardOutput=journal
   StandardError=journal

   [Install]
   WantedBy=multi-user.target
   ```

   Two lines in that unit are worth calling out. `WatchdogSec=90` arms the
   systemd watchdog: the sampler loop pings systemd on every fast tick (default
   every 5 seconds), comfortably inside the 90-second window, so if the loop
   ever wedges and the pings stop, systemd restarts the unit for you.
   `RuntimeDirectory=trinetra` tells systemd to create `/run/trinetra`
   before the service starts and remove it when the service stops; that is
   where the daemon puts its control socket (see
   [Architecture](02-architecture.md)), so the directory is always present
   with the right lifetime. Together with `Restart=always` and
   `WantedBy=multi-user.target`, the service survives crashes and comes back on
   every boot.

6. **Seeds `/etc/trinetra/config.json`** if it does not already exist. The
   file is created with mode `0600`, root-owned, because it holds your bot
   token and other secrets. An existing config is left untouched, so a re-run
   of `install` (for example, to upgrade the binary) never overwrites your
   settings.

7. **Starts the service.** By the time the command returns, the daemon is
   already running.

You will see a confirmation line naming which plugins were installed (or
noting that none were found next to the source binary), followed by a
reminder to set a token, which is exactly what you do in step 5 below.

## Upgrading from a serverwatch install

trinetra is the rename of what used to be called serverwatch: same daemon,
same data, new name. If this host already runs a `serverwatch` install
(`/etc/serverwatch/config.json` or `/var/lib/serverwatch` exists), do not
follow the fresh-install path above expecting a clean slate — `sudo trinetra
install` detects it and migrates in place instead, in the **same single
command** you already know from step 3. There is no separate migration tool
to run.

**First put `trinetra` and the plugins you use into one directory**, exactly
as in step 2's download loop: `trinetra-ctl` if you use the terminal UI
(`serverwatch cli`), `trinetra-web` if you use the web UI. `trinetra install`
installs only the plugins it finds next to the `trinetra` binary it runs
from, and the migration removes the old `serverwatch-ctl` /
`serverwatch-web`. Upgrading with the core binary alone takes the web UI and
the terminal UI down until you add the plugins and re-run `sudo trinetra
install`; the migration summary ends with a `WARNING:` line for each old
plugin that has no new counterpart.

```bash
cd /tmp && arch=linux-arm64     # the same loop as step 2
for b in trinetra trinetra-ctl trinetra-web; do
  curl -fsSL -o "$b" "https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/$b-$arch"
done
chmod +x trinetra trinetra-ctl trinetra-web
sudo /tmp/trinetra install
```

Before the normal install steps run, this:

1. **Stops and disables `serverwatch.service`** (`systemctl stop` then
   `disable`; a missing unit is fine). It then insists systemd confirms the
   old daemon is actually stopped before touching its files — moving a state
   directory out from under a live process would lose writes. If systemd
   cannot confirm that (an unusual host, or systemd being slow to report), the
   migration refuses and tells you to re-run with `--force` once you have
   confirmed serverwatch is not running yourself.

   A `serverwatch daemon` started by hand (in a terminal, tmux, or a
   container without systemd) is invisible to `systemctl`, so the migration
   also reads the old daemon's pid file (`/var/lib/serverwatch/serverwatch.pid`).
   If that process is alive and its executable is `serverwatch`, install
   refuses. When `serverwatch.service` is not running, it refuses before
   touching anything ("is running outside serverwatch.service ... Nothing was
   changed"). When the service is running, the process may be the service's
   own daemon, so install stops the service first and looks again. If the
   process is still alive after the stop, install refuses with
   `serverwatch.service` already stopped and disabled, and changes nothing
   else. The message says so, and gives `sudo systemctl enable --now
   serverwatch` as the way back. Either way, stop the process (`sudo kill
   <pid>`) and re-run. `--force` proceeds anyway and prints a warning. A stale
   pid file (no such process, or the pid now belongs to something else) is
   ignored.
2. **Moves `/etc/serverwatch` → `/etc/trinetra` and `/var/lib/serverwatch` →
   `/var/lib/trinetra`.** This is an atomic rename on the same filesystem.
   If the two paths are on different filesystems (`EXDEV`), it instead copies
   the directory into a staging sibling of the new path, verifies every entry
   byte-for-byte (type, mode, owner, size, symlink target, and a SHA-256 of
   file content) against the original, and only removes the original once the
   copy is proven complete. Nothing is ever deleted before its replacement is
   verified.
3. **Rewrites config paths that pointed inside the old directories** (for
   example `web.tls_cert` / `web.tls_key`), so a TLS cert path under
   `/etc/serverwatch/tls` becomes `/etc/trinetra/tls` automatically. No other
   value changes. When a path is rewritten, the file is re-serialised, so
   the key order and indentation may differ from the original. It is left
   byte-for-byte untouched when there is nothing to rewrite.
4. Runs the **normal install** (copies the binary and any plugins, writes the
   plugin manifest, writes and enables `trinetra.service`, starts it) — see
   steps 1-7 above.
5. **Removes the old install**: the `serverwatch.service` unit file, and any
   `serverwatch-ctl` / `serverwatch-web` plugin binaries found in
   `/usr/local/bin`. (A drop-in override directory for the old unit, if you
   had one, is left in place with a note — copy what you need into
   `/etc/systemd/system/trinetra.service.d/` yourself.) If an old plugin had
   no `trinetra-ctl` / `trinetra-web` counterpart installed in step 4, the
   summary warns, for example "serverwatch-web was installed but no
   trinetra-web was found next to trinetra; the web UI is down until you put
   trinetra-web in the same directory as the trinetra binary and re-run `sudo
   trinetra install`".
6. **Replaces `/usr/local/bin/serverwatch` with a compat symlink to
   `/usr/local/bin/trinetra`**, kept for one release so any script or muscle
   memory still calling `serverwatch ...` keeps working; running it prints a
   one-line deprecation notice to stderr. `/usr/bin/serverwatch` is deliberately
   left in place too, still pointing at `/usr/local/bin/serverwatch` (it is not
   redirected straight to `trinetra` or removed), so the full chain is
   `/usr/bin/serverwatch` → `/usr/local/bin/serverwatch` →
   `/usr/local/bin/trinetra` — `sudo serverwatch ...` keeps resolving on
   distros whose `secure_path` omits `/usr/local/bin`, exactly the reason that
   symlink exists in the first place (see step 2 above). This compat link is
   only installed over a binary the migration recognizes as its own previous
   serverwatch build (or an existing link to itself); anything else is left
   untouched and noted.
7. **Writes `/var/lib/trinetra/migrated-from-serverwatch`**, a timestamp
   marker recording that (and when) this host was migrated, and prints a
   summary of exactly what moved.

Because fleet identity, PKI, the node registry, replicas, and the outbox all
live under the state directory, they move with it — a fleet child or master
that upgrades this way keeps its role and certificates with no re-enrollment.

**It refuses rather than merges.** If both a `serverwatch` install and
existing `trinetra` data are present, install stops without touching
anything and tells you to archive or remove one side yourself; it never
guesses which one you want to keep. If a legacy directory exists but is
*empty*, that is treated as suspicious rather than "nothing to migrate" — most
likely its volume just is not mounted this boot, and migrating the rest would
strand the real data there. Check `/etc/fstab` and `systemctl list-units
--type=mount`, mount it, and re-run. If you moved the serverwatch *state*
volume to `/var/lib/trinetra` yourself ahead of time, tell the migration so it
adopts it instead of erroring:

```bash
sudo trinetra install --state-already-at-new-path
```

**Before the migration, other trinetra commands wait for it.** On a host
that has a serverwatch install with data and no trinetra config or state
yet, two kinds of command refuse instead of starting from scratch:

- `trinetra daemon` (the service's `ExecStart`) exits with an error rather
  than starting an empty history and a fresh Telegram enrollment next to
  your real data. The message is "found a serverwatch install at
  /etc/serverwatch and /var/lib/serverwatch; run `sudo trinetra install` to
  migrate". You would see this if you start the daemon by hand, or start a
  `trinetra.service` written some other way, before running install.
  `systemctl status trinetra` shows it as a failed start.
- CLI commands that write the config or state stop with "found a serverwatch
  install at …; run `sudo trinetra install` first to migrate it, then
  re-run this command. Nothing was changed". These are `config set|unset`,
  `telegram set-token`, `monitor enable|disable|threshold`, `schedule`,
  `quiet-hours`, `healthchecks`, `channel add|remove|set`, `downtime purge`,
  `alerts ack|unack`, `migrate`, `fleet init|join|leave|disable`, and also
  `doctor` and `dump` (opening the sample store creates its directories).
  Without this guard they would create `/etc/trinetra` or
  `/var/lib/trinetra`, and install would then refuse to merge it with the
  serverwatch data. To inspect the old install before upgrading, use the old
  `serverwatch doctor`. Read-only commands such as `config get` and `status`
  are unaffected.

An empty legacy directory (an old `uninstall --purge` can leave one behind)
does not count as an install for either guard. Likewise, when every legacy
directory is empty, the migration has nothing to migrate.

A legacy directory that is itself a symlink (for example
`/var/lib/serverwatch -> /data/sw`) is moved as a symlink:
`/var/lib/trinetra -> /data/sw`, with the data staying where it is. An
interrupted run recognises it and resumes.

The migration is **idempotent and resumable**: it checkpoints before each
step, so re-running `sudo trinetra install` after an interruption (a crash,
a reboot, Ctrl-C) picks up exactly where it stopped rather than redoing
completed work or losing anything. If it does stop partway, the error message
names the exact step, the current state of every path involved, and both how
to finish it (just re-run the same command) and how to roll it back by hand if
you would rather not — moving directories back with `mv`, removing the
migration marker files it leaves inside them, and re-enabling
`serverwatch.service`. It is worth reading that message in full if you ever
see it; it is written to be followed literally, without guessing.

Once migrated, continue with the rest of this chapter as normal — step 4
onward works identically for a migrated host and a fresh one.

## 4. Verify discovery and permissions

Before wiring up Telegram, confirm the daemon can actually see the host. Run
the built-in doctor:

```bash
sudo trinetra doctor
```

This runs the same probes the daemon itself uses and prints what works on this
particular machine. The report covers:

- **Docker access and method.** Whether Docker is reachable and how, reported
  as one of `socket`, `group`, or `sudo`, or `unavailable` if the daemon
  cannot reach it at all.
- **smartctl availability.** Whether `smartctl` responds (directly or through
  sudo), which tells you if SMART health data will be collected.
- **Thermal zones.** How many `/sys/class/thermal` zones exist, the source of
  temperature readings.
- **Discovered target count.** The total number of things discovery found to
  monitor across containers, filesystems, interfaces, thermal zones, and SMART
  devices.
- **Collector on/off state.** The current setting of each opt-in extended
  collector: `container_stats`, `net_throughput`, `services`, `processes`, and
  `smart_attrs`.
- **Time-series store stats.** The configured store's current series count and
  on-disk size, printed like `time-series: 14 series, 2.3 MB on disk
  (raw+1m)`, or `unavailable` if the store failed to open. This is a quick
  sanity check on how much history you are keeping, especially handy on a small
  device.

Anything the host does not expose (no Docker, no SMART) shows up as unavailable
and is simply not monitored. Nothing here crashes the daemon; doctor is a
read-only diagnostic. This is a good command to run right after install, and
again any time you are chasing a permission gap.

To see the individual targets discovery found, along with each one's on/off
state and effective threshold:

```bash
sudo trinetra monitor list
```

## 5. Connect Telegram and enroll as owner

Now give the bot its token. You can do this from the command line as shown
below, or from the guided `trinetra-ctl` first-run onboarding screen
(`sudo trinetra cli`; see [Managing with
trinetra-ctl](plugins/trinetra-ctl.md#managing-with-trinetra-ctl)); both
paths surface the same enrollment PIN. This runbook continues with the
command line: use the token you copied from @BotFather in step 1:

```bash
sudo trinetra telegram set-token <token>
```

This writes the token into the config and signals the running daemon to reload,
so there is no restart to do. The daemon starts talking to Telegram
immediately, and `telegram set-token` prints the next step straight to your
terminal.

At this point the bot is running but **unclaimed**. It will not answer just
anyone. To stop a stranger who stumbles onto your bot from reading your data,
trinetra requires a one-time enrollment step that ties the bot to a single
Telegram chat: yours.

Here is how enrollment works, and it is important to get this right because the
old "just message the bot" behavior is gone. While the bot is unclaimed, the
daemon holds a one-time **6-digit enrollment PIN**. `telegram set-token`
dials the daemon over the control socket right after saving the token and
prints that PIN along with the `/start` instruction, so in the normal case
you never have to leave the terminal you ran it in. The `trinetra-ctl`
first-run onboarding screen (see the [Command
reference](plugins/trinetra-ctl.md)) shows the exact
same PIN the same way, if you set the token through the guided TUI instead.
If the daemon cannot be reached, for example it is not installed yet or is
still starting, `telegram set-token` falls back to pointing you at the
journal, where the daemon also logs the PIN.

```mermaid
flowchart TD
  a[Admin runs trinetra telegram set-token, or completes the token step in ctl onboarding] --> b[Token saved, daemon reloads, bot unclaimed]
  b --> c[Daemon holds a one-time 6-digit enrollment PIN]
  c --> d{Control socket reachable right now?}
  d -->|Yes| e[set-token or ctl onboarding prints the PIN and the /start instruction]
  d -->|No| f[Fallback: read the PIN from journalctl -u trinetra]
  e --> g[Admin sends /start PIN to the bot from their Telegram account]
  f --> g
  g --> h{PIN matches?}
  h -->|Yes| i[Chat claimed as owner, only that chat is answered]
  h -->|No| c
```

1. **Get the PIN.** `telegram set-token` prints it directly:

   ```
   Telegram token saved. To finish enrollment, from your Telegram account message the bot:
     /start 123456
   ```

   If it could not reach the daemon, it prints a fallback instead, and you
   read the PIN from the journal:

   ```bash
   sudo journalctl -u trinetra | grep "/start"
   ```

2. **Claim the bot from your Telegram account.** Open Telegram, find your bot,
   and send it the `/start` command with the PIN, like so:

   ```
   /start 123456
   ```

   Send `/start <pin>`, with the actual six digits in place of `123456`. This
   is the exact form the daemon expects. Sending the bot a plain message, or
   `/start` with no PIN, will not claim it; the PIN is what proves the chat is
   yours.

> **Brute-force protection.** The PIN is bounded against guessing. After a run
> of wrong `/start <pin>` attempts (default 5) the daemon ignores further
> `/start` messages for a short cooldown (default 60 seconds) and rotates the
> PIN to a fresh value, so a partial guessing run is never able to converge on
> the six-digit space. If you fat-finger the PIN enough times to trip this, just
> re-read the new PIN from the journal (`sudo journalctl -u trinetra | grep
> "/start"`) and send that one. The threshold and cooldown are tunable via
> `telegram.enroll_max_attempts` and `telegram.enroll_cooldown` (seconds).

Once you send the correct PIN, that chat is registered as the owner chat. From
then on, only that chat is answered. Anyone else who messages the bot is
ignored, and they cannot claim it because the PIN was single-use.

3. **Confirm it works.** Still in Telegram, send the bot a command:

   ```
   /stats
   ```

   You should get back the current readings: CPU, memory, swap, load,
   temperature, internet reachability, and disk usage. Send `/help` to see the
   full list of commands the bot understands.

If `/stats` comes back with live numbers, you are done. The service is
installed, enabled at boot, discovering the host, and reporting to you over
Telegram. From here everything else, thresholds, schedules, quiet hours,
extra notification channels, is optional tuning (see
[Configuration](04-configuration.md)); the defaults already work.

---

[Previous: Architecture](02-architecture.md) | [Handbook index](README.md) | [Next: Configuration](04-configuration.md)
