# The trinetra handbook

trinetra is a home-server monitor: a single Go binary that watches a
Linux/systemd host and reports over Telegram, with an optional web UI. This
handbook is the full guide, organized as a book you can read start to finish or
dip into by chapter.

New here? Read the [Introduction](01-introduction.md), then
[Installation and first run](03-installation.md).

Every chapter ends with previous/next links, so you can read the handbook
straight through and return here from any page.

## Contents

1. [Introduction](01-introduction.md) - what trinetra is and the ideas behind it.
2. [Architecture](02-architecture.md) - the daemon, the tiered sampler, the core and its control socket, and the systemd unit.
3. [Installation and first run](03-installation.md) - prerequisites, install, verify, and Telegram enrollment.
4. [Configuration](04-configuration.md) - the trinetra-ctl-first configuration model.
   - [Advanced configuration and management](advanced-configuration.md) - the low-level, scriptable reference and the full config-key reference behind the ctl screens.
5. [Monitoring: what gets collected](05-monitoring.md) - the fast and slow tiers, self-discovery, and monitor targets.
6. [Alerting and notification channels](06-alerting-and-channels.md) - the anomaly engine, routing, and the seven channel types.
7. [Downtime and liveness](07-downtime-and-liveness.md) - heartbeats, power-down reconstruction, and the safety nets.
8. [The web UI](08-web-ui.md) - the two deployment shapes, serving modes, passkeys, and the public page.
9. [Storage and the data model](09-storage-and-data-model.md) - the time-series store, the live snapshot, and the event log.
10. [Operations](10-operations.md) - managing, upgrading, uninstalling, migrating, and troubleshooting.
11. [Command reference](11-command-reference.md) - every `trinetra` command, plus the plugin binaries.
12. [Roadmap and status](12-roadmap-and-status.md) - what is released, what is in progress, and what is still coming.
13. [Fleet mode](13-fleet.md) - concepts, setup, incidents, routing and policies, silences, managed config, audit, and day-to-day fleet operations.
14. [Security](14-security.md) - the trust model, signed releases (CI + maintainer co-signature), safe self-update with automatic rollback, verifying a download by hand, and every other security layer.

### Plugins

- [Plugins](plugins/README.md) - the lean core plus optional plugin binaries, and the front-door safe-exec model.
  - [trinetra-ctl](plugins/trinetra-ctl.md) - the interactive management TUI.
  - [trinetra-web](plugins/trinetra-web.md) - the supervised web UI binary.

## A note on the plugins

The `trinetra-ctl` and `trinetra-web` plugin binaries, and the control
socket they talk to, are part of an in-progress core-plus-plugin architecture.
They each have a dedicated page under [Plugins](plugins/README.md):
[trinetra-ctl](plugins/trinetra-ctl.md) and
[trinetra-web](plugins/trinetra-web.md).
[trinetra-ctl](plugins/trinetra-ctl.md) is a complete, supported tool and
the primary interactive way to manage a running trinetra: every config key is
reachable through its screens, including a generic all-settings screen, and its
web-setup wizard is functional in every serving mode.
[trinetra-web](plugins/trinetra-web.md) is a supervised, separate binary
with no build tag, and the daemon runs it whenever `web.enabled` is set. The
current stable release is v0.5.0, built on the v0.4.x core-plus-plugin line (see
[Roadmap and status](12-roadmap-and-status.md)), and both plugins are
feature-complete for what they do.
