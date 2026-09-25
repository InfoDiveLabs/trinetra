# Plugins

trinetra is built as a lean, standard-library-only core plus a small set of
optional plugin binaries. The core (`trinetra`) is both the daemon and the
operator CLI; it depends on nothing beyond the Go standard library. The
plugins are separate binaries that carry their own, heavier dependencies and
dial the daemon's control socket rather than reading state in-process. Keeping
them out of the core is what lets the daemon stay small and dependency-free.

There are two plugins today:

- **[trinetra-ctl](trinetra-ctl.md)** -- the interactive management TUI.
  It is the primary, recommended way to manage a running trinetra day to
  day: guided, validated screens for the schedule, quiet hours, healthchecks,
  monitor thresholds, and notification channels, plus a first-run Telegram
  onboarding flow. It is a complete, supported tool: every config key is
  reachable through its screens, including a generic all-settings screen.
- **[trinetra-web](trinetra-web.md)** -- the web UI. A plain separate
  binary that the daemon supervises (verify, spawn, restart, stop) whenever
  `web.enabled` is set, or that you launch directly via `trinetra web`.

## Installing plugins

Install is a one-step process. The default path is to download the prebuilt
`trinetra`, `trinetra-ctl`, and `trinetra-web` binaries from the
releases page into the same directory (building them from source is the
secondary option), then run `sudo ./trinetra install` once. `trinetra
install`
copies any plugin binary it finds next to the source `trinetra` binary
into `/usr/local/bin` alongside the daemon, then records the SHA-256 of each
one it just copied into a root-only install manifest, so the core can later
verify it is about to run the genuine binary it installed. A plugin that is
not present next to the source binary is simply skipped, not an error; the
daemon itself still installs.

If you build or hand-copy a plugin straight into `/usr/local/bin` yourself
instead, bypassing the copy step above, re-run `trinetra install`
afterward so its checksum is recorded; until then the front-door has nothing
to verify the binary against and refuses to run it.

## The front-door and safe-exec model

You do not need to know the plugin binary names or where they live. The core
exposes two front-door subcommands that launch them for you:

- `trinetra cli` -- verify and exec `trinetra-ctl`.
- `trinetra web` -- verify and exec `trinetra-web`.

Both are typically run with `sudo`, because that is how the daemon itself
runs. Launching a plugin as root means the core must first prove it is about
to exec the genuine, unmodified binary it installed, not something an attacker
planted or altered. Three checks all have to pass before it execs:

1. **Absolute path from the core's own directory.** The plugin is resolved as
   `trinetra-<name>` in the same directory as the running core binary
   (symlinks resolved), never looked up via `$PATH`.
2. **Owner and permissions.** The plugin file and its parent directory must be
   owned by root (uid 0) or by the core binary's owner, and neither may be
   group- or world-writable.
3. **Checksum against the install manifest.** The core recomputes the plugin's
   SHA-256 and requires it to match the root-only manifest that `trinetra
   install` recorded.

A missing binary yields an install/build instruction; a failed owner,
permission, or checksum check is refused with a tampering warning. A mismatch
is refused, not run. Nothing is exec'd in either failing case.

For the full trust model -- the exact path resolution, the owner and
permission rules, the manifest format, and the supervisor that reuses the same
check for the web plugin -- see [the front-door safe-exec trust model in
Architecture](../02-architecture.md#the-front-door-safe-exec-trust-model).

---

[Handbook index](../README.md) | [trinetra-ctl](trinetra-ctl.md) | [trinetra-web](trinetra-web.md)
