BINARY  := qbt-replicator
PKG     := github.com/williamokano/qbt-replicator
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(VERSION) \
           -X $(PKG)/internal/version.Commit=$(COMMIT) \
           -X $(PKG)/internal/version.Date=$(DATE)
IMAGE   ?= ghcr.io/williamokano/qbt-replicator

.PHONY: all build test cover lint fmt tidy run docker clean

all: fmt lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/qbt-replicator

test:
	go test -race -cover ./...

cover:
	go test -race -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

# Run locally against ./config.yaml (copy config.example.yaml first).
run: build
	./bin/$(BINARY) --config ./config.yaml serve

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMAGE):dev .

clean:
	rm -rf bin dist coverage.out
