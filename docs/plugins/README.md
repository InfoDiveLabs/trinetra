# Trinetra plugins

A plugin is a separate, signed program that extends Trinetra without touching the core. It runs as its own process under a sandbox, talks to the daemon over a local socket, and describes its UI as data that `trinetra-web` renders. The core binary stays standard-library only.

| Page | What it covers |
|---|---|
| [Authoring guide](authoring.md) | Build a plugin with the Go SDK, using the `prometheus` plugin as the worked example. Local loop, testing, packaging. |
| [Manifest reference](manifest-reference.md) | Every `plugin.json` field and the rules that are enforced. |
| [Slots and widgets](slots-and-widgets.md) | Every slot a plugin can fill, every widget kind, limits and error cards. |
| [Permissions](permissions.md) | The permission vocabulary, consent flows and the sandbox each grant produces. |

The worked example lives in [`plugins/prometheus`](../../plugins/prometheus). A smaller starting point is [`registry/plugins/_template`](../../registry/plugins/_template).

Companion binaries that ship with Trinetra (`trinetra-ctl`, `trinetra-web`) are not plugins in this sense; see the [handbook](../handbook/plugins/README.md).

## How it fits together

```
trinetra (daemon)  <--control socket, per-plugin token-->  plugin process
       |                                                       ^
       +-- core calls: RenderSlot, InvokeAction, ValidateConfig, OnEvent
trinetra-web renders the widgets the plugin returns; the browser never talks to the plugin.
```

- Plugin to core calls (`Plugin.*`) are limited by the permissions you approved when enabling the plugin.
- Core to plugin calls always have a deadline, so a hung plugin shows an error card in its own slot and nothing else.
- Install does not enable. Enabling shows the plugin's permissions and records your consent.
