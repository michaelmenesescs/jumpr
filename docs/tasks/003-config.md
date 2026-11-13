# Task 003 — Config paths, schema, loader, validation

- **Milestone:** M0
- **Depends on:** 002
- **Parallel group:** M0-A (with 004, 008, 009)
- **Status:** done

## Goal
Load `config.yaml` into typed structs with defaults and clear validation errors.

## Context
BUILD-PLAN §11 (schema and defaults — implement every key shown there), SPEC §14.

## Files
- create `internal/config/config.go` (structs + `Default()`)
- create `internal/config/load.go` (`Paths`, `Load`)
- create `internal/config/duration.go` (YAML duration type)
- create `internal/config/config_test.go`
- create `internal/config/testdata/full.yaml`, `internal/config/testdata/bad-prefix.yaml`

## Requirements
1. `type Paths struct { ConfigDir, CacheDir, KnowledgeRoot string }` and
   `func ResolvePaths(home, configDirOverride string) Paths`: config dir default `<home>/.config/jumper`, cache
   dir `<home>/.cache/jumper`. `home` is a parameter (rule §6.1: no `os.Getenv` here).
2. `type Config struct { UI, Mux, SSH, Inventory, Knowledge, Rollout, Salt, Interact }` with nested structs and
   `yaml:"…"` tags matching BUILD-PLAN §11 exactly.
3. `type Duration time.Duration` implementing `yaml.Unmarshaler` (accepts `"10s"`, `"5m"`, `"2h"`) and
   `MarshalYAML`.
4. `func Default() Config` returns every default from BUILD-PLAN §11.
5. `func Load(path string) (Config, error)`: a missing file returns `Default()` and no error. Otherwise decode
   onto a copy of `Default()` with `yaml.Decoder.KnownFields(true)` so typos are errors. All errors are
   `*errs.Error{Kind: errs.Config}` and include the file path and, where YAML gives one, the line.
6. `func (c *Config) Validate() error`: `mux.prefix` and `mux.prefix_in_tmux` match `^C-[a-z\\\]\^_@]$`;
   positive numbers for limits and timeouts; `ui.color` in the allowed set; `inventory.sources[].type` in
   `ssh_config|static|salt|k8s|prom|chef`; `static` requires `path`; identity rewrites compile as regexps.
7. `func ExpandHome(p, home string) string` turns a leading `~/` into the home dir; `Load` applies it to path
   fields (`ssh.config_files`, `ssh.known_hosts`, `inventory.sources[].path`, `knowledge.root`) — pass `home`
   into `Load` as a second parameter: `Load(path, home string)`.

## Out of scope
`policy.yaml`, `connections.yaml`, `keymap.yaml`, plugins, `jumper init`, environments (`--env`).

## Acceptance
```
go test ./internal/config/ -run . -v
# must include: TestLoadMissingFileGivesDefaults, TestLoadFull, TestUnknownKeyIsError,
# TestBadPrefixIsConfigError, TestDurationRoundTrip, TestExpandHome
make lint
```

## Pitfalls
- `KnownFields(true)` is on the decoder, not on `yaml.Unmarshal`.
- Decode onto the defaults so omitted keys keep their default; do not decode into a zero struct.
- Do not read environment variables or the real home directory in this package.
