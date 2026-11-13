# Task 116 — Plugin output parsers

- **Milestone:** M10
- **Depends on:** 115
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
`parse.type`: `json | jq | regex | columns | kv | lines | raw` → rows (SPEC §12).

## Files
- create `internal/plugin/parse.go`, `internal/plugin/parse_test.go`

## Requirements
`func Parse(def ParseDef, out []byte) ([]map[string]any, error)`:
`json` (object → one row; array of objects → rows); `jq` (gojq with a 2 s timeout and a 10,000-row cap; each
emitted object is a row, scalars become `{"value": …}`); `regex` (per line, named groups; non-matching lines
skipped; optional `multiline: true` applies to the whole output with all matches); `columns` (whitespace split
with optional `header: true`, `names: [...]`, `skip: N`, last column takes the rest); `kv` (separator default
`=` or `:`; one row); `lines` (`{"line": …}`); `raw` (`{"output": …}`, used with `group_by_output`). Values are
typed: integers and floats are detected for `columns`/`regex`/`kv` unless `strings: true`.

## Acceptance
```
go test ./internal/plugin/ -run Parse -v     # table per type incl. the nginx example and a jq error
```

## Pitfalls
- gojq: run with `code.RunWithContext`; iterate results; an `error` value in the iterator is a parse error.
