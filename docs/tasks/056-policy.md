# Task 056 — Policy: deny rules, protected targets, limits, read-only

- **Milestone:** M3
- **Depends on:** 055
- **Parallel group:** M3-B (with 058)
- **Status:** todo

## Goal
Load `policy.yaml` and answer: is this plan allowed, and are any targets protected?

## Context
SPEC §11 (policy), §16 (read-only default in protected contexts). Mockup: `policy.yaml deny: "systemctl stop
sshd"`.

## Files
- create `internal/bus/policy.go`, `internal/bus/policy_test.go`, `internal/bus/testdata/policy.yaml`

## Requirements
1. File format:
   ```yaml
   readonly: false
   max_targets: 0                 # 0 = unlimited
   protected: ["env=prod role=db", "protected"]     # selectors; a target matching any is protected
   deny:
     - pattern: 'systemctl\s+(stop|disable|mask)\s+sshd'     # regexp on Step.Display
       reason: never cut off SSH
     - spec: salt.key.delete                               # exact Spec.ID or glob ("salt.*")
       selector: "env=prod"                                # optional: only for matching targets
       reason: key deletes in prod go through change management
   allow_root_transport: true     # false = plans that run via Salt (root) are denied
   shell_in_readonly: false
   sync:
     raw: confirm                 # confirm | deny
   ```
2. `func LoadPolicy(path string) (*Policy, error)` — missing file → `DefaultPolicy()` which contains the built-in
   deny rules: `systemctl (stop|disable|mask) (sshd|ssh)(\.service)?`, `\brm\s+-[a-z]*r[a-z]*f?\s+/(\s|$)`,
   `\b(shutdown|reboot|halt|poweroff)\b` (reason: "use your change process"), `\bmkfs\b`, `\bdd\s+.*of=/dev/`.
   User rules are added to the built-ins; a rule with `disable_builtin: <name>` is **not** supported (keep it
   simple). Unknown keys are errors.
3. `func (p *Policy) Check(plan action.Plan, readonlyOverride bool) *Denial`:
   order: read-only (policy or override) → `max_targets` → `allow_root_transport` (when `plan.Via` starts with
   `salt`) → deny rules (a `pattern` rule fires if any step's `Display` matches; a `spec` rule fires on
   `plan.Spec.ID` and, if it has a selector, only when some target's labels match). The `Denial.Rule` is the
   rule's text as it would appear in the file; `Reason` as given.
4. `func (p *Policy) Protected(t action.Target) bool` — any `protected` selector matches the target labels.
5. `func (p *Policy) CheckLine(line string) *Denial` — pattern rules only (for synchronized input, task 064).
6. Hot values are read under a lock; `func (p *Policy) Reload(path string) error`.

## Out of scope
Hot-reload watching. Per-context policies (`--env`).

## Acceptance
```
go test ./internal/bus/ -run Policy -v
# TestDefaultDeniesStopSSHD ("sudo -n systemctl stop sshd.service" → denied),
# TestDefaultAllowsRestartNginx, TestReadOnlyDeniesEverything, TestMaxTargets, TestSpecRuleWithSelector,
# TestProtectedSelector, TestRootTransportDenied, TestCheckLine, TestUnknownKeyIsError
```

## Pitfalls
- Deny patterns match the **display** string, which includes `sudo -n --` prefixes and quoting; write patterns
  with `\s+` and without anchors.
- Policy is a guard rail against mistakes, not a sandbox: do not claim otherwise in messages.
