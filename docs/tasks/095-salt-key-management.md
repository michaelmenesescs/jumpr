# Task 095 — Salt key management actions

- **Milestone:** M7
- **Depends on:** 076
- **Parallel group:** M7-B (with 097)
- **Status:** done

## Goal
Accept, reject and delete minion keys through the bus (SPEC §8.6 "Key management").

## Files
- modify `internal/backend/salt/actions.go`, `internal/backend/salt/actions_test.go`
- create `internal/backend/salt/lab_keys_test.go` (`//go:build lab`)

## Requirements
1. Specs: `salt.key.accept`, `salt.key.reject` (destructive), `salt.key.delete` (destructive). Offered on
   `salt-key` rows according to state (pending → accept/reject; accepted → delete; rejected/denied → delete).
2. `Plan`: target = the master host with `Ref` = the key; one step per key; `Display` =
   `salt-key -a <id> -y` (`-r`, `-d`); IDs validated (`^[A-Za-z0-9._-]+$`) — never globs, so `-a '*'` cannot be
   produced. `Preview` lists each key with its fingerprint (`salt-key -f <id>`), and for accept adds
   `verify this fingerprint on the minion: salt-call --local key.finger`. For delete the preview shows the
   minion's last-seen/up state and a warning when it is currently up.
3. `salt.key.delete` always uses `ConfirmTypedCount` semantics: implement by marking the key targets `protected`
   through a planner-provided label `_confirm=typed` that `bus.Propose` honours (add that check to `bus.go` with
   a test: any target label `_confirm=typed` forces `ConfirmTypedCount`).
4. After a run, refresh the keys list and invalidate the accepted-key cache (068).
5. Lab test: create a pending key with `labtest.Break` on a lab host (stop minion, change its id via a
   `minion.d` override, start; undo restores and deletes the stray key) → accept through the bus → minion
   appears up → delete in cleanup. If this proves fragile, skip with a note and add the `pending-key` fault to
   "Requests to the lab track".

## Acceptance
```
go test -race ./internal/backend/salt/ -run 'Key.*Plan|KeyAction' -v
go test -race ./internal/bus/ -run TypedLabel -v
make lab-test
```

## Pitfalls
- `salt-key` without `-y` prompts and would hang until the timeout. Always `-y`, and always exact IDs.
