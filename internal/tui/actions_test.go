package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/bus/bustest"
	"jumper/internal/resource"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// The action flow of task 061: the menu, the plan dialog, the confirmation
// controls, the denial and the run dialog with its canary pause.

// actionHarness is a root model over a real bus and scripted planners.
type actionHarness struct {
	env  *Env
	root *Root
	b    bus.Bus
}

// actionRollout is the rollout shape the tests plan against: one canary host,
// then rolling batches.
func actionRollout(percent int) bus.RolloutConfig {
	return bus.RolloutConfig{
		Canary:                  1,
		BatchPercent:            percent,
		BatchMax:                50,
		FailureThresholdPercent: 5,
		TypedConfirmOver:        20,
	}
}

// newActionHarness builds a root with the planners registered on a real bus.
func newActionHarness(t *testing.T, planners map[string]action.Planner, rollout bus.RolloutConfig) *actionHarness {
	t.Helper()
	theme.SetProfile("none")
	b := bus.New(bus.Options{Rollout: rollout})
	env := &Env{
		Session:  &SessionState{Name: "main"},
		Theme:    theme.Dark(),
		Keys:     keymap.Default(),
		Now:      time.Now,
		Bus:      b,
		Planners: make(map[string]action.Planner, len(planners)),
		ResultsView: func(env *Env, title string, results []action.StepResult) View {
			return newRunResults(bus.Proposal{Plan: action.Plan{Title: title}}, results)
		},
	}
	for name, pl := range planners {
		if err := b.Register(name, pl); err != nil {
			t.Fatalf("register planner %q: %v", name, err)
		}
		env.Planners[name] = pl
	}
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	root := NewRoot(env, cmds, nil, resource.Query{Kind: "host"}, "ctrl+a")
	root.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	return &actionHarness{env: env, root: root, b: b}
}

// hosts returns n host resources, named web-01, web-02, …
func actionHosts(n int) []resource.Resource {
	out := make([]resource.Resource, 0, n)
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("web-%02d", i)
		out = append(out, resource.Resource{
			Ref:    resource.Ref{Kind: "host", Name: name, Host: resource.HostID(name)},
			Fields: map[string]any{"host": name},
		})
	}
	return out
}

// targets turns host resources into bus targets, the way the menu does.
func selTargets(sel []resource.Resource) []action.Target {
	out := make([]action.Target, 0, len(sel))
	for _, r := range sel {
		out = append(out, actionTargetOf(r))
	}
	return out
}

// propose plans spec over n hosts through the harness's bus.
func (h *actionHarness) propose(t *testing.T, spec string, n int) bus.Proposal {
	t.Helper()
	p, err := h.b.Propose(context.Background(), action.Request{
		SpecID:  spec,
		Targets: selTargets(actionHosts(n)),
		Actor:   action.Human,
	})
	if err != nil {
		t.Fatalf("propose %s over %d hosts: %v", spec, n, err)
	}
	return p
}

// keys sends one key press to the running model.
func actionKey(t *testing.T, tm *teatest.TestModel, key string) {
	t.Helper()
	switch key {
	case "enter":
		tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	case "esc":
		tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	default:
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	}
}

// actionWait waits until the output produced from now on contains every want.
func actionWait(t *testing.T, tm *teatest.TestModel, want ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, w := range want {
			if !strings.Contains(string(b), w) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(5*time.Second))
}

// actionFinish quits the program and returns the final screen.
func actionFinish(t *testing.T, tm *teatest.TestModel) string {
	t.Helper()
	tm.Quit()
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	root, ok := m.(*Root)
	if !ok {
		t.Fatalf("final model is %T", m)
	}
	return root.View()
}

// actionStart launches the harness at 120×36.
func (h *actionHarness) start(t *testing.T) *teatest.TestModel {
	t.Helper()
	return teatest.NewTestModel(t, h.root, teatest.WithInitialTermSize(120, 36))
}

// TestMenuListsSpecs checks requirement 1: `x` yields every planner's specs,
// de-duplicated by ID, and destructive ones carry a "!".
func TestActionMenuListsSpecs(t *testing.T) {
	restart := bustest.New(bustest.Options{SpecID: "fake.restart", Title: "restart"})
	stop := bustest.New(bustest.Options{SpecID: "fake.stop", Title: "stop", Risk: action.RiskDestructive})
	// A second planner offering the same ID must not add a second row.
	dup := bustest.New(bustest.Options{SpecID: "fake.restart", Title: "restart"})
	h := newActionHarness(t, map[string]action.Planner{
		"fake": restart, "danger": stop, "dup": dup,
	}, actionRollout(100))

	h.root.Update(ActionsRequestMsg{Kind: "host", Selection: actionHosts(2)})
	menu, ok := h.root.dialog.(*actionsMenu)
	if !ok {
		t.Fatalf("dialog = %T, want the actions menu", h.root.dialog)
	}
	if len(menu.specs) != 2 {
		t.Fatalf("menu has %d specs, want 2 (de-duplicated): %+v", len(menu.specs), menu.specs)
	}
	view := menu.View(60, 10)
	for _, want := range []string{"restart", "stop", "! stop"} {
		if !strings.Contains(view, want) {
			t.Fatalf("menu view has no %q:\n%s", want, view)
		}
	}

	// An empty planner set is a flash, not an empty box.
	h2 := newActionHarness(t, nil, actionRollout(100))
	cmd := h2.root.openActions(ActionsRequestMsg{Kind: "host", Selection: actionHosts(1)})
	if cmd == nil {
		t.Fatal("an empty menu produced no command")
	}
	flash, ok := cmd().(FlashMsg)
	if !ok || !strings.Contains(flash.Text, "no actions for host") {
		t.Fatalf("empty menu flash = %#v", cmd())
	}
}

// TestPlanShowsCommandAndRollout checks requirement 3: the plan dialog shows the
// target count, the first ten hosts, the command, the transport, the warnings,
// the preview and the rollout shape.
func TestPlanShowsCommandAndRollout(t *testing.T) {
	h := newActionHarness(t, nil, actionRollout(100))
	steps := make([]action.Step, 0, 12)
	for _, host := range actionHosts(12) {
		steps = append(steps, action.Step{
			Target:  actionTargetOf(host),
			Display: "systemctl restart nginx",
		})
	}
	p := bus.Proposal{
		ID: "p1",
		Plan: action.Plan{
			Spec:     action.Spec{ID: "fake.restart", Title: "restart"},
			Title:    "restart nginx on 12 hosts",
			Steps:    steps,
			Via:      "ssh",
			Preview:  "nginx: active\nnginx: enabled",
			Warnings: []string{"this is irreversible"},
		},
		Confirm: bus.ConfirmList,
		Rollout: bus.Rollout{Canary: 1, BatchSize: 6, Batches: 4, FailureThreshold: 0.05},
	}
	h.root.Update(ProposalMsg{Proposal: p})
	d, ok := h.root.dialog.(*planDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the plan dialog", h.root.dialog)
	}
	view := d.View(100, 30)
	for _, want := range []string{
		"targets: 12 hosts",
		"web-01",
		"web-10",
		"+2 more",
		"command: systemctl restart nginx",
		"via: ssh",
		"warning: this is irreversible",
		"preview:",
		"nginx: enabled",
		"rollout: canary 1 → pause → 4 batches of 6 · stop if >5% fail",
		"y confirm · n cancel",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("plan view has no %q:\n%s", want, view)
		}
	}
	if !strings.Contains(strings.Join(d.body(200), "\n"), "+2 more · l list all") {
		t.Fatalf("the summary does not point at the full list:\n%s", strings.Join(d.body(200), "\n"))
	}
	if strings.Contains(view, "web-11") {
		t.Fatalf("plan view lists more than ten hosts:\n%s", view)
	}

	// `l` lists every host, in a scrollable body.
	h.root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	body := strings.Join(d.body(100), "\n")
	if !strings.Contains(body, "web-12") {
		t.Fatalf("the host list has no web-12:\n%s", body)
	}

	// Per-host commands are not printed in the summary.
	p.Plan.Steps[1].Display = "systemctl restart haproxy"
	other := newPlanDialog(theme.Dark(), p)
	if view := other.View(100, 30); !strings.Contains(view, "per-host commands (12) · l to list") {
		t.Fatalf("per-host plan has no pointer at the list:\n%s", view)
	}
}

// TestYesNoConfirmExecutes checks requirements 3 and 4 for a one-target plan:
// `y` executes through the bus and the run dialog reports the rollout.
func TestActionYesNoConfirmExecutes(t *testing.T) {
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Title: "restart"})
	h := newActionHarness(t, map[string]action.Planner{"fake": pl}, actionRollout(100))
	p := h.propose(t, "fake.restart", 1)
	if p.Confirm != bus.ConfirmYesNo {
		t.Fatalf("confirm level = %v, want yes/no", p.Confirm)
	}

	tm := h.start(t)
	tm.Send(ProposalMsg{Proposal: p})
	actionWait(t, tm, "command: fake.restart web-01", "y confirm · n cancel")

	actionKey(t, tm, "y")
	actionWait(t, tm, "ok web-01", "done · ok 1")
	if got := len(pl.Ran()); got != 1 {
		t.Fatalf("the rollout ran %d steps, want 1", got)
	}
	if _, open := h.root.dialog.(*runDialog); !open {
		// The dialog stays up for the summary; only `enter` closes it.
		t.Logf("dialog after finish = %T", h.root.dialog)
	}
	actionFinish(t, tm)
}

// TestTypedCountWrongThenRight checks requirement 3's typed-count control on 21
// targets: the wrong number shakes and stays, the right one executes.
func TestActionTypedCountWrongThenRight(t *testing.T) {
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Title: "restart"})
	h := newActionHarness(t, map[string]action.Planner{"fake": pl}, actionRollout(100))
	p := h.propose(t, "fake.restart", 21)
	if p.Confirm != bus.ConfirmTypedCount {
		t.Fatalf("confirm level = %v, want typed count", p.Confirm)
	}

	tm := h.start(t)
	tm.Send(ProposalMsg{Proposal: p})
	actionWait(t, tm, "type 21 to confirm")

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("15")})
	actionKey(t, tm, "enter")
	actionWait(t, tm, "does not match — type 21")
	if ran := len(pl.Ran()); ran != 0 {
		t.Fatalf("a wrong count executed %d steps", ran)
	}

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("21")})
	actionKey(t, tm, "enter")
	// 21 targets keep the canary, so the rollout pauses once more.
	actionWait(t, tm, "canary ok — c continue · esc abort")
	actionKey(t, tm, "c")
	actionWait(t, tm, "done · ok 21")
	if ran := len(pl.Ran()); ran != 21 {
		t.Fatalf("the rollout ran %d steps, want 21", ran)
	}
	actionFinish(t, tm)
}

// TestDeniedShowsRule checks requirement 3's denial: the rule and the reason are
// shown and there is no way to execute the plan.
func TestActionDeniedShowsRule(t *testing.T) {
	h := newActionHarness(t, nil, actionRollout(100))
	p := bus.Proposal{
		ID:      "denied-1",
		Plan:    action.Plan{Title: "stop sshd", Spec: action.Spec{ID: "fake.stop"}},
		Confirm: bus.ConfirmYesNo,
		Denied:  &bus.Denial{Rule: "never stop sshd", Reason: "use your change process"},
	}
	h.root.Update(ProposalMsg{Proposal: p})
	d, ok := h.root.dialog.(*planDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the plan dialog", h.root.dialog)
	}
	view := d.View(100, 20)
	for _, want := range []string{"denied by policy", "rule: never stop sshd", "reason: use your change process", "esc close"} {
		if !strings.Contains(view, want) {
			t.Fatalf("denied plan has no %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "y confirm") {
		t.Fatalf("a denied plan offers a confirmation control:\n%s", view)
	}

	// y must not produce a confirmation, and the dialog must stay up.
	if _, cmd := h.root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); cmd != nil {
		t.Fatalf("y on a denied plan produced %T", cmd())
	}
	if len(h.root.runs) != 0 {
		t.Fatalf("a denied plan started %d rollouts", len(h.root.runs))
	}
	if _, open := h.root.dialog.(*planDialog); !open {
		t.Fatalf("the denial closed itself: %T", h.root.dialog)
	}
	// Only esc closes it.
	h.root.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.root.dialog != nil {
		t.Fatalf("esc left %T on screen", h.root.dialog)
	}
}

// TestCanaryPauseContinue checks requirement 4's pause: the canary runs, the
// dialog says how to continue, and `c` finishes the batches.
func TestActionCanaryPauseContinue(t *testing.T) {
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Title: "restart"})
	h := newActionHarness(t, map[string]action.Planner{"fake": pl}, actionRollout(100))
	p := h.propose(t, "fake.restart", 3)

	tm := h.start(t)
	tm.Send(ProposalMsg{Proposal: p})
	actionWait(t, tm, "canary 1 → pause", "y confirm")
	actionKey(t, tm, "y")

	actionWait(t, tm, "canary · 1 host", "canary ok — c continue · esc abort")
	actionKey(t, tm, "c")
	actionWait(t, tm, "batch 1/1 · 2 hosts · ok 2", "done · ok 3")
	if ran := len(pl.Ran()); ran != 3 {
		t.Fatalf("the rollout ran %d steps, want 3", ran)
	}
	actionFinish(t, tm)
}

// fakeRun is a bus.Run the abort test drives by hand.
type fakeRun struct {
	events    chan bus.Event
	aborted   bool
	continued bool
}

func (f *fakeRun) Events() <-chan bus.Event { return f.events }
func (f *fakeRun) Continue()                { f.continued = true }
func (f *fakeRun) Abort()                   { f.aborted = true }

// TestAbort checks requirement 4's abort: esc at the canary pause aborts, and
// esc during the batches asks "abort remaining batches?" first.
func TestActionAbort(t *testing.T) {
	// During the batches esc asks before it aborts.
	d := newRunDialog(nil, bus.Proposal{
		Plan:    action.Plan{Title: "restart"},
		Rollout: bus.Rollout{Canary: 1, BatchSize: 3, Batches: 2},
	}, 1)
	run := &fakeRun{events: make(chan bus.Event)}
	d.run = run
	d.phase = bus.PhaseBatch
	if next, _ := d.Esc(); next == nil {
		t.Fatal("esc closed a running dialog instead of asking")
	}
	if !strings.Contains(d.View(80, 10), "abort remaining batches? y/N") {
		t.Fatalf("esc did not ask:\n%s", d.View(80, 10))
	}
	// n leaves the rollout alone.
	d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if run.aborted {
		t.Fatal("n aborted the rollout")
	}
	// y aborts it.
	d.Esc()
	d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if !run.aborted {
		t.Fatal("y did not abort the rollout")
	}

	// At the canary pause esc aborts without asking, through the real bus.
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Title: "restart"})
	h := newActionHarness(t, map[string]action.Planner{"fake": pl}, actionRollout(50))
	p := h.propose(t, "fake.restart", 6)
	if p.Rollout.Batches != 2 {
		t.Fatalf("rollout has %d batches, want 2", p.Rollout.Batches)
	}

	tm := h.start(t)
	tm.Send(ProposalMsg{Proposal: p})
	actionWait(t, tm, "y confirm")
	actionKey(t, tm, "y")
	actionWait(t, tm, "canary ok — c continue · esc abort")
	actionKey(t, tm, "esc")
	actionWait(t, tm, "aborted · ok 1")
	if ran := len(pl.Ran()); ran != 1 {
		t.Fatalf("aborting the canary ran %d steps beyond it, want 1 total", ran)
	}
	actionFinish(t, tm)
}

// TestHideAndReopenRun checks requirement 5: `h` hides the dialog, the status
// line carries the badge and `:runs` reopens the rollout.
func TestRunDialogHideAndReopen(t *testing.T) {
	pl := bustest.New(bustest.Options{SpecID: "fake.restart", Title: "restart"})
	h := newActionHarness(t, map[string]action.Planner{"fake": pl}, actionRollout(50))

	// The badge itself, before a program exists: a hidden run is advertised in
	// the status line's context slot.
	hidden := newRunDialog(h.env, bus.Proposal{
		Plan:    action.Plan{Title: "restart nginx"},
		Rollout: bus.Rollout{Canary: 1, BatchSize: 6, Batches: 4},
	}, 7)
	hidden.hidden = true
	hidden.batch = 2
	h.root.runs = append(h.root.runs, hidden)
	if got := h.root.statusData().Context; got != "▶ rollout 2/4" {
		t.Fatalf("status badge = %q, want %q", got, "▶ rollout 2/4")
	}
	if !strings.Contains(h.root.View(), "rollout 2/4") {
		t.Fatalf("the status line has no rollout badge:\n%s", h.root.View())
	}
	h.root.runs = nil

	p := h.propose(t, "fake.restart", 6)
	tm := h.start(t)
	tm.Send(ProposalMsg{Proposal: p})
	actionWait(t, tm, "y confirm")
	actionKey(t, tm, "y")
	actionWait(t, tm, "canary ok — c continue · esc abort")

	actionKey(t, tm, "h")
	// The hide is a message of its own: wait for it before typing, or the
	// keys land in the run dialog that is still on screen.
	actionWait(t, tm, ":runs to reopen")
	tm.Type(":runs")
	actionKey(t, tm, "enter")
	actionWait(t, tm, "active runs", "paused after canary")
	actionKey(t, tm, "enter")
	actionWait(t, tm, "canary ok — c continue · esc abort")

	// Continuing proves the reopened dialog is the live one.
	actionKey(t, tm, "c")
	actionWait(t, tm, "done · ok 6")
	actionFinish(t, tm)
}

// TestRunDialogResultsView checks requirement 4's `o`: a finished rollout opens
// its per-host results as a grouped view over the focused pane.
func TestRunDialogResultsView(t *testing.T) {
	h := newActionHarness(t, nil, actionRollout(100))
	p := bus.Proposal{
		ID:   "p1",
		Plan: action.Plan{Title: "restart nginx", Spec: action.Spec{ID: "fake.restart"}},
	}
	d := newRunDialog(h.env, p, 1)
	d.done = true
	d.summary = &bus.Summary{OK: 1}
	d.results = []action.StepResult{{Target: action.Target{Host: "web-01"}, Output: "hi\n"}}
	h.root.runs = []*runDialog{d}
	h.root.dialog = d

	_, cmd := h.root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if cmd == nil {
		t.Fatal("o produced no command")
	}
	msg := cmd()
	if _, ok := msg.(runResultsRequestMsg); !ok {
		t.Fatalf("o produced %T, want the results request", msg)
	}
	_, _ = h.root.Update(msg)
	if h.root.dialog != nil {
		t.Fatalf("o left %T on screen", h.root.dialog)
	}
	_, c := h.root.mux.Focused()
	vp, ok := c.(*ViewPane)
	if !ok {
		t.Fatalf("focused content is %T, want a view pane", c)
	}
	if vp.stack.Len() != 2 {
		t.Fatalf("the pane has %d views, want the results view pushed over the list", vp.stack.Len())
	}
	if crumb := vp.stack.Top().Crumb(); !strings.Contains(crumb, "restart nginx") {
		t.Fatalf("results crumb = %q", crumb)
	}
}

// paramPlanner is a planner with parameters: it offers one spec with two params
// and a Plan that can be made to wait for its context.
type paramPlanner struct {
	spec  action.Spec
	block bool
}

func (p *paramPlanner) Actions([]resource.Resource) []action.Spec {
	return []action.Spec{p.spec}
}

func (p *paramPlanner) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	if p.block {
		<-ctx.Done()
		return action.Plan{}, ctx.Err()
	}
	plan := action.Plan{Spec: p.spec, Request: req, Title: "restart " + req.Params["service"], Via: "ssh"}
	for _, target := range req.Targets {
		target := target
		plan.Steps = append(plan.Steps, action.Step{
			Target:  target,
			Display: "systemctl restart " + req.Params["service"],
		})
	}
	return plan, nil
}

// TestActionParamsFormAndPlanning checks requirement 2: the parameter form with
// defaults and choices, the planning spinner, and that esc cancels the planning
// context.
func TestActionParamsFormAndPlanning(t *testing.T) {
	pp := &paramPlanner{
		spec: action.Spec{
			ID:    "svc.restart",
			Title: "restart service",
			Params: []action.Param{
				{Name: "service", Title: "service", Default: "nginx", Required: true},
				{Name: "signal", Title: "signal", Default: "TERM", Choices: []string{"TERM", "HUP"}},
			},
		},
		block: true,
	}
	h := newActionHarness(t, map[string]action.Planner{"svc": pp}, actionRollout(100))

	h.root.Update(ActionsRequestMsg{Kind: "host", Selection: actionHosts(1)})
	menu, ok := h.root.dialog.(*actionsMenu)
	if !ok {
		t.Fatalf("dialog = %T, want the menu", h.root.dialog)
	}
	if len(menu.specs[0].Params) != 2 {
		t.Fatalf("spec has %d params, want 2", len(menu.specs[0].Params))
	}

	// Choosing a spec with params opens the form with the defaults filled in.
	_, cmd := h.root.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the menu produced no command")
	}
	h.root.Update(cmd())
	form, ok := h.root.dialog.(*paramForm)
	if !ok {
		t.Fatalf("dialog = %T, want the parameter form", h.root.dialog)
	}
	if got := form.fields[0].input.Value(); got != "nginx" {
		t.Fatalf("service default = %q, want nginx", got)
	}
	// left/right cycles the choices of the second field.
	h.root.Update(tea.KeyMsg{Type: tea.KeyTab})
	h.root.Update(tea.KeyMsg{Type: tea.KeyRight})
	if got := form.fields[1].input.Value(); got != "HUP" {
		t.Fatalf("signal after right = %q, want HUP", got)
	}

	// Enter submits the form; the root turns that into a planning round-trip
	// and shows the spinner while the command runs.
	_, submitted := h.root.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if submitted == nil {
		t.Fatal("the form produced no message")
	}
	params, ok := submitted().(actionParamsMsg)
	if !ok {
		t.Fatalf("the form produced %T", submitted())
	}
	if params.Params["service"] != "nginx" || params.Params["signal"] != "HUP" {
		t.Fatalf("form params = %#v", params.Params)
	}
	_, plan := h.root.Update(params)
	if plan == nil {
		t.Fatal("the form produced no planning command")
	}
	if _, ok := h.root.dialog.(*planningDialog); !ok {
		t.Fatalf("dialog = %T, want the planning spinner", h.root.dialog)
	}
	if !strings.Contains(h.root.dialog.View(40, 6), "planning…") {
		t.Fatalf("the spinner has no planning text:\n%s", h.root.dialog.View(40, 6))
	}

	// esc cancels the planning context and the spinner goes away.
	h.root.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.root.dialog != nil {
		t.Fatalf("esc left %T on screen", h.root.dialog)
	}
	msg := plan()
	proposal, ok := msg.(ProposalMsg)
	if !ok {
		t.Fatalf("planning produced %T", msg)
	}
	if proposal.Err == nil {
		t.Fatal("a cancelled planning round-trip reported no error")
	}
	h.root.Update(proposal)
	if _, ok := h.root.dialog.(*planDialog); ok {
		t.Fatal("a failed plan opened the plan dialog")
	}
}

// refreshSpy is a view that records the refresh request the root sends when a
// rollout finishes.
type refreshSpy struct{ handled int }

var _ View = (*refreshSpy)(nil)

func (v *refreshSpy) Init() tea.Cmd { return nil }

func (v *refreshSpy) Update(msg tea.Msg) (View, tea.Cmd) {
	if _, ok := msg.(RefreshRequestMsg); ok {
		v.handled++
	}
	return v, nil
}

func (v *refreshSpy) Render(w, h int) string { return "" }
func (v *refreshSpy) Crumb() string          { return "spy" }
func (v *refreshSpy) Hints() []KeyHint       { return nil }
func (v *refreshSpy) Subject() Subject       { return Subject{} }
func (v *refreshSpy) Capturing() bool        { return false }
func (v *refreshSpy) CopyLines() []string    { return nil }

// TestRunDialogRefreshFocused checks requirement 6: a finished rollout asks the
// focused view to reload.
func TestRunDialogRefreshFocused(t *testing.T) {
	h := newActionHarness(t, nil, actionRollout(100))
	spy := &refreshSpy{}
	h.root.pushFocused(spy)

	d := newRunDialog(h.env, bus.Proposal{
		Plan:    action.Plan{Title: "restart", Spec: action.Spec{ID: "fake.restart"}},
		Rollout: bus.Rollout{BatchSize: 1, Batches: 1},
	}, 3)
	h.root.runs = append(h.root.runs, d)
	h.root.dialog = d

	h.root.Update(runEventsForMsg{ID: 3, Msg: RunEventsMsg{
		Events: []bus.Event{{Phase: bus.PhaseDone, Summary: &bus.Summary{OK: 1}}},
		Closed: true,
	}})
	if spy.handled != 1 {
		t.Fatalf("the focused view got %d refresh requests, want 1", spy.handled)
	}
	if !d.done {
		t.Fatal("the rollout did not finish")
	}
}
