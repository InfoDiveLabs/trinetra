# Manifest reference

Every plugin ships a `plugin.json`. It is decoded strictly: unknown fields, trailing bytes and files over 256 KiB are rejected. The registry tool fills in `binaries` at build time, so the checked-in manifest keeps `"binaries": []`.

## Top-level fields

| Field | Required | Rule |
|---|---|---|
| `schema` | yes | Must be `1`. |
| `id` | yes | `^[a-z][a-z0-9-]{1,31}$`. Also names the binary `trinetra-plugin-<id>` and namespaces metrics and alert keys. |
| `name` | yes | 1-64 characters. |
| `version` | yes | Semantic version, for example `0.1.0`. |
| `publisher` | yes | 1-64 characters. |
| `description` | no | At most 280 characters. |
| `homepage` | no | Must start with `https://`. |
| `host_api` | yes | Semver range of the host API you need, for example `^1.0`. An incompatible range is refused at install time. |
| `min_trinetra` | no | Oldest Trinetra version that can run the plugin. |
| `permissions` | yes | List from the [permission vocabulary](permissions.md). No duplicates; reserved names are refused. |
| `binaries` | yes | One entry per platform: `os` (`linux` or `darwin`), `arch` (`amd64`, `arm64`, `arm`), `sha256` (64 lowercase hex characters), `size` (positive). Filled in by `trinetra-registry hash`. |
| `config_schema` | no | Settings, see below. |
| `contributes` | no | Slots, actions and files, see below. |
| `activation` | no | `always` (default), `on-slot` or `on-event`. |

## `config_schema`

A JSON Schema subset. The root is `{"type": "object", "properties": {...}}` with at most 64 properties. Property names match `^[a-z][a-z0-9_]{0,63}$`. The host validates saved values against it, applies defaults, and hands the plugin the result; plugins also get a `ValidateConfig` call for rules a schema cannot express.

Top-level keys:

| Key | Meaning |
|---|---|
| `required` | Property names that must have a value (after defaults). |
| `x-order` | Property names in display order; the rest follow alphabetically. |

Property keys:

| Key | Applies to | Meaning |
|---|---|---|
| `type` | all | `string`, `integer`, `number`, `boolean` or `array`. |
| `title` | all | Label in the settings form. |
| `description` | all | Help text under the field. |
| `default` | all | JSON value of the declared type. |
| `format` | string | `secret` (write-only, never echoed back to the UI) or `duration` (for example `30s`). |
| `enum` | string | Up to 64 allowed values. Cannot be combined with `format`. |
| `pattern` | string | Regular expression the value must match. |
| `minimum`, `maximum` | integer, number, duration | Inclusive bounds. |
| `maxItems` | array | At most 200. |
| `items` | array | Must be `{"type": "string"}`. |
| `x-section` | all | Where the field renders: `settings.form` (default) or `alert.rules`. |

Strings are capped at 4096 characters.

## `contributes`

### `slots`

Each entry fills one place in the UI. See [Slots and widgets](slots-and-widgets.md).

| Field | Rule |
|---|---|
| `slot` | One of the v1 slots. |
| `id` | `^[a-z][a-z0-9-]{0,31}$`, unique per slot. Passed to your handler as `slot_id`. |
| `title` | 1-64 characters. |
| `min_role` | `viewer`, `responder` or `admin`. Users below it do not see the slot. |

`settings.form` and `alert.rules` are rendered by the host from `config_schema`, so the schema must contain at least one property in that section. `container.action` needs an action with the same `id`.

### `actions`

Buttons and forms invoke actions. Role and permission are enforced from this declaration, never from what a widget claims.

| Field | Rule |
|---|---|
| `id` | Same pattern as a slot id. |
| `label` | 1-40 characters. |
| `confirm` | Optional confirmation text shown before a mutating action runs. |
| `min_role` | `viewer`, `responder` or `admin`. |
| `permission` | Optional permission the plugin must hold. |
| `mutating` | When true the call needs confirmation and is written to the web audit log. |

### `files`

Downloads the web UI serves for the plugin, stored at `files/<name>` in the package. They are sent as attachments with a fixed content type and `nosniff`.

| Field | Rule |
|---|---|
| `name` | `^[a-z0-9][a-z0-9._-]{0,63}$`, unique. |
| `title` | Link text. |
| `content_type` | `application/json` or `text/plain`. |

## Example

The `prometheus` plugin's manifest, trimmed:

```json
{
  "schema": 1,
  "id": "prometheus",
  "name": "Prometheus exporter",
  "version": "0.1.0",
  "publisher": "InfoDiveLabs",
  "host_api": "^1.0",
  "permissions": ["metrics.read", "net.listen", "fleet.read"],
  "binaries": [],
  "config_schema": {
    "type": "object",
    "properties": {
      "listen": {"type": "string", "title": "Listen address", "pattern": "^[A-Za-z0-9.:\\[\\]-]*:[0-9]{1,5}$", "default": "127.0.0.1:9469"},
      "bearer_token": {"type": "string", "format": "secret", "title": "Bearer token"},
      "include_fleet": {"type": "boolean", "title": "Include fleet nodes", "default": false}
    },
    "x-order": ["listen", "bearer_token", "include_fleet"]
  },
  "contributes": {
    "slots": [{"slot": "settings.form", "id": "settings", "title": "Prometheus exporter", "min_role": "admin"}],
    "files": [{"name": "grafana-dashboard.json", "title": "Grafana dashboard", "content_type": "application/json"}]
  }
}
```
