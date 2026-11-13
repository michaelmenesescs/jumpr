package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/bus/generic"
	"jumper/internal/errs"
	"jumper/internal/group"
	"jumper/internal/render"
	"jumper/internal/resource"
)

// `:run` and its dialogs (SPEC §5 W5, §8.1). Every `:run` is a write by
// definition: it is planned, confirmed and rolled out through the action bus
// like any other action, and recorded as one.

const (
	// runUsage is the argument grammar shown on a parse error.
	runUsage = "usage: :run [--sudo] [--via ssh|salt] [--no-rollout] <command…>"
	// runProposeTimeout bounds the planning round-trip.
	runProposeTimeout = 30 * time.Second
	// runBatchMax is the most rollout events one runEventsMsg carries.
	runBatchMax = 200
	// noRolloutMax is the largest target set `--no-rollout` may skip the
	// canary for (rollout.typed_confirm_over).
	noRolloutMax = 20
	// runResultLines is how many group rows the results view lists.
	runResultLines = 200
)

// runArgs is a parsed `:run` command line.
type runArgs struct {
	command   string // the shell text, verbatim apart from surrounding quotes
	sudo      bool
	via       string // "" = the planner's default transport
	noRollout bool
}

// RegisterRunCommands registers `:run` and `:rerun` on c. b is the action bus
// the command plans and executes through; nil still registers the commands but
// every attempt flashes instead of running. The last `:run` line is kept in
// this closure, never in package state.
func RegisterRunCommands(c *Commands, b bus.Bus) error {
	if c == nil {
		return nil
	}
	var mu sync.Mutex
	var last runArgs
	haveLast := false

	run := Command{
		Name:  "run",
		Usage: ":run [--sudo] [--via ssh|salt] [--no-rollout] <command…>",
		Help:  "run a command on the targets through the action bus",
		Run: func(env *Env, args string) tea.Cmd {
			a, err := parseRunArgs(args)
			if err != nil {
				return Flash(FlashError, "%s", err)
			}
			mu.Lock()
			last, haveLast = a, true
			mu.Unlock()
			return proposeRun(env, b, a)
		},
	}
	rerun := Command{
		Name:  "rerun",
		Usage: ":rerun",
		Help:  "run the last :run command again",
		Run: func(env *Env, args string) tea.Cmd {
			if strings.TrimSpace(args) != "" {
				return Flash(FlashError, "usage: :rerun")
			}
			mu.Lock()
			a, ok := last, haveLast
			mu.Unlock()
			if !ok {
				return Flash(FlashError, "no previous :run")
			}
			return proposeRun(env, b, a)
		},
	}
	if err := c.Register(run); err != nil {
		return err
	}
	return c.Register(rerun)
}

// parseRunArgs reads the flags in front of the command and keeps everything
// after them verbatim. The only quoting handled here is one surrounding pair
// around the whole command: the shell text itself is never re-split.
func parseRunArgs(args string) (runArgs, error) {
	var a runArgs
	rest := strings.TrimSpace(args)
	for {
		if rest == "" {
			return a, fmt.Errorf("%s", runUsage)
		}
		if rest == "--" {
			rest = strings.TrimSpace(rest[2:])
			break
		}
		if !strings.HasPrefix(rest, "--") {
			break
		}
		flag, tail := splitFirstWord(rest)
		switch flag {
		case "--":
			rest = strings.TrimSpace(tail)
			goto done
		case "--sudo":
			a.sudo = true
		case "--no-rollout":
			a.noRollout = true
		case "--via":
			value, after := splitFirstWord(tail)
			switch value {
			case "ssh", "salt":
				a.via = value
			case "":
				return a, fmt.Errorf("--via needs ssh or salt; %s", runUsage)
			default:
				return a, fmt.Errorf("unknown transport %q; %s", value, runUsage)
			}
			rest = after
			continue
		default:
			return a, fmt.Errorf("unknown flag %q; %s", flag, runUsage)
		}
		rest = tail
	}
done:
	command := unquoteWhole(strings.TrimSpace(rest))
	if command == "" {
		return a, fmt.Errorf("%s", runUsage)
	}
	a.command = command
	return a, nil
}

// unquoteWhole removes one pair of quotes around the whole command, so
// `:run 'ss -s'` and `:run ss -s` mean the same thing. Quotes inside the text
// are the shell's business.
func unquoteWhole(s string) string {
	if len(s) < 2 {
		return s
	}
	if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
		return s[1 : len(s)-1]
	}
	return s
}

// sudoWord renders the sudo parameter.
func sudoWord(sudo bool) string {
	if sudo {
		return "yes"
	}
	return "no"
}

// proposeRun plans the command through the bus. The planning round-trip runs in
// the command, never in Update; the result is the plan dialog (or the denial).
func proposeRun(env *Env, b bus.Bus, a runArgs) tea.Cmd {
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :targets <selector> first")
	}
	if b == nil {
		return Flash(FlashError, "no action bus configured")
	}
	params := map[string]string{
		generic.ParamCommand: a.command,
		generic.ParamSudo:    sudoWord(a.sudo),
	}
	if a.via != "" {
		params[generic.ParamVia] = a.via
	}
	req := action.Request{
		SpecID:  generic.SpecRun,
		Targets: actionTargets(env, hosts),
		Params:  params,
		Actor:   action.Human,
	}
	noRollout := a.noRollout
	viaSalt := a.via == saltBackendName
	// The hint and the once-per-session warning are decided here, in the model
	// goroutine; the health check and the planning run in the command.
	hint := ""
	first := false
	if a.via == "" {
		hint = saltSuggestHint(env, len(hosts))
	} else if viaSalt {
		first = noteSaltUse(env)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), runProposeTimeout)
		defer cancel()
		p, err := b.Propose(ctx, req)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: "run: " + err.Error()}
		}
		if noRollout {
			p = applyNoRollout(p)
		}
		if first {
			p = saltPlanWarning(p, first)
		}
		if hint != "" && saltHealthy(ctx, env) {
			p.Plan.Warnings = append(p.Plan.Warnings, hint)
		}
		return OpenDialogMsg{Dialog: newRunPlanDialog(b, env, p)}
	}
}

// actionTargets turns the session target set into bus targets, carrying the
// inventory labels policy matches on.
func actionTargets(env *Env, hosts []resource.HostID) []action.Target {
	out := make([]action.Target, 0, len(hosts))
	for _, h := range hosts {
		t := action.Target{Host: h}
		if env != nil && env.Inventory != nil {
			if host, ok := env.Inventory.Resolve(string(h)); ok {
				t.Labels = host.Labels
			}
		}
		out = append(out, t)
	}
	return out
}

// applyNoRollout turns the plan into one batch without a canary pause, but only
// for a set small enough and unprotected enough to skip it: more than
// noRolloutMax targets, or a protected set (which the bus confirms by typed
// count), keeps the canary and says why.
func applyNoRollout(p bus.Proposal) bus.Proposal {
	n := len(p.Plan.Steps)
	if n > noRolloutMax || p.Confirm == bus.ConfirmTypedCount {
		p.Plan.Warnings = append(p.Plan.Warnings,
			"--no-rollout ignored: more than 20 targets or a protected set")
		return p
	}
	p.Rollout = bus.Rollout{
		Canary:           0,
		BatchSize:        n,
		Batches:          1,
		FailureThreshold: p.Rollout.FailureThreshold,
	}
	return p
}

// targetCount is how many targets a proposal acts on.
func targetCount(p bus.Proposal) int {
	if n := len(p.Plan.Steps); n > 0 {
		return n
	}
	return len(p.Plan.Request.Targets)
}

// runPlanDialog is the plan dialog: what will run, where, how, and the control
// that confirms it. A denied plan has no control; only esc closes it.
type runPlanDialog struct {
	b     bus.Bus
	env   *Env
	p     bus.Proposal
	typed string
	note  string
}

var _ Dialog = (*runPlanDialog)(nil)

// newRunPlanDialog builds the dialog for a proposed (or denied) `:run`.
func newRunPlanDialog(b bus.Bus, env *Env, p bus.Proposal) *runPlanDialog {
	return &runPlanDialog{b: b, env: env, p: p}
}

// Title is the dialog's border title.
func (d *runPlanDialog) Title() string {
	if d.p.Denied != nil {
		return "run command · denied"
	}
	return "run command"
}

// View renders the plan body, clamped to the space the dialog has.
func (d *runPlanDialog) View(maxW int, _ int) string {
	var lines []string
	add := func(format string, args ...any) {
		line := format
		if len(args) > 0 {
			line = fmt.Sprintf(format, args...)
		}
		lines = append(lines, render.Truncate(line, max(1, maxW)))
	}
	add("%s", d.p.Plan.Title)
	add("")
	add("targets: %s", hostCount(targetCount(d.p)))
	add("%s", d.commandLine())
	add("via: %s", orNone(d.p.Plan.Via))
	if r := rolloutLine(d.p.Rollout, targetCount(d.p)); r != "" {
		add("rollout: %s", r)
	}
	for _, w := range d.p.Plan.Warnings {
		add("warning: %s", w)
	}
	add("")

	if d.p.Denied != nil {
		add("denied by policy")
		if d.p.Denied.Rule != "" {
			add("rule: %s", d.p.Denied.Rule)
		}
		if d.p.Denied.Reason != "" {
			add("reason: %s", d.p.Denied.Reason)
		}
		add("")
		add("esc close")
		return strings.Join(lines, "\n")
	}

	if d.p.Confirm == bus.ConfirmTypedCount {
		add("type %d to confirm: %s", targetCount(d.p), d.typed+"_")
		if d.note != "" {
			add("%s", d.note)
		}
		add("enter confirm · n cancel")
		return strings.Join(lines, "\n")
	}
	add("y confirm · n cancel")
	return strings.Join(lines, "\n")
}

// commandLine says what runs: the exact Display when every target runs the same
// command, a count when they do not.
func (d *runPlanDialog) commandLine() string {
	steps := d.p.Plan.Steps
	if len(steps) == 0 {
		return "command: (none)"
	}
	first := steps[0].Display
	for _, s := range steps[1:] {
		if s.Display != first {
			return fmt.Sprintf("per-host commands (%d)", len(steps))
		}
	}
	return "command: " + first
}

// Update handles the confirmation keys. Nothing here does I/O: confirming
// returns the push that starts the run, whose Init calls the bus.
func (d *runPlanDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	if d.p.Denied != nil {
		return d, nil
	}
	switch k.String() {
	case "y", "Y":
		if d.p.Confirm == bus.ConfirmTypedCount {
			d.note = fmt.Sprintf("type %d and press enter", targetCount(d.p))
			return d, nil
		}
		return d, d.confirm()
	case "n", "N":
		return d, CloseDialog()
	case "enter":
		if d.p.Confirm != bus.ConfirmTypedCount {
			return d, nil
		}
		if strings.TrimSpace(d.typed) != fmt.Sprintf("%d", targetCount(d.p)) {
			d.note = "typed count does not match"
			return d, nil
		}
		return d, d.confirm()
	case "backspace":
		if d.typed != "" {
			d.typed = d.typed[:len(d.typed)-1]
		}
		return d, nil
	}
	if k.Type == tea.KeyRunes {
		for _, r := range k.Runes {
			if r >= '0' && r <= '9' {
				d.typed += string(r)
			}
		}
	}
	return d, nil
}

// confirm closes the dialog and opens the run dialog over the focused pane.
func (d *runPlanDialog) confirm() tea.Cmd {
	conf := bus.Confirmation{ProposalID: d.p.ID}
	if d.p.Confirm == bus.ConfirmTypedCount {
		conf.Typed = strings.TrimSpace(d.typed)
	}
	return tea.Sequence(CloseDialog(), Push(newRunProgress(d.env, d.b, d.p, conf)))
}

// rolloutLine renders the canary/batch plan the user is agreeing to.
func rolloutLine(r bus.Rollout, targets int) string {
	if targets == 0 {
		return ""
	}
	text := ""
	if r.Canary > 0 {
		text = fmt.Sprintf("canary %d → pause → ", r.Canary)
	}
	batches := r.Batches
	if batches < 1 {
		batches = 1
	}
	size := r.BatchSize
	if size < 1 {
		size = targets
	}
	text += fmt.Sprintf("%d %s of %d", batches, pluralWord(batches, "batch"), size)
	if r.FailureThreshold > 0 {
		text += fmt.Sprintf(" · stop if >%g%% fail", 100*r.FailureThreshold)
	}
	return text
}

// orNone is a dash for an empty value.
func orNone(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// hostCount renders "1 host" / "12 hosts".
func hostCount(n int) string {
	if n == 1 {
		return "1 host"
	}
	return fmt.Sprintf("%d hosts", n)
}

// pluralWord returns one or many of word.
func pluralWord(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "es"
}

// runStartedMsg carries the rollout the run dialog must follow.
type runStartedMsg struct {
	run bus.Run
	err error
}

// runEventsMsg is one batch of rollout events.
type runEventsMsg struct {
	Seq    int
	Events []bus.Event
	Closed bool
}

// runProgress is the run dialog: the rollout log, its counters and the canary
// pause. It is a view rather than a modal dialog because a modal cannot receive
// stream messages; it is pushed over the focused pane and shows the same
// content the mockup's dialog does.
//
// Every pane receives the push (navigation messages broadcast), so the execute
// command is guarded by once: the rollout must start exactly once.
type runProgress struct {
	env *Env
	b   bus.Bus
	p   bus.Proposal
	c   bus.Confirmation

	now func() time.Time

	once sync.Once

	run     bus.Run
	ch      <-chan bus.Event
	seq     int
	started time.Time
	lines   []string
	ok      int
	failed  int
	skipped int
	phase   bus.Phase
	summary *bus.Summary
	done    bool
	errText string
}

var _ View = (*runProgress)(nil)

// newRunProgress builds the run dialog for a confirmed proposal.
func newRunProgress(env *Env, b bus.Bus, p bus.Proposal, c bus.Confirmation) *runProgress {
	v := &runProgress{env: env, b: b, p: p, c: c}
	if env != nil && env.Now != nil {
		v.now = env.Now
	} else {
		v.now = time.Now
	}
	return v
}

// Init starts the rollout. Only the first pane to get here executes it.
func (v *runProgress) Init() tea.Cmd {
	var cmd tea.Cmd
	v.once.Do(func() { cmd = v.execute() })
	return cmd
}

// execute asks the bus to start the confirmed proposal.
func (v *runProgress) execute() tea.Cmd {
	b, p, c := v.b, v.p, v.c
	return func() tea.Msg {
		if b == nil {
			return runStartedMsg{err: fmt.Errorf("no action bus configured")}
		}
		run, err := b.Execute(context.Background(), p, c)
		return runStartedMsg{run: run, err: err}
	}
}

// Update follows the rollout's event stream and the pause keys.
func (v *runProgress) Update(msg tea.Msg) (View, tea.Cmd) {
	switch m := msg.(type) {
	case runStartedMsg:
		return v, v.startedCmd(m)
	case runEventsMsg:
		if m.Seq != v.seq {
			return v, nil
		}
		return v, v.applyEvents(m)
	case tea.KeyMsg:
		return v, v.handleKey(m)
	}
	return v, nil
}

// startedCmd records the started rollout and asks for its first event batch.
func (v *runProgress) startedCmd(m runStartedMsg) tea.Cmd {
	if m.err != nil {
		v.errText = m.err.Error()
		v.done = true
		v.line("error: " + v.errText)
		return nil
	}
	if m.run == nil {
		v.errText = "no rollout"
		v.done = true
		v.line("error: " + v.errText)
		return nil
	}
	v.run = m.run
	v.ch = m.run.Events()
	v.started = v.now()
	v.seq++
	return v.nextBatch()
}

// nextBatch asks for the next batch of events.
func (v *runProgress) nextBatch() tea.Cmd {
	ch, seq := v.ch, v.seq
	return func() tea.Msg {
		events, closed := NextBatch(ch, runBatchMax)
		return runEventsMsg{Seq: seq, Events: events, Closed: closed}
	}
}

// applyEvents appends the batch to the log and, once the stream closes, opens
// the grouped results view.
func (v *runProgress) applyEvents(m runEventsMsg) tea.Cmd {
	for _, ev := range m.Events {
		v.applyEvent(ev)
	}
	if !m.Closed {
		return v.nextBatch()
	}
	v.done = true
	v.line(v.summaryLine())
	return Push(newRunResults(v.p, v.results()))
}

// applyEvent records one rollout event.
func (v *runProgress) applyEvent(ev bus.Event) {
	switch {
	case ev.Summary != nil:
		s := *ev.Summary
		v.summary = &s
		v.ok, v.failed, v.skipped = s.OK, s.Failed, s.Skipped
		v.phase = ev.Phase
		return
	case ev.Result != nil:
		res := *ev.Result
		if res.Err != nil || res.RC != 0 {
			v.failed++
		} else {
			v.ok++
		}
		v.line(resultLine(res))
		return
	}
	v.phase = ev.Phase
	switch ev.Phase {
	case bus.PhaseCanary:
		v.line(fmt.Sprintf("canary · %s", hostCount(v.p.Rollout.Canary)))
	case bus.PhasePaused:
		v.line("canary ok — c continue · esc abort")
	case bus.PhaseBatch:
		v.line(fmt.Sprintf("batch %d · %s", ev.Batch, hostCount(v.p.Rollout.BatchSize)))
	}
}

// results is the rollout's own result list, redacted and capped by the bus,
// falling back to what the log saw.
func (v *runProgress) results() []action.StepResult {
	if ri, ok := v.run.(bus.RunInfo); ok {
		return ri.Results()
	}
	return nil
}

// summaryLine is the closing line of the log.
func (v *runProgress) summaryLine() string {
	if v.summary == nil {
		return "finished"
	}
	return fmt.Sprintf("done · ok %d · failed %d · skipped %d · %s",
		v.summary.OK, v.summary.Failed, v.summary.Skipped, render.Duration(v.summary.Duration))
}

// handleKey drives the pause and the way back.
func (v *runProgress) handleKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "c", "C":
		if v.phase == bus.PhasePaused && v.run != nil {
			v.run.Continue()
			v.line("continuing")
		}
		return nil
	case "esc":
		if v.phase == bus.PhasePaused && !v.done && v.run != nil {
			v.run.Abort()
			return nil
		}
		// Leaving the view does not stop the rollout: it runs on the bus's
		// root context and is recorded whether anyone watches it or not.
		return Pop()
	}
	return nil
}

// line appends one log line.
func (v *runProgress) line(text string) {
	v.lines = append(v.lines, text)
}

// resultLine renders one host's outcome.
func resultLine(res action.StepResult) string {
	host := string(res.Target.Host)
	if res.Err != nil {
		return fmt.Sprintf("failed %s %s", host, errs.KindOf(res.Err))
	}
	if res.RC != 0 {
		return fmt.Sprintf("failed %s rc=%d %s", host, res.RC, runFirstLine(res.Output))
	}
	return fmt.Sprintf("ok %s", host)
}

// firstLine is the first non-empty line of s, trimmed.
func runFirstLine(s string) string {
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return render.Truncate(v, 60)
		}
	}
	return ""
}

// Crumb is the run dialog's breadcrumb.
func (v *runProgress) Crumb() string {
	return "run · " + v.command()
}

// command is the plan's first display, for crumbs and titles.
func (v *runProgress) command() string {
	if len(v.p.Plan.Steps) == 0 {
		return "(no command)"
	}
	return v.p.Plan.Steps[0].Display
}

// Hints are the keys the run dialog answers.
func (v *runProgress) Hints() []KeyHint {
	if v.phase == bus.PhasePaused && !v.done {
		return []KeyHint{{Key: "c", Text: "continue"}, {Key: "esc", Text: "abort"}}
	}
	return []KeyHint{{Key: "esc", Text: "back"}}
}

// Subject is the run's targets.
func (v *runProgress) Subject() Subject {
	hosts := make([]resource.HostID, 0, len(v.p.Plan.Steps))
	for _, s := range v.p.Plan.Steps {
		hosts = append(hosts, s.Target.Host)
	}
	return Subject{Hosts: hosts}
}

// Capturing is always false: the run dialog has no input of its own.
func (v *runProgress) Capturing() bool { return false }

// CopyLines is the log, oldest first.
func (v *runProgress) CopyLines() []string {
	out := []string{"run " + v.command()}
	return append(out, v.lines...)
}

// Render draws the log, exactly w × h.
func (v *runProgress) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := make([]string, 0, h)
	head := fmt.Sprintf("run %s on %s", v.command(), hostCount(targetCount(v.p)))
	lines = append(lines, render.Truncate(head, w))
	for _, l := range v.lines {
		if len(lines) >= h {
			break
		}
		lines = append(lines, render.Truncate(l, w))
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// runResults is the grouped results view a finished `:run` opens: one row per
// group of identical output, one row per error kind.
type runResults struct {
	title string
	sum   group.Summary
}

var _ View = (*runResults)(nil)

// newRunResults groups the rollout's results for display.
func newRunResults(p bus.Proposal, results []action.StepResult) *runResults {
	title := p.Plan.Title
	g := group.NewGrouper(targetCount(p), nil)
	for _, res := range results {
		if res.Err != nil {
			g.AddError(res.Target.Host, res.Err)
			continue
		}
		g.Add(res.Target.Host, res.Output, res.RC)
	}
	return &runResults{title: title, sum: g.Summary()}
}

// Init does nothing: the results are already in hand.
func (r *runResults) Init() tea.Cmd { return nil }

// Update answers esc and q with a pop.
func (r *runResults) Update(msg tea.Msg) (View, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return r, nil
	}
	switch k.String() {
	case "esc", "q":
		return r, Pop()
	}
	return r, nil
}

// Crumb is the results' breadcrumb.
func (r *runResults) Crumb() string { return r.title }

// Hints are the keys the view answers.
func (r *runResults) Hints() []KeyHint { return []KeyHint{{Key: "esc", Text: "back"}} }

// Subject is unclear from grouped output; the run dialog above carries it.
func (r *runResults) Subject() Subject { return Subject{} }

// Capturing is always false.
func (r *runResults) Capturing() bool { return false }

// CopyLines is the headline and every group's hosts and output.
func (r *runResults) CopyLines() []string {
	lines := []string{r.title, r.sum.Headline()}
	for i, g := range r.sum.Groups {
		label := "differs"
		if i == 0 {
			label = "majority"
		}
		lines = append(lines, fmt.Sprintf("%s  %d hosts  rc %d", label, len(g.Hosts), g.RC))
		if text := strings.TrimRight(g.Output, "\n"); text != "" {
			lines = append(lines, strings.Split(text, "\n")...)
		}
	}
	return lines
}

// Render draws the headline then one row per group, exactly w × h.
func (r *runResults) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := make([]string, 0, h)
	lines = append(lines, render.Truncate(r.title, w))
	lines = append(lines, render.Truncate(r.sum.Headline(), w))
	lines = append(lines, "")
	for _, g := range r.sum.Groups {
		if len(lines) >= h {
			break
		}
		row := fmt.Sprintf("%6d hosts  rc %3d  %s", len(g.Hosts), g.RC, firstOutputLine(g.Output))
		lines = append(lines, render.Truncate(row, w))
	}
	for i, kind := range r.errKinds() {
		if len(lines) >= h || i >= runResultLines {
			break
		}
		list := r.sum.Errors[kind]
		lines = append(lines, render.Truncate(fmt.Sprintf("%6d hosts  %s", len(list), kind), w))
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// errKinds lists the error kinds in a stable order.
func (r *runResults) errKinds() []errs.Kind {
	names := make([]string, 0, len(r.sum.Errors))
	for k := range r.sum.Errors {
		names = append(names, string(k))
	}
	sort.Strings(names)
	out := make([]errs.Kind, len(names))
	for i, n := range names {
		out[i] = errs.Kind(n)
	}
	return out
}

// firstOutputLine is the first line of out, or a placeholder.
func firstOutputLine(s string) string {
	if v := runFirstLine(s); v != "" {
		return v
	}
	return "(no output)"
}
