# Authoring a plugin

This guide builds a plugin the way `plugins/prometheus` is built. That plugin serves `GET /metrics` in Prometheus text format; it is small enough to read in one sitting and touches every part of the SDK you will normally need: config, status, host reads, a validator, tests.

You need Go and a Trinetra checkout. Plugins are Go programs built on `pkg/pluginsdk` and use only the standard library plus modules the repository already depends on.

## 1. Layout

```
plugins/prometheus/
  plugin.json                  manifest
  main.go                      wiring: SDK, config, validator
  server.go                    the HTTP listener and /metrics handler
  exposition.go                Prometheus text writer
  files/grafana-dashboard.json downloadable file declared in the manifest
  *_test.go
```

The directory name is the manifest `id`. To start from nothing, copy `registry/plugins/_template`.

## 2. The manifest

Declare what the plugin is, what it needs and what it shows. See the [manifest reference](manifest-reference.md) for every field.

```json
"permissions": ["metrics.read", "net.listen", "fleet.read"],
"config_schema": { "...": "listen, bearer_token, include_fleet" },
"contributes": { "slots": [{"slot": "settings.form", "id": "settings", "title": "Prometheus exporter", "min_role": "admin"}] }
```

Because `settings.form` is rendered by the host from `config_schema`, this plugin needs no UI code at all. The three settings become a form, the bearer token is a write-only `secret`, and `listen` carries a `pattern` so bad values are rejected before they reach you.

## 3. The program

A plugin is a `main` package. `pluginsdk.New` returns a `Plugin`; you register handlers, then `Run` connects to the daemon and blocks until the context ends.

```go
func newPlugin() (*pluginsdk.Plugin, *exporter) {
    p := pluginsdk.New(pluginsdk.Options{Version: version})
    exp := newExporter(p, func(state, msg string) { _ = p.ReportStatus(state, msg) })
    p.HandleValidateConfig(validateListen)
    p.OnConfig(func(c pluginsdk.Config) { exp.apply(settingsFrom(c)) })
    return p, exp
}

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    p, exp := newPlugin()
    defer exp.close()
    if err := p.Run(ctx); err != nil {
        log.Fatal(err)
    }
}
```

Returning the plugin from a constructor instead of building it inside `main` is what makes it testable.

### What the SDK gives you

| Call | Purpose | Permission |
|---|---|---|
| `OnConfig(f)` | Runs with the initial config and after every change. | none |
| `Config` accessors | `String`, `Int`, `Float`, `Bool`, `Duration`, `Strings`. Schema defaults are already applied. | none |
| `HandleValidateConfig(f)` | Extra validation before the host saves settings. | none |
| `ReportStatus(state, msg)` | `ok`, `degraded` or `error`, shown on the plugin's card. | none |
| `Logf(level, ...)` | Rate-limited log line to the host. | none |
| `HandleSlot`, `HandleAction`, `HandleEvent` | Answer UI, action and event calls. | none |
| `Snapshot()` | Live host metrics. | `metrics.read` |
| `Series(metric, from, to, res)` | Stored history for a metric. | `metrics.read` |
| `ActiveAlerts()` | Alerts active on this node. | `alerts.read` |
| `Containers()` | Docker containers with CPU and memory use. | `docker.read` |
| `ContainerLogs(name, lines)` | The last lines of a container's log. | `docker.read`, `logs.read` |
| `FleetNodes()` | Fleet roster on a master. | `fleet.read` |
| `Publish(ts, metrics)` | Add metrics to the store as `plugin.<id>.<name>`. | `metrics.publish` |
| `RaiseAlert`, `ResolveAlert` | Alerts through the core's routing. | `alerts.raise` |

Widget constructors (`Stat`, `Table`, `Timeseries`, ...) are covered in [Slots and widgets](slots-and-widgets.md). Every type a handler needs (`SlotRequest`, `Widget`, `ActionResult`, `DashboardView`, `Permission`, ...) is exported by `pkg/pluginsdk`, so a plugin in its own repository imports only `pkg/pluginsdk` and `pkg/pluginsdk/pluginsdktest`.

### Config changes

`OnConfig` is how the Prometheus plugin applies settings. `exporter.apply` keeps the listener when only the token or fleet flag changes and rebinds when `listen` changes, then reports `ok` with `serving on <addr>` or `error` with the reason. Settings arrive over a change stream, so no restart is needed.

### Reading from the host

The `/metrics` handler calls `p.Snapshot()` per scrape and turns the view into gauges. The exposition writer is a few dozen lines: `# HELP` and `# TYPE` per family, escaped label values (`\\`, `\"`, `\n`), `NaN` and `+Inf` spelled the way Prometheus expects. With `include_fleet` on, it also calls `p.FleetNodes()` and adds `trinetra_node_up{node,id}`.

A host call without the permission fails; handle the error. The exporter answers `503` if the snapshot is unavailable and serves host metrics alone if the fleet call fails.

### Serving your own port

A plugin that listens must hold `net.listen`, and must authenticate its own clients because the daemon does not sit in front of it. The exporter compares the bearer token in constant time (it hashes both sides first so the comparison length does not leak the secret's length).

## 4. Test it

Write the test first. `pkg/pluginsdk/pluginsdktest` is an in-memory fake of the host: it serves `Plugin.*`, enforces the permission allowlist you pass it, records what the plugin sends, and can call your slots and actions.

```go
h := pluginsdktest.New(t, "prometheus", manifest.Permissions, map[string]any{"listen": "127.0.0.1:0", "bearer_token": "tok"})
h.SetSnapshot(view)
p, exp := newPlugin()
t.Cleanup(exp.close)
h.Start(p)
// scrape exp.addr(), then assert on h.Status(), h.Published(), h.Alerts(), h.Render(...)
```

Useful pieces of the fake host: `SetConfig` (pushes a change), `SetSnapshot`, `SetFleetNodes`, `Status`, `Logs`, `Published`, `Alerts`, `Resolved`, `Render`, `Invoke` and `Call` for raw core-to-plugin calls.

Test the pure parts directly. The exposition writer has a golden-output test, an escaping test and a NaN test, and none of them need a host. Also run `trinetra-registry check -dir .` so a manifest typo fails before install.

For a test against the real daemon path, build and install the plugin the way the user would. `internal/plugin/plugintest.BuildFrom` compiles a plugin directory, signs a throwaway manifest and returns a package that `internal/trinetra`'s end-to-end harness can install and enable; see `internal/trinetra/plugins_prometheus_e2e_test.go`.

```
go vet ./plugins/prometheus/ && go test ./plugins/prometheus/
```

## 5. Run it locally

```
trinetra plugin dev plugins/prometheus
```

Dev mode builds and runs the directory against your daemon without a signature, marked as a development plugin. Edit, save, repeat. Check state with `trinetra plugin status prometheus`, set values with `trinetra plugin set prometheus listen=0.0.0.0:9469`, and scrape it:

```
curl -s localhost:9469/metrics | head
```

The environment a plugin process receives is `TRINETRA_PLUGIN_ID`, `TRINETRA_PLUGIN_HOST_SOCKET`, `TRINETRA_PLUGIN_SOCKET`, `TRINETRA_PLUGIN_TOKEN_FILE` and `TRINETRA_PLUGIN_DATA_DIR`. The SDK reads them for you. `DATA_DIR` is the only place the sandbox lets you write.

## 6. Build and package

```
make plugins
```

checks each manifest under `plugins/` and builds it for the host platform into `dist/plugins/<id>/` as `bin/<os>-<arch>/trinetra-plugin-<id>`, next to `plugin.json` (with its hashes filled in) and `files/`. Set `PLUGINS="uptime"` to build one plugin, or `PLUGIN_MATRIX=1` to build the Linux matrix (amd64, arm64, arm) instead. Signing and packaging into `.tar.gz` is done by `trinetra-registry` (`sign`, then `package`) and by CI when a plugin is merged into the registry.

Install a package with `trinetra plugin install <file>` and enable it with `trinetra plugin enable <id>`, which shows the permission prompt.

## Checklist

- Smallest permission set that works.
- Every host call and goroutine honors the context passed to `Run` or the handler, so shutdown is prompt.
- Secrets use `format: secret`; never log them.
- Defaults for every optional setting, so a fresh install runs.
- A status report on startup and whenever the plugin's health changes.
- Tests for the manifest, the config handling and one round trip through `pluginsdktest`.
