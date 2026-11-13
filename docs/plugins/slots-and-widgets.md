# Slots and widgets

A plugin never ships HTML or JavaScript. It declares slots in its manifest and answers `RenderSlot` calls with a list of widgets. `trinetra-web` renders them with its own templates, so styling, escaping and theming stay consistent.

## Slots

| Slot | Renders in | Who fills it |
|---|---|---|
| `settings.form` | The plugin's card on the Integrations page. | Host, from `config_schema`. |
| `alert.rules` | An "Alert rules" section on the plugin's page or settings. | Host, from `config_schema` properties with `x-section: alert.rules`. Rules are plugin config, evaluated by the plugin. |
| `nav.page` | A sidebar entry and a page of widgets. | Plugin. |
| `dashboard.panel` | An optional panel users can add to the dashboard. | Plugin. |
| `host.section` | A section on the host overview. | Plugin. |
| `container.section` | A section in the container drawer. `context.container` names the container. | Plugin. |
| `container.action` | A button in the container drawer header. Needs an action with the same id. | Plugin. |

Host-rendered slots need no handler. The `prometheus` plugin only declares `settings.form`: its three settings become a form, and the plugin reports a status line (`serving on 127.0.0.1:9469`) that shows on the card.

Plugin-rendered slots register a handler:

```go
p.HandleSlot("main", func(ctx context.Context, req pluginsdk.SlotRequest) ([]pluginsdk.Widget, error) {
    return []pluginsdk.Widget{pluginsdk.Stat("Checks", "12", "")}, nil
})
```

`req` carries the slot id, the `SlotContext` (`node`, `container`, `params`) and the user (`name`, `role`). The call has a 2 second deadline.

## Widgets

A widget has a `kind`, an optional `title` and exactly one matching body. The SDK has a constructor for each kind in `pkg/pluginsdk`.

| Kind | Constructor | Body |
|---|---|---|
| `stat` | `Stat(title, value, unit)`, `StatTone(w, tone)` | `value`, `unit`, `hint`, `tone` (`ok`, `warn`, `crit`). |
| `timeseries` | `Timeseries(title, unit, series...)`, `Series(metric, label)` | `series` list of `{metric, label}`, `range` (`1h`, `6h`, `24h`, `7d`; default `6h`), `unit`. The host draws it from the metric store; your plugin sends no points. |
| `table` | `Table(title, columns, rows)` | `columns`, `rows` of strings. |
| `kv` | `KV(title, items...)` | `items` of `{key, value}`. |
| `log` | `Log(title, lines)` | `lines` of text. |
| `text` | `Text(title, text)` | `text`, `format` (`plain` or `lite`, a sanitized subset). |
| `status` | `Status(title, items...)` | `items` of `{name, state, detail}`; state is `up`, `down` or `unknown`. |
| `action` | `Action(actionID, params)` | `action` id declared in the manifest, `params`. Renders a button. |
| `form` | `Form(title, actionID, submit, fields...)` | `action`, `fields`, `submit` label. Field types: `string`, `integer`, `number`, `boolean`, `enum`, `secret`. |

`Series` takes your plugin-local metric name. The SDK rewrites it to the stored name `plugin.<id>.<metric>` when the slot is rendered.

## Limits

| Limit | Value |
|---|---|
| Widgets per slot | 20 |
| Table rows / columns | 500 / 12 |
| Log lines | 1000 |
| Series per chart | 12 |
| Any widget string | 4096 characters |
| Message size on the socket | 1 MiB |

## Actions

`InvokeAction` runs a declared action with a 10 second deadline:

```go
p.HandleAction("restart-probe", func(ctx context.Context, req pluginsdk.ActionRequest) (pluginsdk.ActionResult, error) {
    return pluginsdk.ActionResult{OK: true, Message: "restarted"}, nil
})
```

The web UI checks the user's role against the action's `min_role` and the plugin's grant against its `permission` before calling you. Mutating actions require confirmation and are written to the audit log. Never rely on parameters to carry authorization.

## Error cards

Each slot renders on its own. If your handler returns an error, times out, panics or returns an invalid spec (unknown kind, a body that does not match its kind, a limit exceeded), that slot shows an error card with the reason and the rest of the page renders normally. Return an error rather than a half-filled widget.
