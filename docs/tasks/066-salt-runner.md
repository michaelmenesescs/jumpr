# Task 066 — Salt runner: run Salt CLIs on the master and decode their output

- **Milestone:** M4
- **Depends on:** 026, 027
- **Parallel group:** — (serial; first Salt task)
- **Status:** todo

## Goal
A small, tolerant wrapper that builds `salt` / `salt-run` / `salt-key` command lines, runs them on the master
through any `transport.Executor`, and decodes JSON despite noise.

## Context
SPEC §8.6. BUILD-PLAN D9, §12 (risks: Salt output variability, Salt argument parsing).

## Files
- create `internal/backend/salt/runner.go`, `internal/backend/salt/decode.go`
- create `internal/backend/salt/runner_test.go`, `internal/backend/salt/decode_test.go`

## Requirements
1. `type Runner struct`; `func NewRunner(ex transport.Executor, master resource.HostID, o RunnerOptions) *Runner`
   with `RunnerOptions { Sudo bool; Timeout time.Duration; Log *slog.Logger }`.
2. `type Call struct { Tool string; Target *Target; Fun string; Args []string; Kwargs map[string]string; Flags
   []string; Timeout time.Duration }` where `Tool` is `salt`, `salt-run` or `salt-key`, and
   `type Target struct { Type string; Expr string }` with `Type` in `glob|list|compound|grain|pcre|nodegroup`.
3. `func (c Call) CommandLine() (string, error)` — built with `shq.Join`, always in this order:
   `<tool> --out=json --out-indent=-1 --no-color <flags…> <target flag + expr> <fun> <args…> <k=v…>`
   - target flags: glob → none; list → `-L`; compound → `-C`; grain → `-G`; pcre → `-E`; nodegroup → `-N`.
   - `salt-key` and `salt-run` take no target.
   - kwargs sorted by key and rendered as one shell word each: `key=value`.
   - reject (`errs.Config`) a `Fun` not matching `^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)*$`, a kwarg key not
     matching `^[a-z_][a-z0-9_]*$`, and any positional `Args` element that matches `^[A-Za-z_][A-Za-z0-9_]*=`
     (Salt would treat it as a keyword argument; callers must use `Kwargs`).
4. `func (r *Runner) Run(ctx context.Context, c Call) (stdout []byte, err error)` — `Exec` on the master with
   `Sudo` from options; transport errors pass through; rc ≠ 0 with empty stdout → `errs.Remote` with the first
   stderr line; stderr containing `sudo:` patterns → `errs.Sudo`; `salt: command not found` → `errs.NotAvailable`;
   stderr containing `Salt request timed out` or `The salt master could not be contacted` → `errs.Unreachable`.
   A non-zero rc **with** stdout is returned as data (Salt exits non-zero when any minion fails).
5. `decode.go`:
   - `func FirstJSON(b []byte) ([]byte, error)` — skip leading lines until one that starts (after spaces) with `{`
     or `[`; return from there to the end. No JSON → `errs.Parse` with the first 200 bytes.
   - `func DecodeObject(b []byte) (map[string]json.RawMessage, error)` — uses `FirstJSON`, then a `json.Decoder`
     reading **all** concatenated top-level objects and merging their keys (non-static `salt` output is one
     object per minion).
   - `func DecodeInto(b []byte, v any) error` — `FirstJSON` + a single value.
   - `func NoReturn(raw json.RawMessage) bool` — true only for JSON `false`, or a JSON string that starts with
     `Minion did not return` or `No response`. JSON `null` is **not** a no-return (it is a legitimate return
     value of some functions).
   - `func ParseJID(stdout []byte) (string, error)` — first run of exactly 20 digits (works for
     `Executed command with job ID: 20261112135501123456` and for JSON forms).
6. `func (r *Runner) Version(ctx) (string, error)` — `salt --version` → `3007.1`-style string.

## Out of scope
Jobs, targeting logic, any specific Salt function.

## Acceptance
```
go test ./internal/backend/salt/ -run 'CommandLine|Run|FirstJSON|Decode|NoReturn|ParseJID|Version' -v
# CommandLine table with exact strings, e.g.
#   salt --out=json --out-indent=-1 --no-color --async -L web-01,web-02 state.apply test=True
#   salt-run --out=json --out-indent=-1 --no-color jobs.list_job 20261112135501123456
# rejects "a=b" positional; Decode: noise line before JSON, two concatenated objects, pretty-printed object,
# no JSON at all; NoReturn table; Run error mapping with transporttest.Fake
```

## Pitfalls
- Do not build Salt command lines with `fmt.Sprintf`. Only `shq.Join`.
- `--static` is not used here; concatenated objects are handled by `DecodeObject`.
- Never pass user text as a positional argument without the `=` check; use `Kwargs` (e.g. `cmd=…`).
