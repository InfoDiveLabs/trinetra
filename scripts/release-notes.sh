#!/usr/bin/env bash
# Prints the GitHub release notes for tag vX.Y.Z(-pre): the matching
# "## [X.Y.Z]" section of CHANGELOG.md, with repo-relative links made
# absolute (release bodies don't resolve them), followed by how to verify
# the release. A tag with no CHANGELOG section gets a one-line note and a
# workflow warning instead of failing the release.
set -euo pipefail
tag=${1:?usage: release-notes.sh vX.Y.Z [CHANGELOG.md]}
changelog=${2:-CHANGELOG.md}
ver=${tag#v}
base="https://github.com/InfoDiveLabs/trinetra/blob/$tag"

section=$(awk -v hdr="## [$ver]" '
  index($0, hdr) == 1 { f = 1; next }
  /^## \[/ { f = 0 }
  f' "$changelog")

if [ -n "$(printf '%s' "$section" | tr -d '[:space:]')" ]; then
  printf '%s\n' "$section" | sed -E \
    -e "s#\]\((docs/[^)]*)\)#](${base}/\1)#g" \
    -e "s#\]\((LICENSE|SECURITY\.md|README\.md|CHANGELOG\.md)([)\#])#](${base}/\1\2#g"
else
  echo "::warning::CHANGELOG.md has no [$ver] section" >&2
  echo "See [CHANGELOG.md](${base}/CHANGELOG.md)."
fi

cat <<EOF

### Verifying this release

Every binary is listed with its SHA-256 in \`manifest.json\`, which carries two independent Ed25519 signatures: \`manifest.ci.sig\` (made by CI) and \`manifest.maint.sig\` (the maintainer's offline co-signature). \`trinetra update\` and \`trinetra install --require-signed\` check both before anything runs. Binaries also carry a GitHub build-provenance attestation:

\`\`\`sh
gh attestation verify trinetra-linux-amd64 --repo InfoDiveLabs/trinetra
\`\`\`

For manual verification with only \`openssl\` and \`sha256sum\`, see [Verify a download yourself](${base}/docs/handbook/14-security.md#verify-a-download-yourself).
EOF
