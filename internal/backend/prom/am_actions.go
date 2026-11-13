// The Alertmanager silence planner (SPEC §8.3): create, expire and extend
// silences through the action bus.
//
// Every silence write is an HTTP call, so a step's Display is the exact call the
// bus shows before confirmation and writes to the record, and Run performs it.
// The plan is built from the selected rows' refs and labels; only extend reads
// the current silences back, because extending needs their matchers.

package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// Spec IDs of the silence actions. They share the "alertmanager." prefix, which
// is what Owns reports and what the bus registers.
const (
	// SpecSilenceCreate silences a selected alert.
	SpecSilenceCreate = "alertmanager.silence.create"
	// SpecSilenceExpire deletes a selected silence.
	SpecSilenceExpire = "alertmanager.silence.expire"
	// SpecSilenceExtend replaces a selected silence with a longer one.
	SpecSilenceExtend = "alertmanager.silence.extend"
)

// Parameter names and defaults of the silence actions.
const (
	// ParamDuration is how long a new or extended silence lasts.
	ParamDuration = "duration"
	// ParamComment is the free text Alertmanager stores with a silence.
	ParamComment = "comment"

	// DefaultSilenceDuration is the duration a silence gets when the user
	// names none: two hours, long enough for the usual incident.
	DefaultSilenceDuration = 2 * time.Hour
)

// specPrefix is the ID prefix every silence spec shares.
const specPrefix = "alertmanager."

// planVia is the transport name a plan reports: these writes are HTTP calls to
// Alertmanager, not commands on a host.
const planVia = "http"

// AlertmanagerPlannerOptions configures an AlertmanagerPlanner.
type AlertmanagerPlannerOptions struct {
	Servers []AlertmanagerServer
	// Creator is the local user name Alertmanager records as the silence's
	// author. app passes it in; it is never guessed here.
	Creator string
	// Now is the clock the silence windows are computed with.
	Now func() time.Time
	// Timeout bounds one HTTP write. Zero means 30 s.
	Timeout time.Duration
}

// AlertmanagerPlanner implements action.Planner for the silence specs.
type AlertmanagerPlanner struct {
	servers map[string]*AlertmanagerClient
	creator string
	now     func() time.Time
	timeout time.Duration
}

var _ action.Planner = (*AlertmanagerPlanner)(nil)

// NewAlertmanagerPlanner builds the planner over the configured servers. A
// server without a client is kept out of the map: a plan naming it fails with
// errs.Config rather than a nil dereference.
func NewAlertmanagerPlanner(o AlertmanagerPlannerOptions) *AlertmanagerPlanner {
	p := &AlertmanagerPlanner{
		servers: make(map[string]*AlertmanagerClient, len(o.Servers)),
		creator: strings.TrimSpace(o.Creator),
		now:     o.Now,
		timeout: o.Timeout,
	}
	for _, s := range o.Servers {
		if s.Client != nil {
			p.servers[s.Name] = s.Client
		}
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.timeout <= 0 {
		p.timeout = amStepTimeout
	}
	return p
}

// Owns reports whether this planner owns specID.
func (p *AlertmanagerPlanner) Owns(specID string) bool { return strings.HasPrefix(specID, specPrefix) }

// Actions lists what can be done to selection: silence the selected alerts, or
// expire and extend the selected silences. Pure, no I/O.
func (p *AlertmanagerPlanner) Actions(selection []resource.Resource) []action.Spec {
	if len(selection) == 0 {
		return nil
	}
	kind, ok := normalizeAMKind(selection[0].Ref.Kind)
	if !ok {
		return nil
	}
	for _, r := range selection[1:] {
		if k, ok := normalizeAMKind(r.Ref.Kind); !ok || k != kind {
			return nil
		}
	}
	switch kind {
	case KindAlert:
		return []action.Spec{createSilenceSpec()}
	case KindSilence:
		return silenceSpecs(selection)
	}
	return nil
}

// createSilenceSpec is the create action: a duration with a default, and the
// comment Alertmanager requires.
func createSilenceSpec() action.Spec {
	return action.Spec{
		ID:    SpecSilenceCreate,
		Title: "silence",
		Risk:  action.RiskNormal,
		Params: []action.Param{
			{Name: ParamDuration, Title: "duration", Default: DefaultSilenceDuration.String()},
			{Name: ParamComment, Title: "comment", Required: true},
		},
	}
}

// expireSpec is the expire action: the delete call takes no parameters.
func expireSpec() action.Spec {
	return action.Spec{ID: SpecSilenceExpire, Title: "expire silence", Risk: action.RiskNormal}
}

// extendSpec is the extend action: a duration with a default and an optional
// comment that replaces the silence's own.
func extendSpec() action.Spec {
	return action.Spec{
		ID:    SpecSilenceExtend,
		Title: "extend silence",
		Risk:  action.RiskNormal,
		Params: []action.Param{
			{Name: ParamDuration, Title: "duration", Default: DefaultSilenceDuration.String()},
			{Name: ParamComment, Title: "comment"},
		},
	}
}

// silenceSpecs offers both silence actions, but no expire for a selection that
// is entirely expired: there is nothing left to delete.
func silenceSpecs(selection []resource.Resource) []action.Spec {
	out := make([]action.Spec, 0, 2)
	anyLive := false
	for _, r := range selection {
		if r.Str("state") != SilenceExpired {
			anyLive = true
			break
		}
	}
	if anyLive {
		out = append(out, expireSpec())
	}
	return append(out, extendSpec())
}

// Plan resolves req into one step per target. Only extend reads anything back
// (the current silences); nothing is written.
func (p *AlertmanagerPlanner) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	if !p.Owns(req.SpecID) {
		return action.Plan{}, errs.New(errs.Config, "prom.am.Plan", fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, "prom.am.Plan", "no targets")
	}
	switch req.SpecID {
	case SpecSilenceCreate:
		return p.planCreate(req)
	case SpecSilenceExpire:
		return p.planExpire(req)
	case SpecSilenceExtend:
		return p.planExtend(ctx, req)
	}
	return action.Plan{}, errs.New(errs.Config, "prom.am.Plan", fmt.Sprintf("unknown spec %q", req.SpecID))
}

// planCreate builds one POST per selected alert.
func (p *AlertmanagerPlanner) planCreate(req action.Request) (action.Plan, error) {
	creator, err := p.checkCreator()
	if err != nil {
		return action.Plan{}, err
	}
	comment := strings.TrimSpace(req.Params[ParamComment])
	if comment == "" {
		return action.Plan{}, errs.New(errs.Config, "prom.am.Plan", "a silence needs a comment")
	}
	dur, err := parseSilenceDuration(req.Params[ParamDuration])
	if err != nil {
		return action.Plan{}, err
	}
	startsAt := p.now().UTC().Truncate(time.Second)
	endsAt := startsAt.Add(dur)

	spec := createSilenceSpec()
	plan := action.Plan{Spec: spec, Request: req, Via: planVia}
	names := make([]string, 0, len(req.Targets))
	for _, t := range req.Targets {
		if !isKind(t.Ref.Kind, KindAlert) {
			return action.Plan{}, errs.New(errs.Config, "prom.am.Plan",
				fmt.Sprintf("spec %q acts on alerts, not %q", spec.ID, t.Ref.Kind))
		}
		client, err := p.clientFor(t.Ref.Namespace)
		if err != nil {
			return action.Plan{}, err
		}
		matchers := matchersFromAlert(targetLabels(t))
		if len(matchers) == 0 {
			return action.Plan{}, errs.New(errs.Config, "prom.am.Plan",
				fmt.Sprintf("alert %q has no labels to match", t.Ref.Name))
		}
		body := SilenceRequest{
			Matchers:  matchers,
			StartsAt:  silenceTime(startsAt),
			EndsAt:    silenceTime(endsAt),
			CreatedBy: creator,
			Comment:   comment,
		}
		name := strings.TrimSpace(t.Labels["alertname"])
		if name == "" {
			name = strings.TrimSpace(t.Ref.Name)
		}
		if name == "" {
			name = matcherText(matchers)
		}
		names = append(names, name)
		display := fmt.Sprintf("POST %s%s %s", client.BaseURL(), pathAMSilences, compactJSON(body))
		stepClient, stepBody := client, body
		plan.Steps = append(plan.Steps, action.Step{
			Target:  t,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return p.createStep(ctx, stepClient, stepBody, t)
			},
		})
	}
	plan.Title = fmt.Sprintf("silence %s for %s", subject(names, "alert"), dur)
	return plan, nil
}

// planExpire builds one DELETE per selected silence.
func (p *AlertmanagerPlanner) planExpire(req action.Request) (action.Plan, error) {
	spec := expireSpec()
	plan := action.Plan{Spec: spec, Request: req, Via: planVia}
	ids := make([]string, 0, len(req.Targets))
	for _, t := range req.Targets {
		if !isKind(t.Ref.Kind, KindSilence) {
			return action.Plan{}, errs.New(errs.Config, "prom.am.Plan",
				fmt.Sprintf("spec %q acts on silences, not %q", spec.ID, t.Ref.Kind))
		}
		client, err := p.clientFor(t.Ref.Namespace)
		if err != nil {
			return action.Plan{}, err
		}
		id := strings.TrimSpace(t.Ref.Name)
		if id == "" {
			return action.Plan{}, errs.New(errs.Config, "prom.am.Plan", "a silence needs an id")
		}
		ids = append(ids, shortSilenceID(id))
		display := fmt.Sprintf("DELETE %s%s%s", client.BaseURL(), pathAMSilence, id)
		plan.Steps = append(plan.Steps, action.Step{
			Target:  t,
			Display: display,
			Run:     func(ctx context.Context) action.StepResult { return p.expireStep(ctx, client, id, t) },
		})
	}
	plan.Title = fmt.Sprintf("expire %s", subject(ids, "silence"))
	return plan, nil
}

// planExtend reads the selected silences back and builds one POST per silence
// with a later end. The old silence is left to expire on its own once the
// longer one takes over.
func (p *AlertmanagerPlanner) planExtend(ctx context.Context, req action.Request) (action.Plan, error) {
	creator, err := p.checkCreator()
	if err != nil {
		return action.Plan{}, err
	}
	dur, err := parseSilenceDuration(req.Params[ParamDuration])
	if err != nil {
		return action.Plan{}, err
	}
	spec := extendSpec()
	plan := action.Plan{Spec: spec, Request: req, Via: planVia}
	ids := make([]string, 0, len(req.Targets))
	for _, t := range req.Targets {
		if !isKind(t.Ref.Kind, KindSilence) {
			return action.Plan{}, errs.New(errs.Config, "prom.am.Plan",
				fmt.Sprintf("spec %q acts on silences, not %q", spec.ID, t.Ref.Kind))
		}
		client, err := p.clientFor(t.Ref.Namespace)
		if err != nil {
			return action.Plan{}, err
		}
		id := strings.TrimSpace(t.Ref.Name)
		if id == "" {
			return action.Plan{}, errs.New(errs.Config, "prom.am.Plan", "a silence needs an id")
		}
		current, err := p.findSilence(ctx, client, id)
		if err != nil {
			return action.Plan{}, err
		}
		now := p.now().UTC().Truncate(time.Second)
		from := current.EndsAt.UTC().Truncate(time.Second)
		if from.Before(now) {
			from = now
		}
		comment := strings.TrimSpace(req.Params[ParamComment])
		if comment == "" {
			comment = current.Comment
		}
		body := SilenceRequest{
			Matchers:  current.Matchers,
			StartsAt:  silenceTime(current.StartsAt.UTC().Truncate(time.Second)),
			EndsAt:    silenceTime(from.Add(dur)),
			CreatedBy: creator,
			Comment:   comment,
		}
		ids = append(ids, shortSilenceID(id))
		display := fmt.Sprintf("POST %s%s %s", client.BaseURL(), pathAMSilences, compactJSON(body))
		stepClient, stepBody := client, body
		plan.Steps = append(plan.Steps, action.Step{
			Target:  t,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return p.createStep(ctx, stepClient, stepBody, t)
			},
		})
	}
	plan.Title = fmt.Sprintf("extend %s by %s", subject(ids, "silence"), dur)
	return plan, nil
}

// findSilence returns one current silence by its full ID.
func (p *AlertmanagerPlanner) findSilence(ctx context.Context, client *AlertmanagerClient, id string) (AMSilence, error) {
	ctx, cancel := context.WithTimeout(ctxOr(ctx), p.timeout)
	defer cancel()
	silences, err := client.Silences(ctx)
	if err != nil {
		return AMSilence{}, err
	}
	for _, s := range silences {
		if s.ID == id || shortSilenceID(s.ID) == id {
			return s, nil
		}
	}
	return AMSilence{}, errs.New(errs.NotAvailable, "prom.am.Plan",
		fmt.Sprintf("no silence %q on %s", id, client.Name()))
}

// createStep posts one silence.
func (p *AlertmanagerPlanner) createStep(ctx context.Context, client *AlertmanagerClient, body SilenceRequest, t action.Target) action.StepResult {
	res := action.StepResult{Target: t}
	start := p.now()
	ctx, cancel := context.WithTimeout(ctxOr(ctx), p.timeout)
	defer cancel()
	id, err := client.CreateSilence(ctx, body)
	res.Duration = p.now().Sub(start)
	if err != nil {
		res.RC = 1
		res.Err = err
		return res
	}
	res.Output = "created silence " + id
	return res
}

// expireStep deletes one silence.
func (p *AlertmanagerPlanner) expireStep(ctx context.Context, client *AlertmanagerClient, id string, t action.Target) action.StepResult {
	res := action.StepResult{Target: t}
	start := p.now()
	ctx, cancel := context.WithTimeout(ctxOr(ctx), p.timeout)
	defer cancel()
	err := client.ExpireSilence(ctx, id)
	res.Duration = p.now().Sub(start)
	if err != nil {
		res.RC = 1
		res.Err = err
		return res
	}
	res.Output = "expired silence " + shortSilenceID(id)
	return res
}

// checkCreator refuses to sign a silence without an author.
func (p *AlertmanagerPlanner) checkCreator() (string, error) {
	if p.creator == "" {
		return "", errs.New(errs.Config, "prom.am.Plan", "a silence needs the local user as its creator")
	}
	return p.creator, nil
}

// clientFor returns the client of the server a target names.
func (p *AlertmanagerPlanner) clientFor(server string) (*AlertmanagerClient, error) {
	name := strings.TrimSpace(server)
	if c, ok := p.servers[name]; ok {
		return c, nil
	}
	return nil, errs.New(errs.Config, "prom.am.Plan",
		fmt.Sprintf("unknown alertmanager server %q", name))
}

// isKind reports whether a ref's kind, alias included, is want.
func isKind(kind resource.KindName, want resource.KindName) bool {
	got, ok := normalizeAMKind(kind)
	return ok && got == want
}

// targetLabels is the label set a target carries.
func targetLabels(t action.Target) map[string]string {
	return t.Labels
}

// subject renders the plan title's object: one name, or a count.
func subject(names []string, noun string) string {
	if len(names) == 0 {
		return "0 " + noun + "s"
	}
	if len(names) == 1 {
		return names[0]
	}
	return strconv.Itoa(len(names)) + " " + noun + "s"
}

// silenceTime renders an instant the way Alertmanager reads it: RFC 3339, UTC,
// whole seconds.
func silenceTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// compactJSON renders a request body as one line, for Display. JSON encoding is
// the only way user text reaches the call: a comment is never concatenated.
func compactJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// parseSilenceDuration parses a duration parameter. Go durations are accepted,
// plus the `d` and `w` suffixes Alertmanager users write.
func parseSilenceDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultSilenceDuration, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, errs.New(errs.Config, "prom.am.Plan",
				fmt.Sprintf("duration %q must be positive", s))
		}
		return d, nil
	}
	if hours, ok := dayWeekHours(s); ok {
		return time.Duration(hours * float64(time.Hour)), nil
	}
	return 0, errs.New(errs.Config, "prom.am.Plan",
		fmt.Sprintf("duration %q is not a duration like 2h, 90m, 1d or 2w", s))
}

// dayWeekHours parses the `Nd` and `Nw` forms a duration parameter may use and
// returns the duration in hours.
func dayWeekHours(s string) (float64, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1:]
	if unit != "d" && unit != "w" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	if unit == "w" {
		n *= 7
	}
	return n * 24, true
}
