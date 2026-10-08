# Configuration

The default way to configure trinetra is the interactive `trinetra-ctl`
TUI: guided, validated screens for the schedule, quiet hours, healthchecks,
monitor thresholds, and notification channels, plus first-run Telegram
onboarding, and a generic **all settings** screen that reaches every
remaining flat config key. Start it with `sudo trinetra cli`. Every
setting has a screen there, so there is no key you need `trinetra config
set` for, and each change is validated and applied over the control socket in
one step.

This chapter is the trinetra-ctl-first guide to configuration. The
underlying configuration model, the scriptable CLI verbs, and the full
config-key reference now live on their own page,
[Advanced configuration and management](advanced-configuration.md), for
automation, cron, or a headless box managed without `trinetra-ctl`.

## Managing configuration with trinetra-ctl

Run `sudo trinetra cli` and press `m` from the Home screen to open the
management menu. Each entry is a guided screen that fetches the current config,
lets you edit it with validation, and applies it atomically over the control
socket:

| Setting | trinetra-ctl screen |
| --- | --- |
| Digest schedule (daily / weekly / off) | Schedule |
| Quiet-hours window | Quiet hours |
| Healthchecks ping URL | Healthchecks |
| Per-target enable/disable and thresholds | Monitor thresholds |
| Notification channels (add / edit / remove / test) | Channels |
| Web UI setup (mode, listen, domain, `rp_id`, origin, and manual-mode certs) | the `s` guided web-setup wizard from Home |
| Telegram bot token and enrollment | first-run onboarding |
| Sampling intervals, baseline/anomaly tuning, global thresholds, `critical_overrides_quiet`, storage backend/retention, collection toggles, the remaining `web.*` keys, and `public.*` | All settings |

The Channels screen validates an enabled channel over the socket before it
saves, so a channel that would fail to deliver can never be persisted while
turned on. The All settings screen is the generic catch-all: pick a group,
then a key, and it shows the CURRENT value alongside a one-line description;
editing a key applies through the exact same validated setter the dedicated
screens above use, so a rejected value is never persisted, and keys that only
take effect after a restart (`storage.*`, `web.enabled`, `web.listen`) carry
that caveat. For the full screen-by-screen walkthrough, see [Managing with
trinetra-ctl](plugins/trinetra-ctl.md#managing-with-trinetra-ctl).

For automation, scripting, or a box without `trinetra-ctl`, every one of
these settings is also a scriptable `trinetra` CLI verb, documented in
[Advanced configuration and management](advanced-configuration.md) and
collected in [Daemon-only config
management](11-command-reference.md#3-daemon-only-config-management).

## Naming this host

By default trinetra identifies the host by its system hostname: the name
shows in the web panel's sidebar brand and is prefixed onto every outbound alert
(for example `[attic-pi] disk:/ = 91.0`) so a setup with several monitored hosts
tells you at a glance which one fired. Override it with `server.name`:

```bash
sudo trinetra config set server.name attic-pi
sudo trinetra config unset server.name   # back to the system hostname
```

Setting it to an empty value is the same as unsetting it: the effective name
falls back to the hostname, so you never have to hardcode one.

## Fleet child timing keys

Two child-only keys tune the alert-handoff timing described in [Fleet
alerting](06-alerting-and-channels.md#lease-receipt-and-local-fallback). Both
live under the trinetra-ctl All settings screen's Fleet group; there is no
dedicated wizard for them, since they are only worth touching if the
defaults do not fit your network:

| Key | Default | Minimum | Meaning |
| --- | --- | --- | --- |
| `fleet.fallback_after` | `2m` | `5s` | How long a child waits for the master's delivery receipt on a routed alert before delivering it locally instead ("via local fallback: master unreachable"). |
| `fleet.link_down_warn_after` | `10m` | `30s` | How long the link to the master must be down before the child raises its own local "fleet link down" warning alert. |

```bash
sudo trinetra config set fleet.fallback_after 90s
sudo trinetra config set fleet.link_down_warn_after 5m
```

Both apply live: no restart or SIGHUP special-case is needed beyond the
usual persist-and-reload every `config set` already does. See [Fleet
mode](02-architecture.md#fleet-mode) for where these two keys sit among the
rest of the fleet configuration.

## Status page keys

These tune the [public status page](08-web-ui.md#public-status-page). All three
apply live, and the page itself only appears on a standalone host or a fleet
master:

| Key | Default | Meaning |
| --- | --- | --- |
| `status.title` | `Status` | Heading shown on the public page. |
| `status.auto_resolve_after` | `24h` | When an automatic incident resolves itself after recovery. `0` never resolves; the minimum is `1h`. |
| `status.echo_channels` | none | Comma-separated channels that also receive each new incident update. |

```bash
sudo trinetra config set status.title "Acme status"
sudo trinetra config set status.auto_resolve_after 12h
sudo trinetra config set status.echo_channels telegram,slack
```

## Self-update settings

`sudo trinetra update check` / `apply` (see [Operations: Updating](10-operations.md#updating))
look for and install new signed releases. Four keys, all under the
trinetra-ctl All settings screen's Updates group, control it; there is no
dedicated wizard screen, since most hosts never need to touch them:

| Key | Default | Meaning |
| --- | --- | --- |
| `update.channel` | `stable` | `stable`, `beta`, or `off`. A `stable` host installs only stable releases; a `beta` host installs beta and stable releases (whichever is newest). `off` refuses `update check`/`update apply` against the network; an explicit `--bundle DIR` install still works. |
| `update.source` | `github` | `github` or `none`. `none` disables the network source entirely; only `--bundle DIR` installs are possible. |
| `update.github_token` | (unset) | Read-only token for the release repo, only needed while it is private. A secret key: `config get` always shows `(set)` / `(not set)`, never the raw value. |
| `update.check_interval` | `24h` | How often the daemon checks the configured channel for a new release. Minimum `1h`; anything lower is rejected. |

```bash
sudo trinetra config set update.channel beta
sudo trinetra config set update.github_token ghp_xxxxxxxxxxxx
sudo trinetra config set update.check_interval 6h
```

## Where to go next

Everything below the trinetra-ctl screens -- the config file model, `config
get`/`set`/`unset`, the dedicated scriptable verbs, persistence and hot reload,
per-target overrides, and the full config-key reference -- lives on
[Advanced configuration and management](advanced-configuration.md). Reach for
that page when you are scripting a change or managing a headless box; day to
day, the screens above set all of it for you.

---

[Previous: Installation and first run](03-installation.md) | [Handbook index](README.md) | [Next: Monitoring: what gets collected](05-monitoring.md)
