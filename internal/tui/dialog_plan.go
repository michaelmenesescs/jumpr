package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

// planDialog is the plan dialog: what will run, where and how, and the control
// that confirms it (SPEC §11, mockup "plan"). A denied plan has no control; only
// esc closes it.
type planDialog struct {
	t     *theme.Theme
	p     bus.Proposal
	typed string
	note  string
	// list is true while `l` shows every host (and every per-host command) in
	// a scrollable list instead of the summary.
	list bool
	top  int
	rows int
}

var _ Dialog = (*planDialog)(nil)

// newPlanDialog builds the dialog for a planned (or denied) proposal.
func newPlanDialog(t *theme.Theme, p bus.Proposal) Dialog {
	return &planDialog{t: t, p: p}
}

// Title is the dialog's border title: the plan's own title.
func (d *planDialog) Title() string {
	title := d.p.Plan.Title
	if title == "" {
		title = d.p.Plan.Spec.Title
	}
	if title == "" {
		title = "action"
	}
	if d.p.Denied != nil {
		return title + " · denied"
	}
	return title
}

// Update handles the confirmation keys and the scrolling. Nothing here does
// I/O: confirming returns the message the root turns into a bus.Execute.
func (d *planDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	if d.p.Denied != nil {
		// A denied proposal has no confirmation control.
		if k.String() == "l" {
			d.list = !d.list
			d.top = 0
		}
		return d, nil
	}
	switch k.String() {
	case "l":
		d.list = !d.list
		d.top = 0
		return d, nil
	case "up", "k":
		d.scroll(-1)
		return d, nil
	case "down", "j":
		d.scroll(1)
		return d, nil
	case "pgup":
		d.scroll(-max(1, d.rows-1))
		return d, nil
	case "pgdown":
		d.scroll(max(1, d.rows-1))
		return d, nil
	}
	if d.p.Confirm == bus.ConfirmTypedCount {
		return d, d.typedKey(k)
	}
	switch k.String() {
	case "y", "Y":
		return d, d.confirm()
	case "n", "N":
		return d, CloseDialog()
	}
	return d, nil
}

// typedKey edits the typed target count.
func (d *planDialog) typedKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "enter":
		if strings.TrimSpace(d.typed) != strconv.Itoa(targetCount(d.p)) {
			d.note = fmt.Sprintf("does not match — type %d", targetCount(d.p))
			d.typed = ""
			return nil
		}
		return d.confirm()
	case "n", "N":
		if d.typed == "" {
			return CloseDialog()
		}
		return nil
	case "backspace":
		if d.typed != "" {
			d.typed = d.typed[:len(d.typed)-1]
		}
		return nil
	}
	if k.Type == tea.KeyRunes {
		for _, r := range k.Runes {
			if r >= '0' && r <= '9' {
				d.typed += string(r)
			}
		}
	}
	return nil
}

// confirm builds the bus confirmation the root executes.
func (d *planDialog) confirm() tea.Cmd {
	conf := bus.Confirmation{ProposalID: d.p.ID}
	if d.p.Confirm == bus.ConfirmTypedCount {
		conf.Typed = strings.TrimSpace(d.typed)
	}
	p := d.p
	return func() tea.Msg { return planConfirmedMsg{Proposal: p, Confirm: conf} }
}

// scroll moves the scrollable body.
func (d *planDialog) scroll(delta int) {
	d.top += delta
	if d.top < 0 {
		d.top = 0
	}
}

// View renders the plan and the confirmation control, exactly maxW cells wide.
func (d *planDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	d.rows = rows
	body := d.body(maxW)
	if d.top > len(body)-rows {
		d.top = max(0, len(body)-rows)
	}
	lines := make([]string, 0, rows+1)
	for i := 0; i < rows; i++ {
		idx := d.top + i
		if idx >= len(body) {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, render.PadRight(render.Truncate(body[idx], maxW), maxW))
	}
	footer := d.footer()
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, render.PadRight(render.Truncate(footer, maxW), maxW))
	return strings.Join(lines, "\n")
}

// body is every line above the footer.
func (d *planDialog) body(maxW int) []string {
	if d.list {
		return d.hostList(maxW)
	}
	var lines []string
	add := func(format string, args ...any) {
		line := format
		if len(args) > 0 {
			line = fmt.Sprintf(format, args...)
		}
		lines = append(lines, line)
	}

	n := targetCount(d.p)
	add("targets: %s", hostCount(n))
	add("%s", d.hostSummary())
	steps := d.p.Plan.Steps
	if len(steps) > 0 {
		add("%s", commandSummary(steps))
	}
	add("via: %s", orNone(d.p.Plan.Via))
	for _, w := range d.p.Plan.Warnings {
		add("warning: %s", w)
	}
	if preview := strings.TrimRight(d.p.Plan.Preview, "\n"); preview != "" {
		add("preview:")
		for _, l := range strings.Split(preview, "\n") {
			add("  %s", l)
		}
	}
	if r := rolloutLine(d.p.Rollout, n); r != "" {
		add("rollout: %s", r)
	}
	if d.p.Denied != nil {
		add("denied by policy")
		if d.p.Denied.Rule != "" {
			add("rule: %s", d.p.Denied.Rule)
		}
		if d.p.Denied.Reason != "" {
			add("reason: %s", d.p.Denied.Reason)
		}
	}
	if d.note != "" {
		add("%s", d.note)
	}
	return lines
}

// hostList is the `l` body: every target host, with its command when the
// displays differ.
func (d *planDialog) hostList(maxW int) []string {
	steps := d.p.Plan.Steps
	lines := make([]string, 0, len(steps)+2)
	lines = append(lines, fmt.Sprintf("%s (%s):", hostCount(len(steps)), d.p.Plan.Spec.ID))
	perHost := d.perHostCommands()
	for i, s := range steps {
		line := fmt.Sprintf("%3d  %s", i+1, string(s.Target.Host))
		if perHost {
			line += "  " + s.Display
		}
		lines = append(lines, line)
	}
	return lines
}

// hostSummary is the first-actionHostPreview hosts of the plan, then a count.
func (d *planDialog) hostSummary() string {
	steps := d.p.Plan.Steps
	if len(steps) == 0 {
		return "hosts: (none)"
	}
	shown := steps
	more := 0
	if len(shown) > actionHostPreview {
		more = len(shown) - actionHostPreview
		shown = shown[:actionHostPreview]
	}
	names := make([]string, len(shown))
	for i, s := range shown {
		names[i] = string(s.Target.Host)
	}
	text := "hosts: " + strings.Join(names, ", ")
	if more > 0 {
		text += fmt.Sprintf(" +%d more · l list all", more)
	} else if d.perHostCommands() {
		text += " · l list all"
	}
	return text
}

// perHostCommands reports whether the steps do not all run the same display.
func (d *planDialog) perHostCommands() bool {
	steps := d.p.Plan.Steps
	if len(steps) < 2 {
		return false
	}
	for _, s := range steps[1:] {
		if s.Display != steps[0].Display {
			return true
		}
	}
	return false
}

// commandSummary says exactly what runs: the command when every host runs the
// same one, a pointer at the list when they do not.
func commandSummary(steps []action.Step) string {
	if len(steps) == 0 {
		return "command: (none)"
	}
	first := steps[0].Display
	for _, s := range steps[1:] {
		if s.Display != first {
			return fmt.Sprintf("per-host commands (%d) · l to list", len(steps))
		}
	}
	return "command: " + first
}

// footer is the confirmation control.
func (d *planDialog) footer() string {
	if d.p.Denied != nil {
		return "esc close"
	}
	if d.p.Confirm == bus.ConfirmTypedCount {
		return fmt.Sprintf("type %d to confirm · enter confirm · esc cancel  [%s]", targetCount(d.p), d.typed+"_")
	}
	return "y confirm · n cancel"
}
