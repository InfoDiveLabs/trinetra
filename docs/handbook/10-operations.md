# Operations

Once `serverwatch` is installed and answering you over Telegram, it mostly
looks after itself. `Restart=always` brings it back after a crash, and
`WantedBy=multi-user.target` starts it at every boot. This chapter covers the
handful of things you will still do by hand over the life of the install:
watching the running service, upgrading the binary in place, removing it,
pulling old data forward from a legacy install, and working out what is wrong
when something looks off.

Everything here is designed to be safe to run on a live daemon. Nothing in this
chapter needs you to stop monitoring first.

## Day-to-day management

There is nothing special about how `serverwatch` runs. It is an ordinary
systemd unit called `serverwatch`, so every tool you already use for systemd
services works exactly as you would expect.

Check that it is up, see when it last restarted, and read its exit code:

```bash
systemctl status serverwatch
```

Restart it (you will only need this after a `storage.*` config change, see
[Configuration](04-configuration.md), or a binary upgrade, since every other
setting hot-reloads over SIGHUP):

```bash
sudo systemctl restart serverwatch
```

Read the logs. All of the daemon's output, both stdout and stderr, goes to the
journal, because the unit sets `StandardOutput=journal` and
`StandardError=journal`. There is no separate log file to hunt for:

```bash
journalctl -u serverwatch          # full history
journalctl -u serverwatch -f       # follow live
journalctl -u serverwatch | grep "/start"   # find the one-time enrollment PIN
```

Note that you can run `serverwatch` from anywhere on the box, not just from
wherever you left the downloaded binary. The install step copies the binary to
`/usr/local/bin/serverwatch` and also drops a symlink at `/usr/bin/serverwatch`.
That second path exists so `sudo serverwatch ...` resolves on every distro (more
on why in the troubleshooting section below). So these both work regardless of
your current directory:

```bash
serverwatch status                 # current snapshot, no root needed to read
sudo serverwatch config get        # effective config
```

## Upgrading in place

Upgrading is a re-install. You do not stop the service, edit the unit, or touch
your config and history; all of that is preserved. You simply get the newer
binary onto the box and run its `install` command.

First, get the new binary. Download the release asset for your architecture, or
build it from source, the same way you did originally:

```bash
curl -fsSL -o /tmp/serverwatch \
  https://github.com/Suraj-Tiwari/server-monitor/releases/latest/download/serverwatch-linux-arm64
chmod +x /tmp/serverwatch
```

Then run `install` from the new binary:

```bash
sudo /tmp/serverwatch install
```

This is safe even though the old daemon is still running from the very file it
is about to replace. `install` does not truncate and overwrite
`/usr/local/bin/serverwatch` in place. It writes the new bytes to a temporary
file in the same directory and then does an atomic `rename` over the existing
path. A `rename` swaps the directory entry without touching the running file's
inode, so it succeeds against a live executable. A plain overwrite would fail
here with the classic `ETXTBSY` "text file busy" error, which is exactly the
trap this design avoids.

Because the swap does not disturb the process that is currently running, the
daemon keeps monitoring uninterrupted on the old binary. It only starts running
the new code on its next restart. When you are ready to cut over, restart the
unit:

```bash
sudo systemctl restart serverwatch
```

If you are coming from a pre-storage version that stored history as JSONL files,
run the one-time migration described below after the upgrade to pull that old
history into the new store.

## Uninstalling

To remove the service cleanly:

```bash
sudo serverwatch uninstall
```

This disables and stops the unit (`systemctl disable --now serverwatch`),
removes `/etc/systemd/system/serverwatch.service`, reloads systemd, and deletes
the `/usr/bin/serverwatch` symlink. It removes that symlink only when it is
still the one `install` created pointing back into `/usr/local/bin`, so it will
never delete a real distro-provided binary that happened to share the name.

By default `uninstall` leaves your data and configuration in place, so you can
reinstall later and pick up where you left off. Everything under
`/var/lib/serverwatch` (status, time-series history, downtime events) and the
config at `/etc/serverwatch/config.json` stays put.

To wipe those too, add `--purge`:

```bash
sudo serverwatch uninstall --purge
```

That additionally deletes the entire state directory and the config file,
secrets included. Use it when you are done with the host for good.

## Migrating legacy data

Older, pre-storage-epic versions of `serverwatch` wrote their history as
per-day JSONL files (`samples/YYYY-MM-DD.jsonl`) plus a `downtime.jsonl` log,
rather than the compact time-series store under `ts/`. If you are upgrading from
one of those, a one-time import pulls that old history into the new store so
your graphs and downtime reports stay continuous across the upgrade:

```bash
sudo serverwatch migrate
```

This reads every legacy sample and downtime record, appends them into the
configured time-series store using the same metric keys the live daemon uses,
downsamples the result, and then archives the old files by renaming them to a
`*.migrated` sibling. It never deletes the originals, so the archive is an audit
trail you can remove yourself later if you want.

The command is idempotent. After a successful import it drops a `.migrated`
marker file in the state directory, and a second run sees that marker and does
nothing:

```
already migrated at 2026-07-31T10:04:11Z; nothing to do (use --force to re-import)
```

That makes it safe to leave in an upgrade script that might run more than once.
Running it on a fresh install with no legacy files present is also harmless: it
reports `nothing to migrate` and leaves no marker, so it stays re-runnable if
legacy data appears later. If you ever genuinely need to import again, force
past the marker:

```bash
sudo serverwatch migrate --force
```

## Troubleshooting

When something is not behaving, start with the built-in diagnostic before
anything else:

```bash
sudo serverwatch doctor
```

`doctor` runs the same probes the daemon runs and prints what actually works on
this host: the Docker access method it found (`socket`, `group`, `sudo`, or
`unavailable`), whether `smartctl` responds, how many thermal zones exist, the
total number of targets discovery found, the on/off state of each opt-in
collector (`container_stats`, `net_throughput`, `services`, `processes`,
`smart_attrs`), and the time-series store's current series count and on-disk
size. A typical run looks like:

```
docker: available=true method=sudo
smartctl: ok
thermal zones: 2
targets discovered: 14
collectors: container_stats=on net_throughput=on services=on processes=on smart_attrs=on
time-series: 14 series, 2.3 MB on disk (raw+1m)
```

That single output answers most "why isn't X being monitored" questions
directly. Anything shown as unavailable is simply skipped, never a crash.

A few specific things worth knowing:

**`sudo serverwatch` says command not found.** This only happens on older or
minimal distributions (RHEL and CentOS 7 family, some stripped-down images)
whose sudo `secure_path` does not include `/usr/local/bin`. The install step
handles it by also linking the binary into `/usr/bin`, which is on
`secure_path` everywhere. If you hit this, you are almost certainly on a host
where the `/usr/bin/serverwatch` symlink failed to be created (`install` prints
a warning when it cannot make it, but keeps going because the service itself
runs fine off its absolute `ExecStart`). Re-running `sudo /usr/local/bin/serverwatch install`
recreates the link.

**The control socket.** The daemon serves its control API over a Unix socket at
`/run/serverwatch/control.sock` (see [Architecture](02-architecture.md)). It is mode `0600` and owned by root, so only
root can talk to it, and it is created for you by systemd, not the daemon: the
unit sets `RuntimeDirectory=serverwatch`, which makes systemd create
`/run/serverwatch` before the service starts and clean it up when the service
stops. If the socket is missing, the daemon is not running; check
`systemctl status serverwatch`.

**`serverwatch cli` / `serverwatch web` refuses to run, warning about
tampering.** Both are front-doors that exec the companion plugin binaries as
root, so before handing off control the core verifies the plugin's owner,
permissions, and SHA-256 against the install-time manifest at
`/var/lib/serverwatch/plugins.json` (see [Architecture](02-architecture.md)
and [Command reference](11-command-reference.md)). A refusal means one of
those checks failed. If you just rebuilt or hand-copied `serverwatch-ctl` or
`serverwatch-web` into place yourself, this is expected: run `sudo serverwatch
install` to record its checksum, then try again. If you did not touch the
plugin binary, do not just re-run install to make the warning go away;
investigate first, since it means something replaced or modified the file
since the last install.

**The web UI would not start.** (See [The web UI](08-web-ui.md).) If you are running the `serverwatch-web` binary
with `web.enabled true` and the web server fails to come up (a bad TLS config, a
port already in use), that failure is logged to the journal but never stops
monitoring. The daemon keeps sampling and alerting over Telegram regardless.
Look for the error with `journalctl -u serverwatch` and fix the `web.*` config,
then restart, since `web.*` needs a restart rather than a SIGHUP.

**Docker is not being monitored.** Run `doctor`. If it reports docker
`unavailable`, either add the service user to the `docker` group or make sure
`sudo docker` works without a password. The daemon runs as root by default, so
the socket is normally reachable directly; if it is not, it falls back to `sudo`
automatically.

**SMART shows nothing.** Install `smartmontools` (`apt install smartmontools`
or your distro's equivalent). Some disks and USB-to-SATA bridges do not expose
SMART at all; those devices are simply skipped.

---

[Previous: Storage and the data model](09-storage-and-data-model.md) | [Handbook index](README.md) | [Next: Command reference](11-command-reference.md)
