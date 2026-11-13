#!/usr/bin/env bash
# Usage: scripts/build-all.sh <out-dir> [plugin-id ...]
# Env:   ARCHES        linux architectures to build (default "amd64")
#        TRINETRA_DIR  checkout of the trinetra repo (default .trinetra)
# Builds, tests and hashes plugins into <out-dir>/<id>/ (plugin.json, bin/, files/).
# Requires the registry tool on PATH.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
out=$1
shift
arches=${ARCHES:-amd64}
td=$(cd "${TRINETRA_DIR:-$root/.trinetra}" && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)

if [ "$#" -gt 0 ]; then
	ids=("$@")
else
	ids=()
	for d in "$root"/plugins/*/; do
		id=$(basename "$d")
		case "$id" in _*) continue ;; esac
		ids+=("$id")
	done
fi

for id in "${ids[@]}"; do
	src="$root/plugins/$id"
	trinetra-registry check -dir "$src"
	rm -rf "$td/plugins/$id" "$out/$id"
	mkdir -p "$td/plugins"
	cp -R "$src" "$td/plugins/$id"
	for arch in $arches; do
		(
			cd "$td"
			go vet "./plugins/$id/"
			if [ "$arch" = amd64 ]; then
				go test -count=1 "./plugins/$id/"
			fi
			mkdir -p "$out/$id/bin/linux-$arch"
			CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$out/$id/bin/linux-$arch/trinetra-plugin-$id" "./plugins/$id/"
		)
	done
	cp "$src/plugin.json" "$out/$id/plugin.json"
	if [ -d "$src/files" ]; then
		cp -R "$src/files" "$out/$id/files"
	fi
	trinetra-registry hash -dir "$out/$id" -manifest plugin.json
done
