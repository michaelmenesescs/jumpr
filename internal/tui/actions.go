package tui

import (
	"context"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/resource"
)

// The action flow from the views to the bus: `x` asks for the menu, the menu
// asks a planner for a plan, the plan dialog confirms it and the run dialog
// follows the rollout (SPEC §11, BUILD-PLAN §4.5, §5). Every remote call runs in
// a tea.Cmd; nothing in this file does I/O.

const (
	// actionProposeTimeout bounds one planning round-trip.
	actionProposeTimeout = 30 * time.Second
	// actionHostPreview is how many hosts the plan dialog lists before +N more.
	actionHostPreview = 10
	// actionBatchMax is the most rollout events one RunEventsMsg carries.
	actionBatchMax = 200
)

// ActionsRequestMsg asks the root to open the actions menu for a selection. A
// view emits it on `x`: the marked rows, or the row under the cursor.
type ActionsRequestMsg struct {
	Kind      resource.KindName
	Selection []resource.Resource
}

// actionSpecChosenMsg is the spec the user picked in the menu.
type actionSpecChosenMsg struct {
	Spec      action.Spec
	Selection []resource.Resource
}

// actionParamsMsg is the parameter form's answer.
type actionParamsMsg struct {
	Spec      action.Spec
	Params    map[string]string
	Selection []resource.Resource
}

// planConfirmedMsg is the plan dialog's confirmation, ready for the bus.
type planConfirmedMsg struct {
	Proposal bus.Proposal
	Confirm  bus.Confirmation
}

// runBeginMsg carries the rollout a run dialog must follow.
type runBeginMsg struct {
	ID  int
	Run bus.Run
	Err error
}

// runEventsForMsg tags a batch of events with the run dialog that asked for it,
// since a batch of one rollout must not reach another rollout's dialog.
type runEventsForMsg struct {
	ID  int
	Msg RunEventsMsg
}

// hideRunMsg hides a run dialog; the rollout keeps going.
type hideRunMsg struct{ ID int }

// runOpenMsg reopens a hidden run dialog.
type runOpenMsg struct{ ID int }

// runResultsRequestMsg asks the root for the grouped results of a run.
type runResultsRequestMsg struct{ ID int }

// RunListRequestedMsg asks the root to list the rollouts in flight.
type RunListRequestedMsg struct{}

// escDialog is a Dialog that handles esc itself instead of being dismissed by
// the root (the run dialog aborts on esc; the planning dialog cancels).
type escDialog interface {
	Esc() (Dialog, tea.Cmd)
}

// activeBus is the optional Bus method that lists the rollouts in flight.
type activeBus interface{ Active() []bus.Run }

// busOf returns the action bus of the environment, if there is one.
func (r *Root) busOf() bus.Bus {
	if r.env == nil {
		return nil
	}
	return r.env.Bus
}

// actionSpecs collects the specs every planner offers for sel, de-duplicated by
// Spec.ID and sorted by ID so the menu is stable between frames.
func actionSpecs(planners map[string]action.Planner, sel []resource.Resource) []action.Spec {
	names := make([]string, 0, len(planners))
	for name := range planners {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]bool, len(planners))
	var out []action.Spec
	for _, name := range names {
		pl := planners[name]
		if pl == nil {
			continue
		}
		for _, spec := range pl.Actions(sel) {
			if spec.ID == "" || seen[spec.ID] {
				continue
			}
			seen[spec.ID] = true
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// requestFor builds the bus request for spec over the selection.
func requestFor(spec action.Spec, params map[string]string, sel []resource.Resource) action.Request {
	targets := make([]action.Target, 0, len(sel))
	for _, res := range sel {
		targets = append(targets, actionTargetOf(res))
	}
	return action.Request{
		SpecID:  spec.ID,
		Targets: targets,
		Params:  params,
		Actor:   action.Human,
	}
}

// actionTargetOf turns one selected resource into a bus target, carrying the
// labels policy matches on.
func actionTargetOf(res resource.Resource) action.Target {
	t := action.Target{Ref: res.Ref, Labels: res.Labels}
	switch {
	case res.Ref.Host != "":
		t.Host = res.Ref.Host
	default:
		if v, ok := res.Fields["host"].(string); ok && v != "" {
			t.Host = resource.NormalizeHostID(v)
		} else if res.Ref.Kind == "host" && res.Ref.Name != "" {
			t.Host = resource.NormalizeHostID(res.Ref.Name)
		}
	}
	return t
}

// proposeCmd plans req through the bus, in a command.
func proposeCmd(ctx context.Context, b bus.Bus, req action.Request) tea.Cmd {
	return func() tea.Msg {
		p, err := b.Propose(ctx, req)
		return ProposalMsg{Proposal: p, Err: err}
	}
}

// actionKindName is the word the empty-menu flash uses for a kind.
func actionKindName(kind resource.KindName) string {
	if kind == "" {
		return "this selection"
	}
	return string(kind)
}

// openActions opens the actions menu for a selection, or says there is none.
func (r *Root) openActions(m ActionsRequestMsg) tea.Cmd {
	sel := append([]resource.Resource(nil), m.Selection...)
	if len(sel) == 0 {
		return Flash(FlashInfo, "no actions for %s", actionKindName(m.Kind))
	}
	var planners map[string]action.Planner
	if r.env != nil {
		planners = r.env.Planners
	}
	specs := actionSpecs(planners, sel)
	if len(specs) == 0 {
		return Flash(FlashInfo, "no actions for %s", actionKindName(m.Kind))
	}
	r.dialog = newActionsMenu(themeOf(r.env), m.Kind, specs, func(spec action.Spec) tea.Cmd {
		return func() tea.Msg { return actionSpecChosenMsg{Spec: spec, Selection: sel} }
	})
	return nil
}

// chooseSpec opens the parameter form of a spec, or plans it directly when it
// takes no parameters.
func (r *Root) chooseSpec(m actionSpecChosenMsg) tea.Cmd {
	r.dialog = nil
	if len(m.Spec.Params) == 0 {
		return r.startPlanning(m.Spec, nil, m.Selection)
	}
	sel := m.Selection
	r.dialog = newParamForm(themeOf(r.env), m.Spec, func(spec action.Spec, params map[string]string) tea.Cmd {
		return func() tea.Msg { return actionParamsMsg{Spec: spec, Params: params, Selection: sel} }
	})
	return nil
}

// startPlanning asks the bus for a plan and shows the spinner meanwhile. The
// planning context is kept so esc can cancel it.
func (r *Root) startPlanning(spec action.Spec, params map[string]string, sel []resource.Resource) tea.Cmd {
	r.dialog = nil
	b := r.busOf()
	if b == nil {
		return Flash(FlashError, "no action bus configured")
	}
	if len(sel) == 0 {
		return Flash(FlashError, "no targets for %s", spec.Title)
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionProposeTimeout)
	if r.planCancel != nil {
		r.planCancel()
	}
	r.planCancel = cancel
	r.dialog = newPlanningDialog(themeOf(r.env))
	return proposeCmd(ctx, b, requestFor(spec, params, sel))
}

// openProposal replaces the spinner with the plan dialog (or reports why the
// plan failed).
func (r *Root) openProposal(m ProposalMsg) tea.Cmd {
	if r.planCancel != nil {
		r.planCancel()
		r.planCancel = nil
	}
	r.dialog = nil
	if m.Err != nil {
		return Flash(FlashError, "%v", m.Err)
	}
	r.dialog = newPlanDialog(themeOf(r.env), m.Proposal)
	return nil
}

// confirmPlan starts the rollout: the run dialog goes up at once and the bus
// executes in a command.
func (r *Root) confirmPlan(m planConfirmedMsg) tea.Cmd {
	r.dialog = nil
	b := r.busOf()
	if b == nil {
		return Flash(FlashError, "no action bus configured")
	}
	if m.Proposal.Denied != nil {
		return Flash(FlashError, "denied by policy")
	}
	r.runSeq++
	d := newRunDialog(r.env, m.Proposal, r.runSeq)
	r.runs = append(r.runs, d)
	r.dialog = d
	return d.start(b, m.Confirm)
}

// routeRun hands msg to the run dialog with this ID and keeps its follow-up
// command. A rollout that just finished asks the focused view to refresh once.
func (r *Root) routeRun(id int, msg tea.Msg) tea.Cmd {
	d := r.runByID(id)
	if d == nil {
		return nil
	}
	next, cmd := d.Update(msg)
	if nd, ok := next.(*runDialog); ok && nd != d {
		r.runs = replaceRun(r.runs, d, nd)
		if r.dialog == d {
			r.dialog = nd
		}
		d = nd
	}
	if d.done && !d.refreshed {
		d.refreshed = true
		refresh := r.refreshFocused()
		if cmd == nil {
			return refresh
		}
		return tea.Batch(cmd, refresh)
	}
	return cmd
}

// replaceRun swaps old for next in runs.
func replaceRun(runs []*runDialog, old, next *runDialog) []*runDialog {
	for i, d := range runs {
		if d == old {
			runs[i] = next
			break
		}
	}
	return runs
}

// runByID finds the run dialog with this ID.
func (r *Root) runByID(id int) *runDialog {
	for _, d := range r.runs {
		if d.id == id {
			return d
		}
	}
	return nil
}

// activeRunDialog is the run dialog a bare RunEventsMsg belongs to: the one on
// screen, else the newest rollout that has not finished.
func (r *Root) activeRunDialog() *runDialog {
	if d, ok := r.dialog.(*runDialog); ok {
		return d
	}
	for i := len(r.runs) - 1; i >= 0; i-- {
		if !r.runs[i].done {
			return r.runs[i]
		}
	}
	return nil
}

// hideRun takes a rollout off screen without stopping it.
func (r *Root) hideRun(id int) tea.Cmd {
	d := r.runByID(id)
	if d == nil {
		return nil
	}
	d.hidden = true
	if r.dialog == d {
		r.dialog = nil
	}
	return Flash(FlashInfo, "%s continues · :runs to reopen", d.Title())
}

// showRun reopens a hidden rollout.
func (r *Root) showRun(id int) tea.Cmd {
	d := r.runByID(id)
	if d == nil {
		return Flash(FlashWarn, "that run is gone")
	}
	d.hidden = false
	d.follow = true
	r.dialog = d
	return nil
}

// openRunResults shows the finished rollout's results as a grouped view.
func (r *Root) openRunResults(id int) tea.Cmd {
	d := r.runByID(id)
	if d == nil || !d.done {
		return Flash(FlashInfo, "the rollout has not finished")
	}
	results := d.results
	if ri, ok := d.run.(bus.RunInfo); ok {
		results = ri.Results()
	}
	if r.dialog == d {
		r.dialog = nil
	}
	title := d.Title()
	if r.env != nil && r.env.ResultsView != nil {
		return r.pushFocused(r.env.ResultsView(r.env, title, results))
	}
	return r.pushFocused(newRunResults(d.p, results))
}

// openRunList lists the rollouts in flight and reopens the chosen one.
func (r *Root) openRunList() tea.Cmd {
	active := r.activeRuns()
	if len(active) == 0 {
		return Flash(FlashInfo, "no active runs")
	}
	items := make([]ListItem, 0, len(active))
	ids := make([]int, 0, len(active))
	for _, d := range active {
		items = append(items, ListItem{Label: d.Title(), Detail: d.progress()})
		ids = append(ids, d.id)
	}
	r.dialog = NewListDialog(themeOf(r.env), "active runs", items, func(i int) tea.Cmd {
		if i < 0 || i >= len(ids) {
			return nil
		}
		// The pick runs in the root's goroutine; the dialog's own
		// CloseDialogMsg follows it, and the root consumes pendingRun there
		// (tea.Batch does not order its commands).
		r.pendingRun = ids[i]
		return nil
	})
	return nil
}

// activeRuns are the rollouts that have not finished, newest last. When the bus
// can list its runs, that list decides: a rollout the bus no longer knows about
// is finished.
func (r *Root) activeRuns() []*runDialog {
	var live map[bus.Run]bool
	if ab, ok := r.busOf().(activeBus); ok {
		live = make(map[bus.Run]bool)
		for _, run := range ab.Active() {
			live[run] = true
		}
	}
	out := make([]*runDialog, 0, len(r.runs))
	for _, d := range r.runs {
		if d.done {
			continue
		}
		if live != nil && d.run != nil && !live[d.run] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// rolloutBadge is the status-line marker of a hidden rollout, if any.
func (r *Root) rolloutBadge() string {
	for i := len(r.runs) - 1; i >= 0; i-- {
		d := r.runs[i]
		if d.done || !d.hidden {
			continue
		}
		return d.badge()
	}
	return ""
}

// refreshFocused asks the focused view to reload its data.
func (r *Root) refreshFocused() tea.Cmd {
	if r.mux == nil {
		return nil
	}
	if _, c := r.mux.Focused(); c == nil {
		return nil
	}
	return r.toFocused(RefreshRequestMsg{})
}

// RegisterActionsCommands registers `:runs`. The list itself is built by the
// root, which is the only model that holds the run dialogs.
func RegisterActionsCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	return c.Register(Command{
		Name:  "runs",
		Usage: ":runs",
		Help:  "list the rollouts in flight and reopen one",
		Run: func(env *Env, args string) tea.Cmd {
			if strings.TrimSpace(args) != "" {
				return Flash(FlashError, "usage: :runs")
			}
			return func() tea.Msg { return RunListRequestedMsg{} }
		},
	})
}
