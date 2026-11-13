# Permissions

A plugin can only do what its manifest requests and the operator approved. The core gives each plugin its own token whose method allowlist is exactly its granted permissions, and the sandbox is derived from the same list.

## Vocabulary (host_api 1)

| Permission | Consent line | Unlocks |
|---|---|---|
| `metrics.read` | can read this host's metrics and history | `Plugin.Snapshot`, `Plugin.Series` (`p.Snapshot()`, `p.Series()`) |
| `metrics.publish` | can add its own metrics to Trinetra's charts and history | `Plugin.PublishSeries` (`p.Publish`) |
| `alerts.read` | can see active and past alerts | `Plugin.ActiveAlerts` (`p.ActiveAlerts()`) |
| `alerts.raise` | can raise and resolve its own alerts through your notification channels | `Plugin.RaiseAlert`, `Plugin.ResolveAlert` |
| `docker.read` | can see your Docker containers and their resource use | `Plugin.Containers` (`p.Containers()`), container fields in `Snapshot` |
| `logs.read` | can read container logs, which may contain secrets | `Plugin.ContainerLogs` (`p.ContainerLogs()`, also needs `docker.read`) |
| `fleet.read` | can see the fleet's nodes and their status | `Plugin.FleetNodes` (`p.FleetNodes()`) |
| `net.outbound` | can make outbound network connections | Network access to other hosts |
| `net.listen` | can open a network port on this host | Binding a listener |

Calls that need no permission: `Plugin.Hello`, `Plugin.GetConfig`, `Plugin.WatchConfig`, `Plugin.ReportStatus`, `Plugin.Log`.

Reserved names `docker.control` and `exec` are held for a later release. Manifests that request them are refused.

A call outside the grant fails with `method "<name>" not permitted for this token`. Request the smallest set you need: the `prometheus` plugin asks for `metrics.read`, `net.listen` and `fleet.read`, and nothing else.

Container series in the Prometheus export appear only on a host where the operator also granted `docker.read`; the exporter works without it.

## Consent

- Install does not enable. Enabling shows each permission with its consent line.
- Registry plugins (verified: `plugin.json` signed with Trinetra's release keys by both CI and a maintainer): the permissions are listed and confirmed once.
- Community plugins (unverified): a risk warning first, then the operator acknowledges the risk. Administrators can turn unverified plugins off with the `plugins.allow_unverified` setting.
- An update that requests new permissions needs re-consent. Until then the plugin keeps running on its old grant.
- Install, enable, consent, config changes and disable are written to the plugin audit log.

CLI: `trinetra plugin enable <id>` prompts; the web Integrations page shows the same list.

## Sandbox

Each plugin runs as its own process, as a systemd service with a dynamic user:

- No new privileges, read-only system, no home directory, private `/tmp`, empty capability set, system call filter `@system-service`.
- A private state directory (`TRINETRA_PLUGIN_DATA_DIR`) is the only writable location.
- Defaults: 256M memory, 50% of one CPU, 64 tasks.
- Without `net.outbound` or `net.listen` the plugin gets Unix sockets only and all IP traffic is denied. With either, IPv4 and IPv6 are allowed.
- Without `net.listen`, binding sockets is denied.

Where systemd is not available, the runtime falls back to a child process running as the `trinetra-plugin` user with `no_new_privs` and resource limits: data memory capped at the plugin's memory limit, and a task ceiling of 512 shared by all plugins (they share one user, so per-plugin task and CPU limits need systemd). There is no filesystem or system call sandbox. Treat it as development only.

A plugin that serves its own endpoint, like `/metrics`, does so on its own listener under `net.listen` and must do its own authentication. The browser never reaches a plugin through `trinetra-web`; only JSON slot and action calls pass through.
