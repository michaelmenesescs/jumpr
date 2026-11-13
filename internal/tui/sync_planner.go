package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/mux"
	"jumper/internal/resource"
)

// The two synchronized-input specs (SPEC §11.1). Both are writes in the sense
// the action bus means: the bus applies policy, asks for the confirmation the
// blast radius needs, runs the steps and records the outcome.
const (
	// SpecSyncLine is a broadcast line: one step per terminal pane.
	SpecSyncLine = "mux.sync.line"
	// SpecSyncRaw turns raw synchronized input on or off: one step.
	SpecSyncRaw = "mux.sync.raw"
)

// syncRawWarning says plainly what the deny rules of the raw mode are worth.
// The UI shows it in the plan dialog (task 064 requirement 4).
const syncRawWarning = "deny rules here are a seat belt, not a boundary: history, aliases and editors bypass them"

// syncPlanner plans the synchronized-input specs. It is registered on the bus
// under "mux" and only plans: the steps it builds are run by the bus, which is
// the only place allowed to call action.Step.Run.
type syncPlanner struct {
	// panes is the mux's pane map, read at plan time so a step sends the line
	// to the pane the target names.
	panes func() map[mux.PaneID]mux.Content
	// mode receives the on/off decision the raw step makes; the root listens on
	// the other end and switches the mode in the UI's goroutine.
	mode chan<- bool
}

var _ action.Planner = (*syncPlanner)(nil)

// Owns claims the two spec IDs, which do not follow a backend's prefix.
func (p *syncPlanner) Owns(specID string) bool {
	return specID == SpecSyncLine || specID == SpecSyncRaw
}

// Actions offers nothing to the actions menu: synchronized input is a pane
// operation, not something a row selection does.
func (p *syncPlanner) Actions([]resource.Resource) []action.Spec { return nil }

// Plan resolves one synchronized-input request into steps.
func (p *syncPlanner) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	_ = ctx
	switch req.SpecID {
	case SpecSyncLine:
		return p.planLine(req)
	case SpecSyncRaw:
		return p.planMode(req)
	}
	return action.Plan{}, errs.New(errs.Config, "mux.sync.plan",
		fmt.Sprintf("unknown spec %q", req.SpecID))
}

// planLine builds one step per pane, in the request's order: the root puts the
// focused pane first, so the canary is the pane the user is looking at.
func (p *syncPlanner) planLine(req action.Request) (action.Plan, error) {
	const op = "mux.sync.plan"
	line := req.Params["line"]
	if strings.TrimSpace(line) == "" {
		return action.Plan{}, errs.New(errs.Config, op, "an empty line cannot be broadcast")
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, op, "no panes to broadcast to")
	}
	steps := make([]action.Step, 0, len(req.Targets))
	for _, target := range req.Targets {
		pane, err := p.paneFor(target)
		if err != nil {
			return action.Plan{}, err
		}
		steps = append(steps, action.Step{
			Target: target,
			// The exact line is what the user confirms and what the record
			// keeps; policy matches its patterns against it.
			Display: line,
			Run: func(context.Context) action.StepResult {
				if err := pane.SendLine(line); err != nil {
					return action.StepResult{Target: target, RC: 1, Err: err}
				}
				return action.StepResult{Target: target}
			},
		})
	}
	return action.Plan{
		Spec:    action.Spec{ID: SpecSyncLine, Title: "broadcast line"},
		Request: req,
		Title:   fmt.Sprintf("send %q to %s", line, paneCount(len(steps))),
		Steps:   steps,
		Via:     "ssh",
	}, nil
}

// planMode builds the single step that turns raw synchronized input on or off.
func (p *syncPlanner) planMode(req action.Request) (action.Plan, error) {
	const op = "mux.sync.plan"
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, op, "no panes for raw sync")
	}
	on := req.Params["on"] != "false"
	n := 1
	if v, err := strconv.Atoi(req.Params["panes"]); err == nil && v > 0 {
		n = v
	}
	target := req.Targets[0]
	display, title := "sync raw off", "raw synchronized input off"
	if on {
		display = fmt.Sprintf("sync raw on: keys to %s", paneCount(n))
		title = fmt.Sprintf("raw synchronized input on: %s", paneCount(n))
	}
	step := action.Step{
		Target:  target,
		Display: display,
		Run: func(context.Context) action.StepResult {
			p.publish(on)
			return action.StepResult{Target: target}
		},
	}
	plan := action.Plan{
		Spec:    action.Spec{ID: SpecSyncRaw, Title: "sync raw"},
		Request: req,
		Title:   title,
		Steps:   []action.Step{step},
		Via:     "tui",
	}
	if on {
		plan.Warnings = []string{syncRawWarning}
	}
	return plan, nil
}

// paneFor resolves the pane a target names, through the ref the root set.
func (p *syncPlanner) paneFor(t action.Target) (terminalPane, error) {
	const op = "mux.sync.plan"
	id, err := strconv.Atoi(t.Ref.Name)
	if err != nil {
		return nil, errs.New(errs.Config, op, "target names no pane").WithHost(string(t.Host))
	}
	if p.panes == nil {
		return nil, errs.New(errs.Config, op, "no panes configured")
	}
	c, ok := p.panes()[mux.PaneID(id)]
	if !ok || c == nil {
		return nil, errs.New(errs.NotAvailable, op, "pane is gone").WithHost(string(t.Host))
	}
	pane, ok := c.(terminalPane)
	if !ok {
		return nil, errs.New(errs.Config, op, "pane is not a terminal").WithHost(string(t.Host))
	}
	return pane, nil
}

// publish hands the mode decision to the root without ever blocking the
// rollout: a second decision replaces nothing, the root re-reads the mode.
func (p *syncPlanner) publish(on bool) {
	if p.mode == nil {
		return
	}
	select {
	case p.mode <- on:
	default:
	}
}

// paneCount says how many panes an action covers, in sentences.
func paneCount(n int) string {
	if n == 1 {
		return "1 pane"
	}
	return fmt.Sprintf("%d panes", n)
}
