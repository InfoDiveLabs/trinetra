# syntax=docker/dockerfile:1
# Image for the serverwatch -> trinetra migration scenario (run.sh).
#
# Build context: the repository root (the current, trinetra-named tree).
# Named context "legacysrc": the source of the last serverwatch-named commit,
# exported by run.sh with `git archive` (the build cannot run git itself).
#
#   docker build -f test/docker/migration/Dockerfile \
#     --build-context legacysrc=<dir> -t trmig-e2e:latest .

# Both Go stages use BuildKit cache mounts for the module and build caches,
# so a rebuild after a repo change (COPY . . invalidates the layer) does not
# recompile every dependency from scratch.

# Legacy serverwatch build (module "serverwatch", cmd/serverwatch*), the way a
# v0.4.x user has it installed.
FROM golang:1.24-bookworm AS legacy
WORKDIR /legacy
COPY --from=legacysrc . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -o /out/serverwatch ./cmd/serverwatch \
 && go build -o /out/serverwatch-ctl ./cmd/serverwatch-ctl \
 && go build -o /out/serverwatch-web ./cmd/serverwatch-web

# Current trinetra build plus the mock Telegram API.
FROM golang:1.24-bookworm AS current
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -o /out/trinetra ./cmd/trinetra \
 && go build -o /out/trinetra-ctl ./cmd/trinetra-ctl \
 && go build -o /out/trinetra-web ./cmd/trinetra-web \
 && go build -o /mocktg ./test/docker/mocktg

# A systemd host: run.sh starts it privileged with /sbin/init as PID 1, so
# `serverwatch install` and `trinetra install` really drive systemctl.
FROM debian:bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
    systemd systemd-sysv dbus curl jq procps ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty@.service console-getty.service systemd-logind.service \
      systemd-timesyncd.service apt-daily.timer apt-daily-upgrade.timer \
 && mkdir -p /etc/systemd/system.conf.d \
 && printf '[Manager]\nDefaultEnvironment=TELEGRAM_BASE_URL=http://mocktg:8080\n' \
      > /etc/systemd/system.conf.d/mocktg.conf
# Each binary directory holds the daemon next to its plugins, as a release
# tarball would unpack; install copies the plugins it finds alongside itself.
COPY --from=legacy /out/ /opt/legacy/
COPY --from=current /out/ /opt/trinetra/
COPY --from=current /mocktg /usr/local/sbin/mocktg
ENV container=docker
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
