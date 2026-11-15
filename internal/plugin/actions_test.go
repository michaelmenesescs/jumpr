package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"jumpr/internal/action"
	"jumpr/internal/backend"
	"jumpr/internal/bus"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// loadedManager writes defs into a temp plugins directory, loads them and
// returns the manager with its registry.
func loadedManager(t *testing.T, ex transport.Executor, defs ...Definition) (*Manager, *backend.Registry) {
	t.Helper()
	dir := t.TempDir()
	for _, def := range defs {
		path := filepath.Join(dir, def.Name+".yaml")
		data, err := yaml.Marshal(def)
		if err != nil {
			t.Fatalf("marshal %s: %v", def.Name, err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	reg := backend.NewRegistry()
	m, err := NewManager(ManagerOptions{Dir: dir, Registry: reg, Exec: ex, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if problems := m.Load(); len(problems) > 0 {
		t.Fatalf("load problems: %v", problems)
	}
	return m, reg
}

// TestActionSpecIDs checks requirement 2: a plugin action is a spec of the
// "plugin" planner named plugin.<name>.<action>.
func TestActionSpecIDs(t *testing.T) {
	m, _ := loadedManager(t, transporttest.New(), testDefinition("nginx-status"))
	p := NewPlanner(m)
	if !p.Owns("plugin.nginx-status.reload") {
		t.Fatal("Owns must accept a plugin spec")
	}
	if p.Owns("generic.run") {
		t.Fatal("Owns must refuse another planner's spec")
	}

	specs := p.Actions([]resource.Resource{{Ref: resource.Ref{Kind: "host", Host: "web-01"}}})
	if len(specs) != 1 {
		t.Fatalf("Actions returned %d specs, want 1: %+v", len(specs), specs)
	}
	if specs[0].ID != "plugin.nginx-status.reload" {
		t.Fatalf("spec ID = %q, want plugin.nginx-status.reload", specs[0].ID)
	}
	if got := p.Actions(nil); len(got) != 0 {
		t.Fatalf("Actions on an empty selection = %+v, want none", got)
	}
}

// TestActionPlanBuildsOneStepPerTarget checks the plan the user confirms.
func TestActionPlanBuildsOneStepPerTarget(t *testing.T) {
	ex := transporttest.New()
	ex.On("", transporttest.Reply{})
	m, _ := loadedManager(t, ex, testDefinition("nginx-status"))
	p := NewPlanner(m)

	req := Request("nginx-status", "reload", []action.Target{{Host: "web-01"}, {Host: "web-02"}}, nil)
	plan, err := p.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want one per target", len(plan.Steps))
	}
	if plan.Steps[0].Display != "sudo -n systemctl reload nginx" {
		t.Fatalf("display = %q", plan.Steps[0].Display)
	}
	if plan.Request.Actor != "plugin:nginx-status" {
		t.Fatalf("actor = %q, want plugin:nginx-status", plan.Request.Actor)
	}
	if plan.Spec.ID != "plugin.nginx-status.reload" {
		t.Fatalf("plan spec = %q", plan.Spec.ID)
	}
	if len(plan.Warnings) == 0 || !strings.Contains(strings.Join(plan.Warnings, " "), "root") {
		t.Fatalf("warnings = %v, want the sudo warning", plan.Warnings)
	}
}

// TestActionUnknownSpecFails checks that a spec nobody declares is refused.
func TestActionUnknownSpecFails(t *testing.T) {
	m, _ := loadedManager(t, transporttest.New(), testDefinition("nginx-status"))
	p := NewPlanner(m)
	for _, id := range []string{
		"plugin.nginx-status.missing",
		"plugin.other.reload",
		"plugin.nginx-status",
	} {
		if _, err := p.Plan(context.Background(), action.Request{
			SpecID:  id,
			Targets: []action.Target{{Host: "web-01"}},
		}); err == nil {
			t.Errorf("spec %q was planned", id)
		}
	}
}

// TestActionThroughTheBus is requirement 2's real rule: the action only runs
// through the bus, which applies policy, rolls it out and records the actor
// plugin:<name>.
func TestActionThroughTheBus(t *testing.T) {
	ex := transporttest.New()
	ex.On("", transporttest.Reply{Stdout: "reloading\n"})
	def := testDefinition("nginx-status")
	def.Actions[0].Privileged = false
	m, _ := loadedManager(t, ex, def)

	b := bus.New(bus.Options{
		Context: context.Background(),
		Rollout: bus.RolloutConfig{
			Canary:                  1,
			BatchPercent:            100,
			BatchMax:                50,
			FailureThresholdPercent: 5,
			TypedConfirmOver:        20,
		},
	})
	if err := b.Register("plugin", NewPlanner(m)); err != nil {
		t.Fatalf("register planner: %v", err)
	}
	req := Request("nginx-status", "reload", []action.Target{{Host: "web-01"}}, nil)
	p, err := b.Propose(context.Background(), req)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.Plan.Request.Actor != "plugin:nginx-status" {
		t.Fatalf("recorded actor = %q, want plugin:nginx-status", p.Plan.Request.Actor)
	}
	if len(ex.Calls()) != 0 {
		t.Fatalf("proposing must not run anything, saw %d calls", len(ex.Calls()))
	}
	run, err := b.Execute(context.Background(), p, bus.Confirmation{ProposalID: p.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var summary *bus.Summary
	for ev := range run.Events() {
		switch {
		case ev.Phase == bus.PhasePaused:
			run.Continue()
		case ev.Summary != nil:
			s := *ev.Summary
			summary = &s
		}
	}
	if summary == nil || summary.OK != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want 1 ok", summary)
	}
	calls := ex.Calls()
	if len(calls) != 1 || calls[0].Command.Script != "systemctl reload nginx" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Host != "web-01" {
		t.Fatalf("host = %q", calls[0].Host)
	}
}

// TestActionRequestCarriesThePluginActor keeps the helper honest: the actor is
// set when the request is built, not only when it is planned.
func TestActionRequestCarriesThePluginActor(t *testing.T) {
	req := Request("cert-expiry", "resync", []action.Target{{Host: "web-01"}}, map[string]string{"x": "y"})
	if req.Actor != action.Actor("plugin:cert-expiry") {
		t.Fatalf("actor = %q", req.Actor)
	}
	if req.SpecID != "plugin.cert-expiry.resync" {
		t.Fatalf("spec = %q", req.SpecID)
	}
	if fmt.Sprint(Actor("x")) != "plugin:x" {
		t.Fatalf("Actor = %q", Actor("x"))
	}
}
