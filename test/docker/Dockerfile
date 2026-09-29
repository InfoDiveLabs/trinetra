# syntax=docker/dockerfile:1
# Image for the fleet e2e harness (fleet/compose.yml) and the older
# single-host scenarios (docker-compose.yml). Build context: the repo root.
FROM golang:1.24-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
    stress-ng smartmontools docker.io curl sudo procps && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
# BuildKit cache mounts keep Go's module and build caches across image
# builds (COPY . . above invalidates this layer on any repo change, so layer
# caching alone recompiles everything every run). tscmp (the fleet harness's
# replica comparer) is built here too rather than inside a running
# container, where the build cache is not mounted.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -o /usr/local/bin/trinetra ./cmd/trinetra \
 && go build -o /usr/local/bin/trinetra-ctl ./cmd/trinetra-ctl \
 && go build -o /usr/local/bin/trinetra-web ./cmd/trinetra-web \
 && go build -o /usr/local/bin/mocktg ./test/docker/mocktg \
 && go build -o /usr/local/bin/tscmp ./test/docker/fleet/tscmp
# The telegram client is redirected to the mock via TELEGRAM_BASE_URL (set in compose).
ENV SW_STATE=/var/lib/trinetra
CMD ["sleep", "infinity"]
