package generic

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

// request builds a run request for hosts with the given params.
func request(hosts []resource.HostID, params map[string]string) action.Request {
	targets := make([]action.Target, 0, len(hosts))
	for _, h := range hosts {
		targets = append(targets, action.Target{Host: h})
	}
	return action.Request{SpecID: SpecRun, Targets: targets, Params: params, Actor: action.Human}
}

// hosts returns n fake host IDs.
func hosts(n int) []resource.HostID {
	out := make([]resource.HostID, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, resource.HostID("web-ewr-00"+string(rune('0'+i))))
	}
	return out
}

func TestActionsOffersRunCommand(t *testing.T) {
	fake := transporttest.New()
	p := New(map[string]transport.Executor{"ssh": fake}, "ssh")

	if got := p.Actions(nil); got != nil {
		t.Fatalf("Actions(nil) = %v, want nil", got)
	}
	specs := p.Actions([]resource.Resource{{Ref: resource.Ref{Kind: "host", Name: "web-ewr-001"}}})
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	s := specs[0]
	if s.ID != SpecRun || s.Title != "run command" {
		t.Fatalf("spec = %q %q, want %q %q", s.ID, s.Title, SpecRun, "run command")
	}
	if s.Risk != action.RiskDestructive {
		t.Fatalf("Risk = %v, want destructive", s.Risk)
	}
	got := map[string]action.Param{}
	for _, prm := range s.Params {
		got[prm.Name] = prm
	}
	if _, ok := got[ParamCommand]; !ok || !got[ParamCommand].Required {
		t.Fatalf("command param = %+v, want required", got[ParamCommand])
	}
	if got[ParamSudo].Default != "no" {
		t.Fatalf("sudo default = %q, want no", got[ParamSudo].Default)
	}
	if got[ParamVia].Default != "ssh" {
		t.Fatalf("via default = %q, want ssh", got[ParamVia].Default)
	}
}

func TestPlanOneStepPerTarget(t *testing.T) {
	fake := transporttest.New().On("ss -tlnp | wc -l", transporttest.Reply{Stdout: "TCP: 12\n"})
	p := New(map[string]transport.Executor{"ssh": fake}, "ssh")

	hs := hosts(3)
	plan, err := p.Plan(context.Background(), request(hs, map[string]string{ParamCommand: "ss -tlnp | wc -l"}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Spec.ID != SpecRun {
		t.Fatalf("spec = %q", plan.Spec.ID)
	}
	if plan.Preview != "" {
		t.Fatalf("preview = %q, want none", plan.Preview)
	}
	if len(plan.Steps) != len(hs) {
		t.Fatalf("got %d steps, want %d", len(plan.Steps), len(hs))
	}
	for i, st := range plan.Steps {
		if st.Target.Host != hs[i] {
			t.Fatalf("step %d host = %q, want %q", i, st.Target.Host, hs[i])
		}
		if st.Display != "ss -tlnp | wc -l" {
			t.Fatalf("step %d display = %q", i, st.Display)
		}
	}
	if plan.Via != "fake" {
		t.Fatalf("Via = %q, want the executor name", plan.Via)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", plan.Warnings)
	}
	if !strings.Contains(plan.Title, "run ss -tlnp | wc -l on 3 hosts") {
		t.Fatalf("Title = %q", plan.Title)
	}

	res := plan.Steps[1].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if res.RC != 0 || res.Output != "TCP: 12\n" {
		t.Fatalf("result = %+v", res)
	}
	calls := fake.Calls()
	if len(calls) != 1 || string(calls[0].Host) != "web-ewr-002" {
		t.Fatalf("executor calls = %+v", calls)
	}
	if calls[0].Command.Script != "ss -tlnp | wc -l" {
		t.Fatalf("script = %q, want the command verbatim", calls[0].Command.Script)
	}
	if calls[0].Command.Sudo {
		t.Fatal("command ran with sudo, want none")
	}
}

func TestPlanSudoPrefixedDisplay(t *testing.T) {
	p := New(map[string]transport.Executor{"ssh": transporttest.New()}, "ssh")
	plan, err := p.Plan(context.Background(), request(hosts(1), map[string]string{
		ParamCommand: "ss -s",
		ParamSudo:    "yes",
	}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := plan.Steps[0].Display; got != "sudo -n ss -s" {
		t.Fatalf("Display = %q, want %q", got, "sudo -n ss -s")
	}
	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
}

func TestPlanKeepsCommandVerbatim(t *testing.T) {
	fake := transporttest.New()
	p := New(map[string]transport.Executor{"ssh": fake}, "ssh")
	command := `printf '%s\n' "a  b" | sed -e 's/a/x/'`
	plan, err := p.Plan(context.Background(), request(hosts(1), map[string]string{ParamCommand: command}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Steps[0].Display != command {
		t.Fatalf("Display = %q, want the command untouched", plan.Steps[0].Display)
	}
	plan.Steps[0].Run(context.Background())
	if calls := fake.Calls(); len(calls) != 1 || calls[0].Command.Script != command {
		t.Fatalf("calls = %+v, want the command untouched", calls)
	}
}

func TestPlanRejectsBadParams(t *testing.T) {
	p := New(map[string]transport.Executor{"ssh": transporttest.New()}, "ssh")
	cases := []struct {
		name   string
		params map[string]string
	}{
		{"empty command", map[string]string{ParamCommand: "  "}},
		{"bad sudo", map[string]string{ParamCommand: "true", ParamSudo: "maybe"}},
		{"unknown transport", map[string]string{ParamCommand: "true", ParamVia: "salt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.Plan(context.Background(), request(hosts(1), c.params))
			if err == nil {
				t.Fatal("Plan succeeded, want an error")
			}
			if errs.KindOf(err) != errs.Config {
				t.Fatalf("kind = %v, want config", errs.KindOf(err))
			}
		})
	}
}

// rootExecutor is a transport that reports itself as Salt, which runs as root.
type rootExecutor struct{ *transporttest.Fake }

func (rootExecutor) Name() string { return "salt:salt.example.internal" }

func TestPlanWarnsOnRootTransport(t *testing.T) {
	p := New(map[string]transport.Executor{"salt": &rootExecutor{transporttest.New()}}, "salt")
	plan, err := p.Plan(context.Background(), request(hosts(2), map[string]string{ParamCommand: "uptime"}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Via != "salt:salt.example.internal" {
		t.Fatalf("Via = %q", plan.Via)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0] != "runs as root on every target" {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
}

func TestStepNonZeroRCIsFailure(t *testing.T) {
	fake := transporttest.New().On("false", transporttest.Reply{RC: 1, Stderr: "boom\n"})
	p := New(map[string]transport.Executor{"ssh": fake}, "ssh")

	b := bus.New(bus.Options{Rollout: bus.RolloutConfig{Canary: 1, BatchPercent: 100, FailureThresholdPercent: 5, TypedConfirmOver: 20}})
	if err := b.Register("generic", p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	prop, err := b.Propose(context.Background(), request(hosts(1), map[string]string{ParamCommand: "false"}))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	run, err := b.Execute(context.Background(), prop, bus.Confirmation{ProposalID: prop.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	s := waitSummary(t, run)
	if s.Failed != 1 || s.OK != 0 {
		t.Fatalf("summary = %+v, want one failed step", s)
	}
}

// waitSummary drains a run and returns its summary.
func waitSummary(t *testing.T, run bus.Run) bus.Summary {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				t.Fatal("run ended without a summary")
			}
			if ev.Summary != nil {
				return *ev.Summary
			}
		case <-timeout:
			t.Fatal("run did not finish")
		}
	}
}
