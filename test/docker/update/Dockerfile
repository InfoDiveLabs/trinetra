# syntax=docker/dockerfile:1
# Image for the signed self-update e2e scenarios (run.sh): a systemd-less
# "host" container that runs the real trinetra binaries -- every one of them
# built with -tags trinetra_testkeys, so ProductionKeys() trusts the
# deterministic test key set (updatetest.TestKeySet()) instead of this repo's
# (currently empty) real production keys -- against a fake GitHub API
# (relsrv) serving pre-built, pre-signed release fixtures, and a mock
# Telegram API (mocktg, shared with the other docker harnesses).
#
# Build context: the repository root.
#
#   docker build -f test/docker/update/Dockerfile -t swupdate-e2e:latest .

FROM golang:1.24-bookworm AS builder
WORKDIR /src
COPY . .
# Cache mounts (not layer caching -- COPY . . above invalidates on ANY repo
# change, docs included, so plain layer caching buys nothing here) keep the
# module/build cache across rebuilds, so editing a file elsewhere in the
# repo does not force every dependency to be re-downloaded from the Go
# module proxy again.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -tags trinetra_testkeys -o /usr/local/bin/trinetra-release ./cmd/trinetra-release \
 && go build -o /usr/local/bin/relsrv ./test/docker/update/relsrv \
 && go build -o /usr/local/bin/mocktg ./test/docker/mocktg
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    PATH="/usr/local/bin:$PATH" RELEASES_DIR=/releases bash test/docker/update/build-fixtures.sh

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq procps psmisc util-linux ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /releases /releases
COPY --from=builder /usr/local/bin/relsrv /usr/local/bin/relsrv
COPY --from=builder /usr/local/bin/mocktg /usr/local/bin/mocktg
COPY test/docker/update/fake-systemctl.sh /usr/local/bin/systemctl
COPY test/docker/update/e2e-restart.sh /usr/local/libexec/e2e-restart.sh
COPY test/docker/update/e2e-guard.sh /usr/local/libexec/e2e-guard.sh
RUN chmod +x /usr/local/bin/systemctl /usr/local/libexec/e2e-restart.sh /usr/local/libexec/e2e-guard.sh
# The e2e-only hooks (internal/trinetra/update_e2e_hooks_testkeys.go), read
# only by a trinetra_testkeys build: point self-update at relsrv instead of
# the real GitHub API, and replace the guard's systemctl/systemd-run calls
# with the scripts above.
ENV TRINETRA_E2E_GITHUB_BASE_URL="http://relsrv:8080"
ENV TRINETRA_E2E_RESTART_CMD="/usr/local/libexec/e2e-restart.sh"
ENV TRINETRA_E2E_GUARD_CMD="/usr/local/libexec/e2e-guard.sh"
ENV TELEGRAM_BASE_URL="http://mocktg:8080"
CMD ["sleep", "infinity"]
