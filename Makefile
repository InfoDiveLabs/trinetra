BIN=serverwatch

.PHONY: test vet build linux web web-cross release validate fmt

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

build:
	go build -o dist/$(BIN) ./cmd/serverwatch

linux:
	GOOS=linux GOARCH=amd64 go build -o dist/$(BIN)-linux-amd64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm64 go build -o dist/$(BIN)-linux-arm64 ./cmd/serverwatch

# web builds the local (host GOOS/GOARCH) serverwatch-web binary: the same
# daemon, plus the embedded web UI (auth, dashboard, config, etc.), linked in
# via the `web` build tag. This is the ONLY build variant whose module graph
# includes go-webauthn/x-crypto-autocert/etc; `build`/`linux` above stay
# stdlib-only.
web:
	go build -tags web -o dist/$(BIN)-web ./cmd/serverwatch

# web-cross builds the full release matrix: serverwatch (stdlib) +
# serverwatch-web (-tags web) for linux amd64/arm64/arm, plus darwin
# (dev/homelab convenience, not part of the linux release set). One
# stdlib build and one -tags web build per platform, so a difference
# between the two is always a `web`-tag-only regression, never a
# cross-compile fluke.
web-cross:
	GOOS=linux GOARCH=amd64 go build -o dist/$(BIN)-linux-amd64 ./cmd/serverwatch
	GOOS=linux GOARCH=amd64 go build -tags web -o dist/$(BIN)-web-linux-amd64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm64 go build -o dist/$(BIN)-linux-arm64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm64 go build -tags web -o dist/$(BIN)-web-linux-arm64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm GOARM=7 go build -o dist/$(BIN)-linux-arm ./cmd/serverwatch
	GOOS=linux GOARCH=arm GOARM=7 go build -tags web -o dist/$(BIN)-web-linux-arm ./cmd/serverwatch
	GOOS=darwin GOARCH=amd64 go build -o dist/$(BIN)-darwin-amd64 ./cmd/serverwatch
	GOOS=darwin GOARCH=amd64 go build -tags web -o dist/$(BIN)-web-darwin-amd64 ./cmd/serverwatch
	GOOS=darwin GOARCH=arm64 go build -o dist/$(BIN)-darwin-arm64 ./cmd/serverwatch
	GOOS=darwin GOARCH=arm64 go build -tags web -o dist/$(BIN)-web-darwin-arm64 ./cmd/serverwatch

# release is the full cut a GitHub release ships: web-cross's whole matrix
# (serverwatch + serverwatch-web, every platform above) plus a
# dist/checksums.txt covering every artifact, so `sha256sum -c checksums.txt`
# verifies a downloaded binary against the same file the release page links.
# rm -f first so a re-run never appends onto (or hashes) a stale checksums.txt
# from a previous invocation.
release: web-cross
	rm -f dist/checksums.txt
	cd dist && sha256sum $(BIN)-* > checksums.txt

validate:
	bash test/docker/scenarios.sh
