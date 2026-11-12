# Contributing a plugin

## Author checklist

Manifest (`plugin.json`):

- [ ] `schema` is `1`. `id` is 2-32 characters, lowercase letters, digits and `-`, starting with a letter, and matches the directory name.
- [ ] `name`, `publisher`, `description` (at most 280 characters) are filled in. `homepage`, if set, is https.
- [ ] `version` is semver (`1.2.3`). A published version is immutable: ship fixes as a new version.
- [ ] `host_api` is the range you built against, for example `^1.0`. `trinetra-registry check` fails if this core cannot serve it.
- [ ] `permissions` lists only what the plugin uses. Each one is shown to the operator at install time.
- [ ] `binaries` is `[]` in source. CI fills it in with `trinetra-registry hash`.
- [ ] `config_schema` describes every setting. Mark secrets with `"format": "secret"`.
- [ ] `contributes` declares every slot, action and downloadable file. An action carries its own `min_role` and `permission`.

Code:

- [ ] Go standard library and the Trinetra plugin SDK only. No other third-party modules.
- [ ] `go vet ./...` and `go test ./...` pass. Tests use the SDK's fake host (`pluginsdktest`).
- [ ] The plugin never blocks the host: every outbound call has a timeout, and the plugin keeps working when its target is down.
- [ ] No data leaves the machine unless the plugin declares `net.outbound`, and the README says where it goes.

Pull request:

- [ ] A short README in the plugin directory: what it does, each permission and why it is needed, configuration reference.
- [ ] No binaries, generated files or vendored dependencies are committed.

## Permission rationale

Reviewers ask for a one-line reason per permission. State it in the plugin README, for example `net.outbound: probes the HTTP endpoints you configure`. Requests that are broader than the features need are sent back.

## Local workflow

```
trinetra plugin dev ./plugins/<id>      # run against a local daemon
trinetra-registry check -dir plugins/<id>
```

## Review criteria

- Manifest honesty: permissions, config and slots match the code.
- Failure behavior: bad config, unreachable targets and slow responses degrade the plugin, not the host.
- Size and scope: one plugin does one job.
- Maintenance: a named publisher who will answer security reports.
