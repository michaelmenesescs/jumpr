# Task 093 — Pillar view, redacted by default

- **Milestone:** M7
- **Depends on:** 092
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`:pillar <minion>`: structure and keys visible, values hidden unless explicitly revealed (SPEC §8.6 "Pillar").

## Files
- create `internal/backend/salt/pillar.go`, `internal/backend/salt/pillar_test.go`
- create `internal/tui/views/saltpillar.go`, `internal/tui/views/saltpillar_test.go`
- modify `internal/tui/commands_salt.go`

## Requirements
1. `func (b *Backend) PillarKeys(ctx, minion) (PillarTree, error)` — runs `pillar.items` as a job and immediately
   converts the return into a tree of `PillarNode{Key string; Type string (dict|list|str|int|bool|null); Len int;
   Children []PillarNode}`. **Leaf values are dropped inside this function**; the raw JSON is zeroed and not
   retained, logged or returned.
2. View: an expandable tree: `nginx:` → `tls:` → `key  ‹str, 1,704 chars›`. `/` filters by key path.
3. Reveal: `R` on a leaf → y/N dialog (`reveal pillar value nginx:tls:key on web-01? it will be shown on screen`)
   → `pillar.get <path>` for that one key → shown in a dialog only; closed with `esc`; never copied by copy mode
   (`CopyLines` of the view never includes values), never recorded (the timeline gets `pillar value revealed:
   <path>` without the value). Reveal is refused in read-only mode.
4. Compare: `:pillar-diff <a> <b>` shows which key paths and types differ between two minions (no values).
5. Debug logging in the runner must skip stdout for `pillar.*` calls (add a `Sensitive bool` field to `Call`
   that suppresses logging of output and marks the job arguments/returns as hidden in the jobs browser:
   job detail for `pillar.*` functions shows `‹pillar data hidden›` instead of returns).

## Acceptance
```
go test ./internal/backend/salt/ -run Pillar -v
# TestPillarKeysDropsValues: input contains "s3cr3t-value" → no byte of it in the returned tree (search the
#   %#v of the result), TestSensitiveCallNotLogged (slog handler capturing output), TestJobDetailHidesPillar
go test -race ./internal/tui/views/ -run Pillar -v      # tree renders types and lengths; reveal flow; CopyLines
                                                        # never contains the secret
```

## Pitfalls
- The pillar passes through the master's job cache and the SSH stream regardless; jumpr's promise is only that
  it does not persist or display it by accident. State that in the reveal dialog.
