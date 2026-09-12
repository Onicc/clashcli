VERSION ?= $(shell git describe --tags --always --dirty)

.PHONY: build test check audit install release e2e
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o bin/clashcli ./cmd/clashcli
test:
	go test -race ./...
	python3 tests/test_install.py
check:
	go vet ./...
	test -z "$$(gofmt -l cmd internal)"
	sh -n install.sh
audit:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
install: build
	install -m 0755 bin/clashcli /usr/local/bin/clashcli
release:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o dist/clashcli-linux-amd64 ./cmd/clashcli
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o dist/clashcli-linux-arm64 ./cmd/clashcli
e2e: release
	python3 tests/run_linux.py
