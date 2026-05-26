.PHONY: build test clean lint ci

VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
LDFLAGS := -ldflags="-s -w -X 'github.com/luuuc/beacon/internal/version.Version=$(VERSION)'"

build:
	go build $(LDFLAGS) -trimpath -o bin/beacon ./cmd/beacon

test:
	go test -v ./...

clean:
	rm -rf bin/ dist/

lint:
	@command -v golangci-lint >/dev/null 2>&1 || \
		(echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)
	@PATH="$$PATH:$$(go env GOPATH)/bin" golangci-lint run

ci: build test lint
	@echo "All CI checks passed!"
