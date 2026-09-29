#!/bin/bash
# Builds every signed-release fixture the update-e2e harness (run.sh) needs,
# at image build time (see the Dockerfile next to this file). Run from the
# repository root inside the golang:1.24-bookworm builder stage, with GOPATH
# build caches already warm from the tool builds above it.
#
# Produces, under $RELEASES_DIR (default /releases):
#
#   v0.5.0/          good release: the "install 0.5.0" bundle (scenario 1);
#                    also carries bare trinetra/trinetra-ctl/trinetra-web
#                    copies so `trinetra install --require-signed` can run
#                    straight out of this directory as a local bundle.
#   v0.5.1/          good release: what the beta channel pointer names
#                    (scenario 2).
#   v0.5.2/          good signatures, but the core binary was built with
#                    e2eCrashOnStart=1 -- it exits immediately instead of
#                    starting the daemon (scenario 6: rollback).
#   v0.5.3-badci/    good maintainer signature, CI signature from an
#                    UNTRUSTED test key (scenario 3).
#   v0.5.4-badmaint/ good CI signature, no manifest.maint.sig at all
#                    (scenario 4).
#   v0.5.5-tampered/ good signatures over the ORIGINAL binary bytes, then
#                    one byte flipped in trinetra-linux-$GOARCH afterwards,
#                    so the file no longer matches the signed manifest
#                    (scenario 5).
#   v0.5.6/          good release, applied and confirmed in scenario 7.
#   channels/        beta.json + beta.json.sig, naming v0.5.1 (scenario 2).
#
# Every "good" release is signed with the deterministic test keys
# (updatetest.TestKeySet(): CI seed 1, maintainer seed 2 via `sign --role
# maint-test`, channel-pointer seed 3), which is exactly what a
# trinetra_testkeys build's ProductionKeys() trusts (internal/update/keys_testkeys.go).
set -euo pipefail
cd "$(dirname "$0")/../../.."

RELEASES_DIR=${RELEASES_DIR:-/releases}
BUILD=${BUILD_DIR:-/build-fixtures}
GOARCH=$(go env GOARCH)
PUBLISHED=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Deterministic test-signer seeds (base64 of 32 bytes, all equal to the seed
# byte -- see updatetest.NewTestSigner). Seed 9 is NOT in updatetest.TestKeySet(), so
# signing with it produces a signature that fails verification against every
# trusted CI key: exactly the "bad CI signature" fixture.
SEED_CI=AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=
SEED_POINTER=AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM=
SEED_UNTRUSTED=CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk=

mkdir -p "$BUILD" "$RELEASES_DIR/channels"

echo "== shared plugin binaries (ctl/web; content is not version-specific) =="
go build -tags trinetra_testkeys -o "$BUILD/trinetra-ctl-linux-$GOARCH" ./cmd/trinetra-ctl
go build -tags trinetra_testkeys -o "$BUILD/trinetra-web-linux-$GOARCH" ./cmd/trinetra-web
for a in amd64 arm64 arm; do
	[ "$a" = "$GOARCH" ] && continue
	cp "$BUILD/trinetra-ctl-linux-$GOARCH" "$BUILD/trinetra-ctl-linux-$a"
	cp "$BUILD/trinetra-web-linux-$GOARCH" "$BUILD/trinetra-web-linux-$a"
done

# build_core VERSION [EXTRA_LDFLAGS...]: builds cmd/trinetra for the native
# GOARCH, stamped with VERSION (and any extra -X flags, e.g. the crash hook),
# and copies it out to the other two arch names so the 9-file manifest set
# is satisfiable without cross-compiling (only the native-arch file is ever
# actually executed by this harness).
build_core() {
	local v=$1
	local out="$BUILD/trinetra-linux-$GOARCH-$v" ldflags
	shift
	ldflags="-X github.com/InfoDiveLabs/trinetra/internal/version.Version=$v"
	for extra in "$@"; do ldflags="$ldflags $extra"; done
	go build -tags trinetra_testkeys -ldflags "$ldflags" -o "$out" ./cmd/trinetra
	for a in amd64 arm64 arm; do
		[ "$a" = "$GOARCH" ] && continue
		cp "$out" "$BUILD/trinetra-linux-$a-$v"
	done
}

echo "== core binaries =="
build_core 0.5.0
build_core 0.5.1
build_core 0.5.2 "-X github.com/InfoDiveLabs/trinetra/internal/trinetra.e2eCrashOnStart=1"
build_core 0.5.3-badci
build_core 0.5.4-badmaint
build_core 0.5.5-tampered
build_core 0.5.6

# assemble_files VERSION: copies this version's core + the shared plugin
# binaries into $RELEASES_DIR/v<VERSION>/, under the release asset names
# cmd/trinetra-release manifest expects (9 files: 3 stems x 3 linux arches).
assemble_files() {
	local v=$1
	local dir="$RELEASES_DIR/v$v"
	mkdir -p "$dir"
	for a in amd64 arm64 arm; do
		cp "$BUILD/trinetra-linux-$a-$v" "$dir/trinetra-linux-$a"
		cp "$BUILD/trinetra-ctl-linux-$a" "$dir/trinetra-ctl-linux-$a"
		cp "$BUILD/trinetra-web-linux-$a" "$dir/trinetra-web-linux-$a"
	done
}

# sign_good VERSION CHANNEL: manifest + a valid CI signature (seed 1) + a
# valid maintainer signature (sign --role maint-test, seed 2) -- a release a
# trinetra_testkeys host fully trusts.
sign_good() {
	local v=$1 channel=$2
	local dir="$RELEASES_DIR/v$v"
	trinetra-release manifest --dir "$dir" --version "$v" --channel "$channel" \
		--min-upgrade-from 0.1.0 --published "$PUBLISHED"
	TRINETRA_SIGNING_KEY=$SEED_CI trinetra-release sign --role ci --in "$dir/manifest.json" --out "$dir/manifest.ci.sig"
	trinetra-release sign --role maint-test --in "$dir/manifest.json" --out "$dir/manifest.maint.sig"
}

echo "== v0.5.0 (install bundle) =="
assemble_files 0.5.0
sign_good 0.5.0 beta
# Bare-named copies so `trinetra install --require-signed`, run as
# $RELEASES_DIR/v0.5.0/trinetra, finds trinetra/trinetra-ctl/trinetra-web
# (no arch suffix -- see companionInstallNames/verifyInstallBundle) right
# next to manifest.json, hashing identically to the arch-suffixed entries
# the manifest already lists.
cp "$RELEASES_DIR/v0.5.0/trinetra-linux-$GOARCH" "$RELEASES_DIR/v0.5.0/trinetra"
cp "$RELEASES_DIR/v0.5.0/trinetra-ctl-linux-$GOARCH" "$RELEASES_DIR/v0.5.0/trinetra-ctl"
cp "$RELEASES_DIR/v0.5.0/trinetra-web-linux-$GOARCH" "$RELEASES_DIR/v0.5.0/trinetra-web"

echo "== v0.5.1 (channel target) =="
assemble_files 0.5.1
sign_good 0.5.1 beta

echo "== v0.5.2 (crashes on start) =="
assemble_files 0.5.2
sign_good 0.5.2 beta

echo "== v0.5.3-badci (untrusted CI key) =="
assemble_files 0.5.3-badci
dir="$RELEASES_DIR/v0.5.3-badci"
trinetra-release manifest --dir "$dir" --version 0.5.3-badci --channel beta --min-upgrade-from 0.1.0 --published "$PUBLISHED"
TRINETRA_SIGNING_KEY=$SEED_UNTRUSTED trinetra-release sign --role ci --in "$dir/manifest.json" --out "$dir/manifest.ci.sig"
trinetra-release sign --role maint-test --in "$dir/manifest.json" --out "$dir/manifest.maint.sig"

echo "== v0.5.4-badmaint (no maintainer signature) =="
assemble_files 0.5.4-badmaint
dir="$RELEASES_DIR/v0.5.4-badmaint"
trinetra-release manifest --dir "$dir" --version 0.5.4-badmaint --channel beta --min-upgrade-from 0.1.0 --published "$PUBLISHED"
TRINETRA_SIGNING_KEY=$SEED_CI trinetra-release sign --role ci --in "$dir/manifest.json" --out "$dir/manifest.ci.sig"
# manifest.maint.sig deliberately not written.

echo "== v0.5.5-tampered (binary changed after signing) =="
assemble_files 0.5.5-tampered
sign_good 0.5.5-tampered beta
# Overwrite byte 0 (always 0x7F, the start of the ELF magic number, for any
# real compiled binary) with a NUL, in the file the host will actually fetch
# (native GOARCH): the size is unchanged but the sha256 no longer matches
# the already-signed manifest. The file is never executed after this (the
# host is expected to reject it on the hash mismatch before it would be),
# so corrupting its ELF header is not a concern.
printf '\0' | dd of="$RELEASES_DIR/v0.5.5-tampered/trinetra-linux-$GOARCH" bs=1 seek=0 count=1 conv=notrunc status=none

echo "== v0.5.6 (guard-kill target) =="
assemble_files 0.5.6
sign_good 0.5.6 beta

echo "== channels/beta (points at v0.5.1) =="
trinetra-release pointer --channel beta --version 0.5.1 --issued "$PUBLISHED" --out "$RELEASES_DIR/channels/beta.json"
TRINETRA_SIGNING_KEY=$SEED_POINTER trinetra-release sign --role pointer --in "$RELEASES_DIR/channels/beta.json" --out "$RELEASES_DIR/channels/beta.json.sig"

rm -rf "$BUILD"
echo "fixtures ready under $RELEASES_DIR:"
find "$RELEASES_DIR" -maxdepth 1 -mindepth 1 | sort
