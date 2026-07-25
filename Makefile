BIN=serverwatch

.PHONY: test vet build linux validate fmt

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

validate:
	bash test/docker/scenarios.sh
