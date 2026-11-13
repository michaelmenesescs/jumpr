# Task 001 — Repo bootstrap: module, Makefile, pinned dependencies

- **Milestone:** M0
- **Depends on:** —
- **Parallel group:** — (serial)
- **Status:** done

## Goal
A Go module that builds a `sitrep` binary printing its version, a Makefile with the standard targets, and every
third-party dependency for M0–M7 pinned in `go.mod` so no later task needs `go get`.

## Context
BUILD-PLAN §1 (D1, D2, D11, D13), §9 (standard commands), §10 (dependency table).

## Files
- modify `go.mod`; create `go.sum`
- create `Makefile`
- create `cmd/sitrep/main.go`
- create `internal/deps/deps.go`
- modify `.gitignore`

## Requirements
1. `go.mod` already exists (module `sitrep`, created by the lab track together with `cmd/sitrep-lab`). Keep the
   module path and the `go` line as they are. Do not touch `cmd/sitrep-lab/` or `lab/`.
2. Run exactly (versions are pinned on purpose):
   ```
   go get github.com/charmbracelet/bubbletea@v1.3.10 github.com/charmbracelet/lipgloss@v1.1.0 \
     github.com/charmbracelet/bubbles@v1.0.0 github.com/charmbracelet/x/ansi@v0.11.9 \
     github.com/charmbracelet/x/exp/teatest@v0.0.0-20261101151554-27ecf3e4d454 \
     github.com/charmbracelet/x/vt@v0.1.0 github.com/creack/pty@v1.1.24 github.com/gliderlabs/ssh@v0.3.8 \
     github.com/kevinburke/ssh_config@v1.6.0 github.com/mattn/go-runewidth@v0.0.31 \
     github.com/pmezard/go-difflib@v1.0.0 github.com/skeema/knownhosts@v1.3.3 golang.org/x/crypto@v0.59.0 \
     golang.org/x/sync@v0.25.0 golang.org/x/sys@v0.50.0 golang.org/x/term@v0.48.0 gopkg.in/yaml.v3@v3.0.1
   ```
3. `internal/deps/deps.go`: first line `//go:build deps`, `package deps`, and one blank import per dependency so
   `go mod tidy` keeps them. Import these exact packages: `github.com/charmbracelet/bubbletea`,
   `github.com/charmbracelet/lipgloss`, `github.com/charmbracelet/bubbles/textinput`,
   `github.com/charmbracelet/x/ansi`, `github.com/charmbracelet/x/exp/teatest`, `github.com/charmbracelet/x/vt`,
   `github.com/creack/pty`, `github.com/gliderlabs/ssh`, `github.com/kevinburke/ssh_config`,
   `github.com/mattn/go-runewidth`, `github.com/pmezard/go-difflib/difflib`, `github.com/skeema/knownhosts`,
   `golang.org/x/crypto/ssh`, `golang.org/x/crypto/ssh/agent`, `golang.org/x/sync/errgroup`,
   `golang.org/x/sys/unix`, `golang.org/x/term`, `gopkg.in/yaml.v3`.
4. `cmd/sitrep/main.go`: `var version = "dev"` (set with `-ldflags "-X main.version=…"`). `sitrep version` and
   `sitrep --version` print `sitrep <version>` and exit 0. Any other invocation prints
   `sitrep: not implemented yet` to stderr and exits 2 (task 016 replaces this).
5. `Makefile` targets (each recipe is a single `go`/`gofmt` invocation; no shell scripts):
   - `build`: `CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o sitrep ./cmd/sitrep` with
     `VERSION ?= dev`
   - `test`: `go test -race ./...`
   - `lint`: fail if `gofmt -l .` prints anything, then `go vet ./...`
   - `lab-test`: `go test -tags lab -run 'TestLab' -count=1 ./...`
   - `check`: `lint test build`
   - `tidy`: `go mod tidy`
6. `.gitignore`: keep existing lines (the lab entries are already there); add `/sitrep-lab`, `/sitrep-dev`,
   `/lab/.knowledge/`, `*.test`, `coverage.out`.

## Out of scope
Any TUI code. CI configuration. `cmd/sitrep-lab` (owned by the lab track; do not create or edit it).

## Acceptance
```
make check                      # exits 0
./sitrep version                # prints: sitrep dev
go mod tidy && git diff --exit-code go.mod go.sum   # tidy removes nothing
CGO_ENABLED=0 GOARCH=amd64 go build -o /dev/null ./cmd/sitrep
go vet -tags deps ./internal/deps/
go build ./cmd/sitrep-lab        # the lab tool still builds
```

## Pitfalls
- Do not use `charm.land/bubbletea/v2` or any `/v2` Charm module. v1 import paths only.
- The `lint` recipe must fail on unformatted files: `test -z "$$(gofmt -l .)"`.
- Makefile recipes are indented with a tab.
- Do not add cobra, viper, testify, zap or logrus.
