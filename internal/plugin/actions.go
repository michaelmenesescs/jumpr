package plugin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

// Plugin actions through the action bus (SPEC §12, BUILD-PLAN §4.5): a plugin
// declares writes under `actions`, and each one is a spec of the "plugin"
// planner. Nothing here writes: Plan builds one step per target, and the bus
// confirms, applies policy, rolls out and records the result with the actor
// `plugin:<name>`.

const (
	// SpecPrefix is the spec ID prefix of every plugin action:
	// "plugin.<plugin-name>.<action-name>".
	SpecPrefix = "plugin."
	// ActorPrefix is the actor recorded for every plugin action.
	ActorPrefix = "plugin:"
	// actionOp is the errs.Error op of Plan.
	actionOp = "plugin.Plan"
	// actionItemSeparator separates the plugin name from the action name in a
	// spec ID.
	actionItemSeparator = "."
)

// SpecID builds the spec ID of one plugin action.
func SpecID(pluginName, actionName string) string {
	return SpecPrefix + pluginName + actionItemSeparator + actionName
}

// Actor is the actor a plugin's actions are recorded under.
func Actor(pluginName string) action.Actor { return action.Actor(ActorPrefix + pluginName) }

// Request builds the bus request for one plugin action. The actor is
// plugin:<name>, so the investigation record says who asked, and every action
// therefore always goes through the bus.
func Request(pluginName, actionName string, targets []action.Target, params map[string]string) action.Request {
	return action.Request{
		SpecID:  SpecID(pluginName, actionName),
		Targets: targets,
		Params:  params,
		Actor:   Actor(pluginName),
	}
}

// Planner plans the actions of every loaded plugin. It reads the definitions
// from the Manager on every call, so a reload that replaces a plugin's file is
// visible to the next proposal without re-registering anything.
type Planner struct {
	m *Manager
	// timeout bounds one target's action command; 0 means defaultTimeout.
	timeout time.Duration
}

var _ action.Planner = (*Planner)(nil)

// NewPlanner builds the planner over a manager. A nil manager plans nothing.
func NewPlanner(m *Manager) *Planner { return &Planner{m: m, timeout: defaultTimeout} }

// Owns reports whether specID is a plugin action.
func (p *Planner) Owns(specID string) bool {
	return strings.HasPrefix(specID, SpecPrefix)
}

// Actions lists what the loaded plugins can do to selection, sorted by spec ID.
// Every plugin action applies to any host selection, so a non-empty selection
// is all that is required.
func (p *Planner) Actions(selection []resource.Resource) []action.Spec {
	if len(selection) == 0 || p == nil || p.m == nil {
		return nil
	}
	var out []action.Spec
	for _, def := range p.m.Definitions() {
		if def.Target != TargetHost {
			// An HTTP plugin's writes are the job of its own path; a host
			// executor cannot run them.
			continue
		}
		for _, a := range def.Actions {
			out = append(out, action.Spec{
				ID:    SpecID(def.Name, a.Name),
				Title: a.Name + " (" + def.Name + ")",
				Risk:  action.RiskNormal,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Plan resolves a request into one step per target. It renders the action's
// command with RenderCommand, so a substituted value is one shell argument, and
// records the actor as plugin:<name>.
func (p *Planner) Plan(_ context.Context, req action.Request) (action.Plan, error) {
	if p == nil || p.m == nil {
		return action.Plan{}, errs.New(errs.Config, actionOp, "no plugin manager configured")
	}
	def, act, err := p.lookup(req.SpecID)
	if err != nil {
		return action.Plan{}, err
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, actionOp, "no targets")
	}
	ex := p.exec()
	if ex == nil {
		return action.Plan{}, errs.New(errs.Config, actionOp, "no transport executor configured")
	}

	// The plugin asked, so the record says so whichever caller built the
	// request.
	req.Actor = Actor(def.Name)
	timeout := p.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	plan := action.Plan{
		Spec: action.Spec{
			ID:    SpecID(def.Name, act.Name),
			Title: act.Name + " (" + def.Name + ")",
			Risk:  action.RiskNormal,
		},
		Request: req,
		Title:   fmt.Sprintf("%s %s on %s", def.Name, act.Name, hostCount(len(req.Targets))),
		Steps:   make([]action.Step, 0, len(req.Targets)),
		Via:     ex.Name(),
	}
	if plan.Via == "" {
		plan.Via = "ssh"
	}
	if act.Privileged {
		plan.Warnings = append(plan.Warnings, "runs as root (sudo -n) on every target")
	}

	for _, t := range req.Targets {
		host := resource.NormalizeHostID(string(t.Host))
		if host == "" {
			return action.Plan{}, errs.New(errs.Config, actionOp, "target needs a host")
		}
		script, rerr := RenderCommand(act.Command, req.Params, string(host))
		if rerr != nil {
			return action.Plan{}, errs.Wrap(errs.Config, actionOp, rerr).WithHost(string(host))
		}
		display := script
		if act.Privileged {
			display = "sudo -n " + script
		}
		target := t
		target.Host = host
		cmd := transport.Command{Script: script, Sudo: act.Privileged, Timeout: timeout}
		plan.Steps = append(plan.Steps, action.Step{
			Target:  target,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return runStep(ctx, ex, target, cmd, timeout)
			},
		})
	}
	return plan, nil
}

// lookup finds the definition and the action of a spec ID.
func (p *Planner) lookup(specID string) (Definition, ActionDef, error) {
	rest := strings.TrimPrefix(specID, SpecPrefix)
	name, actionName, ok := strings.Cut(rest, actionItemSeparator)
	if !ok || name == "" || actionName == "" {
		return Definition{}, ActionDef{}, errs.New(errs.Config, actionOp,
			fmt.Sprintf("unknown spec %q: want plugin.<name>.<action>", specID))
	}
	for _, def := range p.m.Definitions() {
		if def.Name != name || def.Target != TargetHost {
			continue
		}
		for _, a := range def.Actions {
			if a.Name == actionName {
				return def, a, nil
			}
		}
		return Definition{}, ActionDef{}, errs.New(errs.Config, actionOp,
			fmt.Sprintf("plugin %q has no action %q", name, actionName))
	}
	return Definition{}, ActionDef{}, errs.New(errs.Config, actionOp,
		fmt.Sprintf("no plugin named %q", name))
}

// exec is the transport the planner runs through.
func (p *Planner) exec() transport.Executor {
	if p.m == nil {
		return nil
	}
	p.m.mu.RLock()
	defer p.m.mu.RUnlock()
	return p.m.opts.Exec
}

// runStep executes one action command. A non-zero exit code is a failed step,
// never an error: the result carries the code and the combined output.
func runStep(ctx context.Context, ex transport.Executor, target action.Target, cmd transport.Command, timeout time.Duration) action.StepResult {
	res := action.StepResult{Target: target}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := ex.Exec(cctx, target.Host, cmd)
	res.RC = out.RC
	res.Output = string(out.Stdout) + string(out.Stderr)
	res.Duration = out.Duration
	if err != nil {
		res.Err = err
	}
	return res
}

// hostCount renders "1 host" / "12 hosts".
func hostCount(n int) string {
	if n == 1 {
		return "1 host"
	}
	return fmt.Sprintf("%d hosts", n)
}
