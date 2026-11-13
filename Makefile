VERSION ?= dev
GO ?= go

.PHONY: build test lint lab-test check tidy

build:
	CGO_ENABLED=0 $(GO) build -ldflags "-X main.version=$(VERSION)" -o jumper ./cmd/jumper

test:
	$(GO) test -race ./...

lint:
	test -z "$$(gofmt -l .)"
	$(GO) vet ./...

lab-test:
	$(GO) test -tags lab -run 'TestLab' -count=1 ./...

check: lint test build

tidy:
	$(GO) mod tidy
