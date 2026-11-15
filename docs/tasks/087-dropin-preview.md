# Task 087 — Drop-in override preview (no remote write)

- **Milestone:** M6
- **Depends on:** 086
- **Parallel group:** — (serial)
- **Status:** done

## Goal
SPEC §8.1.1 "Edit (drop-in override): preview only in v1".

## Context
SPEC §20 open question 7. The zero-footprint rule: this task must not write on the node.

## Files
- create `internal/tui/views/dropin.go`, `internal/tui/views/dropin_test.go`
- create `internal/backend/linux/dropin.go`, `internal/backend/linux/dropin_test.go`

## Requirements
1. From the unit detail `cat` tab, `E` opens the drop-in editor: a multi-line text input (use
   `bubbles/textarea`) pre-filled with `[Service]\n`, or with the existing
   `override.conf` content if one exists.
2. `func ValidateDropIn(text string) []string` — warnings: no section header, unknown section (not one of
   `Unit Service Socket Timer Install Mount Path Slice`), lines without `=`, `ExecStart=` added without an empty
   `ExecStart=` reset line before it.
3. `ctrl+s` shows the preview (a text view): the unified diff between the current `systemctl cat` output and
   what it would be with the new drop-in, the warnings, and a "to apply by hand" block with the exact commands
   (`sudo systemctl edit <unit>` and the content, or the `tee` + `daemon-reload` + `restart` sequence). A line at
   the top states: `preview only — jumpr does not write files on nodes`.
4. `y` in the preview copies the "apply by hand" block to the clipboard. The preview is recorded as a `note`
   entry when an investigation is open (text + diff attached).
5. No `action.Spec` is added. No remote command other than the `cat` already loaded.

## Acceptance
```
go test ./internal/backend/linux/ -run DropIn -v
go test -race ./internal/tui/views/ -run DropIn -v      # edit → preview contains diff and the banner line;
                                                        # fake executor recorded zero write-like commands
```

## Pitfalls
- Do not implement apply "just in case". It is an open question for the user.
