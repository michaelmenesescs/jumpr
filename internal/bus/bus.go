package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/redact"
)

// Defaults of the engine. They are constants, not configuration, so a caller
// that leaves Options empty still gets a working bus.
const (
	defaultConcurrency = 16
	defaultStepTimeout = 5 * time.Minute
	planTimeout        = 30 * time.Second
	eventBuffer        = 256
	maxStepOutput      = 64 << 10
)

// Options configures a Bus.
type Options struct {
	// Policy is the guard rail. nil means DefaultPolicy().
	Policy *Policy
	// Rollout is the canary/batch shape and typed-confirm threshold.
	Rollout RolloutConfig
	// Recorder persists the outcome. nil records nothing.
	Recorder Recorder
	// Redact redacts step output before it is recorded or returned. nil means
	// the built-in patterns: output never leaves the bus unredacted.
	Redact *redact.Redactor
	// ReadOnly reports a session-level read-only switch. nil means writable.
	ReadOnly func() bool
	// Concurrency bounds the steps in flight inside one batch. 0 means 16.
	Concurrency int
	// StepTimeout bounds one step. 0 means 5 minutes.
	StepTimeout time.Duration
	// Now is the clock. nil means time.Now.
	Now func() time.Time
	// Log receives errors that have no caller to return to. nil discards them.
	Log *slog.Logger
	// Context is the bus's root context. Rollouts derive from it, not from
	// the context of the call that started them, so closing a dialog or
	// detaching does not kill a run. nil means context.Background().
	Context context.Context
}

// Owner is the optional interface a planner implements to claim spec IDs that
// do not follow its registration prefix. A planner without Owns owns exactly
// the IDs that start with "<registered name>.".
type Owner interface {
	Owns(specID string) bool
}

// RunInfo is what the Run returned by Execute offers beyond the frozen Run
// interface: the proposal it belongs to and the results so far, so a UI can
// reattach to a rollout that is already running.
type RunInfo interface {
	Run
	Proposal() Proposal
	Results() []action.StepResult
}

// bus is the Bus implementation.
type bus struct {
	policy      *Policy
	rollout     RolloutConfig
	recorder    Recorder
	redact      *redact.Redactor
	readOnly    func() bool
	concurrency int
	stepTimeout time.Duration
	now         func() time.Time
	log         *slog.Logger
	ctx         context.Context

	mu       sync.Mutex
	planners []namedPlanner
	runs     []*run
	seq      int
}

// namedPlanner is a planner and the name it was registered under.
type namedPlanner struct {
	name string
	pl   action.Planner
}

// New builds a Bus from o, filling in the defaults.
func New(o Options) Bus {
	b := &bus{
		policy:      o.Policy,
		rollout:     o.Rollout,
		recorder:    o.Recorder,
		redact:      o.Redact,
		readOnly:    o.ReadOnly,
		concurrency: o.Concurrency,
		stepTimeout: o.StepTimeout,
		now:         o.Now,
		log:         o.Log,
		ctx:         o.Context,
	}
	if b.policy == nil {
		b.policy = DefaultPolicy()
	}
	if b.concurrency < 1 {
		b.concurrency = defaultConcurrency
	}
	if b.stepTimeout <= 0 {
		b.stepTimeout = defaultStepTimeout
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.redact == nil {
		// Built-in patterns only; New cannot fail without user patterns.
		b.redact, _ = redact.New(nil)
	}
	if b.ctx == nil {
		b.ctx = context.Background()
	}
	return b
}

// Register adds a planner under name. Names must be unique; a planner claims
// spec IDs lazily, so two planners claiming the same ID is only visible when a
// Propose asks for it and the first registered planner wins.
func (b *bus) Register(name string, pl action.Planner) error {
	if pl == nil {
		return errs.New(errs.Config, "bus.register", "nil planner")
	}
	if strings.TrimSpace(name) == "" {
		return errs.New(errs.Config, "bus.register", "planner name is empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, np := range b.planners {
		if np.name == name {
			return errs.New(errs.Config, "bus.register", fmt.Sprintf("planner %q is already registered", name))
		}
	}
	b.planners = append(b.planners, namedPlanner{name: name, pl: pl})
	return nil
}

// Propose validates the request, asks the owning planner for a plan, applies
// policy and decides the confirmation level and the rollout shape. A denied
// proposal is returned with Denied set and recorded at once.
func (b *bus) Propose(ctx context.Context, req action.Request) (Proposal, error) {
	targets := dedupeTargets(req.Targets)
	if len(targets) == 0 {
		return Proposal{}, errs.New(errs.Config, "bus.propose", "request has no targets")
	}
	req.Targets = targets

	pl, ok := b.plannerFor(req.SpecID)
	if !ok {
		return Proposal{}, errs.New(errs.Config, "bus.propose",
			fmt.Sprintf("no planner for spec %q", req.SpecID))
	}

	planCtx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	plan, err := pl.Plan(planCtx, req)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) {
			return Proposal{}, err
		}
		return Proposal{}, errs.Wrap(errs.Internal, "bus.plan", err)
	}

	p := Proposal{
		ID:      b.newID(),
		Plan:    plan,
		Rollout: PlanRollout(len(targets), b.rollout),
	}
	p.Confirm = ConfirmFor(len(targets), b.anyProtected(targets), plan.Spec.Risk, b.rollout)
	if wantsTypedConfirm(plan, targets) {
		p.Confirm = ConfirmTypedCount
	}
	p.Denied = b.policy.Check(plan, b.isReadOnly())
	if p.Denied != nil {
		b.recordDenied(p)
	}
	return p, nil
}

// Execute validates the confirmation, re-checks policy and the read-only
// switch, then starts the rollout on the bus's root context and returns it.
func (b *bus) Execute(ctx context.Context, p Proposal, c Confirmation) (Run, error) {
	// The rollout's lifetime is the bus's root context, not the caller's:
	// closing the dialog that confirmed it must not stop it.
	_ = ctx

	if err := CheckConfirmation(p, c); err != nil {
		return nil, err
	}
	if d := b.policy.Check(p.Plan, b.isReadOnly()); d != nil {
		return nil, errs.New(errs.Policy, "bus.execute", d.Reason)
	}

	r := newRun(b, p)
	b.mu.Lock()
	b.runs = append(b.runs, r)
	b.mu.Unlock()
	go r.loop()
	return r, nil
}

// Active returns the runs that have not finished, newest last.
func (b *bus) Active() []Run {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.runs[:0]
	for _, r := range b.runs {
		if !r.finished() {
			kept = append(kept, r)
		}
	}
	b.runs = kept
	out := make([]Run, len(kept))
	for i, r := range kept {
		out[i] = r
	}
	return out
}

// plannerFor finds the planner that owns specID: an own-planning planner
// decides for itself, any other owns the IDs under its registration name.
func (b *bus) plannerFor(specID string) (action.Planner, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, np := range b.planners {
		if o, ok := np.pl.(Owner); ok {
			if o.Owns(specID) {
				return np.pl, true
			}
			continue
		}
		if strings.HasPrefix(specID, np.name+".") {
			return np.pl, true
		}
	}
	return nil, false
}

// anyProtected reports whether any target is protected by policy.
func (b *bus) anyProtected(targets []action.Target) bool {
	for _, t := range targets {
		if b.policy.Protected(t) {
			return true
		}
	}
	return false
}

// The planner-provided label that forces the typed-count confirmation. It is
// how a planner marks a write whose blast radius the target count does not
// show, such as deleting a Salt key: the plan's targets carry it and Propose
// honours it whatever the count and the risk.
const (
	confirmLabel = "_confirm"
	confirmTyped = "typed"
)

// wantsTypedConfirm reports whether the plan asked for the typed count through
// the _confirm=typed label, on a request target or on a planned step's target.
func wantsTypedConfirm(plan action.Plan, targets []action.Target) bool {
	for _, s := range plan.Steps {
		if s.Target.Labels[confirmLabel] == confirmTyped {
			return true
		}
	}
	for _, t := range targets {
		if t.Labels[confirmLabel] == confirmTyped {
			return true
		}
	}
	return false
}

// isReadOnly combines the policy switch with the session-level override.
func (b *bus) isReadOnly() bool {
	return b.readOnly != nil && b.readOnly()
}

// recordDenied stores a denied proposal. There is no caller to return the
// error to, so it goes to the log when a logger is configured.
func (b *bus) recordDenied(p Proposal) {
	if b.recorder == nil {
		return
	}
	if err := b.recorder.RecordDenied(p); err != nil && b.log != nil {
		b.log.Warn("bus: record denied proposal", "spec", p.Plan.Spec.ID, "err", err)
	}
}

// recordAction stores a finished rollout with redacted, capped output.
func (b *bus) recordAction(p Proposal, s Summary, results []action.StepResult) {
	if b.recorder == nil {
		return
	}
	if err := b.recorder.RecordAction(p, s, b.redacted(results)); err != nil && b.log != nil {
		b.log.Warn("bus: record action", "spec", p.Plan.Spec.ID, "err", err)
	}
}

// redacted returns a copy of results whose output went through the redactor
// and the 64 KiB cap.
func (b *bus) redacted(results []action.StepResult) []action.StepResult {
	out := make([]action.StepResult, len(results))
	copy(out, results)
	for i := range out {
		out[i].Output = b.redact.Cap(out[i].Output, maxStepOutput)
	}
	return out
}

// newID returns a time-ordered proposal ID, e.g. "20261112T140512-3f9a".
func (b *bus) newID() string {
	stamp := b.now().UTC().Format("20060102T150405")
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return stamp + "-" + hex.EncodeToString(buf[:])
	}
	b.mu.Lock()
	b.seq++
	n := b.seq
	b.mu.Unlock()
	return fmt.Sprintf("%s-%04x", stamp, n)
}

// dedupeTargets drops repeated targets, keeping the first occurrence's labels.
func dedupeTargets(in []action.Target) []action.Target {
	out := make([]action.Target, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, t := range in {
		key := string(t.Host) + "\x00" + t.Ref.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	return out
}
