GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test check clean
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o bin/arnold ./cmd/arnold

test:
	$(GO) test -race ./...

check:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin
