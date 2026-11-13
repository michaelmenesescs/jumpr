# Task 115 — YAML plugin schema, loader, validation

- **Milestone:** M10
- **Depends on:** 065
- **Parallel group:** M10-A (with 113)
- **Status:** todo

## Goal
Load `~/.config/sitrep/plugins/*.yaml` (SPEC §12) into validated definitions with helpful errors.

## Files
- modify `go.mod`/`go.sum`, `internal/deps/deps.go` (`go get github.com/itchyny/gojq@v0.12.20`)
- create `internal/plugin/schema.go`, `load.go`, `_test.go` files, `internal/plugin/testdata/*.yaml`

## Requirements
1. Structs for both examples in SPEC §12 (`apiVersion: sitrep/v1`, `kind: Plugin`, `name`, `description`,
   `target: host|k8s|http|inventory`, `privileged`, `command`, `params`, `parse`, `view`, `links`, `actions`,
   `connection`, `request`, `emits_hosts`). Strict decoding.
2. Validation with file, line and field in every message: name `^[a-z][a-z0-9-]*$` and unique; parse type known;
   regex compiles and has named groups; jq compiles (`gojq.Parse`); view columns exist in the parse output
   definition; `command` must not be empty for host plugins; actions require `confirm: true` semantics (always
   through the bus regardless); `{{ .Param "x" }}` templates reference declared params; a `command` template may
   only use `{{ .Param }}` and `{{ .Host }}` and each substitution is shell-quoted by the engine.
3. `func LoadDir(dir string) (ok []Definition, problems []Problem)` — one bad file never hides the others.
4. Hot reload: `func Watch(ctx, dir string, every time.Duration, onChange func())` polling mtimes (no fsnotify
   dependency).

## Acceptance
```
go test ./internal/plugin/ -run 'Load|Validate|Watch' -v     # both SPEC examples load; 12 broken files each give
                                                             # one precise problem
```

## Pitfalls
- Template substitution into shell must quote: `{{ .Param "pool" }}` with value `a; rm -rf /` becomes one
  argument.
