package bus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
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
	redact          []string
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
	// Redact lists extra secret patterns for internal/redact (task 065):
	// policy is where an operator already writes what must not happen.
	Redact []string `yaml:"redact"`
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

// RedactPatterns returns the extra secret patterns of the file's `redact: []`
// key, in order. internal/app feeds them to redact.New; the built-in patterns
// are always applied and are not listed here.
func (p *Policy) RedactPatterns() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.st.redact...)
}

// Describe returns the effective policy as plain text lines: the switches it
// enforces and the deny rules in the order Check applies them (the built-ins
// first, then the file's). `:policy` shows it in a text view.
func (p *Policy) Describe() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st := &p.st
	out := []string{
		"readonly: " + strconv.FormatBool(st.readonly),
		"max_targets: " + unlimitedText(st.maxTargets),
		"protected: " + selectorsText(st.protected),
		"allow_root_transport: " + strconv.FormatBool(st.allowRoot),
		"shell_in_readonly: " + strconv.FormatBool(st.shellInReadonly),
		"sync.raw: " + st.syncRaw,
		"redact: " + countText(len(st.redact), "extra pattern") + " (built-in patterns always apply)",
		"",
		fmt.Sprintf("deny rules (%d, built-ins first):", len(st.rules)),
	}
	for i := range st.rules {
		r := &st.rules[i]
		line := fmt.Sprintf("  %2d. %s", i+1, r.text)
		if r.reason != "" {
			line += " — " + r.reason
		}
		out = append(out, line)
	}
	return out
}

// unlimitedText renders max_targets, where 0 means unlimited.
func unlimitedText(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// selectorsText renders the protected selectors as one line.
func selectorsText(sels []resource.Selector) string {
	if len(sels) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(sels))
	for i := range sels {
		parts = append(parts, sels[i].String())
	}
	return strings.Join(parts, ", ")
}

// countText renders "1 pattern" / "2 patterns".
func countText(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
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
	if doc.Redact != nil {
		// Validate here, while the file name is still in the error: a bad
		// pattern must not surface later as a redactor failure.
		st.redact = make([]string, 0, len(doc.Redact))
		for i, pat := range doc.Redact {
			if strings.TrimSpace(pat) == "" {
				return fmt.Errorf("redact[%d]: empty pattern", i)
			}
			if _, err := regexp.Compile(pat); err != nil {
				return fmt.Errorf("redact[%d]: %w", i, err)
			}
			st.redact = append(st.redact, pat)
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
