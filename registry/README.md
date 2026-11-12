# Trinetra plugin registry

This repository is the public registry of Trinetra plugins. Every plugin listed here is built, signed and indexed by CI. Trinetra hosts fetch only the signed index and the packages it points to.

## Trust model

- The registry signing key lives in the `REGISTRY_SIGNING_KEY` secret of the `registry` environment. Only the publish and resign workflows can read it.
- The matching public key is compiled into each Trinetra release (`internal/registry/keys.go`). A host with no registry key refuses registry installs.
- Two things are signed, each with its own domain prefix so one signature can never stand in for the other:
  - `plugin.json.sig` signs the plugin manifest, which carries the sha256 of every per-platform binary.
  - `index.json.sig` signs the catalog (`index.json`).
- The index carries a serial and an expiry. Hosts refuse a lower serial, refuse an index that has been valid for more than 30 days, and treat an expired index as stale: installed plugins keep running, new registry installs are refused until a fresh index is fetched.
- Revocations live in `revoked.json` and ship inside the signed index. Hosts disable a revoked plugin on the next refresh.
- Each package URL in the index is https and is pinned by sha256 and size.

## Layout

```
plugins/<id>/            plugin source: plugin.json, main.go, tests, optional files/
plugins/_template/       starting point for a new plugin
revoked.json             list of revoked plugin versions
scripts/build-all.sh     builds, tests and hashes plugins
.github/workflows/       pr.yml, publish.yml, resign.yml
```

Packages are release assets named `<id>-v<version>`. The index is the `index` release (`index.json` and `index.json.sig`).

## Submitting a plugin

1. Copy `plugins/_template` to `plugins/<id>` and implement it. Read `CONTRIBUTING.md` first.
2. Open a pull request. CI runs `go vet`, `go test` and `go build` for linux on amd64, arm64 and arm, validates the manifest and its `host_api` range, and checks that the binaries hash. PR builds have no secrets.
3. A maintainer reviews the code and the requested permissions.
4. On merge to `main`, CI builds every plugin, writes the hashes into the manifest, signs it, packages each platform, uploads the packages and publishes a new signed index.

## Weekly re-sign

The index expires after 21 days. `resign.yml` runs weekly and re-issues the same content with a new serial and expiry, so hosts never see an expired catalog while the registry is healthy. If it stops running, hosts show a stale catalog warning after the expiry.

## Key rotation

1. Generate a new key with `trinetra-registry keygen -out new.key`.
2. Ship a Trinetra release that trusts both the old and the new public key.
3. After that release is widely deployed, replace the `REGISTRY_SIGNING_KEY` secret with the new key and re-sign: run `resign.yml`, then re-sign every manifest by re-running the publish workflow with a version bump.
4. Ship a later release that drops the old public key.

If a key is compromised, revoke it by shipping a Trinetra release without it and rotate immediately; hosts on older releases keep trusting the old key until they update.
