#!/usr/bin/env bash
# Usage: scripts/build-plugins.sh [plugin-id ...]
# Env:   PLUGIN_MATRIX=1  build linux amd64, arm64 and arm instead of the host platform
#        OUT              output directory (default dist/plugins)
# Builds each plugins/<id> with scripts/build-plugin.sh: checked manifest, binaries and filled hashes,
# ready for trinetra-registry sign and package.
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
		[ -f "plugins/$id/plugin.json" ] || continue
		ids+=("$id")
	done
fi

for id in "${ids[@]}"; do
	bash scripts/build-plugin.sh "$id" "$out"
done
