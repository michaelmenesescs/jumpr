package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/backend/chef"
	"jumper/internal/bus"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// chefFake is the read half of a Chef backend: it answers the drift check and
// records what it was asked for.
type chefFake struct {
	rep      chef.DriftReport
	err      error
	gotHosts []resource.HostID
	gotAttr  string
}

var _ ChefDrift = (*chefFake)(nil)

func (f *chefFake) Name() string                            { return chefBackendName }
func (f *chefFake) Kinds() []resource.KindSpec              { return nil }
func (f *chefFake) Links(resource.Resource) []resource.Link { return nil }

func (f *chefFake) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	return nil, nil
}

func (f *chefFake) Get(_ context.Context, ref resource.Ref) (resource.Resource, error) {
	return resource.Resource{Ref: ref}, nil
}

func (f *chefFake) Health(context.Context) backend.Health {
	return backend.Health{State: backend.HealthOK, Checked: time.Now()}
}

func (f *chefFake) Drift(_ context.Context, hosts []resource.HostID, attr string) (chef.DriftReport, error) {
	f.gotHosts, f.gotAttr = hosts, attr
	return f.rep, f.err
}

// chefHarness is a root with `:chef` registered over a fake backend and a bus
// holding the real chef planner.
type chefHarness struct {
	env  *Env
	cmds *Commands
	ops  *chefFake
	b    bus.Bus
}

// newChefHarness builds the harness for the given session targets. When drift
// is false the registry holds no Chef backend, which is what an unconfigured
// deployment looks like.
func newChefHarness(t *testing.T, hosts []resource.HostID, withChef bool, opts ChefOptions) *chefHarness {
	t.Helper()
	theme.SetProfile("none")

	exec := transporttest.New().OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if strings.Contains(cmd.Script, "--why-run") {
			return transporttest.Reply{Stdout: "Chef Infra Client finished, 1/4 resources would have been updated\n"}, true
		}
		return transporttest.Reply{Stdout: "Chef Infra Client finished, 1/4 resources updated\n"}, true
	})

	reg := backend.NewRegistry()
	ops := &chefFake{}
	if withChef {
		if err := reg.Register(ops); err != nil {
			t.Fatalf("register chef fake: %v", err)
		}
	}

	b := bus.New(bus.Options{
		Now: time.Now,
		Rollout: bus.RolloutConfig{
			Canary:                  1,
			BatchPercent:            100,
			BatchMax:                50,
			FailureThresholdPercent: 100,
			TypedConfirmOver:        20,
		},
	})
	if err := b.Register(chefBackendName, chef.NewPlanner(chef.NewClient(chef.ClientOptions{Exec: exec}))); err != nil {
		t.Fatalf("register chef planner: %v", err)
	}

	env := &Env{
		Registry:  reg,
		Session:   &SessionState{Name: "main", Targets: targets.FromHosts("test", hosts)},
		Exec:      exec,
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: map[resource.KindName]Factory{},
	}
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	if err := RegisterChefCommands(cmds, b, opts); err != nil {
		t.Fatalf("register :chef: %v", err)
	}
	return &chefHarness{env: env, cmds: cmds, ops: ops, b: b}
}

// run dispatches one `:` line and returns the single message it produced.
func (h *chefHarness) run(t *testing.T, line string) tea.Msg {
	t.Helper()
	cmd := h.cmds.Exec(h.env, line)
	if cmd == nil {
		t.Fatalf(":%s produced no command", line)
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if msg == nil {
			t.Fatalf(":%s produced no message", line)
		}
		return msg
	case <-time.After(10 * time.Second):
		t.Fatalf(":%s timed out", line)
	}
	return nil
}

// driftReport is a small report: two web nodes with one run list, one db node
// with another, and a host Chef does not know.
func driftReport() chef.DriftReport {
	return chef.DriftReport{
		Rows: []chef.DriftRow{
			{Host: "web-01", Node: "web-01", Known: true, RunList: []string{"role[web]"},
				Checkin: 5 * time.Minute, HasCheckin: true, Status: resource.StatusOK},
			{Host: "web-02", Node: "web-02", Known: true, RunList: []string{"role[web]"},
				Checkin: 3 * time.Hour, HasCheckin: true, Status: resource.StatusWarn},
			{Host: "db-01", Node: "db-01", Known: true, RunList: []string{"role[db]"},
				Checkin: 30 * time.Hour, HasCheckin: true, Status: resource.StatusCrit},
			{Host: "ghost-01", Status: resource.StatusNA},
		},
		Groups: []chef.RunListGroup{
			{Key: "role[web]", RunList: []string{"role[web]"}, Nodes: []string{"web-01", "web-02"}},
			{Key: "role[db]", RunList: []string{"role[db]"}, Nodes: []string{"db-01"}},
		},
	}
}

// TestChefDriftOpensTheTable checks requirement 1: `:chef drift` reads the
// session targets and opens a table of check-in age, status and run-list
// groups.
func TestChefDriftOpensTheTable(t *testing.T) {
	hosts := []resource.HostID{"web-01", "web-02", "db-01", "ghost-01"}
	h := newChefHarness(t, hosts, true, ChefOptions{RunStatusAttr: "run_status.success"})
	h.ops.rep = driftReport()
	h.ops.rep.Attr = "run_status.success"
	h.ops.rep.Rows[0].RunStatus = "true"

	msg := h.run(t, "chef drift")
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf("msg = %T, want PushViewMsg", msg)
	}
	page, ok := push.View.(*textPage)
	if !ok {
		t.Fatalf("view = %T, want the text page", push.View)
	}
	body := strings.Join(page.lines, "\n")
	for _, want := range []string{"4 nodes", "2 identical · 1 differ", "1 not in chef", "NODE",
		"CHECK-IN", "RUN STATUS", "web-01", "warn", "crit", "ghost-01", "role[web]"} {
		if !strings.Contains(body, want) {
			t.Errorf("drift page has no %q:\n%s", want, body)
		}
	}
	if h.ops.gotAttr != "run_status.success" {
		t.Errorf("attribute = %q, want the configured path", h.ops.gotAttr)
	}
	if len(h.ops.gotHosts) != 4 {
		t.Errorf("hosts = %v, want the session targets", h.ops.gotHosts)
	}
}

// TestChefDriftHidesRunStatusByDefault checks SPEC §20 question 3's default: no
// attribute configured, no column.
func TestChefDriftHidesRunStatusByDefault(t *testing.T) {
	h := newChefHarness(t, []resource.HostID{"web-01"}, true, ChefOptions{})
	h.ops.rep = chef.DriftReport{Rows: []chef.DriftRow{{
		Host: "web-01", Node: "web-01", Known: true, HasCheckin: true, Status: resource.StatusOK,
	}}}
	msg := h.run(t, "chef drift")
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf("msg = %T, want PushViewMsg", msg)
	}
	body := strings.Join(push.View.(*textPage).lines, "\n")
	if strings.Contains(body, "RUN STATUS") {
		t.Errorf("the run-status column is shown with no attribute configured:\n%s", body)
	}
	if !strings.Contains(body, "chef.run_status_attr") {
		t.Errorf("the page does not say how to show the column:\n%s", body)
	}
}

// TestChefDriftRefusals checks the two things that stop a drift check before a
// knife call: no Chef backend and no targets.
func TestChefDriftRefusals(t *testing.T) {
	none := newChefHarness(t, []resource.HostID{"web-01"}, false, ChefOptions{})
	msg := none.run(t, "chef drift")
	flash, ok := msg.(FlashMsg)
	if !ok || flash.Level != FlashError || !strings.Contains(flash.Text, "chef.workstation") {
		t.Errorf("unconfigured message = %+v, want a flash naming chef.workstation", msg)
	}

	empty := newChefHarness(t, nil, true, ChefOptions{})
	msg = empty.run(t, "chef drift")
	flash, ok = msg.(FlashMsg)
	if !ok || !strings.Contains(flash.Text, "no targets") {
		t.Errorf("no-targets message = %+v, want a flash about targets", msg)
	}
}

// TestChefWhyrunProposesThroughTheBus checks requirement 2: the why-run is an
// action of its own, proposed at normal risk.
func TestChefWhyrunProposesThroughTheBus(t *testing.T) {
	h := newChefHarness(t, []resource.HostID{"web-01", "web-02"}, true, ChefOptions{})
	msg := h.run(t, "chef whyrun")
	open, ok := msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf("msg = %T, want OpenDialogMsg", msg)
	}
	dialog, ok := open.Dialog.(*runPlanDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the run plan dialog", open.Dialog)
	}
	p := dialog.p
	if p.Plan.Spec.ID != chef.SpecWhyRun {
		t.Errorf("spec = %q, want %q", p.Plan.Spec.ID, chef.SpecWhyRun)
	}
	if p.Plan.Spec.Risk != action.RiskNormal {
		t.Errorf("risk = %v, want normal", p.Plan.Spec.Risk)
	}
	if len(p.Plan.Steps) != 2 {
		t.Errorf("steps = %d, want one per host", len(p.Plan.Steps))
	}
	if p.Plan.Preview != "" {
		t.Errorf("preview = %q, want none: the why-run is the dry run", p.Plan.Preview)
	}
}

// TestChefRunPreviewIsTheWhyRun checks requirement 3 from the command side: the
// `:chef run` plan carries the why-run of the same hosts.
func TestChefRunPreviewIsTheWhyRun(t *testing.T) {
	h := newChefHarness(t, []resource.HostID{"web-01", "web-02"}, true, ChefOptions{})
	msg := h.run(t, "chef run")
	open, ok := msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf("msg = %T, want OpenDialogMsg", msg)
	}
	p := open.Dialog.(*runPlanDialog).p
	if p.Plan.Spec.ID != chef.SpecClientRun {
		t.Fatalf("spec = %q, want %q", p.Plan.Spec.ID, chef.SpecClientRun)
	}
	if p.Plan.Spec.Risk != action.RiskDestructive {
		t.Errorf("risk = %v, want destructive", p.Plan.Spec.Risk)
	}
	if !strings.Contains(p.Plan.Preview, "would have been updated") {
		t.Errorf("preview = %q, want the why-run result", p.Plan.Preview)
	}
	if len(p.Plan.Steps) != 2 {
		t.Errorf("steps = %d, want one per host", len(p.Plan.Steps))
	}
	if !strings.Contains(p.Plan.Steps[0].Display, "sudo -n chef-client") {
		t.Errorf("display = %q", p.Plan.Steps[0].Display)
	}
}

// TestChefUsageErrors checks the argument grammar is enforced.
func TestChefUsageErrors(t *testing.T) {
	h := newChefHarness(t, []resource.HostID{"web-01"}, true, ChefOptions{})
	for _, line := range []string{"chef", "chef nope", "chef drift now"} {
		msg := h.run(t, line)
		flash, ok := msg.(FlashMsg)
		if !ok || flash.Level != FlashError {
			t.Errorf(":%s = %+v, want a usage flash", line, msg)
			continue
		}
		if !strings.Contains(flash.Text, ":chef") {
			t.Errorf(":%s message = %q, want the usage line", line, flash.Text)
		}
	}
}
