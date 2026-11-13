// Package bustest provides scripted planners for tests of the action bus.
//
// A Planner turns a request into one step per target and reports what each
// step did: its exit code, output, error, delay, hold or panic, scripted per
// host. It also remembers which hosts actually ran, so a test can assert that
// a failed canary left every other host untouched.
package bustest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"jumper/internal/action"
	"jumper/internal/resource"
)

// Result scripts one step. The zero value is a successful step with no output.
type Result struct {
	RC     int
	Output string
	Err    error
	Delay  time.Duration
	// Hold makes the step wait until the channel is closed, or until its
	// context is done. It scripts a step that is in flight while a test
	// calls Abort.
	Hold <-chan struct{}
	// Panic makes the step panic, to exercise the bus's recovery.
	Panic bool
}

// Options configures a Planner.
type Options struct {
	SpecID  string // default "fake.restart"
	Title   string // default: SpecID
	Risk    action.Risk
	Via     string
	PlanErr error // when set, Plan fails with it
	// OwnsFn, when set, becomes the planner's Owns decision. Otherwise it
	// owns exactly SpecID.
	OwnsFn  func(specID string) bool
	Default Result
	Results map[resource.HostID]Result // per-host overrides of Default
}

// Planner is a scripted action.Planner.
type Planner struct {
	specID  string
	title   string
	risk    action.Risk
	via     string
	planErr error
	ownsFn  func(specID string) bool
	def     Result
	results map[resource.HostID]Result

	mu  sync.Mutex
	ran []resource.HostID
}

// New builds a Planner from o.
func New(o Options) *Planner {
	spec := o.SpecID
	if spec == "" {
		spec = "fake.restart"
	}
	title := o.Title
	if title == "" {
		title = spec
	}
	return &Planner{
		specID:  spec,
		title:   title,
		risk:    o.Risk,
		via:     o.Via,
		planErr: o.PlanErr,
		ownsFn:  o.OwnsFn,
		def:     o.Default,
		results: o.Results,
	}
}

// Actions lists the one spec the planner offers.
func (p *Planner) Actions(selection []resource.Resource) []action.Spec {
	return []action.Spec{{ID: p.specID, Title: p.title, Risk: p.risk, HasDryRun: true}}
}

// Plan builds one step per target, in request order.
func (p *Planner) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	if p.planErr != nil {
		return action.Plan{}, p.planErr
	}
	plan := action.Plan{
		Spec:    action.Spec{ID: p.specID, Title: p.title, Risk: p.risk, HasDryRun: true},
		Request: req,
		Title:   p.title,
		Via:     p.via,
	}
	for _, t := range req.Targets {
		t := t
		plan.Steps = append(plan.Steps, action.Step{
			Target:  t,
			Display: fmt.Sprintf("%s %s", p.specID, t.Host),
			Run: func(ctx context.Context) action.StepResult {
				return p.step(ctx, t)
			},
		})
	}
	return plan, nil
}

// Owns reports whether the planner handles specID.
func (p *Planner) Owns(specID string) bool {
	if p.ownsFn != nil {
		return p.ownsFn(specID)
	}
	return specID == p.specID
}

// Ran returns the hosts whose step ran, in the order they started.
func (p *Planner) Ran() []resource.HostID {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]resource.HostID, len(p.ran))
	copy(out, p.ran)
	return out
}

// Count returns how many steps have run.
func (p *Planner) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ran)
}

// step runs one scripted step.
func (p *Planner) step(ctx context.Context, t action.Target) action.StepResult {
	p.mu.Lock()
	p.ran = append(p.ran, t.Host)
	p.mu.Unlock()

	r := p.def
	if got, ok := p.results[t.Host]; ok {
		r = got
	}
	if r.Panic {
		panic("bustest: scripted panic")
	}
	if r.Hold != nil {
		select {
		case <-r.Hold:
		case <-ctx.Done():
			return action.StepResult{Target: t, RC: -1, Err: ctx.Err()}
		}
	}
	if r.Delay > 0 {
		timer := time.NewTimer(r.Delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return action.StepResult{Target: t, RC: -1, Err: ctx.Err()}
		}
	}
	return action.StepResult{Target: t, RC: r.RC, Output: r.Output, Err: r.Err}
}
