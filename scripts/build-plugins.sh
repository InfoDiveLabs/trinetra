#!/usr/bin/env bash
# Usage: scripts/build-plugins.sh [plugin-id ...]
# Env:   PLUGIN_MATRIX=1  also build linux amd64, arm64 and arm (default: host platform only)
#        OUT              output directory (default dist/plugins)
# Builds plugins/<id> into $OUT/<id>/: plugin.json, files/ and
# bin/<os>-<arch>/trinetra-plugin-<id>. Hash, sign and package the result with trinetra-registry.
set -euo pipefail

cd "$(dirname "$0")/.."
out=${OUT:-dist/plugins}

if [ "$#" -gt 0 ]; then
	ids=("$@")
else
	ids=()
	for d in plugins/*/; do
		id=$(basename "$d")
		case "$id" in _*) continue ;; esac
		ids+=("$id")
	done
fi

if [ "${PLUGIN_MATRIX:-0}" = 1 ]; then
	targets=("linux amd64" "linux arm64" "linux arm")
else
	targets=("$(go env GOOS) $(go env GOARCH)")
fi

for id in "${ids[@]}"; do
	src=plugins/$id
	[ -f "$src/plugin.json" ] || { echo "build-plugins: $src/plugin.json not found" >&2; exit 1; }
	rm -rf "${out:?}/$id"
	for t in "${targets[@]}"; do
		read -r goos goarch <<<"$t"
		dir=$out/$id/bin/$goos-$goarch
		mkdir -p "$dir"
		[ "$goarch" = arm ] && export GOARM=7
		CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags "-s -w" -o "$dir/trinetra-plugin-$id" "./$src"
	done
	cp "$src/plugin.json" "$out/$id/plugin.json"
	if [ -d "$src/files" ]; then
		cp -R "$src/files" "$out/$id/files"
	fi
	echo "built $out/$id"
done
