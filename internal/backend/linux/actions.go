package linux

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// Spec IDs offered by this backend. They all share the "linux." prefix, which
// is also what Owns reports.
const (
	specPrefix        = "linux."
	specSystemctl     = specPrefix + "systemctl."
	specDaemonReload  = specSystemctl + "daemon-reload"
	specKill          = specPrefix + "kill"
	specVacuum        = specPrefix + "journal.vacuum"
	paramSignal       = "signal"
	paramSize         = "size"
	defaultSignal     = "TERM"
	defaultVacuumSize = "1G"
)

// Timing. Step is one write and gets the §8 exec budget; the settle delay lets
// systemd finish starting or stopping before is-active is asked. The preview is
// read-only and must not hold the plan dialog hostage.
const (
	stepTimeout     = 60 * time.Second
	settleDelay     = time.Second
	previewBudget   = 15 * time.Second
	previewTimeout  = 10 * time.Second
	previewMaxRows  = 20
	previewMark     = "== "
	previewSep      = " · "
	previewNotKnown = "unknown"
)

// vacuumSizeRe accepts the sizes journalctl accepts: a number with an optional
// K/M/G/T/P suffix.
var vacuumSizeRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[KMGTP]?$`)

// systemctlVerb is one entry of the systemctl verb table.
type systemctlVerb struct {
	verb string
	risk action.Risk
}

// systemctlVerbs are the unit verbs, in menu order.
func systemctlVerbs() []systemctlVerb {
	return []systemctlVerb{
		{"start", action.RiskNormal},
		{"stop", action.RiskDestructive},
		{"restart", action.RiskNormal},
		{"reload", action.RiskNormal},
		{"enable", action.RiskNormal},
		{"disable", action.RiskDestructive},
		{"mask", action.RiskDestructive},
		{"unmask", action.RiskNormal},
	}
}

// killSpec is the process spec: one signal parameter with a fixed choice list.
func killSpec() action.Spec {
	return action.Spec{
		ID:    specKill,
		Title: "kill",
		Risk:  action.RiskDestructive,
		Params: []action.Param{{
			Name:    paramSignal,
			Title:   "signal",
			Default: defaultSignal,
			Choices: []string{"TERM", "KILL", "HUP", "INT", "USR1", "USR2"},
		}},
	}
}

// daemonReloadSpec applies to a host selection.
func daemonReloadSpec() action.Spec {
	return action.Spec{ID: specDaemonReload, Title: "daemon-reload", Risk: action.RiskNormal}
}

// vacuumSpec vacuums the journal on a host selection.
func vacuumSpec() action.Spec {
	return action.Spec{
		ID:    specVacuum,
		Title: "vacuum journal",
		Risk:  action.RiskDestructive,
		Params: []action.Param{{
			Name:    paramSize,
			Title:   "size",
			Default: defaultVacuumSize,
		}},
	}
}

// systemctlSpecs returns the eight unit verbs. Every one of them has a dry run
// (the is-active/NRestarts preview).
func systemctlSpecs() []action.Spec {
	verbs := systemctlVerbs()
	out := make([]action.Spec, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, action.Spec{
			ID:        specSystemctl + v.verb,
			Title:     v.verb,
			Risk:      v.risk,
			HasDryRun: true,
		})
	}
	return out
}

// allSpecs is every spec this backend can offer, in menu order.
func allSpecs() []action.Spec {
	out := systemctlSpecs()
	out = append(out, daemonReloadSpec(), killSpec(), vacuumSpec())
	return out
}

// specByID looks a spec up by its exact ID.
func specByID(id string) (action.Spec, bool) {
	for _, s := range allSpecs() {
		if s.ID == id {
			return s, true
		}
	}
	return action.Spec{}, false
}

// unitVerbOf reports the systemctl verb of a unit spec and whether id is one.
func unitVerbOf(id string) (string, bool) {
	if !strings.HasPrefix(id, specSystemctl) {
		return "", false
	}
	verb := strings.TrimPrefix(id, specSystemctl)
	for _, v := range systemctlVerbs() {
		if v.verb == verb {
			return verb, true
		}
	}
	return "", false
}

// Owns reports whether this planner owns specID. Every Linux action shares the
// "linux." prefix, including the generic :run spec handled in task 063.
func (b *Backend) Owns(specID string) bool { return strings.HasPrefix(specID, specPrefix) }

// Actions lists what can be done to selection. Pure: it only reads the rows it
// is given.
func (b *Backend) Actions(selection []resource.Resource) []action.Spec {
	if len(selection) == 0 {
		return nil
	}
	kind, ok := normalizeKind(selection[0].Ref.Kind)
	if !ok {
		return nil
	}
	for _, r := range selection[1:] {
		if k, ok := normalizeKind(r.Ref.Kind); !ok || k != kind {
			return nil
		}
	}

	switch kind {
	case KindUnit:
		active, inactive := true, true
		for _, r := range selection {
			if unitActive(r) {
				inactive = false
			} else {
				active = false
			}
		}
		out := make([]action.Spec, 0, len(systemctlVerbs()))
		for _, s := range systemctlSpecs() {
			verb, _ := unitVerbOf(s.ID)
			// Only when the whole selection agrees: an active unit cannot be
			// started, and an inactive one cannot be stopped or restarted.
			if active && verb == "start" {
				continue
			}
			if inactive && (verb == "stop" || verb == "restart" || verb == "reload") {
				continue
			}
			out = append(out, s)
		}
		return out
	case KindProcess:
		return []action.Spec{killSpec()}
	case KindHost:
		return []action.Spec{daemonReloadSpec(), vacuumSpec()}
	default:
		return nil
	}
}

// unitActive reports whether a unit row is in the active state.
func unitActive(r resource.Resource) bool {
	if v, ok := r.Fields["active"].(string); ok {
		return v == "active"
	}
	if u, ok := r.Raw.(Unit); ok {
		return u.Active == "active"
	}
	return false
}

// stepCommand is the resolved command of one target.
type stepCommand struct {
	target  action.Target
	host    resource.HostID
	unit    string // unit verb: the unit, also what is-active asks about
	script  string
	display string
	verify  bool   // wait for the settle delay and require "active" afterwards
	verb    string // for the failure message
}

// Plan resolves req into one step per target. It may run read-only remote
// calls for the preview; it never writes.
func (b *Backend) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	spec, ok := specByID(req.SpecID)
	if !ok {
		return action.Plan{}, errs.New(errs.Config, "linux.Plan", fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, "linux.Plan", "no targets")
	}
	if b.exec == nil {
		return action.Plan{}, errs.New(errs.Config, "linux.Plan", "no transport executor configured")
	}

	cmds, title, err := b.commands(spec, req)
	if err != nil {
		return action.Plan{}, err
	}

	plan := action.Plan{
		Spec:    spec,
		Request: req,
		Title:   title,
		Steps:   make([]action.Step, 0, len(cmds)),
		Via:     b.exec.Name(),
	}
	for _, c := range cmds {
		cmd := c
		plan.Steps = append(plan.Steps, action.Step{
			Target:  cmd.target,
			Display: cmd.display,
			Run:     func(ctx context.Context) action.StepResult { return b.runStep(ctx, cmd) },
		})
	}

	if _, isUnit := unitVerbOf(spec.ID); isUnit {
		preview, warnings := b.preview(ctx, req.Targets)
		plan.Preview = preview
		plan.Warnings = append(plan.Warnings, warnings...)
	}
	return plan, nil
}

// commands validates the request and builds one stepCommand per target, plus
// the plan title. Every validation happens before any step is built, so a bad
// request fails whole.
func (b *Backend) commands(spec action.Spec, req action.Request) ([]stepCommand, string, error) {
	if verb, ok := unitVerbOf(spec.ID); ok {
		return b.unitCommands(verb, req.Targets)
	}
	switch spec.ID {
	case specDaemonReload:
		return b.daemonReloadCommands(req.Targets)
	case specKill:
		return b.killCommands(req.Params, req.Targets)
	case specVacuum:
		return b.vacuumCommands(req.Params, req.Targets)
	default:
		return nil, "", errs.New(errs.Config, "linux.Plan", fmt.Sprintf("spec %q is not plannable", spec.ID))
	}
}

// unitCommands builds the systemctl verb steps for unit targets.
func (b *Backend) unitCommands(verb string, targets []action.Target) ([]stepCommand, string, error) {
	units := make(map[string]bool, len(targets))
	out := make([]stepCommand, 0, len(targets))
	for _, t := range targets {
		host, err := targetHost(t)
		if err != nil {
			return nil, "", err
		}
		unit := t.Ref.Name
		if !shq.ValidUnitName(unit) {
			return nil, "", errs.New(errs.Config, "linux.Plan", fmt.Sprintf("invalid unit name %q", unit)).WithHost(string(host))
		}
		script := shq.Join("systemctl", verb, unit)
		units[unit] = true
		out = append(out, stepCommand{
			target:  t,
			host:    host,
			unit:    unit,
			script:  script,
			display: sudoDisplay(script),
			verify:  verb == "start" || verb == "restart" || verb == "reload",
			verb:    verb,
		})
	}
	var subject string
	if len(units) == 1 {
		for unit := range units {
			subject = unit
		}
	} else {
		subject = fmt.Sprintf("%d units", len(units))
	}
	return out, fmt.Sprintf("%s %s on %s", verb, subject, hostCount(len(out))), nil
}

// daemonReloadCommands builds one systemctl daemon-reload per host target.
func (b *Backend) daemonReloadCommands(targets []action.Target) ([]stepCommand, string, error) {
	script := shq.Join("systemctl", "daemon-reload")
	out := make([]stepCommand, 0, len(targets))
	for _, t := range targets {
		host, err := targetHost(t)
		if err != nil {
			return nil, "", err
		}
		out = append(out, stepCommand{target: t, host: host, script: script, display: sudoDisplay(script)})
	}
	return out, fmt.Sprintf("daemon-reload on %s", hostCount(len(out))), nil
}

// killCommands builds one kill per process target. The PID comes from the
// parsed row's ref name and must be a positive number.
func (b *Backend) killCommands(params map[string]string, targets []action.Target) ([]stepCommand, string, error) {
	signal, err := killSignal(params)
	if err != nil {
		return nil, "", err
	}
	out := make([]stepCommand, 0, len(targets))
	pids := make(map[string]bool, len(targets))
	for _, t := range targets {
		host, err := targetHost(t)
		if err != nil {
			return nil, "", err
		}
		pid, ok := parsePID(t.Ref.Name)
		if !ok {
			return nil, "", errs.New(errs.Config, "linux.Plan", fmt.Sprintf("invalid pid %q", t.Ref.Name)).WithHost(string(host))
		}
		script := shq.Join("kill", "-"+signal, pid)
		pids[pid] = true
		out = append(out, stepCommand{target: t, host: host, script: script, display: sudoDisplay(script)})
	}
	var subject string
	if len(pids) == 1 {
		for pid := range pids {
			subject = pid
		}
	} else {
		subject = fmt.Sprintf("%d processes", len(pids))
	}
	return out, fmt.Sprintf("kill -%s %s on %s", signal, subject, hostCount(len(out))), nil
}

// vacuumCommands builds one journal vacuum per host target.
func (b *Backend) vacuumCommands(params map[string]string, targets []action.Target) ([]stepCommand, string, error) {
	size := strings.TrimSpace(params[paramSize])
	if size == "" {
		size = defaultVacuumSize
	}
	if !vacuumSizeRe.MatchString(size) {
		return nil, "", errs.New(errs.Config, "linux.Plan", fmt.Sprintf("invalid vacuum size %q", size))
	}
	script := shq.Join("journalctl", "--vacuum-size="+size)
	out := make([]stepCommand, 0, len(targets))
	for _, t := range targets {
		host, err := targetHost(t)
		if err != nil {
			return nil, "", err
		}
		out = append(out, stepCommand{target: t, host: host, script: script, display: sudoDisplay(script)})
	}
	return out, fmt.Sprintf("vacuum journal to %s on %s", size, hostCount(len(out))), nil
}

// killSignal reads and validates the signal parameter.
func killSignal(params map[string]string) (string, error) {
	signal := strings.ToUpper(strings.TrimSpace(params[paramSignal]))
	if signal == "" {
		signal = defaultSignal
	}
	if !shq.ValidSignal(signal) {
		return "", errs.New(errs.Config, "linux.Plan", fmt.Sprintf("invalid signal %q", signal))
	}
	return signal, nil
}

// parsePID accepts only a canonical positive decimal PID.
func parsePID(name string) (string, bool) {
	if name == "" || strings.TrimSpace(name) != name {
		return "", false
	}
	pid, err := strconv.Atoi(name)
	if err != nil || pid <= 0 {
		return "", false
	}
	return strconv.Itoa(pid), true
}

// targetHost resolves the host of a target.
func targetHost(t action.Target) (resource.HostID, error) {
	if h := resource.NormalizeHostID(string(t.Host)); h != "" {
		return h, nil
	}
	if h := resource.NormalizeHostID(string(t.Ref.Host)); h != "" {
		return h, nil
	}
	return "", errs.New(errs.Config, "linux.Plan", "target needs a host")
}

// sudoDisplay is the short readable form policy patterns are written against,
// never the fully wrapped `sudo -n -- sh -c '…'` line.
func sudoDisplay(script string) string { return "sudo -n " + script }

// hostCount renders "1 host" / "12 hosts".
func hostCount(n int) string {
	if n == 1 {
		return "1 host"
	}
	return strconv.Itoa(n) + " hosts"
}

// runStep executes one step. It honours ctx, never prompts and never writes
// outside the planned command.
func (b *Backend) runStep(ctx context.Context, c stepCommand) action.StepResult {
	res := action.StepResult{Target: c.target}
	cctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	out, err := b.exec.Exec(cctx, c.host, transport.Command{Script: c.script, Sudo: true, Timeout: stepTimeout})
	res.RC = out.RC
	res.Output = string(out.Stdout) + string(out.Stderr)
	res.Duration = out.Duration
	if err != nil {
		res.Err = err
		return res
	}
	if !c.verify {
		return res
	}
	if err := sleepCtx(cctx, settleDelay); err != nil {
		res.Err = errs.New(errs.KindOf(err), "linux.systemctl."+c.verb, "canceled before the state check").WithHost(string(c.host))
		return res
	}

	vctx, vcancel := context.WithTimeout(ctx, stepTimeout)
	defer vcancel()
	check := shq.Join("systemctl", "is-active", c.unit)
	vres, verr := b.exec.Exec(vctx, c.host, transport.Command{Script: check, Timeout: stepTimeout})
	if verr != nil {
		res.Err = verr
		return res
	}
	state := firstLine(string(vres.Stdout))
	if state == "" {
		state = previewNotKnown
	}
	res.Output = strings.TrimRight(res.Output, "\n") + "\n" + state + "\n"
	if state != "active" {
		msg := fmt.Sprintf("unit is %s after %s", state, c.verb)
		res.RC = 3
		res.Output += msg
		res.Err = errs.New(errs.Remote, "linux.systemctl."+c.verb, msg).WithHost(string(c.host))
	}
	return res
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return v
		}
	}
	return ""
}

// previewState is what the dry run learned about one unit.
type previewState struct {
	host     resource.HostID
	unit     string
	active   string
	restarts int
	hasRst   bool
}

// preview asks every host for the state and restart count of the selected
// units. Only is-active and show are used, so it is safe to run before the
// confirmation. A failure adds a warning and never fails the plan.
func (b *Backend) preview(ctx context.Context, targets []action.Target) (string, []string) {
	var hosts []resource.HostID
	byHost := make(map[resource.HostID][]string, len(targets))
	for _, t := range targets {
		host, err := targetHost(t)
		if err != nil {
			continue
		}
		unit := t.Ref.Name
		if !shq.ValidUnitName(unit) {
			continue
		}
		if _, seen := byHost[host]; !seen {
			hosts = append(hosts, host)
		}
		byHost[host] = append(byHost[host], unit)
	}
	if len(hosts) == 0 {
		return "", nil
	}

	var (
		states   []previewState
		warnings []string
	)
	cctx, cancel := context.WithTimeout(ctx, previewBudget)
	defer cancel()
	for _, host := range hosts {
		units := byHost[host]
		res, err := b.exec.Exec(cctx, host, transport.Command{Script: previewScript(units), Timeout: previewTimeout})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("preview failed on %s: %s", shortHost(host), errs.KindOf(err)))
			continue
		}
		got, ok := parsePreview(host, units, string(res.Stdout))
		if !ok {
			warnings = append(warnings, fmt.Sprintf("preview failed on %s: unreadable systemctl output", shortHost(host)))
			continue
		}
		states = append(states, got...)
	}
	if len(states) == 0 {
		return "", warnings
	}

	if len(targets) > previewMaxRows {
		return groupedPreview(states), warnings
	}
	return detailedPreview(states), warnings
}

// previewScript is one host's read-only batch: a marker, the state and the
// restart counter per unit, in that order.
func previewScript(units []string) string {
	parts := make([]string, 0, len(units)*3)
	for _, unit := range units {
		parts = append(parts,
			shq.Join("printf", "%s\\n", previewMark+unit),
			shq.Join("systemctl", "is-active", unit),
			shq.Join("systemctl", "show", "-p", "NRestarts", "--value", unit),
		)
	}
	return strings.Join(parts, "; ")
}

// parsePreview reads the marker/state/restarts triples the preview script
// prints. It skips anything it does not understand and reports false only when
// a unit produced no state at all.
func parsePreview(host resource.HostID, units []string, out string) ([]previewState, bool) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	states := make([]previewState, 0, len(units))
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, previewMark) {
			continue
		}
		unit := strings.TrimPrefix(line, previewMark)
		st := previewState{host: host, unit: unit}
		j := nextLine(lines, i+1)
		if j < 0 {
			return states, false
		}
		st.active = strings.TrimSpace(lines[j])
		if k := nextLine(lines, j+1); k >= 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(lines[k])); err == nil {
				st.restarts, st.hasRst = n, true
				j = k
			}
		}
		states = append(states, st)
		i = j
	}
	return states, len(states) > 0
}

// nextLine returns the index of the next non-blank line at or after from, or -1.
func nextLine(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

// detailedPreview renders one line per unit: "web-01: failed (restarts 5)".
func detailedPreview(states []previewState) string {
	var b strings.Builder
	for i, st := range states {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(shortHost(st.host))
		b.WriteString(": ")
		if len(states) > 1 && unitsPerHost(states) > 1 {
			b.WriteString(st.unit)
			b.WriteByte(' ')
		}
		b.WriteString(previewStateText(st))
	}
	return b.String()
}

// previewStateText renders the state and, when known, the restart counter.
func previewStateText(st previewState) string {
	state := st.active
	if state == "" {
		state = previewNotKnown
	}
	if st.hasRst {
		return fmt.Sprintf("%s (restarts %d)", state, st.restarts)
	}
	return state
}

// unitsPerHost reports the largest number of units any one host contributed.
func unitsPerHost(states []previewState) int {
	count := make(map[resource.HostID]int, len(states))
	max := 0
	for _, st := range states {
		count[st.host]++
		if count[st.host] > max {
			max = count[st.host]
		}
	}
	return max
}

// groupedPreview renders counts instead of lines: "18 active · 2 failed".
func groupedPreview(states []previewState) string {
	counts := make(map[string]int, 4)
	for _, st := range states {
		state := st.active
		if state == "" {
			state = previewNotKnown
		}
		counts[state]++
	}
	order := make([]string, 0, len(counts))
	if counts["active"] > 0 {
		order = append(order, "active")
	}
	rest := make([]string, 0, len(counts))
	for state := range counts {
		if state != "active" {
			rest = append(rest, state)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	parts := make([]string, 0, len(order))
	for _, state := range order {
		parts = append(parts, strconv.Itoa(counts[state])+" "+state)
	}
	return strings.Join(parts, previewSep)
}

// shortHost is the display form of a host: the first label of its name.
func shortHost(host resource.HostID) string {
	name := string(host)
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}
