#!/usr/bin/env bash
# Usage: scripts/cosign.sh <plugin-id> <version> [--key FILE]
# Maintainer step between publish.yml (CI-signed draft) and release.yml (package + index):
# downloads the draft build, reviews and co-signs plugin.json with the maintainer release key,
# uploads plugin.json.maint.sig and starts release.yml.
set -euo pipefail
id=${1:?usage: cosign.sh <plugin-id> <version> [--key FILE]}
version=${2:?usage: cosign.sh <plugin-id> <version> [--key FILE]}
shift 2
key=${TRINETRA_MAINT_KEY:-$HOME/.config/trinetra/maint.key}
if [ "${1:-}" = "--key" ]; then key=${2:?--key needs a file}; fi
repo=${REGISTRY_REPO:-InfoDiveLabs/trinetra-plugins}
tag="$id-v$version"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gh release download "$tag" -R "$repo" -p "$id-$version-build.tgz" -D "$work"
mkdir "$work/b"
tar -xzf "$work/$id-$version-build.tgz" -C "$work/b"
trinetra-release plugin-cosign --dir "$work/b" --key "$key"
gh release upload "$tag" -R "$repo" "$work/b/plugin.json.maint.sig" --clobber
gh workflow run release.yml -R "$repo" -f tag="$tag"
echo "co-signed $tag; release.yml is packaging and indexing it"
