SHELL := /bin/bash

GO        ?= go
BIN       ?= flotilla
PKG       := github.com/mamidevs/flotilla
LDFLAGS   ?= -s -w \
              -X $(PKG)/internal/buildinfo.AppVersion=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
              -X $(PKG)/internal/buildinfo.CommitHash=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
              -X $(PKG)/internal/buildinfo.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ) \
              -X $(PKG)/internal/buildinfo.BuildId=make \
              -X $(PKG)/internal/buildinfo.Production=1

.PHONY: help build run test lint tidy fmt vet clean docker docker-up docker-down example

help:
	@echo "flotilla — Makefile"
	@echo ""
	@echo "Common targets:"
	@echo "  make build         Build ./$(BIN)"
	@echo "  make run           Run with examples/single-node.yaml (needs Tailnet)"
	@echo "  make test          go test -race ./..."
	@echo "  make lint          golangci-lint run"
	@echo "  make tidy          go mod tidy"
	@echo "  make vet           go vet ./..."
	@echo "  make fmt           gofmt -s -w ."
	@echo ""
	@echo "Docker:"
	@echo "  make docker        Build local container image"
	@echo "  make docker-up     docker compose up -d"
	@echo "  make docker-down   docker compose down"
	@echo ""
	@echo "Scaffold:"
	@echo "  make example       flotilla init -o flotilla.yaml"

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='$(LDFLAGS)' -o ./$(BIN) ./cmd/flotilla

run: build
	./$(BIN) run --config examples/single-node.yaml

test:
	$(GO) test -race -count=1 ./...

lint:
	golangci-lint run

tidy:
	$(GO) mod tidy

fmt:
	gofmt -s -w .

vet:
	$(GO) vet ./...

clean:
	rm -f ./$(BIN)
	rm -rf ./state ./tsnet-state ./flotilla-state

docker:
	docker build -t flotilla:dev .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

example: build
	./$(BIN) init -o flotilla.yaml
