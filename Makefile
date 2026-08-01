BIN=serverwatch
CTL_BIN=serverwatch-ctl
WEB_BIN=serverwatch-web

.PHONY: test vet build linux cross release validate fmt

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
# because of any tag.
build:
	go build -o dist/$(BIN) ./cmd/serverwatch
	go build -o dist/$(CTL_BIN) ./cmd/serverwatch-ctl
	go build -o dist/$(WEB_BIN) ./cmd/serverwatch-web

linux:
	GOOS=linux GOARCH=amd64 go build -o dist/$(BIN)-linux-amd64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm64 go build -o dist/$(BIN)-linux-arm64 ./cmd/serverwatch

# cross builds the full release matrix: all three binaries (serverwatch,
# serverwatch-ctl, serverwatch-web), each from its own ./cmd directory with
# no build tag, for linux amd64/arm64/arm, plus darwin amd64/arm64
# (dev/homelab convenience, not part of the linux release set).
cross:
	GOOS=linux GOARCH=amd64 go build -o dist/$(BIN)-linux-amd64 ./cmd/serverwatch
	GOOS=linux GOARCH=amd64 go build -o dist/$(CTL_BIN)-linux-amd64 ./cmd/serverwatch-ctl
	GOOS=linux GOARCH=amd64 go build -o dist/$(WEB_BIN)-linux-amd64 ./cmd/serverwatch-web
	GOOS=linux GOARCH=arm64 go build -o dist/$(BIN)-linux-arm64 ./cmd/serverwatch
	GOOS=linux GOARCH=arm64 go build -o dist/$(CTL_BIN)-linux-arm64 ./cmd/serverwatch-ctl
	GOOS=linux GOARCH=arm64 go build -o dist/$(WEB_BIN)-linux-arm64 ./cmd/serverwatch-web
	GOOS=linux GOARCH=arm GOARM=7 go build -o dist/$(BIN)-linux-arm ./cmd/serverwatch
	GOOS=linux GOARCH=arm GOARM=7 go build -o dist/$(CTL_BIN)-linux-arm ./cmd/serverwatch-ctl
	GOOS=linux GOARCH=arm GOARM=7 go build -o dist/$(WEB_BIN)-linux-arm ./cmd/serverwatch-web
	GOOS=darwin GOARCH=amd64 go build -o dist/$(BIN)-darwin-amd64 ./cmd/serverwatch
	GOOS=darwin GOARCH=amd64 go build -o dist/$(CTL_BIN)-darwin-amd64 ./cmd/serverwatch-ctl
	GOOS=darwin GOARCH=amd64 go build -o dist/$(WEB_BIN)-darwin-amd64 ./cmd/serverwatch-web
	GOOS=darwin GOARCH=arm64 go build -o dist/$(BIN)-darwin-arm64 ./cmd/serverwatch
	GOOS=darwin GOARCH=arm64 go build -o dist/$(CTL_BIN)-darwin-arm64 ./cmd/serverwatch-ctl
	GOOS=darwin GOARCH=arm64 go build -o dist/$(WEB_BIN)-darwin-arm64 ./cmd/serverwatch-web

# release is the full cut a GitHub release ships: cross's whole matrix
# (serverwatch, serverwatch-ctl, serverwatch-web, every platform above) plus
# a dist/checksums.txt covering every artifact, so sha256sum -c checksums.txt
# verifies a downloaded binary against the same file the release page links.
# rm -f first so a re-run never appends onto (or hashes) a stale checksums.txt
# from a previous invocation.
release: cross
	rm -f dist/checksums.txt
	cd dist && sha256sum $(BIN)-* > checksums.txt

validate:
	bash test/docker/scenarios.sh
