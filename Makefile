BIN=serverwatch
CTL_BIN=serverwatch-ctl
WEB_BIN=serverwatch-web

.PHONY: test vet build linux cross release release-prod validate fmt

# Build flags, per release channel:
#   Beta/dev builds keep the symbol table and DWARF so stack traces, delve, and
#   go tool pprof stay usable when a preview binary misbehaves in the field.
#   Production (main) release builds strip them (-s -w) and trim absolute source
#   paths (-trimpath) for a smaller, reproducible binary, roughly 25-30 percent
#   smaller. These default to empty (the beta/dev channel); release-prod sets
#   them and, being GNU Make target-specific variables, they propagate to the
#   cross prerequisite so every binary in a prod cut is built optimized.
GO_LDFLAGS ?=
GO_TRIMPATH ?=

# VERSION is stamped into each binary via -ldflags -X (#107) so the core daemon
# and each plugin report their own compiled-in version over the control socket /
# in the UI. Derived from git (the tag on a release cut, else a describe of the
# nearest tag + commit, "-dirty" when the tree has uncommitted changes); falls
# back to "dev" outside a git checkout. Plain `go build` (no Makefile) leaves it
# empty and internal/version.String() falls back to build info -> "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS := -X serverwatch/internal/version.Version=$(VERSION)
# ALL_LDFLAGS folds the channel flags (empty for beta, -s -w for prod) together
# with the version stamp, so every build target below carries the version.
ALL_LDFLAGS = $(GO_LDFLAGS) $(VERSION_LDFLAGS)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

# build builds all three binaries for the host GOOS/GOARCH: serverwatch (the
# daemon), serverwatch-ctl (the interactive TUI plugin), and serverwatch-web
# (the web UI plugin). None of them need a build tag. The daemon stays
# stdlib only because it does not import internal/web or internal/tui, not
# because of any tag. This is a dev build (unstripped); use release-prod for
# the optimized main-channel artifacts.
build:
	go build -ldflags "$(VERSION_LDFLAGS)" -o dist/$(BIN) ./cmd/serverwatch
	go build -ldflags "$(VERSION_LDFLAGS)" -o dist/$(CTL_BIN) ./cmd/serverwatch-ctl
	go build -ldflags "$(VERSION_LDFLAGS)" -o dist/$(WEB_BIN) ./cmd/serverwatch-web

linux:
	GOOS=linux GOARCH=amd64 go build -o dist/$(BIN)-linux-amd64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm64 go build -o dist/$(BIN)-linux-arm64 ./cmd/serverwatch

# cross builds the full release matrix: all three binaries (serverwatch,
# serverwatch-ctl, serverwatch-web), each from its own ./cmd directory with
# no build tag, for linux amd64/arm64/arm, plus darwin amd64/arm64
# (dev/homelab convenience, not part of the linux release set). The
# GO_TRIMPATH/GO_LDFLAGS vars are empty by default (beta channel) and set by
# release-prod for the optimized main channel.
cross:
	GOOS=linux GOARCH=amd64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(BIN)-linux-amd64 ./cmd/serverwatch
	GOOS=linux GOARCH=amd64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(CTL_BIN)-linux-amd64 ./cmd/serverwatch-ctl
	GOOS=linux GOARCH=amd64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(WEB_BIN)-linux-amd64 ./cmd/serverwatch-web
	GOOS=linux GOARCH=arm64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(BIN)-linux-arm64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(CTL_BIN)-linux-arm64 ./cmd/serverwatch-ctl
	GOOS=linux GOARCH=arm64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(WEB_BIN)-linux-arm64 ./cmd/serverwatch-web
	GOOS=linux GOARCH=arm GOARM=7 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(BIN)-linux-arm ./cmd/serverwatch
	GOOS=linux GOARCH=arm GOARM=7 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(CTL_BIN)-linux-arm ./cmd/serverwatch-ctl
	GOOS=linux GOARCH=arm GOARM=7 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(WEB_BIN)-linux-arm ./cmd/serverwatch-web
	GOOS=darwin GOARCH=amd64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(BIN)-darwin-amd64 ./cmd/serverwatch
	GOOS=darwin GOARCH=amd64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(CTL_BIN)-darwin-amd64 ./cmd/serverwatch-ctl
	GOOS=darwin GOARCH=amd64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(WEB_BIN)-darwin-amd64 ./cmd/serverwatch-web
	GOOS=darwin GOARCH=arm64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(BIN)-darwin-arm64 ./cmd/serverwatch
	GOOS=darwin GOARCH=arm64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(CTL_BIN)-darwin-arm64 ./cmd/serverwatch-ctl
	GOOS=darwin GOARCH=arm64 go build $(GO_TRIMPATH) -ldflags "$(ALL_LDFLAGS)" -o dist/$(WEB_BIN)-darwin-arm64 ./cmd/serverwatch-web

# release is the BETA/preview cut (unstripped, debuggable): cross's whole
# matrix plus a dist/checksums.txt covering every artifact, so
# sha256sum -c checksums.txt verifies a downloaded binary against the same
# file the release page links. Used for the develop-branch vX.Y.Z-beta.N
# prereleases. rm -f first so a re-run never appends onto (or hashes) a stale
# checksums.txt from a previous invocation.
release: cross
	rm -f dist/checksums.txt
	cd dist && sha256sum $(BIN)-* > checksums.txt

# release-prod is the PRODUCTION (main) cut: identical artifact set and
# checksums as release, but every binary is fully optimized (stripped with
# -s -w and built with -trimpath) for the smallest, reproducible download.
# Use this for the stable vX.Y.Z releases cut off main; use plain release for
# beta prereleases so field debugging stays possible.
release-prod: GO_LDFLAGS := -s -w
release-prod: GO_TRIMPATH := -trimpath
release-prod: cross
	rm -f dist/checksums.txt
	cd dist && sha256sum $(BIN)-* > checksums.txt

validate:
	bash test/docker/scenarios.sh
