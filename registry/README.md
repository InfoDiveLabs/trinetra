# Trinetra plugin registry

This repository is the public registry of Trinetra plugins. Every plugin listed here is built, signed and indexed by CI. Trinetra hosts fetch only the signed index and the packages it points to.

## Trust model

Plugins are signed with the same keys as Trinetra releases, so there is no separate registry key to manage.

- `plugin.json` is the plugin's code identity: it pins the sha256 of every per-platform binary. It carries two signatures, and hosts require both:
  - `plugin.json.ci.sig`, made by CI with the release CI key (`TRINETRA_CI_SIGNING_KEY`);
  - `plugin.json.maint.sig`, made by a maintainer on their own machine with the release maintainer key (`trinetra-release plugin-cosign`).
- `index.json` (the catalog: versions, package URLs pinned by sha256 and size, revocations) is signed by CI with the release pointer key (`TRINETRA_POINTER_SIGNING_KEY`), like the release channel pointer. That lets the weekly re-sign run unattended. The pointer key alone cannot add code: every listed package still needs the maintainer signature.
- Each signature has its own domain prefix, so a plugin signature can never stand in for a release or channel signature, or the reverse.
- The public keys are the release keys compiled into every Trinetra build (`internal/update/keys.go`).
- The index carries a serial and an expiry. Hosts refuse a lower serial, refuse an index valid for more than 30 days, and treat an expired index as stale: installed plugins keep running, new registry installs are refused until a fresh index is fetched.
- Revocations live in `revoked.json` and ship inside the signed index. Hosts disable a revoked plugin on the next refresh.

## Layout

```
plugins/<id>/            plugin source: plugin.json, main.go, tests, optional files/
plugins/_template/       starting point for a new plugin
revoked.json             list of revoked plugin versions
scripts/build-all.sh     builds, tests and hashes plugins
scripts/cosign.sh        maintainer co-sign of a CI-signed draft
.github/workflows/       pr.yml, publish.yml, release.yml, resign.yml
```

Packages are release assets named `<id>-v<version>`. The index is the `index` release (`index.json` and `index.json.sig`).

## Submitting a plugin

1. Copy `plugins/_template` to `plugins/<id>` and implement it. Read `CONTRIBUTING.md` first.
2. Open a pull request. CI runs `go vet`, `go test` and `go build` for linux on amd64, arm64 and arm, validates the manifest and its `host_api` range, and checks that the binaries hash. PR builds have no secrets.
3. A maintainer reviews the code and the requested permissions.
4. On merge to `main`, `publish.yml` builds every new plugin version, writes the binary hashes into the manifest, signs it with the CI key and uploads the build to a draft release `<id>-v<version>`.
5. A maintainer runs `scripts/cosign.sh <id> <version>`. It shows the id, version, publisher, permissions and binary hashes, asks for `<id>@<version>` to be retyped, signs with the maintainer key and starts `release.yml`.
6. `release.yml` packages each platform, refuses anything whose two signatures do not verify against the release keys, publishes the release and signs a new index with the pointer key.

Nothing reaches the verified catalog without a maintainer signature.

## Weekly re-sign

The index expires after 21 days. `resign.yml` runs weekly and re-issues the same content with a new serial and expiry, so hosts never see an expired catalog while the registry is healthy. If it stops running, hosts show a stale catalog warning after the expiry.

## Key rotation

The keys are the release keys, so they rotate with Trinetra's release key rotation (see the release handbook): each build trusts a current and a next key per role. Plugins signed with the old key keep verifying until a Trinetra release drops it; re-sign live plugin versions with the new keys before that release ships.

If a key is compromised, rotate it with the release keys and revoke affected plugin versions in `revoked.json`.
