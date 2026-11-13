package chef

import (
	"context"
	"strings"
	"testing"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

// plannerTargets is the selection the plan tests act on, deliberately not in
// host order so the plan has to sort.
func plannerTargets() []action.Target {
	return []action.Target{
		{Host: "web-02"},
		{Host: "web-01"},
		{Host: "web-03"},
	}
}

// newTestPlanner builds a planner over a scripted fan-out. It returns the fake
// so a test can inspect the calls the plan made.
func newTestPlanner(t *testing.T, o ClientOptions) (*Planner, *transporttest.Fake) {
	t.Helper()
	if o.Exec == nil {
		o.Exec = whyRunFake(t)
	}
	fake, ok := o.Exec.(*transporttest.Fake)
	if !ok {
		t.Fatalf("executor is %T, want a transporttest.Fake", o.Exec)
	}
	return NewPlanner(NewClient(o)), fake
}

// TestPlanClientRunPreviewIsTheWhyRun checks requirement 3: the destructive run
// carries the why-run result of the same hosts as its mandatory preview, one
// step per host in host order, with the exact converging command.
func TestPlanClientRunPreviewIsTheWhyRun(t *testing.T) {
	p, fake := newTestPlanner(t, ClientOptions{})
	plan, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecClientRun,
		Targets: plannerTargets(),
		Actor:   action.Human,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Spec.Risk != action.RiskDestructive {
		t.Errorf("risk = %v, want destructive", plan.Spec.Risk)
	}
	if !plan.Spec.HasDryRun {
		t.Error("HasDryRun = false, want the why-run preview")
	}
	if plan.Title != "chef-client on 3 hosts" {
		t.Errorf("title = %q", plan.Title)
	}
	if plan.Via != defaultClient {
		t.Errorf("via = %q, want the client name", plan.Via)
	}
	if len(plan.Warnings) == 0 {
		t.Error("no warning on a converging run")
	}
	if plan.Preview == "" {
		t.Fatal("the run has no preview; the why-run is mandatory")
	}
	if !strings.Contains(plan.Preview, "3 hosts") || !strings.Contains(plan.Preview, "1 group") {
		t.Errorf("preview = %q, want the grouped why-run counts", plan.Preview)
	}
	if !strings.Contains(plan.Preview, "0/2 resources would have been updated") {
		t.Errorf("preview = %q, want the why-run summary line", plan.Preview)
	}
	if !strings.Contains(plan.Preview, "web-01, web-02, web-03") {
		t.Errorf("preview = %q, want the hosts of the group", plan.Preview)
	}

	if len(plan.Steps) != 3 {
		t.Fatalf("steps = %d, want one per host", len(plan.Steps))
	}
	wantHosts := []resource.HostID{"web-01", "web-02", "web-03"}
	wantDisplay := "sudo -n chef-client --no-color -l warn --run-lock-timeout 30"
	for i, step := range plan.Steps {
		if step.Target.Host != wantHosts[i] {
			t.Errorf("step %d host = %q, want %q", i, step.Target.Host, wantHosts[i])
		}
		if step.Display != wantDisplay {
			t.Errorf("step %d display = %q, want %q", i, step.Display, wantDisplay)
		}
	}

	// The preview ran the why-run once per host; the steps have not run yet.
	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want the why-run only", len(calls))
	}
	for _, call := range calls {
		if !strings.Contains(call.Command.Script, "--why-run") {
			t.Errorf("planning ran %q, want only the dry run", call.Command.Script)
		}
	}
}

// TestPlanWhyRunIsNormalRisk checks requirement 2: the why-run is its own spec
// through the bus at normal risk, planning runs nothing, and the plan says that
// ruby_block resources still execute.
func TestPlanWhyRunIsNormalRisk(t *testing.T) {
	p, fake := newTestPlanner(t, ClientOptions{Client: cincClient})
	plan, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecWhyRun,
		Targets: plannerTargets(),
		Actor:   action.Human,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Spec.Risk != action.RiskNormal {
		t.Errorf("risk = %v, want normal", plan.Spec.Risk)
	}
	if plan.Preview != "" {
		t.Errorf("preview = %q, want none: the why-run is the dry run", plan.Preview)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "ruby_block") {
		t.Errorf("warnings = %v, want the ruby_block warning", plan.Warnings)
	}
	if plan.Title != "cinc-client --why-run on 3 hosts" {
		t.Errorf("title = %q", plan.Title)
	}
	wantDisplay := "sudo -n cinc-client --why-run --no-color -l warn --run-lock-timeout 30"
	if len(plan.Steps) != 3 || plan.Steps[0].Display != wantDisplay {
		t.Errorf("steps = %+v, want %q", plan.Steps, wantDisplay)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %d, want none: planning a why-run writes nothing", len(fake.Calls()))
	}
}

// TestPlanRunStepFailsOnNonZeroRC checks the step of a converging run reports a
// failure when chef-client exits non-zero, and carries the output.
func TestPlanRunStepFailsOnNonZeroRC(t *testing.T) {
	fake := transporttest.New().OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if strings.Contains(cmd.Script, "--why-run") {
			return transporttest.Reply{Stdout: whyRunBody("web-01")}, true
		}
		return transporttest.Reply{RC: 1, Stdout: "Chef Infra Client failed. 0 resources updated in 12 seconds\n"}, true
	})
	p, _ := newTestPlanner(t, ClientOptions{Exec: fake})
	plan, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecClientRun,
		Targets: []action.Target{{Host: "web-01"}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	res := plan.Steps[0].Run(context.Background())
	if res.Err == nil {
		t.Fatalf("result = %+v, want a failure for rc 1", res)
	}
	if errs.KindOf(res.Err) != errs.Remote {
		t.Errorf("kind = %q, want remote", errs.KindOf(res.Err))
	}
	if res.RC != 1 {
		t.Errorf("RC = %d, want 1", res.RC)
	}
	if !strings.Contains(res.Output, "Chef Infra Client failed") {
		t.Errorf("output = %q", res.Output)
	}
}

// TestPlanFailsWhenTheWhyRunFailsEverywhere checks a destructive run without
// its dry run is refused instead of planned blind.
func TestPlanFailsWhenTheWhyRunFailsEverywhere(t *testing.T) {
	fake := transporttest.New().On("chef-client", transporttest.Reply{
		Err: errs.New(errs.Unreachable, "fake.exec", "no route to host"),
	})
	p, _ := newTestPlanner(t, ClientOptions{Exec: fake})
	_, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecClientRun,
		Targets: []action.Target{{Host: "web-01"}},
	})
	if errs.KindOf(err) != errs.Remote {
		t.Fatalf("kind = %q, want remote (%v)", errs.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "every host") {
		t.Errorf("error = %q, want it to say the dry run failed everywhere", err)
	}
}

// TestPlanActionsForHostRows checks the planner offers both specs to a host
// selection and nothing to a selection of units.
func TestPlanActionsForHostRows(t *testing.T) {
	p := NewPlanner(NewClient(ClientOptions{}))
	specs := p.Actions([]resource.Resource{{Ref: resource.Ref{Kind: "host", Name: "web-01"}}})
	if len(specs) != 2 || specs[0].ID != SpecClientRun || specs[1].ID != SpecWhyRun {
		t.Fatalf("specs = %+v, want the run then the why-run", specs)
	}
	nodeSpecs := p.Actions([]resource.Resource{{
		Ref: resource.Ref{Backend: backendName, Kind: KindNode, Name: "web-01"},
	}})
	if len(nodeSpecs) != 2 {
		t.Errorf("chef-node specs = %+v, want both", nodeSpecs)
	}
	if got := p.Actions([]resource.Resource{{Ref: resource.Ref{Kind: "unit", Name: "nginx.service"}}}); got != nil {
		t.Errorf("unit specs = %+v, want none", got)
	}
	if got := p.Actions(nil); got != nil {
		t.Errorf("empty selection specs = %+v, want none", got)
	}
}

// TestPlanRefusals checks the two ways a plan refuses before any remote call.
func TestPlanRefusals(t *testing.T) {
	p, fake := newTestPlanner(t, ClientOptions{})
	if _, err := p.Plan(context.Background(), action.Request{
		SpecID:  "chef.unicorn",
		Targets: []action.Target{{Host: "web-01"}},
	}); errs.KindOf(err) != errs.Config {
		t.Errorf("unknown spec kind = %q, want config", errs.KindOf(err))
	}
	if _, err := p.Plan(context.Background(), action.Request{SpecID: SpecClientRun}); errs.KindOf(err) != errs.Config {
		t.Errorf("no targets kind = %q, want config", errs.KindOf(err))
	}
	if _, err := NewPlanner(NewClient(ClientOptions{})).Plan(context.Background(), action.Request{
		SpecID:  SpecWhyRun,
		Targets: []action.Target{{Host: "web-01"}},
	}); errs.KindOf(err) != errs.Config {
		t.Errorf("no executor kind = %q, want config", errs.KindOf(err))
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("refusals made %d calls", len(fake.Calls()))
	}
}

// TestPlanTargetWithoutHost checks a target that names no host is refused.
func TestPlanTargetWithoutHost(t *testing.T) {
	p, _ := newTestPlanner(t, ClientOptions{})
	_, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecWhyRun,
		Targets: []action.Target{{Ref: resource.Ref{Kind: "unit", Name: "nginx.service"}}},
	})
	if errs.KindOf(err) != errs.Config {
		t.Errorf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
}
