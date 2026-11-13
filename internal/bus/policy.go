package bus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// Policy is the guard rail described by policy.yaml (SPEC §11). It answers two
// questions: may this plan run, and is this target protected? It is a guard
// rail against mistakes, not a sandbox.
//
// The cached values are read under a lock, so a Reload can swap them while
// the UI and the bus are asking questions.
type Policy struct {
	mu sync.RWMutex
	st policyState
}

// policyState is the immutable set of values a loaded policy answers with.
type policyState struct {
	readonly        bool
	maxTargets      int
	protected       []resource.Selector
	allowRoot       bool
	shellInReadonly bool
	syncRaw         string
	rules           []policyRule
}

// policyRule is one deny rule: either a regexp on a step's Display, or a spec
// ID (exact or glob) with an optional selector on the targets.
type policyRule struct {
	text     string // as it would appear in policy.yaml
	reason   string
	pattern  *regexp.Regexp
	spec     string
	selector *resource.Selector
}

// policyDoc is the on-disk shape. Pointers tell an omitted key from a set one.
type policyDoc struct {
	ReadOnly           *bool     `yaml:"readonly"`
	MaxTargets         *int      `yaml:"max_targets"`
	Protected          []string  `yaml:"protected"`
	Deny               []denyDoc `yaml:"deny"`
	AllowRootTransport *bool     `yaml:"allow_root_transport"`
	ShellInReadOnly    *bool     `yaml:"shell_in_readonly"`
	Sync               *syncDoc  `yaml:"sync"`
}

// denyDoc is one entry of the deny list.
type denyDoc struct {
	Pattern        string `yaml:"pattern"`
	Spec           string `yaml:"spec"`
	Selector       string `yaml:"selector"`
	Reason         string `yaml:"reason"`
	DisableBuiltin string `yaml:"disable_builtin"`
}

// syncDoc is the sync section; only raw is defined today.
type syncDoc struct {
	Raw string `yaml:"raw"`
}

// LoadPolicy reads policy.yaml. A missing file yields DefaultPolicy(). User
// deny rules are added to the built-ins; unknown keys, bad regexps, bad
// selectors and unsupported keys are *errs.Error with kind Config.
func LoadPolicy(path string) (*Policy, error) {
	st, err := loadPolicyState(path)
	if err != nil {
		return nil, err
	}
	return &Policy{st: st}, nil
}

// DefaultPolicy returns the built-in policy: writes allowed, no target limit,
// labels named "protected" are protected, and the built-in deny rules that
// stop SSH, wipe a filesystem or write raw to a device.
func DefaultPolicy() *Policy {
	return &Policy{st: defaultPolicyState()}
}

// Reload re-reads path and swaps it in atomically. A missing file restores
// DefaultPolicy(). The old values stay in force when the new ones are invalid.
func (p *Policy) Reload(path string) error {
	st, err := loadPolicyState(path)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.st = st
	p.mu.Unlock()
	return nil
}

// Check reports why plan may not run, or nil when policy allows it. The order
// is read-only, then max_targets, then allow_root_transport for plans that run
// through Salt, then the deny rules. A pattern rule fires when any step's
// Display matches; a spec rule fires on the plan's spec ID and, when it has a
// selector, only if some target matches it.
func (p *Policy) Check(plan action.Plan, readonlyOverride bool) *Denial {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st := &p.st

	if st.readonly || readonlyOverride {
		return &Denial{Rule: "readonly: true", Reason: "read-only mode: writes are disabled"}
	}

	n := len(targetsOf(plan))
	if st.maxTargets > 0 && n > st.maxTargets {
		return &Denial{
			Rule:   fmt.Sprintf("max_targets: %d", st.maxTargets),
			Reason: fmt.Sprintf("plan has %d targets; policy allows at most %d", n, st.maxTargets),
		}
	}

	if !st.allowRoot && strings.HasPrefix(plan.Via, "salt") {
		return &Denial{
			Rule:   "allow_root_transport: false",
			Reason: "plans that run through Salt run as root; denied by policy",
		}
	}

	for i := range st.rules {
		r := &st.rules[i]
		if r.pattern != nil {
			if !anyDisplayMatches(plan, r.pattern) {
				continue
			}
			return &Denial{Rule: r.text, Reason: r.reason}
		}
		if !specMatches(r.spec, plan.Spec.ID) {
			continue
		}
		if r.selector != nil && !anyTargetMatches(plan, *r.selector) {
			continue
		}
		return &Denial{Rule: r.text, Reason: r.reason}
	}
	return nil
}

// Protected reports whether any protected selector matches the target's
// labels.
func (p *Policy) Protected(t action.Target) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.st.protected {
		if p.st.protected[i].Matches(t.Labels) {
			return true
		}
	}
	return false
}

// CheckLine applies the pattern rules to one synchronized-input line (for
// synchronized input, task 064). Spec rules do not apply.
func (p *Policy) CheckLine(line string) *Denial {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.st.rules {
		r := &p.st.rules[i]
		if r.pattern != nil && r.pattern.MatchString(line) {
			return &Denial{Rule: r.text, Reason: r.reason}
		}
	}
	return nil
}

// ReadOnly reports the policy's read-only switch.
func (p *Policy) ReadOnly() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.st.readonly
}

// ShellInReadOnly reports whether interactive shells may be opened while the
// policy is read-only.
func (p *Policy) ShellInReadOnly() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.st.shellInReadonly
}

// SyncRaw reports the raw synchronized-input mode: "confirm" or "deny".
func (p *Policy) SyncRaw() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.st.syncRaw
}

// defaultPolicyState is the built-in policy.
func defaultPolicyState() policyState {
	return policyState{
		protected: []resource.Selector{
			{Terms: []resource.Term{{Key: "protected", Op: resource.OpExists}}, Raw: "protected"},
		},
		allowRoot: true,
		syncRaw:   "confirm",
		rules:     builtinRules(),
	}
}

// builtinRules are the deny rules that always apply. Their patterns are
// constants, so compiling them cannot fail at runtime.
func builtinRules() []policyRule {
	type builtin struct{ pattern, reason string }
	bs := []builtin{
		{`systemctl\s+(stop|disable|mask)\s+(sshd|ssh)(\.service)?`, "never cut off SSH"},
		{`\brm\s+-[a-z]*r[a-z]*f?\s+/(\s|$)`, "recursive delete of the root filesystem"},
		{`\b(shutdown|reboot|halt|poweroff)\b`, "use your change process"},
		{`\bmkfs\b`, "creating a filesystem destroys what was there"},
		{`\bdd\s+.*of=/dev/`, "writing raw to a block device destroys it"},
	}
	rules := make([]policyRule, 0, len(bs))
	for _, b := range bs {
		rules = append(rules, policyRule{
			text:    b.pattern,
			reason:  b.reason,
			pattern: regexp.MustCompile(b.pattern),
		})
	}
	return rules
}

// loadPolicyState reads path and builds the state. A missing file yields the
// defaults.
func loadPolicyState(path string) (policyState, error) {
	st := defaultPolicyState()

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return st, &errs.Error{
			Kind: errs.Config,
			Op:   "bus.policy",
			Msg:  fmt.Sprintf("cannot read %s", path),
			Err:  err,
		}
	}

	var doc policyDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return st, &errs.Error{
			Kind: errs.Config,
			Op:   "bus.policy",
			Msg:  fmt.Sprintf("invalid policy %s", path),
			Err:  err,
		}
	}

	if err := applyPolicyDoc(&st, doc); err != nil {
		return st, &errs.Error{Kind: errs.Config, Op: "bus.policy", Msg: path, Err: err}
	}
	return st, nil
}

// applyPolicyDoc overlays a decoded file on the default state. The deny list is
// appended to the built-ins; protected replaces the default selectors.
func applyPolicyDoc(st *policyState, doc policyDoc) error {
	if doc.ReadOnly != nil {
		st.readonly = *doc.ReadOnly
	}
	if doc.MaxTargets != nil {
		if *doc.MaxTargets < 0 {
			return fmt.Errorf("max_targets must not be negative")
		}
		st.maxTargets = *doc.MaxTargets
	}
	if doc.Protected != nil {
		st.protected = make([]resource.Selector, 0, len(doc.Protected))
		for _, text := range doc.Protected {
			sel, err := resource.ParseSelector(text)
			if err != nil {
				return fmt.Errorf("protected selector %q: %w", text, err)
			}
			if sel.IsZero() {
				return fmt.Errorf("protected selector %q is empty", text)
			}
			st.protected = append(st.protected, sel)
		}
	}
	if doc.AllowRootTransport != nil {
		st.allowRoot = *doc.AllowRootTransport
	}
	if doc.ShellInReadOnly != nil {
		st.shellInReadonly = *doc.ShellInReadOnly
	}
	if doc.Sync != nil {
		switch doc.Sync.Raw {
		case "":
			// keep the default
		case "confirm", "deny":
			st.syncRaw = doc.Sync.Raw
		default:
			return fmt.Errorf("sync.raw must be confirm or deny, not %q", doc.Sync.Raw)
		}
	}

	for i, d := range doc.Deny {
		if d.DisableBuiltin != "" {
			return fmt.Errorf("deny[%d]: disable_builtin (%q) is not supported", i, d.DisableBuiltin)
		}
		r, err := compileDeny(d)
		if err != nil {
			return fmt.Errorf("deny[%d]: %w", i, err)
		}
		st.rules = append(st.rules, r)
	}
	return nil
}

// compileDeny turns one deny entry into a rule. Exactly one of pattern and spec
// must be set.
func compileDeny(d denyDoc) (policyRule, error) {
	switch {
	case d.Pattern != "" && d.Spec != "":
		return policyRule{}, fmt.Errorf("set either pattern or spec, not both")
	case d.Pattern == "" && d.Spec == "":
		return policyRule{}, fmt.Errorf("needs a pattern or a spec")
	}

	if d.Pattern != "" {
		re, err := regexp.Compile(d.Pattern)
		if err != nil {
			return policyRule{}, fmt.Errorf("pattern %q: %w", d.Pattern, err)
		}
		return policyRule{text: d.Pattern, reason: d.Reason, pattern: re}, nil
	}

	if _, err := path.Match(d.Spec, ""); err != nil {
		return policyRule{}, fmt.Errorf("spec %q: %w", d.Spec, err)
	}
	r := policyRule{text: "spec: " + d.Spec, reason: d.Reason, spec: d.Spec}
	if d.Selector != "" {
		sel, err := resource.ParseSelector(d.Selector)
		if err != nil {
			return policyRule{}, fmt.Errorf("selector %q: %w", d.Selector, err)
		}
		if sel.IsZero() {
			return policyRule{}, fmt.Errorf("selector %q is empty", d.Selector)
		}
		r.selector = &sel
		r.text += " selector: " + d.Selector
	}
	return r, nil
}

// specMatches reports whether the plan's spec ID matches a rule's spec text,
// which is either an exact ID or a glob like "salt.*".
func specMatches(pattern, id string) bool {
	if pattern == id {
		return true
	}
	ok, err := path.Match(pattern, id)
	return err == nil && ok
}

// targetsOf returns the plan's targets, falling back to the step targets when
// the request carries none.
func targetsOf(plan action.Plan) []action.Target {
	if len(plan.Request.Targets) > 0 {
		return plan.Request.Targets
	}
	out := make([]action.Target, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		out = append(out, s.Target)
	}
	return out
}

// anyDisplayMatches reports whether any step's Display matches re.
func anyDisplayMatches(plan action.Plan, re *regexp.Regexp) bool {
	for _, s := range plan.Steps {
		if re.MatchString(s.Display) {
			return true
		}
	}
	return false
}

// anyTargetMatches reports whether any target's labels satisfy sel.
func anyTargetMatches(plan action.Plan, sel resource.Selector) bool {
	for _, t := range targetsOf(plan) {
		if sel.Matches(t.Labels) {
			return true
		}
	}
	return false
}
