// Salt actions through the action bus (SPEC §8.6, §11): the state dry run that
// feeds the `:salt state` view and the plan dialog, and the planner behind
// every write this backend offers.
//
// A backend only plans. Plan may run the read-only `state.apply test=True` dry
// run for the preview; everything else is a Step whose Run starts one Salt job
// per minion and waits for that minion's return.

package salt

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
)

// Spec IDs offered by this backend. They all share the "salt." prefix, which is
// also what Owns reports.
const (
	// SpecStateApply applies a state (highstate when the sls parameter is
	// empty). Destructive and the only spec with a mandatory preview.
	SpecStateApply = "salt.state.apply"
	// SpecServiceRestart restarts one service on the targets.
	SpecServiceRestart = "salt.service.restart"
	// SpecCmdRun runs a command on the targets.
	SpecCmdRun = "salt.cmd.run"
	// SpecModuleRun runs any execution module function on the targets, as the
	// user typed it: `:salt run <fun> [args…] [k=v…]`.
	SpecModuleRun = "salt.module.run"
	// SpecRefreshPillar refreshes the minions' pillar cache.
	SpecRefreshPillar = "salt.saltutil.refresh_pillar"
	// SpecSyncAll syncs custom modules, states and grains to the minions.
	SpecSyncAll = "salt.saltutil.sync_all"
	// SpecJobKill kills a running job on the minions it still runs on.
	SpecJobKill = "salt.job.kill"
	// SpecKeyAccept accepts a pending minion key (SPEC §8.6 "Key management").
	SpecKeyAccept = "salt.key.accept"
	// SpecKeyReject rejects a pending minion key. Destructive: the minion
	// cannot authenticate until an operator accepts its key.
	SpecKeyReject = "salt.key.reject"
	// SpecKeyDelete deletes a key the master holds. Destructive: the minion is
	// unlinked from the master until its key is accepted again.
	SpecKeyDelete = "salt.key.delete"
)

// Parameter names of the Salt specs.
const (
	// ParamSLS is the state to apply; empty means the highstate.
	ParamSLS = "sls"
	// ParamName is the service a restart acts on.
	ParamName = "name"
	// ParamCmd is the command cmd.run executes.
	ParamCmd = "cmd"
	// ParamFun is the execution module function `salt.module.run` calls.
	ParamFun = "fun"
	// ParamArgs are the positional arguments of that function, space separated.
	ParamArgs = "args"
	// ParamKwargs are its keyword arguments, "k=v k=v".
	ParamKwargs = "kwargs"
	// ParamSSH asks for salt-ssh instead of the master. It is only honoured
	// when salt.allow_salt_ssh is set (task 097).
	ParamSSH = "ssh"
	// sshParamValue is the ParamSSH value that turns the transport on.
	sshParamValue = "true"
)

const (
	// actionPrefix is the spec ID prefix the Salt planner owns.
	actionPrefix = "salt."

	// dryRunTimeout bounds one state.apply test=True run (60 s, task 075).
	dryRunTimeout = 60 * time.Second
	// stepTimeout bounds one non-state step (BUILD-PLAN §8: Salt job 60 s).
	stepTimeout = 60 * time.Second
	// defaultStateTimeout bounds one state apply step when the caller sets no
	// Options.StateTimeout: a highstate on a large fleet is slow.
	defaultStateTimeout = 15 * time.Minute

	// applyOp is the error Op of a state apply step.
	applyOp = "salt.state.apply"
	// stepOp is the error Op of any other Salt step.
	stepOp = "salt.step"
	// dryRunOp is the error Op of DryRun and its preview.
	dryRunOp = "salt.DryRun"

	// rootWarning is the warning every Salt plan carries (SPEC §8.6).
	rootWarning = "runs as root via the Salt master"
	// noReturnText is how a minion that never answered is labelled.
	noReturnText = "no return"
)

// slsRe matches a state name: dotted paths and a comma-separated list of them.
var slsRe = regexp.MustCompile(`^[A-Za-z0-9_.:/-]+(,[A-Za-z0-9_.:/-]+)*$`)

// Key management. A key action runs one salt-key call on the master, always
// with -y (without it salt-key prompts and the call would hang until its
// timeout) and always with an exact, validated key id: a key id is never a
// glob, so `-a '*'` cannot be produced from user input.
const (
	// keyAcceptFlag accepts a key, keyRejectFlag rejects one, keyDeleteFlag
	// deletes one.
	keyAcceptFlag = "-a"
	keyRejectFlag = "-r"
	keyDeleteFlag = "-d"

	// keyVerifyHint is what an accept preview tells the operator to check on
	// the minion itself.
	keyVerifyHint = "verify this fingerprint on the minion: salt-call --local key.finger"

	// confirmTypedLabel and confirmTypedValue are the target label bus.Propose
	// honours by forcing the typed-count confirmation.
	confirmTypedLabel = "_confirm"
	confirmTypedValue = "typed"
)

// keyIDRe matches a minion ID salt-key may be given: letters, digits, dot,
// underscore and hyphen. Globs, spaces, commas and empty ids are refused.
var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// StateCounts is the per-minion tally of one dry run.
type StateCounts struct {
	Minions     int // minions the run was started for
	Clean       int // nothing to change
	WouldChange int // at least one state would change
	Failed      int // at least one failed state, or an error return
	NoReturn    int // never answered
}

// Summary renders the banner of the state view, e.g.
// "24 minions · 22 clean · 2 would change · 0 failed · 0 no return".
func (c StateCounts) Summary() string {
	return fmt.Sprintf("%d %s · %d clean · %d would change · %d failed · %d no return",
		c.Minions, plural(c.Minions, "minion", "minions"),
		c.Clean, c.WouldChange, c.Failed, c.NoReturn)
}

// StateGroup is one set of minions whose dry-run normalized text is identical.
type StateGroup struct {
	Key        string // the normalized text; also the grouping key
	Minions    []string
	Run        StateRun // representative run (the first minion's)
	firstState string
}

// Normalized is the text the group was built from.
func (g StateGroup) Normalized() string { return g.Key }

// Clean reports whether every state in the group is already in place.
func (g StateGroup) Clean() bool {
	return len(g.Run.Errors) == 0 && g.Run.Status() == resource.StatusOK
}

// NoReturn reports whether the group is minions that never answered.
func (g StateGroup) NoReturn() bool {
	if len(g.Run.Rows) > 0 {
		return false
	}
	for _, e := range g.Run.Errors {
		if e != noReturnText {
			return false
		}
	}
	return len(g.Run.Errors) > 0
}

// SummaryLine is the group's one-line summary: "clean (38 states)",
// "2 would change", "1 failed" or "no return".
func (g StateGroup) SummaryLine() string {
	_, changed, failed, pending := g.Run.Counts()
	switch {
	case g.NoReturn():
		return noReturnText
	case len(g.Run.Errors) > 0:
		return "error"
	case failed > 0:
		return fmt.Sprintf("%d failed", failed)
	case pending > 0:
		return fmt.Sprintf("%d would change", pending)
	case changed > 0:
		return fmt.Sprintf("%d changed", changed)
	}
	return fmt.Sprintf("clean (%d states)", len(g.Run.Rows))
}

// FirstChange is the id of the first state that failed or would change, or ""
// for a clean group.
func (g StateGroup) FirstChange() string { return g.firstState }

// StateRunFromResult parses one minion result of a state job. A minion that
// never answered, or answered with something that is not a state result,
// becomes a run whose Errors explain it, so it groups like any other outcome.
// Test is set: every run this file sees is a dry run.
func StateRunFromResult(mr MinionResult) StateRun {
	run := StateRun{Test: true}
	if mr.NoReturn {
		run.Errors = []string{noReturnText}
		return run
	}
	parsed, err := ParseStateReturn(mr.Return)
	if err != nil {
		text := snippet(mr.Return)
		if text == "" {
			text = noReturnText
		}
		run.Errors = []string{text}
		return run
	}
	parsed.Test = true
	return parsed
}

// StateGroups groups minion dry-run runs by their normalized text. Failures
// come first, then would-change, then clean; inside one kind the largest group
// is first, then by text. Minion lists are sorted.
func StateGroups(runs map[string]StateRun) []StateGroup {
	type acc struct {
		minions []string
		run     StateRun
	}
	byKey := make(map[string]*acc, len(runs))
	for minion, run := range runs {
		key := run.Normalized()
		g := byKey[key]
		if g == nil {
			g = &acc{run: run}
			byKey[key] = g
		}
		g.minions = append(g.minions, minion)
	}
	out := make([]StateGroup, 0, len(byKey))
	for key, g := range byKey {
		sort.Strings(g.minions)
		grp := StateGroup{Key: key, Minions: g.minions, Run: g.run}
		grp.firstState = firstChange(g.run)
		out = append(out, grp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := stateGroupRank(out[i]), stateGroupRank(out[j])
		if ri != rj {
			return ri < rj
		}
		if len(out[i].Minions) != len(out[j].Minions) {
			return len(out[i].Minions) > len(out[j].Minions)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// stateGroupRank orders the groups of a dry run: failures, then pending
// changes, then the clean ones.
func stateGroupRank(g StateGroup) int {
	switch {
	case g.NoReturn() || len(g.Run.Errors) > 0:
		return 0
	case g.Clean():
		return 2
	default:
		return 1
	}
}

// firstChange is the id of the first state that failed or would change.
func firstChange(run StateRun) string {
	for _, row := range run.Rows {
		if _, show := run.rowWord(row); show {
			return row.ID
		}
	}
	return ""
}

// CountStateGroups tallies one dry run: minions is the number of minions it was
// started for, groups what came back.
func CountStateGroups(groups []StateGroup, minions int) StateCounts {
	c := StateCounts{Minions: minions}
	for _, g := range groups {
		n := len(g.Minions)
		switch {
		case g.NoReturn():
			c.NoReturn += n
		case len(g.Run.Errors) > 0:
			c.Failed += n
		case g.Clean():
			c.Clean += n
		default:
			c.WouldChange += n
		}
	}
	return c
}

// DryRun compiles the target and runs state.apply test=True (state.apply <sls>
// test=True when sls is set) as one Salt job. queue=True is deliberately not
// passed: a minion busy with another state run answers with an error string,
// which the caller shows as its own group.
func (b *Backend) DryRun(ctx context.Context, minions []string, sls string, sel resource.Selector) (*Job, Compiled, error) {
	if b.runner == nil {
		return nil, Compiled{}, errs.New(errs.Config, dryRunOp, "salt backend not configured: no runner")
	}
	compiled := b.compile(minions, sel)
	if len(compiled.Targets) == 0 {
		return nil, compiled, errs.New(errs.Config, dryRunOp, "no minions to target")
	}
	call, _, _, err := b.callFor(action.Spec{ID: SpecStateApply}, map[string]string{ParamSLS: sls})
	if err != nil {
		return nil, compiled, err
	}
	call.Kwargs = map[string]string{"test": "True"}
	job, err := b.runner.Start(ctx, Submit{
		Targets: compiled.Targets,
		Fun:     call.Fun,
		Args:    call.Args,
		Kwargs:  call.Kwargs,
		Expect:  compiled.Minions,
		Timeout: dryRunTimeout,
	})
	if err != nil {
		return nil, compiled, err
	}
	return job, compiled, nil
}

// compile is the client-side targeting plan for a selection resolved to
// minions. Grain keys come from the inventory's salt.* labels, so a selector
// only compiles natively when every key is a grain the fleet reports.
func (b *Backend) compile(minions []string, sel resource.Selector) Compiled {
	return Compile(sel, minions, b.grainKeys(), defaultChunk)
}

// grainKeys is the set of grain names the inventory knows: the salt.* label
// keys of every host it holds.
func (b *Backend) grainKeys() map[string]bool {
	if b.inv == nil {
		return nil
	}
	out := make(map[string]bool)
	for _, h := range b.inv.Hosts() {
		for k := range h.Labels {
			if name, ok := strings.CutPrefix(k, saltPrefix); ok && name != "" {
				out[name] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stateCall is the Salt call of a state apply: highstate when sls is empty,
// `state.apply <sls>` otherwise.
func stateCall(sls string, test bool) Call {
	call := Call{Tool: toolSalt, Fun: "state.apply"}
	if s := strings.TrimSpace(sls); s != "" {
		call.Args = []string{s}
	}
	if test {
		call.Kwargs = map[string]string{"test": "True"}
	}
	return call
}

// All spec constructors.

func stateApplySpec() action.Spec {
	return action.Spec{
		ID:        SpecStateApply,
		Title:     "state apply",
		Risk:      action.RiskDestructive,
		HasDryRun: true,
		Params:    []action.Param{{Name: ParamSLS, Title: "sls (empty = highstate)"}},
	}
}

func serviceRestartSpec() action.Spec {
	return action.Spec{
		ID:     SpecServiceRestart,
		Title:  "restart service",
		Risk:   action.RiskNormal,
		Params: []action.Param{{Name: ParamName, Title: "service", Required: true}},
	}
}

func cmdRunSpec() action.Spec {
	return action.Spec{
		ID:     SpecCmdRun,
		Title:  "run command",
		Risk:   action.RiskDestructive,
		Params: []action.Param{{Name: ParamCmd, Title: "cmd", Required: true}},
	}
}

// moduleRunSpec runs any execution module function; the display is the full
// Salt CLI line the step runs.
func moduleRunSpec() action.Spec {
	return action.Spec{
		ID:    SpecModuleRun,
		Title: "run function",
		Risk:  action.RiskDestructive,
		Params: []action.Param{
			{Name: ParamFun, Title: "function", Required: true},
			{Name: ParamArgs, Title: "args"},
			{Name: ParamKwargs, Title: "kwargs (k=v …)"},
		},
	}
}

func refreshPillarSpec() action.Spec {
	return action.Spec{ID: SpecRefreshPillar, Title: "refresh pillar", Risk: action.RiskNormal}
}

func syncAllSpec() action.Spec {
	return action.Spec{ID: SpecSyncAll, Title: "sync all", Risk: action.RiskNormal}
}

func jobKillSpec() action.Spec {
	return action.Spec{ID: SpecJobKill, Title: "kill job", Risk: action.RiskDestructive}
}

// Key specs. Accepting a key is not destructive; rejecting or deleting one
// takes a minion off the master.
func keyAcceptSpec() action.Spec {
	return action.Spec{ID: SpecKeyAccept, Title: "accept key", Risk: action.RiskNormal}
}

func keyRejectSpec() action.Spec {
	return action.Spec{ID: SpecKeyReject, Title: "reject key", Risk: action.RiskDestructive}
}

func keyDeleteSpec() action.Spec {
	return action.Spec{ID: SpecKeyDelete, Title: "delete key", Risk: action.RiskDestructive}
}

// keySpecs is every key spec, in menu order.
func keySpecs() []action.Spec {
	return []action.Spec{keyAcceptSpec(), keyRejectSpec(), keyDeleteSpec()}
}

// keySpecsFor is what can be done to a key in the given state: a pending key is
// accepted or rejected, a key the master already decided on is deleted. A state
// this backend does not know offers nothing.
func keySpecsFor(state string) []action.Spec {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case KeyPending:
		return []action.Spec{keyAcceptSpec(), keyRejectSpec()}
	case KeyAccepted, KeyRejected, KeyDenied:
		return []action.Spec{keyDeleteSpec()}
	}
	return nil
}

// isKeySpec reports whether specID is a key action.
func isKeySpec(specID string) bool {
	switch specID {
	case SpecKeyAccept, SpecKeyReject, SpecKeyDelete:
		return true
	}
	return false
}

// keyFlag is the salt-key flag, the verb and its past tense of a key spec.
func keyFlag(specID string) (flag, verb, past string, ok bool) {
	switch specID {
	case SpecKeyAccept:
		return keyAcceptFlag, "accept", "accepted", true
	case SpecKeyReject:
		return keyRejectFlag, "reject", "rejected", true
	case SpecKeyDelete:
		return keyDeleteFlag, "delete", "deleted", true
	}
	return "", "", "", false
}

// minionSpecs is what can be done to a selection of minions, in menu order.
func minionSpecs() []action.Spec {
	return []action.Spec{stateApplySpec(), serviceRestartSpec(), cmdRunSpec(), refreshPillarSpec(), syncAllSpec()}
}

// actionSpecByID looks a spec up by its exact ID.
func actionSpecByID(id string) (action.Spec, bool) {
	specs := append(append(minionSpecs(), moduleRunSpec(), jobKillSpec()), keySpecs()...)
	for _, s := range specs {
		if s.ID == id {
			return s, true
		}
	}
	return action.Spec{}, false
}

// Owns reports whether this planner owns specID.
func (b *Backend) Owns(specID string) bool { return strings.HasPrefix(specID, actionPrefix) }

// Actions lists what can be done to selection. Pure: it only reads the rows it
// is given. Salt minion rows and state-view groups get the minion specs, a
// running job row gets kill, and host rows that map to a minion get state.apply.
func (b *Backend) Actions(selection []resource.Resource) []action.Spec {
	if len(selection) == 0 {
		return nil
	}
	kind := selection[0].Ref.Kind
	for _, r := range selection[1:] {
		if r.Ref.Kind != kind {
			return nil
		}
	}
	if isHostKind(kind) {
		for _, r := range selection {
			if _, ok := hostMinionRef(r); !ok {
				return nil
			}
		}
		return []action.Spec{stateApplySpec()}
	}
	known, ok := normalizeKind(kind)
	if !ok {
		return nil
	}
	switch known {
	case KindMinion:
		return minionSpecs()
	case KindJobs:
		for _, r := range selection {
			if r.Str("state") != JobRunning {
				return nil
			}
		}
		return []action.Spec{jobKillSpec()}
	case KindKey:
		return keyActions(selection)
	}
	return nil
}

// keyActions offers the specs every selected key row has in common. A selection
// that mixes a pending key with a key the master already holds offers nothing,
// because no single salt-key call covers both.
func keyActions(selection []resource.Resource) []action.Spec {
	var common []action.Spec
	for i, r := range selection {
		offered := keySpecsFor(r.Str("state"))
		if i == 0 {
			common = offered
			continue
		}
		common = commonSpecs(common, offered)
	}
	return common
}

// commonSpecs keeps the specs of a that b offers too, preserving a's order.
func commonSpecs(a, b []action.Spec) []action.Spec {
	offered := make(map[string]bool, len(b))
	for _, s := range b {
		offered[s.ID] = true
	}
	var out []action.Spec
	for _, s := range a {
		if offered[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// isHostKind reports whether kind is the host overview kind. The Salt backend
// does not import the linux backend, so the name is compared literally.
func isHostKind(kind resource.KindName) bool {
	k := strings.ToLower(strings.TrimSpace(string(kind)))
	return k == "host" || k == "hosts"
}

// hostMinionRef is the minion a host row names, from the row's own fields or
// labels. It is the pure half of HostMinion: no key lookup.
func hostMinionRef(r resource.Resource) (string, bool) {
	if v, ok := r.Fields["minion"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), true
	}
	if v := strings.TrimSpace(r.Labels["salt.id"]); v != "" {
		return v, true
	}
	if v, ok := r.Fields["salt.id"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), true
	}
	return "", false
}

// plannedMinion is one step before it is built: which minion and which call.
type plannedMinion struct {
	target action.Target
	minion string
}

// Plan resolves req into one step per minion. Only state.apply reads anything
// back (the dry run for the preview); nothing is written.
func (b *Backend) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	if !b.Owns(req.SpecID) {
		return action.Plan{}, errs.New(errs.Config, "salt.Plan", fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	spec, ok := actionSpecByID(req.SpecID)
	if !ok {
		return action.Plan{}, errs.New(errs.Config, "salt.Plan", fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, "salt.Plan", "no targets")
	}
	if b.runner == nil {
		return action.Plan{}, errs.New(errs.Config, "salt.Plan", "salt backend not configured: no runner")
	}

	steps, title, minions, err := b.planSteps(ctx, spec, req)
	if err != nil {
		return action.Plan{}, err
	}
	plan := action.Plan{
		Spec:     spec,
		Request:  req,
		Title:    title,
		Steps:    steps,
		Via:      b.via(),
		Warnings: []string{rootWarning},
	}
	if req.Params[ParamSSH] == sshParamValue {
		if !b.allowSSH {
			return action.Plan{}, errs.New(errs.Config, "salt.Plan",
				"salt-ssh is disabled; set salt.allow_salt_ssh: true to allow it")
		}
		// SPEC §8.6: a plan that uses salt-ssh says what it does to the
		// target.
		plan.Warnings = append(plan.Warnings, saltSSHPlanWarning()...)
		plan.Via = saltSSHTool
	}
	if spec.ID == SpecStateApply && req.Params[ParamSSH] != sshParamValue {
		preview, err := b.applyPreview(ctx, minions, req.Params[ParamSLS])
		if err != nil {
			return action.Plan{}, err
		}
		plan.Preview = preview
	}
	if isKeySpec(spec.ID) {
		preview, err := b.keyPreview(ctx, spec.ID, minions)
		if err != nil {
			return action.Plan{}, err
		}
		plan.Preview = preview
	}
	return plan, nil
}

// via is the transport name a Salt plan reports.
func (b *Backend) via() string {
	if b.master == "" {
		return backendName
	}
	return backendName + ":" + string(b.master)
}

// planSteps builds the steps of one spec, the plan title and the minions the
// steps act on.
func (b *Backend) planSteps(ctx context.Context, spec action.Spec, req action.Request) ([]action.Step, string, []string, error) {
	if spec.ID == SpecJobKill {
		return b.killJobSteps(ctx, req)
	}
	if isKeySpec(spec.ID) {
		return b.keySteps(ctx, spec, req)
	}
	if req.Params[ParamSSH] == sshParamValue {
		return b.sshSteps(spec, req)
	}
	call, state, timeout, err := b.callFor(spec, req.Params)
	if err != nil {
		return nil, "", nil, err
	}
	planned, err := b.resolveMinions(req.Targets)
	if err != nil {
		return nil, "", nil, err
	}
	minions := make([]string, 0, len(planned))
	steps := make([]action.Step, 0, len(planned))
	for _, p := range planned {
		display, err := displayLine(p.minion, call)
		if err != nil {
			return nil, "", nil, err
		}
		pm := p
		stepCall := call
		steps = append(steps, action.Step{
			Target:  pm.target,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return b.runStep(ctx, pm.minion, stepCall, state, timeout, pm.target)
			},
		})
		minions = append(minions, p.minion)
	}
	return steps, planTitle(spec, req.Params, minions), minions, nil
}

// callFor turns a spec and its parameters into the Salt call every step runs,
// whether the result is a state return, and the step timeout.
func (b *Backend) callFor(spec action.Spec, params map[string]string) (Call, bool, time.Duration, error) {
	switch spec.ID {
	case SpecStateApply:
		sls := strings.TrimSpace(params[ParamSLS])
		if sls != "" && !slsRe.MatchString(sls) {
			return Call{}, false, 0, errs.New(errs.Config, "salt.Plan", fmt.Sprintf("invalid sls %q", sls))
		}
		return stateCall(sls, false), true, b.stateStepTimeout(), nil
	case SpecServiceRestart:
		name := strings.TrimSpace(params[ParamName])
		if !shq.ValidUnitName(name) {
			return Call{}, false, 0, errs.New(errs.Config, "salt.Plan", fmt.Sprintf("invalid service name %q", name))
		}
		return Call{Tool: toolSalt, Fun: "service.restart", Args: []string{name}}, false, stepTimeout, nil
	case SpecCmdRun:
		cmd := strings.TrimSpace(params[ParamCmd])
		if cmd == "" {
			return Call{}, false, 0, errs.New(errs.Config, "salt.Plan", "cmd.run needs a cmd")
		}
		return Call{Tool: toolSalt, Fun: "cmd.run", Args: []string{cmd}}, false, stepTimeout, nil
	case SpecModuleRun:
		return moduleRunCall(params)
	case SpecRefreshPillar:
		return Call{Tool: toolSalt, Fun: "saltutil.refresh_pillar"}, false, stepTimeout, nil
	case SpecSyncAll:
		return Call{Tool: toolSalt, Fun: "saltutil.sync_all"}, false, stepTimeout, nil
	}
	return Call{}, false, 0, errs.New(errs.Config, "salt.Plan", fmt.Sprintf("spec %q is not plannable", spec.ID))
}

// moduleRunCall builds the call of `salt.module.run` from its parameters: the
// function, its positional arguments and its keyword arguments. Every word is
// validated by Call.CommandLine, so nothing reaches a shell line unchecked.
func moduleRunCall(params map[string]string) (Call, bool, time.Duration, error) {
	fun := strings.TrimSpace(params[ParamFun])
	if fun == "" {
		return Call{}, false, 0, errs.New(errs.Config, "salt.Plan", "module.run needs a function")
	}
	call := Call{Tool: toolSalt, Fun: fun}
	call.Args = strings.Fields(params[ParamArgs])
	kwargs, err := parseKwargs(params[ParamKwargs])
	if err != nil {
		return Call{}, false, 0, err
	}
	call.Kwargs = kwargs
	if _, err := call.CommandLine(); err != nil {
		return Call{}, false, 0, err
	}
	return call, false, stepTimeout, nil
}

// parseKwargs turns "k=v k2=v2" into a keyword argument map.
func parseKwargs(text string) (map[string]string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(fields))
	for _, word := range fields {
		k, v, ok := strings.Cut(word, "=")
		if !ok || k == "" {
			return nil, errs.New(errs.Config, "salt.Plan",
				fmt.Sprintf("keyword argument %q is not k=v", word))
		}
		out[k] = v
	}
	return out, nil
}

// stateStepTimeout is the budget of one state apply step.
func (b *Backend) stateStepTimeout() time.Duration {
	if b.stateTimeout > 0 {
		return b.stateTimeout
	}
	return defaultStateTimeout
}

// resolveMinions maps the request's targets onto the minion ids the steps act
// on, de-duplicated, in request order.
func (b *Backend) resolveMinions(targets []action.Target) ([]plannedMinion, error) {
	out := make([]plannedMinion, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		minion, err := b.targetMinion(t)
		if err != nil {
			return nil, err
		}
		if seen[minion] {
			continue
		}
		seen[minion] = true
		out = append(out, plannedMinion{target: t, minion: minion})
	}
	return out, nil
}

// targetMinion resolves one target onto its minion id: a salt-minion row names
// it directly, a salt.id label or an inventory host that Salt manages gives it.
func (b *Backend) targetMinion(t action.Target) (string, error) {
	if kind, ok := normalizeKind(t.Ref.Kind); ok && kind == KindMinion {
		if name := strings.TrimSpace(t.Ref.Name); name != "" {
			if !validMinionID(name) {
				return "", errs.New(errs.Config, "salt.Plan", fmt.Sprintf("invalid minion id %q", name))
			}
			return name, nil
		}
	}
	if v := strings.TrimSpace(t.Labels["salt.id"]); v != "" && validMinionID(v) {
		return v, nil
	}
	host := resource.NormalizeHostID(string(t.Host))
	if host == "" {
		host = resource.NormalizeHostID(string(t.Ref.Host))
	}
	if host == "" {
		return "", errs.New(errs.Config, "salt.Plan", "target is not a salt minion")
	}
	minion, ok := b.HostMinion(host)
	if !ok {
		return "", errs.New(errs.NotAvailable, "salt.Plan", "host is not a salt minion").WithHost(string(host))
	}
	return minion, nil
}

// validMinionID reports whether an id can live in a comma-joined -L target.
func validMinionID(id string) bool {
	return id != "" && !strings.ContainsAny(id, ", \t\r\n")
}

// displayLine is the Salt CLI line for one minion, as the user would type it.
func displayLine(minion string, call Call) (string, error) {
	if _, err := call.CommandLine(); err != nil {
		return "", err
	}
	words := []string{"salt", "-L", minion, call.Fun}
	words = append(words, call.Args...)
	keys := make([]string, 0, len(call.Kwargs))
	for k := range call.Kwargs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		words = append(words, k+"="+call.Kwargs[k])
	}
	return shq.Join(words...), nil
}

// planTitle is the one-line title of the plan dialog.
func planTitle(spec action.Spec, params map[string]string, minions []string) string {
	subject := hostCount(len(minions))
	switch spec.ID {
	case SpecStateApply:
		if sls := strings.TrimSpace(params[ParamSLS]); sls != "" {
			return fmt.Sprintf("state.apply %s on %s", sls, subject)
		}
		return fmt.Sprintf("highstate on %s", subject)
	case SpecServiceRestart:
		return fmt.Sprintf("restart %s on %s", strings.TrimSpace(params[ParamName]), subject)
	case SpecCmdRun:
		return fmt.Sprintf("run %s on %s", strings.TrimSpace(params[ParamCmd]), subject)
	case SpecModuleRun:
		return fmt.Sprintf("run %s on %s", strings.TrimSpace(params[ParamFun]), subject)
	case SpecRefreshPillar:
		return fmt.Sprintf("refresh pillar on %s", subject)
	case SpecSyncAll:
		return fmt.Sprintf("sync all on %s", subject)
	}
	return fmt.Sprintf("%s on %s", spec.Title, subject)
}

// hostCount renders "1 minion" / "12 minions".
func hostCount(n int) string {
	if n == 1 {
		return "1 minion"
	}
	return fmt.Sprintf("%d minions", n)
}

// killJobSteps plans a saltutil.kill_job on the minions a running job still
// runs on. The job is read back: it is a read, not a write.
func (b *Backend) killJobSteps(ctx context.Context, req action.Request) ([]action.Step, string, []string, error) {
	jid := ""
	for _, t := range req.Targets {
		name := strings.TrimSpace(t.Ref.Name)
		if jid != "" && name != jid {
			return nil, "", nil, errs.New(errs.Config, "salt.Plan", "job.kill acts on one job")
		}
		jid = name
	}
	if !validJID(jid) {
		return nil, "", nil, errs.New(errs.Config, "salt.Plan", fmt.Sprintf("invalid job id %q", jid))
	}
	info, err := b.runner.Lookup(ctx, jid)
	if err != nil {
		return nil, "", nil, err
	}
	minions := jobMinions(info)
	if len(minions) == 0 {
		return nil, "", nil, errs.New(errs.NotAvailable, "salt.Plan",
			fmt.Sprintf("job %s has no minions to kill it on", jid))
	}
	call := Call{Tool: toolSalt, Fun: "saltutil.kill_job", Args: []string{jid}}
	target := req.Targets[0]
	steps := make([]action.Step, 0, len(minions))
	for _, m := range minions {
		display, err := displayLine(m, call)
		if err != nil {
			return nil, "", nil, err
		}
		minion := m
		steps = append(steps, action.Step{
			Target:  target,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return b.runStep(ctx, minion, call, false, stepTimeout, target)
			},
		})
	}
	title := fmt.Sprintf("kill job %s on %s", jid, hostCount(len(minions)))
	return steps, title, minions, nil
}

// jobMinions is the sorted union of the job's expected minions and the ones
// that already returned.
func jobMinions(info JobInfo) []string {
	seen := make(map[string]bool, len(info.Minions)+len(info.Result))
	out := make([]string, 0, len(info.Minions)+len(info.Result))
	for _, m := range info.Minions {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	for m := range info.Result {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// keySteps plans one salt-key step per key. A key action runs on the master, so
// each step's target is the master host with the key as its Ref; nothing goes
// through the job engine, and the target count is the number of keys.
func (b *Backend) keySteps(_ context.Context, spec action.Spec, req action.Request) ([]action.Step, string, []string, error) {
	if b.master == "" {
		return nil, "", nil, errs.New(errs.Config, "salt.Plan", "salt backend not configured: no master")
	}
	flag, verb, past, ok := keyFlag(spec.ID)
	if !ok {
		return nil, "", nil, errs.New(errs.Config, "salt.Plan", fmt.Sprintf("spec %q is not a key action", spec.ID))
	}
	ids := make([]string, 0, len(req.Targets))
	steps := make([]action.Step, 0, len(req.Targets))
	seen := make(map[string]bool, len(req.Targets))
	for _, t := range req.Targets {
		id, err := keyID(t)
		if err != nil {
			return nil, "", nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		target := action.Target{
			Host: b.master,
			Ref:  resource.Ref{Backend: backendName, Kind: KindKey, Name: id},
		}
		// Deleting a key unlinks a minion, which a count of one does not show:
		// the label makes bus.Propose demand the typed count.
		if spec.ID == SpecKeyDelete {
			target.Labels = map[string]string{confirmTypedLabel: confirmTypedValue}
		}
		key := id
		display := shq.Join(toolKey, flag, key, "-y")
		stepTarget := target
		steps = append(steps, action.Step{
			Target:  stepTarget,
			Display: display,
			Run: func(ctx context.Context) action.StepResult {
				return b.runKeyStep(ctx, key, flag, past, stepTarget)
			},
		})
		ids = append(ids, key)
	}
	title := fmt.Sprintf("%s %s", verb, keyCount(len(ids)))
	return steps, title, ids, nil
}

// keyID is the minion ID a key target names. Only a salt-key row (or a target
// with no kind at all) may name one, and the id must be an exact ID, never a
// glob.
func keyID(t action.Target) (string, error) {
	if kind := strings.TrimSpace(string(t.Ref.Kind)); kind != "" {
		if known, ok := normalizeKind(t.Ref.Kind); !ok || known != KindKey {
			return "", errs.New(errs.Config, "salt.Plan", "target is not a salt key")
		}
	}
	id := strings.TrimSpace(t.Ref.Name)
	if id == "" {
		return "", errs.New(errs.Config, "salt.Plan", "target is not a salt key")
	}
	if !keyIDRe.MatchString(id) {
		return "", errs.New(errs.Config, "salt.Plan", fmt.Sprintf("invalid key id %q", id))
	}
	return id, nil
}

// runKeyStep runs one salt-key action on the master. -y is always passed:
// without it salt-key prompts and the call would hang until its timeout. The
// key list and everything derived from it is dropped afterwards, so the next
// read sees the new state.
func (b *Backend) runKeyStep(ctx context.Context, id, flag, past string, target action.Target) action.StepResult {
	res := action.StepResult{Target: target}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	out, err := b.runner.Run(cctx, Call{Tool: toolKey, Flags: []string{flag, id, "-y"}})
	res.Duration = time.Since(start)
	b.keysChanged()
	if err != nil {
		res.RC = 1
		res.Err = err
		res.Output = firstLine(err.Error())
		return res
	}
	res.Output = strings.TrimRight(string(out), "\n")
	if strings.TrimSpace(res.Output) == "" {
		res.Output = fmt.Sprintf("key %s %s", id, past)
	}
	return res
}

// keysChanged drops the accepted-key set and the minion health snapshot: a
// salt-key call just changed them. The key list itself is not cached, so the
// next List runs salt-key -L and shows the new state.
func (b *Backend) keysChanged() {
	b.mu.Lock()
	b.accepted = nil
	b.acceptedAt = time.Time{}
	b.acceptedOK = false
	b.health = backend.Health{}
	b.healthAt = time.Time{}
	b.healthOK = false
	b.mu.Unlock()
}

// keyPreview is what the plan dialog shows before a key action: every key with
// the fingerprint the master reports, the accept hint to compare it on the
// minion, and, for a delete, each minion's last seen state plus a warning when
// it is up right now.
func (b *Backend) keyPreview(ctx context.Context, specID string, ids []string) (string, error) {
	var up map[string]bool
	if specID == SpecKeyDelete {
		st, err := b.manageStatus(ctx)
		if err != nil {
			return "", err
		}
		up = make(map[string]bool, len(st.Up))
		for _, id := range st.Up {
			up[id] = true
		}
	}
	var text strings.Builder
	for i, id := range ids {
		if i > 0 {
			text.WriteString("\n")
		}
		fp, err := b.keyFingerprint(ctx, id)
		if err != nil {
			return "", err
		}
		text.WriteString(keyPreviewLine(id, fp))
		if specID == SpecKeyDelete {
			text.WriteString("\n" + keySeenLine(id, up))
		}
	}
	if specID == SpecKeyAccept {
		text.WriteString("\n" + keyVerifyHint)
	}
	if specID == SpecKeyDelete {
		for _, id := range ids {
			if up[id] {
				text.WriteString(fmt.Sprintf("\nwarning: %s is currently up; deleting its key unlinks it "+
					"from the master until its key is accepted again", id))
			}
		}
	}
	return text.String(), nil
}

// keyPreviewLine names one key with the fingerprint salt-key -f reports.
func keyPreviewLine(id, fp string) string {
	if strings.TrimSpace(fp) == "" {
		return id + "  fingerprint unknown"
	}
	return id + "  fingerprint " + fp
}

// keySeenLine is the last seen state of a minion, as salt-run manage.status
// reports it.
func keySeenLine(id string, up map[string]bool) string {
	switch {
	case up == nil:
		return "last seen: unknown"
	case up[id]:
		return "last seen: up"
	default:
		return "last seen: down"
	}
}

// keyFingerprint reads one key's fingerprint with salt-key -f <id>.
func (b *Backend) keyFingerprint(ctx context.Context, id string) (string, error) {
	out, err := b.runner.Run(ctx, Call{Tool: toolKey, Flags: []string{"-f", id}})
	if err != nil {
		return "", err
	}
	fps, err := ParseFingerprints(out)
	if err != nil {
		return "", err
	}
	return fps[id], nil
}

// manageStatus runs salt-run manage.status: which minions answer right now.
func (b *Backend) manageStatus(ctx context.Context) (ManageStatus, error) {
	out, err := b.runner.Run(ctx, Call{Tool: toolRun, Fun: "manage.status"})
	if err != nil {
		return ManageStatus{}, err
	}
	return ParseManageStatus(out)
}

// keyCount renders "1 key" / "12 keys".
func keyCount(n int) string {
	if n == 1 {
		return "1 key"
	}
	return fmt.Sprintf("%d keys", n)
}

// runStep starts one job for one minion and waits for its result. One job per
// minion per step is deliberate (task 075): abort, thresholds and per-host
// records stay identical to every other action.
func (b *Backend) runStep(ctx context.Context, minion string, call Call, state bool, timeout time.Duration, target action.Target) action.StepResult {
	res := action.StepResult{Target: target}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	job, err := b.runner.Start(cctx, Submit{
		Targets: []Target{{Type: "list", Expr: minion}},
		Fun:     call.Fun,
		Args:    call.Args,
		Kwargs:  call.Kwargs,
		Expect:  []string{minion},
		Timeout: timeout,
	})
	if err != nil {
		res.RC = 1
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}
	defer job.Cancel()

	var result MinionResult
	found := false
	for mr := range job.Results() {
		if mr.Minion != minion {
			continue
		}
		result, found = mr, true
	}
	res.Duration = time.Since(start)
	if !found || result.NoReturn {
		res.RC = 1
		res.Output = noReturnText
		res.Err = errs.New(errs.Timeout, stepOp, "no return from minion").WithHost(string(target.Host))
		return res
	}
	if state {
		return stateStepResult(result, res)
	}
	res.RC = result.Retcode
	res.Output = compactReturn(call.Fun, result.Return)
	if res.RC != 0 {
		res.Err = errs.New(errs.Remote, stepOp, firstLine(res.Output)).WithHost(string(target.Host))
	}
	return res
}

// stateStepResult folds one state apply return into a step result: the summary
// and the normalized text, RC 1 for a failed state or an error return.
func stateStepResult(mr MinionResult, res action.StepResult) action.StepResult {
	run := StateRunFromResult(mr)
	res.Output = strings.TrimRight(run.Summary()+"\n"+run.Normalized(), "\n")
	_, _, failed, _ := run.Counts()
	switch {
	case len(run.Errors) > 0 || failed > 0:
		res.RC = 1
		res.Err = errs.New(errs.Remote, applyOp, firstLine(res.Output)).WithHost(string(res.Target.Host))
	}
	return res
}

// compactReturn renders a non-state function's return: the string of cmd.run,
// the fields of cmd.run_all, compact JSON with sorted keys otherwise.
func compactReturn(fun string, raw json.RawMessage) string {
	switch fun {
	case "cmd.run":
		if s, ok := returnString(raw); ok {
			return strings.TrimRight(s, "\n")
		}
	case "cmd.run_all":
		var w struct {
			Stdout  string `json:"stdout"`
			Stderr  string `json:"stderr"`
			Retcode int    `json:"retcode"`
		}
		if err := json.Unmarshal(raw, &w); err == nil {
			out := strings.TrimRight(w.Stdout, "\n")
			if errText := strings.TrimRight(w.Stderr, "\n"); errText != "" {
				if out != "" {
					out += "\n"
				}
				out += errText
			}
			if w.Retcode != 0 {
				if out != "" {
					out += "\n"
				}
				out += fmt.Sprintf("rc=%d", w.Retcode)
			}
			return out
		}
	}
	if s, ok := returnString(raw); ok {
		return strings.TrimRight(s, "\n")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return snippet(raw)
	}
	out, err := json.Marshal(v) // maps marshal with sorted keys
	if err != nil {
		return snippet(raw)
	}
	return string(out)
}

// returnString decodes raw as a JSON string.
func returnString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// applyPreview runs the dry run for exactly these minions and renders what the
// plan dialog shows: the grouped summary, the clean minions called out, then
// every non-clean group's normalized text with its minion list. When the dry
// run fails for every minion it returns that error instead of a plan.
func (b *Backend) applyPreview(ctx context.Context, minions []string, sls string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, dryRunTimeout)
	defer cancel()

	job, compiled, err := b.DryRun(pctx, minions, sls, resource.Selector{})
	if err != nil {
		return "", err
	}
	defer job.Cancel()

	runs := make(map[string]StateRun, len(compiled.Minions))
	for mr := range job.Results() {
		runs[mr.Minion] = StateRunFromResult(mr)
	}
	if len(runs) == 0 {
		return "", dryRunFailure(job)
	}
	groups := StateGroups(runs)
	counts := CountStateGroups(groups, len(compiled.Minions))
	if counts.Clean == 0 && counts.WouldChange == 0 && len(runs) > 0 && allErrorRuns(runs) {
		return "", errs.New(errs.Remote, applyOp,
			"the dry run failed on every minion: "+firstLine(firstRunError(runs)))
	}

	var text strings.Builder
	text.WriteString(fmt.Sprintf("%d clean · %d would change", counts.Clean, counts.WouldChange))
	if counts.Failed > 0 {
		text.WriteString(fmt.Sprintf(" · %d failed", counts.Failed))
	}
	if counts.NoReturn > 0 {
		text.WriteString(fmt.Sprintf(" · %d no return", counts.NoReturn))
	}
	if counts.Clean > 0 {
		text.WriteString(fmt.Sprintf("\n%s included anyway (state.apply is idempotent)", hostCount(counts.Clean)))
	}
	for _, g := range groups {
		if g.Clean() {
			continue
		}
		text.WriteString(fmt.Sprintf("\n\n%s: %s\n", hostCount(len(g.Minions)), strings.Join(g.Minions, ", ")))
		text.WriteString(g.Normalized())
	}
	return text.String(), nil
}

// dryRunFailure is the error a preview reports when the job produced nothing.
func dryRunFailure(job *Job) error {
	if job != nil {
		if err := job.Err(); err != nil {
			return err
		}
		if len(job.SubmitErrors) > 0 && job.SubmitErrors[0] != nil {
			return job.SubmitErrors[0]
		}
	}
	return errs.New(errs.Remote, applyOp, "the dry run returned nothing")
}

// allErrorRuns reports whether every run carries an error and none has rows.
func allErrorRuns(runs map[string]StateRun) bool {
	for _, run := range runs {
		if len(run.Rows) > 0 || len(run.Errors) == 0 {
			return false
		}
	}
	return len(runs) > 0
}

// firstRunError is the first error text of a set of failed runs, by minion.
func firstRunError(runs map[string]StateRun) string {
	minions := make([]string, 0, len(runs))
	for m := range runs {
		minions = append(minions, m)
	}
	sort.Strings(minions)
	for _, m := range minions {
		if run := runs[m]; len(run.Errors) > 0 {
			return strings.Join(run.Errors, "; ")
		}
	}
	return ""
}
