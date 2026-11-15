# Task 147 — Chef: Cinc Client 19 output and the lab drift fixture

- **Milestone:** M14
- **Depends on:** 113, 114
- **Parallel group:** M14-A
- **Status:** todo

## Goal
Two lab tests fail against the lab's Cinc Client 19.4.46:
- `TestLabChefDriftSignals`: the fixture runs `knife exec` with `n.ohai_time = …`, which Cinc 19 rejects
  (`NoMethodError: undefined method 'ohai_time=' for an instance of Chef::Node::Attribute`).
- `TestLabChefWhyRunGroups`: `web-01.lab.example.internal output has no client summary` — the why-run parser in
  `internal/backend/chef/whyrun.go` does not recognise Cinc 19's summary lines (the run prints
  `Infra Phase starting` …).

## Files
- modify `internal/backend/chef/whyrun.go`, `internal/backend/chef/parse_test.go`, `internal/backend/chef/lab_test.go`
- add `testdata/chef/whyrun-cinc19.txt` (captured from the lab, sanitized)

## Requirements
1. Capture a real `chef-client --why-run` output from `web-01` on the lab into testdata; extend the parser so both
   the existing goldens and the Cinc 19 output yield the same summary fields (updated/total resources, would-change
   list, success). Unknown lines are skipped, never fatal.
2. Change the fixture in `lab_test.go` to set the attribute with `n.automatic['ohai_time'] = …` (valid on Chef 18
   and Cinc 19); the product code that reads `ohai_time` does not change.

## Acceptance
```
go test ./internal/backend/chef/ -v
make check
make lab-test   # TestLabChef* pass
```
