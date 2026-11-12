#!/usr/bin/env bash
# Usage: scripts/build-plugin.sh <plugin-id> [out-dir]
# Env:   PLUGIN_MATRIX=1  also build linux amd64/arm64/arm (default: host os/arch only)
# Builds plugins/<id> into <out-dir>/<id>/ (default dist/plugins), fills hashes in plugin.json.
# The result is ready for trinetra-registry sign and package.
set -euo pipefail

id=${1:?usage: build-plugin.sh <plugin-id> [out-dir]}
root=$(cd "$(dirname "$0")/.." && pwd)
out=${2:-$root/dist/plugins}
src="$root/plugins/$id"
[ -d "$src" ] || { echo "no such plugin: $id" >&2; exit 1; }

mkdir -p "$out"
out=$(cd "$out" && pwd)
build="$out/$id"
rm -rf "$build"
mkdir -p "$build"

reg="$out/trinetra-registry"
(cd "$root" && go build -o "$reg" ./cmd/trinetra-registry)
"$reg" check -dir "$src"

platforms=("$(go env GOOS)/$(go env GOARCH)")
if [ "${PLUGIN_MATRIX:-}" = 1 ]; then
	platforms=(linux/amd64 linux/arm64 linux/arm)
fi

for p in "${platforms[@]}"; do
	os=${p%/*}
	arch=${p#*/}
	dir="$build/bin/$os-$arch"
	mkdir -p "$dir"
	(cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath -o "$dir/trinetra-plugin-$id" "./plugins/$id")
done

cp "$src/plugin.json" "$build/plugin.json"
if [ -d "$src/files" ]; then
	cp -R "$src/files" "$build/files"
fi
"$reg" hash -dir "$build" -manifest plugin.json

rm -f "$reg"
echo "built $build (sign plugin.json with trinetra-registry sign, then package)"
