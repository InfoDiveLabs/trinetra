# Configuration

The default way to configure serverwatch is the interactive `serverwatch-ctl`
TUI: guided, validated screens for the schedule, quiet hours, healthchecks,
monitor thresholds, and notification channels, plus first-run Telegram
onboarding, and a generic **all settings** screen that reaches every
remaining flat config key. Start it with `sudo serverwatch cli`. Every
setting has a screen there, so there is no key you need `serverwatch config
set` for, and each change is validated and applied over the control socket in
one step.

This chapter is the serverwatch-ctl-first guide to configuration. The
underlying configuration model, the scriptable CLI verbs, and the full
config-key reference now live on their own page,
[Advanced configuration and management](advanced-configuration.md), for
automation, cron, or a headless box managed without `serverwatch-ctl`.

## Managing configuration with serverwatch-ctl

Run `sudo serverwatch cli` and press `m` from the Home screen to open the
management menu. Each entry is a guided screen that fetches the current config,
lets you edit it with validation, and applies it atomically over the control
socket:

| Setting | serverwatch-ctl screen |
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
serverwatch-ctl](plugins/serverwatch-ctl.md#managing-with-serverwatch-ctl).

For automation, scripting, or a box without `serverwatch-ctl`, every one of
these settings is also a scriptable `serverwatch` CLI verb, documented in
[Advanced configuration and management](advanced-configuration.md) and
collected in [Daemon-only config
management](11-command-reference.md#3-daemon-only-config-management).

## Where to go next

Everything below the serverwatch-ctl screens -- the config file model, `config
get`/`set`/`unset`, the dedicated scriptable verbs, persistence and hot reload,
per-target overrides, and the full config-key reference -- lives on
[Advanced configuration and management](advanced-configuration.md). Reach for
that page when you are scripting a change or managing a headless box; day to
day, the screens above set all of it for you.

---

[Previous: Installation and first run](03-installation.md) | [Handbook index](README.md) | [Next: Monitoring: what gets collected](05-monitoring.md)
