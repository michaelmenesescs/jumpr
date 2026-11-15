VERSION ?= dev
GO ?= go

.PHONY: build test lint lab-test homelab-test check tidy

build:
	CGO_ENABLED=0 $(GO) build -ldflags "-X main.version=$(VERSION)" -o jumpr ./cmd/jumpr

test:
	$(GO) test -race ./...

lint:
	test -z "$$(gofmt -l .)"
	$(GO) vet ./...

lab-test:
	$(GO) test -tags lab -run 'TestLab' -count=1 ./...

# Homelab suite: a real Ubuntu node over SSH, read-only. Needs
# JUMPR_HOMELAB_UBUNTU set to an ssh_config alias; never part of `check`.
homelab-test:
	$(GO) test -tags homelab -count=1 ./internal/transport/ssh/ ./internal/backend/linux/ -v

check: lint test build

tidy:
	$(GO) mod tidy
