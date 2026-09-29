# Operations

Once `trinetra` is installed and answering you over Telegram, it mostly
looks after itself. `Restart=always` brings it back after a crash, and
`WantedBy=multi-user.target` starts it at every boot. This chapter covers the
handful of things you will still do by hand over the life of the install:
watching the running service, upgrading the binary in place, removing it,
pulling old data forward from a legacy install, and working out what is wrong
when something looks off.

Everything here is designed to be safe to run on a live daemon. Nothing in this
chapter needs you to stop monitoring first.

## Day-to-day management

There is nothing special about how `trinetra` runs. It is an ordinary
systemd unit called `trinetra`, so every tool you already use for systemd
services works exactly as you would expect.

Check that it is up, see when it last restarted, and read its exit code:

```bash
systemctl status trinetra
```

Restart it (you will only need this after a `storage.*` config change, see
[Configuration](04-configuration.md), or a binary upgrade, since every other
setting hot-reloads over SIGHUP):

```bash
sudo systemctl restart trinetra
```

Read the logs. All of the daemon's output, both stdout and stderr, goes to the
journal, because the unit sets `StandardOutput=journal` and
`StandardError=journal`. There is no separate log file to hunt for:

```bash
journalctl -u trinetra          # full history
journalctl -u trinetra -f       # follow live
journalctl -u trinetra | grep "/start"   # find the one-time enrollment PIN
```

Note that you can run `trinetra` from anywhere on the box, not just from
wherever you left the downloaded binary. The install step copies the binary to
`/usr/local/bin/trinetra` and also drops a symlink at `/usr/bin/trinetra`.
That second path exists so `sudo trinetra ...` resolves on every distro (more
on why in the troubleshooting section below). So these both work regardless of
your current directory:

```bash
trinetra status                 # current snapshot, no root needed to read
sudo trinetra config get        # effective config
```

## Updating

The recommended way to upgrade is `trinetra update`: it fetches a release,
verifies it against two independent signatures, and only ever swaps it in
once a health check has confirmed the newly restarted daemon actually works
-- automatically rolling back otherwise. You do not stop the service, edit
the unit, or touch your config and history; all of that is preserved. See
[Configuration: Self-update settings](04-configuration.md#self-update-settings)
for the `update.*` keys that control where it looks and how often.

```bash
sudo trinetra update check              # is a newer release available?
sudo trinetra update apply              # fetch, verify, install, and guard it
trinetra update status                  # what's running, pending, and what happened last
sudo trinetra update rollback           # go back to the previously installed build
```

### What `apply` actually does

1. **Fetches** the target release: the configured channel's newest release
   (`update.source=github`, the default) or, with `--bundle DIR`, a release
   bundle already on disk -- a directory holding `manifest.json`,
   `manifest.ci.sig`, `manifest.maint.sig` and the binaries, exactly what a
   release's assets look like downloaded into one folder.
2. **Verifies both signatures** in the manifest against the release keys
   compiled into this binary (`trinetra update status --json`'s
   `keys_loaded`/`fingerprints` say whether any are loaded, and which): one
   from the CI pipeline that built the release, one from a maintainer who
   reviewed and co-signed it. Either signature missing, wrong, or not
   matching a trusted key refuses the update outright, before anything is
   staged.
3. **Checks policy**: the release's channel must be one yours accepts (a
   `stable` host takes only `stable` releases; a `beta` host takes `beta`
   and `stable` ones, so it moves on to each final release too), its version
   must not be lower than this host's floor (the highest version it has ever
   successfully run -- the floor never goes down, even across a rollback),
   and it must not be a version this host already tried and marked bad.
4. **Stages and re-verifies** every binary file's size and SHA-256 against
   the signed manifest as it downloads, then **smoke-tests** the staged core
   binary (`trinetra version --json`) before touching anything installed.
5. **Swaps it in** atomically -- the same rename-based, live-file-safe,
   fsynced swap `install` uses. Before the first binary is replaced it keeps
   a copy of the current build in `update/previous/`, copies the binary
   doing the apply to the **pinned guard** path
   `/usr/local/lib/trinetra/guard/trinetra`, and records the pending update
   on disk, so a crash at any point of the swap is recoverable. It then
   launches a background **health guard** (a transient
   `trinetra-update-guard` unit running the pinned guard, never the new
   build): it restarts the daemon onto the new build and polls, for up to 90
   seconds, whether the unit is active, reports the expected version, and
   has produced a fresh sample.
   - **Healthy**: the guard raises the floor to the new version and clears
     the pending marker. Nothing else to do.
   - **Not healthy in time**: the guard restores the previous build,
     restarts onto it, marks the failed version bad (a plain re-`apply` of it
     is then refused; `--force` overrides that), and sends a critical alert.
     Your data, config, and the floor are untouched.

#### The update watchdog

`install` also sets up a small persistent safety net:
`trinetra-update-watchdog.timer` fires 2 minutes after boot and then every
minute, running `trinetra-update-watchdog.service`, a oneshot that executes
the pinned guard with `update guard --if-pending`. When nothing is pending,
or a guard is already at work (it holds `update/guard.lock`), it exits at
once and does nothing. Otherwise it finishes the job:

- a guard that was killed, or a host that rebooted or lost power inside the
  90-second window: it restarts onto the pending build and runs the health
  check again (commit if healthy, roll back if not);
- an `apply` that died half-way through the swap (the pending update is
  recorded as `swapping` and no apply holds `update/apply.lock` any more):
  it restores the previous build and restarts onto it;
- a `rollback` whose restore was interrupted: it finishes the restore and
  confirms it through the same health check.

Because the watchdog runs the pinned guard, recovery never depends on the new
build being able to start -- a release whose daemon exits at once is rolled
back even if its guard was killed. `update apply` re-creates the timer if it
is missing before it swaps anything. Check it with:

```bash
systemctl status trinetra-update-watchdog.timer
journalctl -u trinetra-update-watchdog -u 'trinetra-update-guard*'
```

Only one apply, rollback or `install` runs at a time on a host
(`update/apply.lock`); a second one -- from the CLI, the web UI or another
shell -- is refused with "an update is already in progress". `install` also
refuses while an update is pending: wait until `trinetra update status`
shows it confirmed or rolled back.

Apply and rollback starts, and the guard's commit or rollback, are recorded
in `/var/lib/trinetra/update/audit.jsonl` (or, on a fleet master, in the
fleet audit log) with who asked: `cli:<user>`, `socket` (web UI or
`trinetra-ctl`) or `guard`.

`update apply --version X.Y.Z` installs an exact version instead of the
channel's latest (still gated by the floor and the release's own minimum
upgrade version); `--channel beta` tries the beta channel for this one apply
without changing `update.channel`; `--force` retries a version this host
previously marked bad.

The daemon also checks the configured channel on its own, every
`update.check_interval` (default 24h), and alerts when a new version becomes
available (only one this host would accept: right channel, above the floor,
not marked bad) and when a background `apply` (yours, or a scripted one)
commits or rolls back -- so `update check` is for "right now", not something
you need to run on a timer yourself. `trinetra update status` shows when the
last successful check ran, and the Telegram `/version` command replies with
the running version plus "update available: X" when there is one.

It also warns when the channel looks **frozen** -- the signal that someone
may be withholding updates -- but only once this host has verified a signed
channel pointer at least once; freeze detection has nothing to compare
against before that. Once it has: the warning fires at once if the signed
channel pointer has expired or is missing (including a pointer this host
used to see going missing later, e.g. `update.github_token` is removed from
a private repo), and otherwise once the newest pointer it has seen is more
than 14 days old, whatever the reason. A plain network outage alone does not
warn until those 14 days have passed. The warning is sent once per episode
and resets when a fresh pointer arrives.

Before the first pointer ever verifies -- a fresh install still pointed at
`update.source=github` with a private release repo and no
`update.github_token`, say -- a failing check is logged once per distinct
cause instead of warning, and `trinetra update status` shows it as the
check error, so an unconfigured host is diagnosable without paging anyone.

### Coming from an unsigned/manual binary

If you would rather not rely on `update.source=github` -- an air-gapped host,
or a release you built yourself -- download or build the three binaries plus
`manifest.json`/`manifest.ci.sig`/`manifest.maint.sig` into one directory and
point `apply` at it:

```bash
sudo trinetra update apply --bundle /tmp/trinetra-0.6.0
```

This runs the exact same verify/stage/smoke-test/swap/guard pipeline as a
network `apply`; only where the bytes came from differs. `trinetra install`
(no `update` prefix) remains the lower-level, unguarded path -- an atomic
binary swap plus a plain `systemctl restart`, no health check or automatic
rollback -- used for the very first install and for the one-time
`serverwatch` migration described below; add `--require-signed` to make it
refuse an unsigned bundle the same way `update apply` always does (see
[Installation: verify what you downloaded](03-installation.md#option-a-prebuilt-release-asset)).

Coming from a `serverwatch` install for the first time is a different,
one-time path — the same `install` command detects it and migrates config,
state, and fleet identity into the new `trinetra` paths automatically. See
[Upgrading from a serverwatch install](03-installation.md#upgrading-from-a-serverwatch-install).

## Uninstalling

To remove the service cleanly:

```bash
sudo trinetra uninstall
```

This disables and stops the unit (`systemctl disable --now trinetra`),
removes `/etc/systemd/system/trinetra.service`, disables and removes the
update watchdog timer and service and the pinned guard under
`/usr/local/lib/trinetra/guard`, reloads systemd, and deletes the
`/usr/bin/trinetra` symlink. It removes that symlink only when it is
still the one `install` created pointing back into `/usr/local/bin`, so it will
never delete a real distro-provided binary that happened to share the name.

By default `uninstall` leaves your data and configuration in place, so you can
reinstall later and pick up where you left off. Everything under
`/var/lib/trinetra` (status, time-series history, downtime events) and the
config at `/etc/trinetra/config.json` stays put.

To wipe those too, add `--purge`:

```bash
sudo trinetra uninstall --purge
```

That additionally deletes the entire state directory and the config file,
secrets included. Use it when you are done with the host for good.

## Migrating legacy data

Older, pre-storage-epic versions of `trinetra` wrote their history as
per-day JSONL files (`samples/YYYY-MM-DD.jsonl`) plus a `downtime.jsonl` log,
rather than the compact time-series store under `ts/`. If you are upgrading from
one of those, a one-time import pulls that old history into the new store so
your graphs and downtime reports stay continuous across the upgrade:

```bash
sudo trinetra migrate
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
sudo trinetra migrate --force
```

## Release keys and releasing (maintainers only)

This section is for whoever cuts trinetra releases, not for operators
running it. Every release ships a `manifest.json` naming its files' exact
sizes and SHA-256 hashes, plus two detached signatures over that manifest --
one from CI (automatic), one from a maintainer (a deliberate human step) --
which is what `trinetra update`/`install --require-signed` verify before
trusting anything. See [internal/update](../../internal/update) for the
verification code, [cmd/trinetra-release](../../cmd/trinetra-release) for
the tooling below, and [Security](14-security.md) for the trust model, the
published public keys and what each key's compromise would give an attacker.

### One-time key ceremony

Three ed25519 key pairs, each a distinct role:

```bash
trinetra-release keygen --role ci      --out ci.key       # seed file, paste into a secret
trinetra-release keygen --role pointer --out pointer.key  # seed file, paste into a secret
trinetra-release keygen --role maint   --out maint.key    # passphrase-encrypted; keep this one offline
```

`keygen` prints each key's public half and fingerprint; the private halves
never touch stdout. `--role ci` and `--role pointer` write a plain base64
seed, meant to live only as a GitHub Actions secret; `--role maint` prompts
for a passphrase and writes a scrypt+XChaCha20-Poly1305-encrypted envelope,
meant to live on a maintainer's own machine, never in CI.

Each role has a **current** and a **next** key, so the ceremony makes six
key pairs (run `keygen` twice per role). Wire the six public keys into this
repo's `internal/update/keys.go` (`productionKeyB64`, current first). A build
with no keys fails closed with "no release keys compiled in", by design. The
production keys are in place (see
[Security: The published release keys](14-security.md#the-published-release-keys));
`go run ./cmd/trinetra-release fingerprints` prints their fingerprints. Only
the **current** CI and pointer seeds go into GitHub; the next seeds are not
stored there until a rotation needs them.

The two seeds live in two GitHub Actions **environments**, each limited in
what may deploy to it:

| Environment | Deployment restricted to | Secret | Used by |
| --- | --- | --- | --- |
| `release` | tags matching `v*` | `TRINETRA_CI_SIGNING_KEY` (the current ci key's seed) | `.github/workflows/release.yml`, on every `vX.Y.Z` tag push: builds, embeds the production keys (`scripts/release-check-keys.sh` fails the build if they are missing or still the test keys), signs the manifest, and opens a draft release |
| `channels` | the `main` branch | `TRINETRA_POINTER_SIGNING_KEY` (the current pointer key's seed) | `.github/workflows/channels.yml`, weekly and on every publish: signs `stable.json`/`beta.json`, the pointers `trinetra update check` reads |

Neither environment has a required reviewer while the repository is private
(GitHub does not offer environment reviewers for it on the current plan), so
the maintainer's offline co-signature in step 2 below is the human approval
gate. Add the maintainer as a required reviewer on `release` when the
repository goes public. The deployment restrictions already mean a run from
any other ref -- a feature branch, or a `workflow_dispatch` from one -- can
never reach either signing key.

For the same private-repo-plan reason, `release.yml`'s
`actions/attest-build-provenance` step (build-provenance attestations) is
conditioned on the repo being public and is skipped, not failed, until then.
Both the required-reviewer gate above and build-provenance attestations turn
on at the same launch moment, when the repository goes public -- see
[Security: Honest limits](14-security.md#honest-limits).

### Publishing a release

1. Push a tag `vX.Y.Z` (or `vX.Y.Z-beta.N` for a beta prerelease).
   `release.yml` builds the linux release matrix, generates and CI-signs
   `manifest.json`, and opens a **draft** GitHub release -- unpublished, so
   nothing downloads it yet.
2. A maintainer reviews the draft and co-signs it:

   ```bash
   trinetra-release cosign vX.Y.Z --key maint.key
   ```

   This downloads the draft's manifest and CI signature, verifies the CI
   signature, shows exactly what is about to be signed (version, channel,
   every file's size and hash, and `keys: unchanged` -- or a prominent
   **KEY ROTATION** block if the release changes the keys hosts will trust:
   `release.yml` fills the manifest's `keys` from the key set compiled into
   the release, so this only appears when keys really changed), requires
   retyping the version on the actual
   terminal, asks for the maintainer key's passphrase, signs, uploads
   `manifest.maint.sig`, re-verifies the complete signed release, and only
   then publishes it.
3. `channels.yml` runs on every published release, every Monday, and on
   demand, and signs fresh `stable.json`/`beta.json` pointers naming
   the highest version on each channel, uploaded to the `channels` release.
   Hosts on that channel see it on their next `update check`. `beta.json`
   names the newest release of either kind, so beta hosts also move to each
   final release. Pointers expire 14 days after they are issued, so the
   weekly run is what keeps a quiet channel from looking frozen.

   A run triggered by the publish event runs against the release's tag, and
   the `channels` environment allows both the `main` branch and `v*` tags, so
   this run goes through automatically right after publishing -- no manual
   step required. If you want to refresh the pointers sooner (or re-run after
   a failure) you can always trigger it by hand:

   ```bash
   gh workflow run channels.yml --ref main
   ```

Nothing here ever needs a repo secret on a maintainer's own machine: the CI
key lives only in the `release` environment, the pointer key only in
`channels`, and the maintainer key never leaves whoever holds it.

## Troubleshooting

When something is not behaving, start with the built-in diagnostic before
anything else:

```bash
sudo trinetra doctor
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

**`sudo trinetra` says command not found.** This only happens on older or
minimal distributions (RHEL and CentOS 7 family, some stripped-down images)
whose sudo `secure_path` does not include `/usr/local/bin`. The install step
handles it by also linking the binary into `/usr/bin`, which is on
`secure_path` everywhere. If you hit this, you are almost certainly on a host
where the `/usr/bin/trinetra` symlink failed to be created (`install` prints
a warning when it cannot make it, but keeps going because the service itself
runs fine off its absolute `ExecStart`). Re-running `sudo /usr/local/bin/trinetra install`
recreates the link.

**The control socket.** The daemon serves its control API over a Unix socket at
`/run/trinetra/control.sock` (see [Architecture](02-architecture.md)). It is mode `0600` and owned by root, so only
root can talk to it, and it is created for you by systemd, not the daemon: the
unit sets `RuntimeDirectory=trinetra`, which makes systemd create
`/run/trinetra` before the service starts and clean it up when the service
stops. If the socket is missing, the daemon is not running; check
`systemctl status trinetra`.

**`trinetra cli` / `trinetra web` refuses to run, warning about
tampering.** Both are front-doors that exec the companion plugin binaries as
root, so before handing off control the core verifies the plugin's owner,
permissions, and SHA-256 against the install-time manifest at
`/var/lib/trinetra/plugins.json` (see [Architecture](02-architecture.md)
and [Command reference](11-command-reference.md)). A refusal means one of
those checks failed. If you just rebuilt or hand-copied `trinetra-ctl` or
`trinetra-web` into place yourself, this is expected: run `sudo trinetra
install` to record its checksum, then try again. If you did not touch the
plugin binary, do not just re-run install to make the warning go away;
investigate first, since it means something replaced or modified the file
since the last install.

**The web UI would not start.** (See [The web UI](08-web-ui.md).) If you are running the `trinetra-web` binary
with `web.enabled true` and the web server fails to come up (a bad TLS config, a
port already in use), that failure is logged to the journal but never stops
monitoring. The daemon keeps sampling and alerting over Telegram regardless.
Look for the error with `journalctl -u trinetra` and fix the `web.*` config,
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
