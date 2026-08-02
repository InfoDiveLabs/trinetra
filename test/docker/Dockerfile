FROM golang:1.24-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
    stress-ng smartmontools docker.io curl sudo procps && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN go build -o /usr/local/bin/serverwatch ./cmd/serverwatch \
 && go build -o /usr/local/bin/mocktg ./test/docker/mocktg
# The telegram client is redirected to the mock via TELEGRAM_BASE_URL (set in compose).
ENV SW_STATE=/var/lib/serverwatch
CMD ["sleep", "infinity"]
