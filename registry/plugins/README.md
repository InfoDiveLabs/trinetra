# Plugins

One directory per plugin, named after its manifest `id`. Start from `_template` (directories whose name starts with `_` are skipped by CI).

Each plugin directory holds:

- `plugin.json`: the manifest, with `binaries` left empty.
- `main.go` and tests.
- `files/`: optional downloadable files declared under `contributes.files`.
- `README.md`.

CI builds, hashes, signs and publishes every plugin directory on merge to `main`.
