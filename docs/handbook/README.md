# The serverwatch handbook

serverwatch is a home-server monitor: a single Go binary that watches a
Linux/systemd host and reports over Telegram, with an optional web UI. This
handbook is the full guide, organized as a book you can read start to finish or
dip into by chapter.

New here? Read the [Introduction](01-introduction.md), then
[Installation and first run](03-installation.md).

Every chapter ends with previous/next links, so you can read the handbook
straight through and return here from any page.

## Contents

1. [Introduction](01-introduction.md) - what serverwatch is and the ideas behind it.
2. [Architecture](02-architecture.md) - the daemon, the tiered sampler, the core and its control socket, and the systemd unit.
3. [Installation and first run](03-installation.md) - prerequisites, install, verify, and Telegram enrollment.
4. [Configuration](04-configuration.md) - the serverwatch-ctl-first configuration model.
   - [Advanced configuration and management](advanced-configuration.md) - the low-level, scriptable reference and the full config-key reference behind the ctl screens.
5. [Monitoring: what gets collected](05-monitoring.md) - the fast and slow tiers, self-discovery, and monitor targets.
6. [Alerting and notification channels](06-alerting-and-channels.md) - the anomaly engine, routing, and the seven channel types.
7. [Downtime and liveness](07-downtime-and-liveness.md) - heartbeats, power-down reconstruction, and the safety nets.
8. [The web UI](08-web-ui.md) - the two deployment shapes, serving modes, passkeys, and the public page.
9. [Storage and the data model](09-storage-and-data-model.md) - the time-series store, the live snapshot, and the event log.
10. [Operations](10-operations.md) - managing, upgrading, uninstalling, migrating, and troubleshooting.
11. [Command reference](11-command-reference.md) - every `serverwatch` command, plus the plugin binaries.
12. [Roadmap and status](12-roadmap-and-status.md) - what is released, what is in preview, and what is still coming.

### Plugins

- [Plugins](plugins/README.md) - the lean core plus optional plugin binaries, and the front-door safe-exec model.
  - [serverwatch-ctl](plugins/serverwatch-ctl.md) - the interactive management TUI.
  - [serverwatch-web](plugins/serverwatch-web.md) - the supervised web UI binary.

## A note on the plugins

The `serverwatch-ctl` and `serverwatch-web` plugin binaries, and the control
socket they talk to, are part of an in-progress core-plus-plugin architecture.
They each have a dedicated page under [Plugins](plugins/README.md):
[serverwatch-ctl](plugins/serverwatch-ctl.md) and
[serverwatch-web](plugins/serverwatch-web.md).
[serverwatch-ctl](plugins/serverwatch-ctl.md) is a complete, supported tool and
the primary interactive way to manage a running serverwatch: every config key is
reachable through its screens, including a generic all-settings screen, and its
web-setup wizard is functional in every serving mode.
[serverwatch-web](plugins/serverwatch-web.md) is a supervised, separate binary
with no build tag, and the daemon runs it whenever `web.enabled` is set. The
overall v0.4 core-plus-plugin release line is still a develop-branch beta
preview (see [Roadmap and status](12-roadmap-and-status.md)), but both plugins
are feature-complete for what they do.
