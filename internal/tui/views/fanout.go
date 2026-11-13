package views

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/errs"
	"jumper/internal/fanout"
	"jumper/internal/group"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// fanoutBatchMax is the most results one FanoutBatchMsg carries (BUILD-PLAN §5:
// the streaming pattern caps a batch at 200).
const fanoutBatchMax = 200

// Layout of the fan-out row: one fixed cell count per column, the rest of the
// width belongs to OUTPUT.
const (
	fanoutHostsWidth = 7 // "HOSTS"
	fanoutShareWidth = 6 // "SHARE" ("100%" is five cells)
	fanoutGroupWidth = 8 // "majority"
	fanoutRCWidth    = 3 // "RC"
	fanoutBarWidth   = 12
)

// Fanout groups the output of one command across hosts: a banner with the
// summary, one row per group of identical output and one row per error kind.
// It is a tui.View: the run happens in internal/fanout goroutines, Update only
// feeds the grouper (never concurrent with itself), Render only draws.
type Fanout struct {
	env       *tui.Env
	title     string
	cmd       transport.Command
	hosts     []resource.HostID
	normalize func(string) string
	keys      *keymap.Map
	glyphs    render.GlyphSet

	// base is the pane's context; every run derives from it, so closing the
	// pane stops the fan-out.
	base context.Context

	// run state
	seq     int
	ctx     context.Context
	cancel  context.CancelFunc
	ch      <-chan fanout.HostResult
	grouper *group.Grouper
	sum     group.Summary
	started time.Time
	elapsed time.Duration
	done    bool

	cursor int
	// marks are the groups marked with space, keyed by group key so a re-run
	// keeps them.
	marks map[string]bool

	w, h int
}

var _ tui.View = (*Fanout)(nil)

// NewFanout builds the grouped fan-out of cmd over hosts. normalize may be nil;
// it is applied to each host's output before grouping.
func NewFanout(env *tui.Env, title string, cmd transport.Command, hosts []resource.HostID, normalize func(string) string) *Fanout {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	return &Fanout{
		env:       env,
		title:     title,
		cmd:       cmd,
		hosts:     append([]resource.HostID(nil), hosts...),
		normalize: normalize,
		keys:      keys,
		glyphs:    glyphs,
	}
}

// Init starts the run.
func (f *Fanout) Init() tea.Cmd { return f.restart() }

// SetContext makes every run derive from ctx: the view pane calls it before
// Init, and closing the pane cancels the fan-out.
func (f *Fanout) SetContext(ctx context.Context) {
	if ctx != nil {
		f.base = ctx
	}
}

// now returns the clock the environment handed us.
func (f *Fanout) now() time.Time {
	if f.env != nil && f.env.Now != nil {
		return f.env.Now()
	}
	return time.Now()
}

// restart abandons any run in flight, bumps Seq (so late batches are dropped),
// clears the groups and starts a fresh fan-out over the same hosts.
func (f *Fanout) restart() tea.Cmd {
	f.stop()
	f.seq++
	f.grouper = group.NewGrouper(len(f.hosts), f.normalize)
	f.sum = group.Summary{}
	f.done = false
	f.elapsed = 0
	f.cursor = 0
	f.started = f.now()

	if f.env == nil || f.env.Exec == nil {
		f.done = true
		f.sum = f.grouper.Summary()
		return tui.Flash(tui.FlashError, "no transport configured for probes")
	}

	parent := f.base
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	f.ctx, f.cancel = ctx, cancel
	f.ch = fanout.Run(ctx, f.env.Exec, f.hosts, f.cmd, fanout.Options{})
	return f.nextBatch()
}

// stop cancels a run in flight and drains its channel, so the runner's
// goroutines always finish.
func (f *Fanout) stop() {
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	if f.ch != nil {
		ch := f.ch
		f.ch = nil
		go func() {
			for range ch {
			}
		}()
	}
}

// nextBatch asks for the next result batch of the current run.
func (f *Fanout) nextBatch() tea.Cmd {
	ch, seq := f.ch, f.seq
	return func() tea.Msg {
		results, closed := tui.NextBatch(ch, fanoutBatchMax)
		return tui.FanoutBatchMsg{Seq: seq, Results: results, Closed: closed}
	}
}

// Update feeds batches into the grouper and routes keys. Stale batches (of a
// superseded run) are ignored entirely.
func (f *Fanout) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case tui.FanoutBatchMsg:
		if m.Seq != f.seq {
			return f, nil
		}
		return f, f.applyBatch(m)
	case tui.MarkedTargetsRequestMsg:
		return f, f.markedTargetsCmd()
	case tea.KeyMsg:
		return f.handleKey(m)
	}
	return f, nil
}

// applyBatch adds one batch to the grouper — in the UI goroutine, as the
// grouper is not thread-safe by contract — and asks for the next one.
func (f *Fanout) applyBatch(m tui.FanoutBatchMsg) tea.Cmd {
	for _, r := range m.Results {
		if r.Err != nil {
			f.grouper.AddError(r.Host, r.Err)
			continue
		}
		// A non-zero exit code is not an error: the output and the code
		// together are what identifies a group.
		f.grouper.Add(r.Host, string(r.Result.Stdout), r.Result.RC)
	}
	f.sum = f.grouper.Summary()
	if m.Closed {
		f.done = true
		f.elapsed = f.now().Sub(f.started)
		f.clampCursor()
		return nil
	}
	return f.nextBatch()
}

// handleKey routes one key press.
func (f *Fanout) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	key := m.String()
	switch {
	case f.keys.Back.Matches(key):
		// Leaving the view stops a run that is still in flight.
		if !f.done {
			f.stop()
			f.done = true
		}
		return f, tui.Pop()
	case f.keys.Up.Matches(key):
		f.moveCursor(-1)
		return f, nil
	case f.keys.Down.Matches(key):
		f.moveCursor(1)
		return f, nil
	case f.keys.PageUp.Matches(key):
		f.moveCursor(-max(1, f.h-3))
		return f, nil
	case f.keys.PageDown.Matches(key):
		f.moveCursor(max(1, f.h-3))
		return f, nil
	case f.keys.Top.Matches(key):
		f.cursor = 0
		return f, nil
	case f.keys.Bottom.Matches(key):
		f.cursor = max(0, f.rowCount()-1)
		return f, nil
	case f.keys.Open.Matches(key):
		return f, f.openMembers()
	case f.keys.Refresh.Matches(key):
		return f, f.restart()
	case f.keys.Diff.Matches(key):
		return f, f.diffCmd()
	}

	switch key {
	case "o":
		return f, f.openOutput()
	case " ", "space":
		f.toggleMark()
		return f, nil
	}
	return f, nil
}

// rowCount is the number of rows: one per group, then one per error kind.
func (f *Fanout) rowCount() int { return len(f.sum.Groups) + len(f.errKinds()) }

// moveCursor moves the selection and keeps it inside the rows.
func (f *Fanout) moveCursor(delta int) {
	f.cursor += delta
	f.clampCursor()
}

func (f *Fanout) clampCursor() {
	if f.cursor < 0 {
		f.cursor = 0
	}
	if last := f.rowCount() - 1; f.cursor > last {
		f.cursor = max(0, last)
	}
}

// errKinds returns the error kinds of the summary, sorted by kind name.
func (f *Fanout) errKinds() []errs.Kind {
	kinds := make([]string, 0, len(f.sum.Errors))
	for k := range f.sum.Errors {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	out := make([]errs.Kind, len(kinds))
	for i, k := range kinds {
		out[i] = errs.Kind(k)
	}
	return out
}

// selectedGroup returns the group under the cursor, when the cursor is on one.
func (f *Fanout) selectedGroup() (group.Group, bool) {
	if f.cursor < 0 || f.cursor >= len(f.sum.Groups) {
		return group.Group{}, false
	}
	return f.sum.Groups[f.cursor], true
}

// openMembers pushes the member list of the selected group. On an error row it
// shows that kind's first failure instead.
func (f *Fanout) openMembers() tea.Cmd {
	if g, ok := f.selectedGroup(); ok {
		return tui.Push(NewFanoutMembers(f.env, f.title, g))
	}
	kinds := f.errKinds()
	if i := f.cursor - len(f.sum.Groups); i >= 0 && i < len(kinds) {
		list := f.sum.Errors[kinds[i]]
		if len(list) > 0 && list[0].Err != nil {
			return tui.Flash(tui.FlashError, "%s: %v", kinds[i], list[0].Err)
		}
	}
	return nil
}

// diffCmd opens the diff of the selected group against the majority, or says
// why there is nothing to show.
func (f *Fanout) diffCmd() tea.Cmd {
	g, ok := f.selectedGroup()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a group to diff")
	}
	majority, hasMajority := f.sum.Majority()
	if !hasMajority {
		return tui.Flash(tui.FlashInfo, "no groups yet")
	}
	if f.cursor == 0 {
		return tui.Flash(tui.FlashWarn, "the majority group is selected; pick a differing group")
	}
	diff := group.UnifiedDiff("majority", majority.Output,
		fmt.Sprintf("%d hosts", len(g.Hosts)), g.Output)
	if strings.TrimSpace(diff) == "" {
		return tui.Flash(tui.FlashInfo, "the group has the same output as the majority")
	}
	var t *theme.Theme
	if f.env != nil {
		t = f.env.Theme
	}
	return func() tea.Msg {
		return tui.OpenDialogMsg{Dialog: tui.NewDiffDialog(t, "diff against majority", diff)}
	}
}

// openOutput shows the selected group's full output in a text view.
func (f *Fanout) openOutput() tea.Cmd {
	g, ok := f.selectedGroup()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a group to show its output")
	}
	lines := outputLines(g.Output)
	if len(lines) == 0 {
		lines = []string{"(no output)"}
	}
	crumb := fmt.Sprintf("output · %d hosts · rc %d", len(g.Hosts), g.RC)
	return tui.Push(NewText(f.env, crumb, lines))
}

// toggleMark marks or unmarks the group under the cursor.
func (f *Fanout) toggleMark() {
	g, ok := f.selectedGroup()
	if !ok {
		return
	}
	if f.marks == nil {
		f.marks = make(map[string]bool)
	}
	if f.marks[g.Key] {
		delete(f.marks, g.Key)
		return
	}
	f.marks[g.Key] = true
}

// MarkedHosts returns the hosts of the marked groups, sorted and de-duplicated.
func (f *Fanout) MarkedHosts() []resource.HostID {
	if len(f.marks) == 0 {
		return nil
	}
	seen := make(map[resource.HostID]bool)
	var out []resource.HostID
	for _, g := range f.sum.Groups {
		if !f.marks[g.Key] {
			continue
		}
		for _, h := range g.Hosts {
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) == 0 {
		return nil
	}
	return out
}

// markedTargetsCmd answers `:targets marked`: the marked groups' hosts become
// the session target set.
func (f *Fanout) markedTargetsCmd() tea.Cmd {
	hosts := f.MarkedHosts()
	if len(hosts) == 0 {
		return tui.Flash(tui.FlashInfo, "no marked groups")
	}
	set := targets.FromHosts(fmt.Sprintf("%d marked", len(hosts)), hosts)
	return func() tea.Msg { return tui.TargetsChangedMsg{Set: set} }
}

// Summary returns the current grouping.
func (f *Fanout) Summary() group.Summary { return f.sum }

// Render draws the banner, the header and the visible rows, exactly w × h.
func (f *Fanout) Render(w, h int) string {
	f.h = h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := make([]string, 0, h)
	lines = append(lines, f.banner())
	if h > 1 {
		lines = append(lines, f.header(w))
		visible := h - 2
		if visible > 0 {
			start := 0
			if f.cursor >= visible {
				start = f.cursor - visible + 1
			}
			for i := start; i < start+visible && i < f.rowCount(); i++ {
				lines = append(lines, f.rowLine(i, w))
			}
		}
	}
	return render.FitWith(f.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner is the summary line: a progress bar while the run is in flight, the
// headline, and the elapsed time.
func (f *Fanout) banner() string {
	var b strings.Builder
	if !f.done {
		frac := 0.0
		if f.sum.Total > 0 {
			frac = float64(f.sum.Done) / float64(f.sum.Total)
		}
		b.WriteString(render.BarWith(f.glyphs, frac, fanoutBarWidth))
		b.WriteString(" ")
	}
	b.WriteString(f.sum.Headline())
	if f.done {
		b.WriteString(" · " + render.Duration(f.elapsed))
	} else if f.sum.Done > 0 {
		b.WriteString(" · " + render.Duration(f.now().Sub(f.started)))
	} else {
		b.WriteString(" " + string(f.glyphs.Ellipsis))
	}
	text := b.String()
	if f.env != nil && f.env.Theme != nil {
		text = f.env.Theme.Dim.Render(text)
	}
	return text
}

// header is the column title row.
func (f *Fanout) header(w int) string {
	line := render.PadLeft("HOSTS", fanoutHostsWidth) + " " +
		render.PadLeft("SHARE", fanoutShareWidth) + " " +
		render.PadRight("GROUP", fanoutGroupWidth) + " " +
		render.PadLeft("RC", fanoutRCWidth) + " " +
		"OUTPUT"
	line = render.Truncate(line, w)
	if f.env != nil && f.env.Theme != nil {
		line = f.env.Theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// rowLine renders one row of the view: a group, or an error kind.
func (f *Fanout) rowLine(i, w int) string {
	groups := f.sum.Groups
	if i < len(groups) {
		return f.groupLine(i, groups[i], w)
	}
	kinds := f.errKinds()
	if i-len(groups) >= len(kinds) {
		return render.PadRight("", w)
	}
	k := kinds[i-len(groups)]
	list := f.sum.Errors[k]
	line := render.PadRight(string(k), fanoutHostsWidth+1+fanoutShareWidth+1+fanoutGroupWidth) +
		" " + fmt.Sprintf("%d %s", len(list), pluralWord(len(list), "host"))
	line = render.PadRight(render.Truncate(line, w), w)
	if f.env != nil && f.env.Theme != nil {
		if i == f.cursor {
			return f.env.Theme.Selected.Render(line)
		}
		return f.env.Theme.Crit.Render(line)
	}
	return line
}

// groupLine renders one group: host count, share of the expected hosts, the
// majority/differs label, the exit code and the first output line.
func (f *Fanout) groupLine(i int, g group.Group, w int) string {
	share := 0.0
	switch {
	case f.sum.Total > 0:
		share = 100 * float64(len(g.Hosts)) / float64(f.sum.Total)
	case len(g.Hosts) > 0:
		share = 100
	}
	label := "differs"
	if i == 0 {
		label = "majority"
	}
	out := firstOutputLine(g.Output)
	if n := len(outputLines(g.Output)); n > 1 {
		out += fmt.Sprintf(" (+%d lines)", n-1)
	}
	line := f.markCell(g) + " " +
		render.PadLeft(strconv.Itoa(len(g.Hosts)), fanoutHostsWidth) + " " +
		render.PadLeft(render.Percent(share), fanoutShareWidth) + " " +
		render.PadRight(label, fanoutGroupWidth) + " " +
		render.PadLeft(strconv.Itoa(g.RC), fanoutRCWidth) + " " +
		out
	line = render.PadRight(render.Truncate(line, w), w)
	if f.env != nil && f.env.Theme != nil {
		if i == f.cursor {
			return f.env.Theme.Selected.Render(line)
		}
		return f.env.Theme.Base.Render(line)
	}
	return line
}

// markCell is the one-cell gutter: the mark glyph for a marked group, a space
// otherwise.
func (f *Fanout) markCell(g group.Group) string {
	if f.marks[g.Key] {
		if f.glyphs.ASCII() {
			return ">"
		}
		return "▸"
	}
	return " "
}

// Crumb is the run's title, e.g. "probe failed-units".
func (f *Fanout) Crumb() string { return f.title }

// Hints are the keys the view answers.
func (f *Fanout) Hints() []tui.KeyHint {
	return []tui.KeyHint{
		{Key: firstKey(f.keys.Open), Text: "hosts"},
		{Key: firstKey(f.keys.Diff), Text: "diff"},
		{Key: "o", Text: "output"},
		{Key: firstKey(f.keys.Refresh), Text: "re-run"},
		{Key: "space", Text: "mark"},
		{Key: firstKey(f.keys.Back), Text: "back"},
	}
}

// Subject is every host the run covers.
func (f *Fanout) Subject() tui.Subject {
	return tui.Subject{Hosts: append([]resource.HostID(nil), f.hosts...)}
}

// Capturing is always false: the fan-out has no input of its own.
func (f *Fanout) Capturing() bool { return false }

// CopyLines returns the headline, then each group's hosts and output.
func (f *Fanout) CopyLines() []string {
	lines := []string{f.sum.Headline()}
	for i, g := range f.sum.Groups {
		label := "differs"
		if i == 0 {
			label = "majority"
		}
		lines = append(lines, fmt.Sprintf("%s  %d hosts  rc %d", label, len(g.Hosts), g.RC))
		hosts := make([]string, len(g.Hosts))
		for j, h := range g.Hosts {
			hosts[j] = string(h)
		}
		lines = append(lines, strings.Join(hosts, ", "))
		lines = append(lines, outputLines(g.Output)...)
	}
	for _, k := range f.errKinds() {
		lines = append(lines, fmt.Sprintf("%s  %d %s", k, len(f.sum.Errors[k]),
			pluralWord(len(f.sum.Errors[k]), "host")))
	}
	return lines
}

// outputLines splits an output into its non-empty-terminated lines.
func outputLines(s string) []string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// firstOutputLine is the first line of out, or a placeholder when there is
// none.
func firstOutputLine(s string) string {
	if lines := outputLines(s); len(lines) > 0 {
		return lines[0]
	}
	return "(no output)"
}
