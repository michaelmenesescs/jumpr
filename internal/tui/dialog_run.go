package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/render"
	"jumpr/internal/tui/theme"
)

// runDialog is the rollout dialog: the log of what happened, the counters, the
// elapsed time, the canary pause and the way to the grouped results. It is a
// modal of the root so it can be hidden (`h`) while the rollout keeps going.
type runDialog struct {
	id  int
	env *Env
	p   bus.Proposal
	now func() time.Time

	run     bus.Run
	ch      <-chan bus.Event
	started time.Time
	lines   []string
	results []action.StepResult

	ok, failed, skipped int
	batchOK             int
	batchOpen           bool
	batchLine           int // index in lines of the open batch header
	phase               bus.Phase
	batch               int
	summary             *bus.Summary
	done                bool
	errText             string

	// hidden means the rollout continues without its dialog on screen.
	hidden bool
	// refreshed is set once the finished rollout asked for a view refresh.
	refreshed bool
	// confirmAbort is true while "abort remaining batches?" is on screen.
	confirmAbort bool

	// follow pins the log to its last line; scrolling up unpins it.
	follow bool
	top    int
	rows   int
}

var _ Dialog = (*runDialog)(nil)

// newRunDialog builds the run dialog of proposal p.
func newRunDialog(env *Env, p bus.Proposal, id int) *runDialog {
	now := time.Now
	if env != nil && env.Now != nil {
		now = env.Now
	}
	return &runDialog{id: id, env: env, p: p, now: now, follow: true}
}

// Title is the dialog's border title.
func (d *runDialog) Title() string {
	title := d.p.Plan.Title
	if title == "" {
		title = d.p.Plan.Spec.Title
	}
	if title == "" {
		title = "run"
	}
	return title
}

// start asks the bus to run the confirmed proposal, in a command.
func (d *runDialog) start(b bus.Bus, c bus.Confirmation) tea.Cmd {
	id, p := d.id, d.p
	return func() tea.Msg {
		if b == nil {
			return runBeginMsg{ID: id, Err: fmt.Errorf("no action bus configured")}
		}
		run, err := b.Execute(context.Background(), p, c)
		return runBeginMsg{ID: id, Run: run, Err: err}
	}
}

// begin follows the started rollout: the first event batch, or the error that
// stopped it.
func (d *runDialog) begin(m runBeginMsg) tea.Cmd {
	if m.Err != nil {
		d.errText = m.Err.Error()
		d.done = true
		d.line("error: " + d.errText)
		return nil
	}
	if m.Run == nil {
		d.errText = "no rollout"
		d.done = true
		d.line("error: " + d.errText)
		return nil
	}
	d.run = m.Run
	d.ch = m.Run.Events()
	d.started = d.now()
	return d.nextBatch()
}

// nextBatch asks for the next batch of rollout events.
func (d *runDialog) nextBatch() tea.Cmd {
	ch, id := d.ch, d.id
	return func() tea.Msg {
		events, closed := NextBatch(ch, actionBatchMax)
		return runEventsForMsg{ID: id, Msg: RunEventsMsg{Events: events, Closed: closed}}
	}
}

// Update follows the event stream and the dialog's keys.
func (d *runDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	switch m := msg.(type) {
	case runBeginMsg:
		return d, d.begin(m)
	case RunEventsMsg:
		return d, d.applyEvents(m)
	case tea.KeyMsg:
		return d, d.handleKey(m)
	}
	return d, nil
}

// applyEvents appends one batch to the log and asks for the next one.
func (d *runDialog) applyEvents(m RunEventsMsg) tea.Cmd {
	for _, ev := range m.Events {
		d.applyEvent(ev)
	}
	if !m.Closed {
		return d.nextBatch()
	}
	d.closeBatch()
	d.done = true
	d.phase = d.finalPhase()
	if ri, ok := d.run.(bus.RunInfo); ok {
		d.results = ri.Results()
	}
	d.line(d.summaryLine())
	return nil
}

// applyEvent records one rollout event.
func (d *runDialog) applyEvent(ev bus.Event) {
	switch {
	case ev.Summary != nil:
		s := *ev.Summary
		d.summary = &s
		d.ok, d.failed, d.skipped = s.OK, s.Failed, s.Skipped
		d.phase = ev.Phase
		return
	case ev.Result != nil:
		res := *ev.Result
		d.results = append(d.results, res)
		if res.Err != nil || res.RC != 0 {
			d.failed++
		} else {
			d.ok++
			d.batchOK++
		}
		d.line(resultLine(res))
		return
	}
	switch ev.Phase {
	case bus.PhaseCanary:
		d.phase = ev.Phase
		d.line(fmt.Sprintf("canary · %s", hostCount(d.p.Rollout.Canary)))
	case bus.PhasePaused:
		d.phase = ev.Phase
		d.line("canary ok — c continue · esc abort")
	case bus.PhaseBatch:
		d.closeBatch()
		d.phase = ev.Phase
		d.batch = ev.Batch
		d.batchOK = 0
		d.batchOpen = true
		d.batchLine = len(d.lines)
		d.line(fmt.Sprintf("batch %d/%d · %s", ev.Batch, d.batches(), hostCount(d.p.Rollout.BatchSize)))
	}
}

// closeBatch appends the batch's tally to its header line.
func (d *runDialog) closeBatch() {
	if !d.batchOpen {
		return
	}
	d.batchOpen = false
	if d.batchLine >= 0 && d.batchLine < len(d.lines) {
		d.lines[d.batchLine] += fmt.Sprintf(" · ok %d", d.batchOK)
	}
}

// batches is the number of rolling batches the plan announced.
func (d *runDialog) batches() int {
	if d.p.Rollout.Batches > 0 {
		return d.p.Rollout.Batches
	}
	return 1
}

// finalPhase is the phase the closed stream ended in.
func (d *runDialog) finalPhase() bus.Phase {
	if d.phase == bus.PhaseStopped || d.phase == bus.PhaseAborted || d.phase == bus.PhaseDone {
		return d.phase
	}
	return bus.PhaseDone
}

// summaryLine is the closing line of the log.
func (d *runDialog) summaryLine() string {
	label := "done"
	switch d.finalPhase() {
	case bus.PhaseAborted:
		label = "aborted"
	case bus.PhaseStopped:
		label = "stopped"
	}
	if d.summary == nil {
		return label
	}
	return fmt.Sprintf("%s · ok %d · failed %d · skipped %d · %s",
		label, d.summary.OK, d.summary.Failed, d.summary.Skipped, render.Duration(d.summary.Duration))
}

// handleKey drives the pause, the abort confirmation, the hide and the way to
// the results.
func (d *runDialog) handleKey(k tea.KeyMsg) tea.Cmd {
	if d.confirmAbort {
		switch k.String() {
		case "y", "Y":
			d.confirmAbort = false
			if d.run != nil {
				d.run.Abort()
			}
			return nil
		case "n", "N":
			d.confirmAbort = false
			return nil
		}
		return nil
	}
	switch k.String() {
	case "c", "C":
		if d.phase == bus.PhasePaused && !d.done && d.run != nil {
			d.run.Continue()
			d.line("continuing")
		}
		return nil
	case "h":
		id := d.id
		return func() tea.Msg { return hideRunMsg{ID: id} }
	case "up", "k":
		d.follow = false
		d.top--
		d.clamp()
		return nil
	case "down", "j":
		d.top++
		d.clamp()
		return nil
	case "enter":
		if d.done {
			return CloseDialog()
		}
		return nil
	case "o":
		if !d.done {
			return Flash(FlashInfo, "the rollout has not finished")
		}
		id := d.id
		return func() tea.Msg { return runResultsRequestMsg{ID: id} }
	}
	return nil
}

// Esc handles the escape key instead of the dialog being dismissed: at the
// canary pause it aborts the rollout, during the batches it asks first, and
// after the rollout it closes the dialog.
func (d *runDialog) Esc() (Dialog, tea.Cmd) {
	switch {
	case d.done:
		return nil, nil
	case d.confirmAbort:
		d.confirmAbort = false
		return d, nil
	case d.phase == bus.PhasePaused:
		if d.run != nil {
			d.run.Abort()
		}
		return d, nil
	default:
		d.confirmAbort = true
		return d, nil
	}
}

// line appends one log line.
func (d *runDialog) line(text string) {
	d.lines = append(d.lines, text)
}

// clamp keeps the scroll position inside the log.
func (d *runDialog) clamp() {
	if d.top < 0 {
		d.top = 0
	}
	if last := len(d.lines) - d.rows; d.top > last {
		d.top = max(0, last)
	}
}

// badge is the status-line marker of a hidden rollout, e.g. "▶ rollout 2/4".
func (d *runDialog) badge() string {
	mark := "▶"
	if d.env != nil && d.env.Glyphs.Name != "" && d.env.Glyphs.ASCII() {
		mark = ">"
	}
	batch := d.batch
	if batch < 1 {
		batch = 1
	}
	return fmt.Sprintf("%s rollout %d/%d", mark, batch, d.batches())
}

// progress is the one-line state shown in the run list.
func (d *runDialog) progress() string {
	switch {
	case d.done:
		return "finished"
	case d.phase == bus.PhasePaused:
		return fmt.Sprintf("paused after canary · batch %d/%d", max(d.batch, 1), d.batches())
	default:
		return fmt.Sprintf("batch %d/%d · ok %d · failed %d", max(d.batch, 1), d.batches(), d.ok, d.failed)
	}
}

// View renders the counters, the log and the footer, exactly maxW cells wide.
func (d *runDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	rows := maxH - 3
	if rows < 1 {
		rows = 1
	}
	d.rows = rows
	if d.follow {
		d.top = max(0, len(d.lines)-rows)
	}
	d.clamp()

	lines := make([]string, 0, maxH)
	head := fmt.Sprintf("run %s on %s", d.Title(), hostCount(targetCount(d.p)))
	lines = append(lines, render.PadRight(render.Truncate(head, maxW), maxW))
	lines = append(lines, render.PadRight(render.Truncate(d.counterLine(), maxW), maxW))
	for i := 0; i < rows; i++ {
		idx := d.top + i
		if idx >= len(d.lines) {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, render.PadRight(render.Truncate(d.lines[idx], maxW), maxW))
	}
	footer := d.footer()
	if d.t() != nil {
		footer = d.t().Dim.Render(footer)
	}
	lines = append(lines, render.PadRight(render.Truncate(footer, maxW), maxW))
	if len(lines) > maxH {
		lines = lines[:maxH]
	}
	return strings.Join(lines, "\n")
}

// t is the theme, when there is an env.
func (d *runDialog) t() *theme.Theme {
	if d.env == nil {
		return nil
	}
	return d.env.Theme
}

// counterLine is the counters and the elapsed time.
func (d *runDialog) counterLine() string {
	elapsed := time.Duration(0)
	if !d.started.IsZero() {
		elapsed = d.now().Sub(d.started)
	}
	return fmt.Sprintf("ok %d · failed %d · skipped %d · %s", d.ok, d.failed, d.skipped, render.Duration(elapsed))
}

// footer is the key hint of the current state.
func (d *runDialog) footer() string {
	switch {
	case d.confirmAbort:
		return "abort remaining batches? y/N"
	case d.done:
		return "enter close · o results · h hide"
	case d.phase == bus.PhasePaused:
		return "c continue · esc abort · h hide"
	default:
		return "esc abort · h hide"
	}
}
