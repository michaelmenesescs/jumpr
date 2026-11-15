// Chef actions through the action bus (SPEC §8.7, §11): the chef-client run and
// the why-run dry run of a host selection.
//
// A backend only plans. The plan of `chef.client.run` carries the why-run result
// for the same hosts as its preview, which is mandatory: the operator confirms
// the dry run before the converging run. The why-run is itself an action
// (`chef.whyrun`, normal risk) because a why-run can execute ruby_block
// resources that are not why-run safe.

package chef

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/group"
	"jumpr/internal/resource"
)

// Spec IDs offered by this backend. Both share the "chef." prefix, which is
// also what Owns reports.
const (
	// SpecClientRun converges a host with chef-client. Destructive, with a
	// mandatory why-run preview.
	SpecClientRun = "chef.client.run"
	// SpecWhyRun runs chef-client --why-run on a host: a dry run, but one that
	// still goes through the bus because some resources execute anyway.
	SpecWhyRun = "chef.whyrun"
	// chefPrefix is the spec ID prefix this planner owns.
	chefPrefix = "chef."
	// planOp is the error Op of Plan and of its preview.
	planOp = "chef.Plan"
)

const (
	// whyRunWarning is what a why-run plan carries: it looks like a dry run and
	// mostly is one, but not entirely.
	whyRunWarning = "why-run is a dry run, but ruby_block resources that are not why-run safe still execute"
	// clientRunWarning is what a converging run carries.
	clientRunWarning = "chef-client converges the node: files, packages and services change"
)

// clientRunSpec is the converging run: destructive, and the only spec here with
// a dry run.
func clientRunSpec() action.Spec {
	return action.Spec{
		ID:        SpecClientRun,
		Title:     "chef-client",
		Risk:      action.RiskDestructive,
		HasDryRun: true,
	}
}

// whyRunSpec is the dry run as an action of its own.
func whyRunSpec() action.Spec {
	return action.Spec{
		ID:    SpecWhyRun,
		Title: "chef-client --why-run",
		Risk:  action.RiskNormal,
	}
}

// clientSpecs is what can be done to a host selection, in menu order.
func clientSpecs() []action.Spec { return []action.Spec{clientRunSpec(), whyRunSpec()} }

// clientSpecByID looks a spec up by its exact ID.
func clientSpecByID(id string) (action.Spec, bool) {
	for _, s := range clientSpecs() {
		if s.ID == id {
			return s, true
		}
	}
	return action.Spec{}, false
}

// Planner plans the chef-client specs. It owns no remote state: the client does
// the fan-out and this type only validates, orders and previews.
type Planner struct {
	client *Client
}

var _ action.Planner = (*Planner)(nil)

// NewPlanner returns a Planner over c. A nil client still plans nothing: Plan
// refuses with an errs.Config error.
func NewPlanner(c *Client) *Planner { return &Planner{client: c} }

// Owns reports whether this planner owns specID.
func (p *Planner) Owns(specID string) bool { return strings.HasPrefix(specID, chefPrefix) }

// Actions lists what can be done to selection. Pure: it only reads the rows it
// is given. A host row and a Chef node row both name a host the client can run
// on; anything else offers nothing.
func (p *Planner) Actions(selection []resource.Resource) []action.Spec {
	if len(selection) == 0 {
		return nil
	}
	for _, r := range selection {
		if !clientRunnable(r.Ref.Kind) {
			return nil
		}
	}
	return clientSpecs()
}

// clientRunnable reports whether a kind names hosts chef-client runs on.
func clientRunnable(kind resource.KindName) bool {
	switch strings.ToLower(strings.TrimSpace(string(kind))) {
	case "host", "hosts", string(KindNode):
		return true
	}
	return false
}

// planTarget is one host of a plan, with the request target it came from so
// policy still sees the labels.
type planTarget struct {
	target action.Target
	host   resource.HostID
}

// Plan resolves req into one step per host. For `chef.client.run` it runs the
// why-run fan-out for exactly those hosts and puts the grouped result in the
// preview; nothing is written.
func (p *Planner) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	if !p.Owns(req.SpecID) {
		return action.Plan{}, errs.New(errs.Config, planOp, fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	spec, ok := clientSpecByID(req.SpecID)
	if !ok {
		return action.Plan{}, errs.New(errs.Config, planOp, fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, planOp, "no targets")
	}
	if p == nil || !p.client.Configured() {
		return action.Plan{}, errs.New(errs.Config, planOp, "chef backend not configured: no transport executor")
	}
	planned, err := p.planTargets(req.Targets)
	if err != nil {
		return action.Plan{}, err
	}

	whyRun := spec.ID == SpecWhyRun
	plan := action.Plan{
		Spec:    spec,
		Request: req,
		Steps:   make([]action.Step, 0, len(planned)),
		Via:     p.client.Name(),
	}
	for _, pt := range planned {
		pt := pt
		_, display := p.client.script(whyRun)
		plan.Steps = append(plan.Steps, action.Step{
			Target:  pt.target,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return p.client.Step(ctx, pt.target, whyRun)
			},
		})
	}

	if whyRun {
		plan.Title = fmt.Sprintf("%s --why-run on %s", p.client.Name(), hostCount(len(planned)))
		plan.Warnings = []string{whyRunWarning}
		return plan, nil
	}
	plan.Title = fmt.Sprintf("%s on %s", p.client.Name(), hostCount(len(planned)))
	plan.Warnings = []string{clientRunWarning}
	preview, err := p.preview(ctx, planned)
	if err != nil {
		return action.Plan{}, err
	}
	plan.Preview = preview
	return plan, nil
}

// preview runs the why-run for exactly these hosts and renders what the plan
// dialog shows. When it fails on every host it returns that error instead of a
// plan, because a destructive run without its dry run is the thing the task
// forbids.
//
// The preview runs on the caller's context, and the bus bounds Plan with its own
// plan timeout (30 s), so a why-run that outlives it comes back partial: the
// hosts that answered are grouped and the rest are counted as errors. Failing
// the proposal instead would make the destructive run unusable on a slow fleet.
func (p *Planner) preview(ctx context.Context, planned []planTarget) (string, error) {
	hosts := make([]resource.HostID, 0, len(planned))
	for _, pt := range planned {
		hosts = append(hosts, pt.host)
	}
	rep, err := p.client.WhyRun(ctx, hosts)
	if err != nil {
		return "", err
	}
	if len(rep.Summary.Groups) == 0 {
		msg := "the why-run returned nothing"
		if text := firstHostError(rep.Summary); text != "" {
			msg = "the why-run failed on every host: " + text
		}
		return "", errs.New(errs.Remote, planOp, msg)
	}
	return rep.Text(), nil
}

// firstHostError is the first transport failure of a why-run, by error kind
// then by host.
func firstHostError(s group.Summary) string {
	kinds := make([]string, 0, len(s.Errors))
	for k := range s.Errors {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		for _, he := range s.Errors[errs.Kind(k)] {
			if he.Err != nil {
				return he.Err.Error()
			}
		}
	}
	return ""
}

// planTargets resolves the request's targets onto the hosts the client runs on,
// de-duplicated, in request order.
func (p *Planner) planTargets(targets []action.Target) ([]planTarget, error) {
	out := make([]planTarget, 0, len(targets))
	seen := make(map[resource.HostID]bool, len(targets))
	for _, t := range targets {
		host, err := clientTargetHost(t)
		if err != nil {
			return nil, err
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, planTarget{target: t, host: host})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].host < out[j].host })
	return out, nil
}

// clientTargetHost resolves the host of a target: the target's host, a host
// reference, or the name of a chef-node row.
func clientTargetHost(t action.Target) (resource.HostID, error) {
	if h := resource.NormalizeHostID(string(t.Host)); h != "" {
		return h, nil
	}
	if h := resource.NormalizeHostID(string(t.Ref.Host)); h != "" {
		return h, nil
	}
	if kind, ok := normalizeKind(t.Ref.Kind); ok && kind == KindNode {
		if h := resource.NormalizeHostID(t.Ref.Name); h != "" {
			return h, nil
		}
	}
	return "", errs.New(errs.Config, planOp, "target needs a host")
}
