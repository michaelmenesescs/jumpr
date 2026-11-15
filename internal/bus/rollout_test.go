package bus

import (
	"context"
	"strconv"
	"testing"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// defaultRollout mirrors config.Default().Rollout.
func defaultRollout() RolloutConfig {
	return RolloutConfig{
		Canary:                  1,
		BatchPercent:            10,
		BatchMax:                50,
		FailureThresholdPercent: 5,
		TypedConfirmOver:        20,
	}
}

func TestPlanRolloutTable(t *testing.T) {
	c := defaultRollout()
	tests := []struct {
		targets int
		canary  int
		size    int
		batches int
	}{
		{targets: 1, canary: 0, size: 1, batches: 1},
		{targets: 2, canary: 1, size: 1, batches: 1},
		{targets: 12, canary: 1, size: 2, batches: 6},
		{targets: 20, canary: 1, size: 2, batches: 10},
		{targets: 21, canary: 1, size: 2, batches: 10},
		{targets: 100, canary: 1, size: 10, batches: 10},
		{targets: 1512, canary: 1, size: 50, batches: 31},
	}
	for _, tt := range tests {
		got := PlanRollout(tt.targets, c)
		want := Rollout{
			Canary:           tt.canary,
			BatchSize:        tt.size,
			Batches:          tt.batches,
			FailureThreshold: 0.05,
		}
		if got != want {
			t.Errorf("PlanRollout(%d) = %+v, want %+v", tt.targets, got, want)
		}
	}
}

func TestPlanRolloutCustomConfig(t *testing.T) {
	c := RolloutConfig{Canary: 5, BatchPercent: 100, BatchMax: 10, FailureThresholdPercent: 25, TypedConfirmOver: 3}
	tests := []struct {
		targets int
		want    Rollout
	}{
		// canary min(5, targets-1), remaining cut into 10s.
		{targets: 3, want: Rollout{Canary: 2, BatchSize: 1, Batches: 1, FailureThreshold: 0.25}},
		{targets: 6, want: Rollout{Canary: 5, BatchSize: 1, Batches: 1, FailureThreshold: 0.25}},
		{targets: 30, want: Rollout{Canary: 5, BatchSize: 10, Batches: 3, FailureThreshold: 0.25}},
		{targets: 1512, want: Rollout{Canary: 5, BatchSize: 10, Batches: 151, FailureThreshold: 0.25}},
	}
	for _, tt := range tests {
		if got := PlanRollout(tt.targets, c); got != tt.want {
			t.Errorf("PlanRollout(%d, custom) = %+v, want %+v", tt.targets, got, tt.want)
		}
	}
}

func TestPlanRolloutNoCanaryWhenConfiguredZero(t *testing.T) {
	c := defaultRollout()
	c.Canary = 0
	got := PlanRollout(12, c)
	want := Rollout{Canary: 0, BatchSize: 2, Batches: 6, FailureThreshold: 0.05}
	if got != want {
		t.Errorf("PlanRollout(12, canary 0) = %+v, want %+v", got, want)
	}
}

func TestPlanRolloutZeroAndNegativeBounds(t *testing.T) {
	// Degenerate config must still produce a usable plan, never a zero batch size.
	c := RolloutConfig{Canary: 1, BatchPercent: 0, BatchMax: 0, FailureThresholdPercent: 0, TypedConfirmOver: 20}
	got := PlanRollout(5, c)
	if got.BatchSize < 1 || got.Batches != 4 || got.Canary != 1 {
		t.Errorf("PlanRollout(5, degenerate) = %+v", got)
	}
}

func TestConfirmForTable(t *testing.T) {
	c := defaultRollout()
	tests := []struct {
		name         string
		targets      int
		anyProtected bool
		risk         action.Risk
		want         ConfirmLevel
	}{
		{name: "single", targets: 1, risk: action.RiskNormal, want: ConfirmYesNo},
		{name: "single destructive shows list", targets: 1, risk: action.RiskDestructive, want: ConfirmList},
		{name: "two", targets: 2, risk: action.RiskNormal, want: ConfirmList},
		{name: "twenty", targets: 20, risk: action.RiskNormal, want: ConfirmList},
		{name: "twenty destructive", targets: 20, risk: action.RiskDestructive, want: ConfirmList},
		{name: "twenty one", targets: 21, risk: action.RiskNormal, want: ConfirmTypedCount},
		{name: "protected single", targets: 1, anyProtected: true, risk: action.RiskNormal, want: ConfirmTypedCount},
		{name: "protected many", targets: 50, anyProtected: true, risk: action.RiskDestructive, want: ConfirmTypedCount},
	}
	for _, tt := range tests {
		if got := ConfirmFor(tt.targets, tt.anyProtected, tt.risk, c); got != tt.want {
			t.Errorf("%s: ConfirmFor(%d, protected=%v, risk=%v) = %v, want %v",
				tt.name, tt.targets, tt.anyProtected, tt.risk, got, tt.want)
		}
	}
}

func newStep(host resource.HostID) action.Step {
	return action.Step{
		Target:  action.Target{Host: host},
		Display: "systemctl restart nginx.service",
		Run: func(context.Context) action.StepResult {
			return action.StepResult{Target: action.Target{Host: host}}
		},
	}
}

func hostSteps(n int) []action.Step {
	steps := make([]action.Step, n)
	for i := range steps {
		steps[i] = newStep(resource.HostID("h" + strconv.Itoa(i+1)))
	}
	return steps
}

func TestBatchesPreservesOrder(t *testing.T) {
	steps := hostSteps(12)
	r := PlanRollout(len(steps), defaultRollout())

	canary, batches := Batches(steps, r)
	if len(canary) != 1 || canary[0].Target.Host != "h1" {
		t.Fatalf("canary = %+v, want the first step only", canary)
	}
	if len(batches) != r.Batches {
		t.Fatalf("got %d batches, want %d", len(batches), r.Batches)
	}
	var flat []action.Step
	flat = append(flat, canary...)
	for i, b := range batches {
		if len(b) == 0 || len(b) > r.BatchSize {
			t.Errorf("batch %d has %d steps, want 1..%d", i+1, len(b), r.BatchSize)
		}
		flat = append(flat, b...)
	}
	if len(flat) != len(steps) {
		t.Fatalf("flattened %d steps, want %d", len(flat), len(steps))
	}
	for i := range steps {
		if flat[i].Target.Host != steps[i].Target.Host {
			t.Fatalf("step %d is %s, want %s (order changed)", i, flat[i].Target.Host, steps[i].Target.Host)
		}
	}
}

func TestBatchesSingleTarget(t *testing.T) {
	steps := hostSteps(1)
	canary, batches := Batches(steps, PlanRollout(1, defaultRollout()))
	if len(canary) != 0 {
		t.Errorf("canary = %v, want none for a single target", canary)
	}
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Target.Host != "h1" {
		t.Errorf("batches = %v, want one batch of the single step", batches)
	}
}

func TestBatchesClampsCanaryAndEmptyInput(t *testing.T) {
	steps := hostSteps(3)
	canary, batches := Batches(steps, Rollout{Canary: 10, BatchSize: 2})
	if len(canary) != 3 || len(batches) != 0 {
		t.Errorf("canary=%d batches=%d, want 3 and 0", len(canary), len(batches))
	}
	canary, batches = Batches(nil, PlanRollout(5, defaultRollout()))
	if len(canary) != 0 || len(batches) != 0 {
		t.Errorf("empty input gave canary=%v batches=%v", canary, batches)
	}
	// A zero batch size must not loop forever or drop steps.
	steps = hostSteps(4)
	canary, batches = Batches(steps, Rollout{Canary: 0, BatchSize: 0})
	if len(canary) != 0 || len(batches) != 1 || len(batches[0]) != 4 {
		t.Errorf("zero batch size gave canary=%v batches=%v", canary, batches)
	}
}

func sampleProposal(confirm ConfirmLevel, steps int) Proposal {
	p := Proposal{ID: "20261112T140512-3f9a", Confirm: confirm}
	p.Plan.Steps = hostSteps(steps)
	p.Plan.Request.Targets = make([]action.Target, steps)
	return p
}

func TestCheckConfirmationTable(t *testing.T) {
	tests := []struct {
		name    string
		steps   int
		mutate  func(*Proposal)
		confirm Confirmation
		wantErr errs.Kind
		wantMsg string
	}{
		{name: "yes no accepted", confirm: Confirmation{ProposalID: "20261112T140512-3f9a"}},
		{name: "list accepted with junk typed", mutate: func(p *Proposal) { p.Confirm = ConfirmList },
			confirm: Confirmation{ProposalID: "20261112T140512-3f9a", Typed: "nonsense"}},
		{name: "typed count accepted", mutate: func(p *Proposal) { p.Confirm = ConfirmTypedCount }, steps: 3,
			confirm: Confirmation{ProposalID: "20261112T140512-3f9a", Typed: "3"}},
		{name: "typed count trimmed", mutate: func(p *Proposal) { p.Confirm = ConfirmTypedCount }, steps: 3,
			confirm: Confirmation{ProposalID: "20261112T140512-3f9a", Typed: " 3\n"}},
		{name: "typed count wrong", mutate: func(p *Proposal) { p.Confirm = ConfirmTypedCount }, steps: 3,
			confirm: Confirmation{ProposalID: "20261112T140512-3f9a", Typed: "4"},
			wantErr: errs.Policy, wantMsg: "typed count does not match"},
		{name: "typed count with separators rejected", mutate: func(p *Proposal) { p.Confirm = ConfirmTypedCount }, steps: 1512,
			confirm: Confirmation{ProposalID: "20261112T140512-3f9a", Typed: "1,512"},
			wantErr: errs.Policy, wantMsg: "typed count does not match"},
		{name: "typed count empty", mutate: func(p *Proposal) { p.Confirm = ConfirmTypedCount }, steps: 3,
			confirm: Confirmation{ProposalID: "20261112T140512-3f9a"},
			wantErr: errs.Policy, wantMsg: "typed count does not match"},
		{name: "wrong proposal id", mutate: func(p *Proposal) { p.Confirm = ConfirmList },
			confirm: Confirmation{ProposalID: "other"}, wantErr: errs.Internal},
		{name: "missing proposal id", confirm: Confirmation{}, wantErr: errs.Internal},
		{name: "denied wins over typed mismatch", mutate: func(p *Proposal) {
			p.Confirm = ConfirmTypedCount
			p.Denied = &Denial{Rule: "never stop sshd", Reason: "denied by policy"}
		}, confirm: Confirmation{ProposalID: "20261112T140512-3f9a", Typed: "0"},
			wantErr: errs.Policy},
	}
	for _, tt := range tests {
		steps := tt.steps
		if steps == 0 {
			steps = 1
		}
		p := sampleProposal(ConfirmYesNo, steps)
		if tt.mutate != nil {
			tt.mutate(&p)
		}
		err := CheckConfirmation(p, tt.confirm)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tt.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: got nil, want %s", tt.name, tt.wantErr)
			continue
		}
		if got := errs.KindOf(err); got != tt.wantErr {
			t.Errorf("%s: kind = %s, want %s (%v)", tt.name, got, tt.wantErr, err)
		}
		if tt.wantMsg != "" && err.Error() != "bus.confirm: "+tt.wantMsg {
			t.Errorf("%s: message = %q, want %q", tt.name, err.Error(), "bus.confirm: "+tt.wantMsg)
		}
	}
}

func TestCheckConfirmationAcceptsZeroTargetProposal(t *testing.T) {
	p := Proposal{ID: "p1", Confirm: ConfirmTypedCount}
	if err := CheckConfirmation(p, Confirmation{ProposalID: "p1", Typed: "0"}); err != nil {
		t.Fatalf("zero-target proposal: %v", err)
	}
}

func TestShouldStop(t *testing.T) {
	tests := []struct {
		name                string
		failed, done, total int
		threshold           float64
		want                bool
	}{
		// Documented edge: with 20 hosts and the default 5 %, one failure is
		// exactly 5 % and does not stop; the second one (10 %) does.
		{name: "one of twenty is exactly five percent", failed: 1, done: 20, total: 20, threshold: 0.05, want: false},
		{name: "two of twenty stops", failed: 2, done: 20, total: 20, threshold: 0.05, want: true},
		{name: "none failed", failed: 0, done: 20, total: 20, threshold: 0.05, want: false},
		{name: "one of ten stops", failed: 1, done: 10, total: 10, threshold: 0.05, want: true},
		{name: "one of twenty one is under five percent", failed: 1, done: 21, total: 21, threshold: 0.05, want: false},
		{name: "zero threshold stops on first failure", failed: 1, done: 1, total: 1, threshold: 0, want: true},
		{name: "no failures with zero threshold", failed: 0, done: 1, total: 1, threshold: 0, want: false},
		{name: "no total", failed: 1, done: 0, total: 0, threshold: 0.05, want: false},
		{name: "half is not over half", failed: 5, done: 10, total: 10, threshold: 0.5, want: false},
		{name: "over half", failed: 6, done: 10, total: 10, threshold: 0.5, want: true},
	}
	for _, tt := range tests {
		if got := ShouldStop(tt.failed, tt.done, tt.total, tt.threshold); got != tt.want {
			t.Errorf("%s: ShouldStop(%d, %d, %d, %v) = %v, want %v",
				tt.name, tt.failed, tt.done, tt.total, tt.threshold, got, tt.want)
		}
	}
}
