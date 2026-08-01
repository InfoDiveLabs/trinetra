# Plugins

serverwatch is built as a lean, standard-library-only core plus a small set of
optional plugin binaries. The core (`serverwatch`) is both the daemon and the
operator CLI; it depends on nothing beyond the Go standard library. The
plugins are separate binaries that carry their own, heavier dependencies and
dial the daemon's control socket rather than reading state in-process. Keeping
them out of the core is what lets the daemon stay small and dependency-free.

There are two plugins today:

- **[serverwatch-ctl](serverwatch-ctl.md)** -- the interactive management TUI.
  It is the primary, recommended way to manage a running serverwatch day to
  day: guided, validated screens for the schedule, quiet hours, healthchecks,
  monitor thresholds, and notification channels, plus a first-run Telegram
  onboarding flow. Still marked beta.
- **[serverwatch-web](serverwatch-web.md)** -- the web UI. A plain separate
  binary that the daemon supervises (verify, spawn, restart, stop) whenever
  `web.enabled` is set, or that you launch directly via `serverwatch web`.

## Installing plugins

Plugins install alongside the daemon. `serverwatch install` records the
SHA-256 of each companion binary it finds next to the core into a root-only
install manifest, so the core can later verify it is about to run the genuine
binary it installed. If you build or hand-copy a plugin into place yourself,
re-run `serverwatch install` afterward so its checksum is recorded; until then
the front-door has nothing to verify the binary against and refuses to run it.

## The front-door and safe-exec model

You do not need to know the plugin binary names or where they live. The core
exposes two front-door subcommands that launch them for you:

- `serverwatch cli` -- verify and exec `serverwatch-ctl`.
- `serverwatch web` -- verify and exec `serverwatch-web`.

Both are typically run with `sudo`, because that is how the daemon itself
runs. Launching a plugin as root means the core must first prove it is about
to exec the genuine, unmodified binary it installed, not something an attacker
planted or altered. Three checks all have to pass before it execs:

1. **Absolute path from the core's own directory.** The plugin is resolved as
   `serverwatch-<name>` in the same directory as the running core binary
   (symlinks resolved), never looked up via `$PATH`.
2. **Owner and permissions.** The plugin file and its parent directory must be
   owned by root (uid 0) or by the core binary's owner, and neither may be
   group- or world-writable.
3. **Checksum against the install manifest.** The core recomputes the plugin's
   SHA-256 and requires it to match the root-only manifest that `serverwatch
   install` recorded.

A missing binary yields an install/build instruction; a failed owner,
permission, or checksum check is refused with a tampering warning. A mismatch
is refused, not run. Nothing is exec'd in either failing case.

For the full trust model -- the exact path resolution, the owner and
permission rules, the manifest format, and the supervisor that reuses the same
check for the web plugin -- see [the front-door safe-exec trust model in
Architecture](../02-architecture.md#the-front-door-safe-exec-trust-model).

---

[Handbook index](../README.md) | [serverwatch-ctl](serverwatch-ctl.md) | [serverwatch-web](serverwatch-web.md)
