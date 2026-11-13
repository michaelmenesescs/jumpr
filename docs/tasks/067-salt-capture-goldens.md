# Task 067 — Capture Salt golden files from the lab (Go tool)

- **Milestone:** M4
- **Depends on:** 053, 066
- **Parallel group:** — (serial; needs the lab)
- **Status:** todo

## Goal
Real Salt CLI output in `testdata/salt/`, so every Salt parser is written against reality instead of memory.

## Context
BUILD-PLAN §12 (risk: Salt output variability), D13. Lab: master `salt.lab.example.internal` (Rocky 9), minions
on every lab host with grains `role` and `dc`, nodegroups `webs` and `k8s`, states `common`, `nginx`, `haproxy`,
`postgres`, a pillar containing a fake secret.

## Files
- modify `cmd/sitrep-dev/main.go`, create `internal/devtools/capture_salt.go`
- add `testdata/salt/*.json|*.txt` (generated), create `testdata/salt/README.md` (one line per file: the exact
  command that produced it)

## Requirements
1. `go run ./cmd/sitrep-dev capture salt [--config-dir lab/sitrep] [--out testdata/salt]` runs through
   `salt.Runner` (so the files are exactly what the code will see) and writes:
   `version.txt`; `key-list.json` (`salt-key -L`); `key-finger.json` (`salt-key -F`); `manage-status.json`;
   `manage-versions.json`; `cache-grains.json` (`salt-run cache.grains tgt='*'`); `nodegroups.json`
   (`salt-run config.get nodegroups`); `preview-compound.txt` and `.json` (`salt -C 'G@role:web' --preview-target`
   without and with `--out=json`); `async-submit.txt` (`salt --async '*' test.ping`); `list-job-ping.json` (that
   jid, after it finished); `jobs-active.json` (captured while a `test.sleep 20` job runs on two minions);
   `jobs-list.json` (`jobs.list_jobs`); `cmd-run-all.json` (`cmd.run_all cmd='echo out; echo err >&2; exit 3'
   python_shell=True` on two minions, via list_job); `state-test-clean.json` and `state-test-changes.json`
   (`state.apply test=True` before and after creating drift on `web-01` by appending a comment line to a
   Salt-managed file, e.g. the nginx config; via list_job);
   `state-render-error.json` (apply a non-existent sls: `state.sls nosuch test=True`); `show-top.json`;
   `show-sls-nginx.json`; `show-highstate.json` (one minion); `pillar-items.json` (**after sanitizing**: every
   leaf value replaced by `REDACTED-<type>`); `mine.json`; `fileserver-envs.json`; `fileserver-file-list.json`;
   `event-sample.txt` (10 s of `salt-run state.event pretty=False` while a `test.ping` runs);
   `no-return.json` (a job targeting a minion whose `salt-minion` service the tool stopped for the capture and
   restarts afterwards).
2. Every file goes through `devtools.Sanitize`; pillar output additionally through a value scrubber
   (`ScrubValues(json) json`) with a unit test.
3. Faults are injected and undone through `go run ./cmd/sitrep-lab fault <name>` / `--undo` where one exists;
   otherwise the capture tool makes the lab-only change itself over SSH (stop a minion, edit a managed file to
   create drift) and always restores it, also on failure.
4. If a command fails or the output shape differs from what tasks 068–075 describe, **record it** in
   `testdata/salt/README.md` under "Deviations" and in `docs/HANDOFF.md`. The later tasks must follow the
   captured files, not their inline samples.

## Out of scope
Parsers. Changing the lab.

## Acceptance
```
go test ./internal/devtools/ -run 'Scrub|Sanitize' -v
go run ./cmd/sitrep-dev capture salt --config-dir lab/sitrep      # lab up: exit 0, files written
ls testdata/salt | wc -l                                          # >= 24
grep -ril 'fake-secret\|BEGIN.*PRIVATE' testdata/salt ; test $? -eq 1
```

## Pitfalls
- The pillar file must never contain the lab's fake secret; the redaction tests in task 093 use the scrubbed
  shape plus their own inline secret.
- If the lab is not available: stop and report in `docs/HANDOFF.md` ("Blocked: lab needed for 067"). Tasks 068+
  can proceed on inline samples but must be re-verified once the captures exist.
