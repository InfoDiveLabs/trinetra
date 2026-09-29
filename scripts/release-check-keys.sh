#!/usr/bin/env bash
# Fails unless the built linux/amd64 core embeds non-empty production keys
# that are not the test keys, and they match what trinetra-release reports.
set -euo pipefail
bin=${1:-dist/trinetra-linux-amd64}
want=$(go run ./cmd/trinetra-release fingerprints)
got=$("$bin" update status --json | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)["fingerprints"] or []))')
test_fp=$(go run -tags trinetra_testkeys ./cmd/trinetra-release fingerprints)
[ -n "$got" ] || { echo "no release keys compiled in"; exit 1; }
[ "$got" = "$want" ] || { echo "key fingerprints differ from trinetra-release"; exit 1; }
[ "$got" != "$test_fp" ] || { echo "binary was built with TEST keys"; exit 1; }
echo "release keys OK"
