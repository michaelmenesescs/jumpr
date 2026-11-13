package views

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/salt"
	"jumper/internal/group"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// KindSaltState is the factory key of the dry-run state screen. It is a view
// kind, not a backend kind: the salt backend serves salt-minion, not this.
const KindSaltState resource.KindName = "salt-state"

const (
	// saltStateBatchMax is the most results one batch message carries
	// (BUILD-PLAN §5: the streaming pattern caps a batch at 200).
	saltStateBatchMax = 200

	// Layout of the group row: one fixed cell count per column.
	saltStateMinionsWidth = 7  // "MINIONS"
	saltStateSummaryWidth = 16 // "2 would change"
)

// SaltStateBackend is the part of the Salt backend the state view needs. It is
// an interface so the view is testable without a master.
type SaltStateBackend interface {
	// DryRun starts state.apply test=True on minions and returns its stream
	// of per-minion results with the compiled target.
	DryRun(ctx context.Context, minions []string, sls string, sel resource.Selector) (*salt.Job, salt.Compiled, error)
	// HostMinion maps an inventory host onto its minion id.
	HostMinion(host resource.HostID) (string, bool)
}

// saltStateStartMsg is the dry run's submission answer, tagged with the load
// Seq so a superseded run is dropped.
type saltStateStartMsg struct {
	Seq       int
	Job       *salt.Job
	Compiled  salt.Compiled
	NotMinion []resource.HostID
	Err       error
}

// saltStateBatchMsg is one batch of per-minion dry-run results.
type saltStateBatchMsg struct {
	Seq     int
	Results []salt.MinionResult
	Closed  bool
}

// SaltState is the `:salt state` screen: a dry run on the session targets,
// grouped by identical normalized diff. It is a tui.View: the run happens in
// the backend's job engine, Update only feeds the groups, Render only draws.
type SaltState struct {
	env  *tui.Env
	b    SaltStateBackend
	sls  string
	keys *keymap.Map
	// glyphs is the active glyph set: every rune the view draws comes from it.
	glyphs render.GlyphSet

	// base is the pane's context; every load derives from it, so closing the
	// pane stops the dry run.
	base context.Context

	seq    int
	cancel context.CancelFunc
	ch     <-chan salt.MinionResult

	minions     []string
	minionCount int
	runs        map[string]salt.StateRun
	groups      []salt.StateGroup
	counts      salt.StateCounts
	notMinion   []resource.HostID
	compiled    salt.Compiled

	started   bool
	done      bool
	loadErr   error
	startedAt time.Time
	elapsed   time.Duration

	cursor       int
	members      bool
	memberCursor int
	// marks are the marked groups, keyed by normalized text so a refresh
	// keeps them.
	marks map[string]bool

	w, h int
}

var _ tui.View = (*SaltState)(nil)

// NewSaltState builds the dry-run state view of sls (empty means highstate) for
// the session's target set.
func NewSaltState(env *tui.Env, b SaltStateBackend, sls string) *SaltState {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	return &SaltState{
		env:    env,
		b:      b,
		sls:    strings.TrimSpace(sls),
		keys:   keys,
		glyphs: glyphs,
		runs:   make(map[string]salt.StateRun),
	}
}

// Init starts the dry run.
func (s *SaltState) Init() tea.Cmd { return s.load() }

// SetContext makes every load derive from ctx, so closing the pane stops the
// dry run.
func (s *SaltState) SetContext(ctx context.Context) {
	if ctx != nil {
		s.base = ctx
	}
}

// Close cancels a dry run in flight. The stack calls it when the view is
// popped.
func (s *SaltState) Close() error {
	s.stop()
	return nil
}

// now returns the clock the environment handed in.
func (s *SaltState) now() time.Time {
	if s.env != nil && s.env.Now != nil {
		return s.env.Now()
	}
	return time.Now()
}

// baseCtx is the context every load derives from.
func (s *SaltState) baseCtx() context.Context {
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// load abandons any run in flight, bumps Seq, and starts a dry run over the
// session targets mapped onto minions.
func (s *SaltState) load() tea.Cmd {
	s.stop()
	s.seq++
	seq := s.seq
	s.runs = make(map[string]salt.StateRun)
	s.groups = nil
	s.counts = salt.StateCounts{}
	s.notMinion = nil
	s.compiled = salt.Compiled{}
	s.started = false
	s.done = false
	s.loadErr = nil
	s.elapsed = 0
	s.cursor = 0
	s.members = false
	s.memberCursor = 0
	s.startedAt = s.now()

	minions, notMinion := s.resolveTargets()
	s.minions, s.minionCount, s.notMinion = minions, len(minions), notMinion
	if s.b == nil {
		s.started, s.done = true, true
		s.loadErr = fmt.Errorf("no salt backend configured")
		return nil
	}
	if len(minions) == 0 {
		s.started, s.done = true, true
		return tui.Flash(tui.FlashWarn, "no target of this session maps to a salt minion")
	}

	ctx, cancel := context.WithCancel(s.baseCtx())
	s.cancel = cancel
	b, sls, sel := s.b, s.sls, s.selector()
	return func() tea.Msg {
		job, compiled, err := b.DryRun(ctx, minions, sls, sel)
		return saltStateStartMsg{Seq: seq, Job: job, Compiled: compiled, NotMinion: notMinion, Err: err}
	}
}

// stop cancels a run in flight and drains its channel, so the job engine's
// goroutines always finish.
func (s *SaltState) stop() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.ch != nil {
		ch := s.ch
		s.ch = nil
		go func() {
			for range ch {
			}
		}()
	}
}

// sessionTargets are the hosts of the session target set.
func (s *SaltState) sessionTargets() []resource.HostID {
	if s.env == nil || s.env.Session == nil {
		return nil
	}
	return s.env.Session.Targets.Hosts
}

// selector is the session selector, used to compile the Salt target.
func (s *SaltState) selector() resource.Selector {
	if s.env == nil || s.env.Session == nil {
		return resource.Selector{}
	}
	return s.env.Session.Targets.Selector
}

// resolveTargets maps the session's hosts onto minion ids. Hosts Salt does not
// manage are returned separately and listed as "not a minion".
func (s *SaltState) resolveTargets() ([]string, []resource.HostID) {
	hosts := s.sessionTargets()
	minions := make([]string, 0, len(hosts))
	var notMinion []resource.HostID
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if s.b == nil {
			notMinion = append(notMinion, h)
			continue
		}
		m, ok := s.b.HostMinion(h)
		if !ok || m == "" {
			notMinion = append(notMinion, h)
			continue
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		minions = append(minions, m)
	}
	sort.Strings(minions)
	return minions, notMinion
}

// nextBatch asks for the next batch of dry-run results.
func (s *SaltState) nextBatch() tea.Cmd {
	ch, seq := s.ch, s.seq
	return func() tea.Msg {
		items, closed := tui.NextBatch(ch, saltStateBatchMax)
		return saltStateBatchMsg{Seq: seq, Results: items, Closed: closed}
	}
}

// Update folds batches into the groups and routes keys. Stale answers (of a
// superseded run) are ignored entirely.
func (s *SaltState) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case saltStateStartMsg:
		if m.Seq != s.seq {
			return s, nil
		}
		return s, s.start(m)
	case saltStateBatchMsg:
		if m.Seq != s.seq {
			return s, nil
		}
		return s, s.apply(m)
	case tui.MarkedTargetsRequestMsg:
		return s, s.markedTargetsCmd()
	case tui.RefreshRequestMsg:
		return s, s.load()
	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return s, nil
}

// start keeps the job's stream and asks for its first batch.
func (s *SaltState) start(m saltStateStartMsg) tea.Cmd {
	s.started = true
	s.compiled = m.Compiled
	if len(m.NotMinion) > 0 {
		s.notMinion = m.NotMinion
	}
	if m.Err != nil {
		s.loadErr = m.Err
		s.done = true
		return nil
	}
	if m.Job == nil {
		s.loadErr = fmt.Errorf("the dry run returned nothing")
		s.done = true
		return nil
	}
	s.ch = m.Job.Results()
	return s.nextBatch()
}

// apply adds one batch to the runs — in the UI goroutine, never concurrently
// with itself — and asks for the next one.
func (s *SaltState) apply(m saltStateBatchMsg) tea.Cmd {
	for _, mr := range m.Results {
		s.runs[mr.Minion] = salt.StateRunFromResult(mr)
	}
	s.rebuild()
	if m.Closed {
		s.done = true
		s.elapsed = s.now().Sub(s.startedAt)
		s.clampCursor()
		return nil
	}
	return s.nextBatch()
}

// rebuild regroups the dry-run returns.
func (s *SaltState) rebuild() {
	s.groups = salt.StateGroups(s.runs)
	s.counts = salt.CountStateGroups(s.groups, s.minionCount)
}

// handleKey routes one key press.
func (s *SaltState) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	key := m.String()

	switch {
	case s.keys.Back.Matches(key):
		if s.members {
			s.members = false
			s.memberCursor = 0
			return s, nil
		}
		if !s.done {
			s.stop()
			s.done = true
		}
		return s, tui.Pop()
	case s.keys.Up.Matches(key):
		s.moveCursor(-1)
		return s, nil
	case s.keys.Down.Matches(key):
		s.moveCursor(1)
		return s, nil
	case s.keys.PageUp.Matches(key):
		s.moveCursor(-max(1, s.h-4))
		return s, nil
	case s.keys.PageDown.Matches(key):
		s.moveCursor(max(1, s.h-4))
		return s, nil
	case s.keys.Top.Matches(key):
		s.setCursor(0)
		return s, nil
	case s.keys.Bottom.Matches(key):
		s.setCursor(s.rowCount() - 1)
		return s, nil
	case s.keys.Open.Matches(key):
		return s, s.open()
	case s.keys.Refresh.Matches(key):
		return s, s.load()
	case s.keys.Diff.Matches(key):
		return s, s.diffCmd()
	case s.keys.Actions.Matches(key):
		return s, s.actionsCmd()
	}

	switch key {
	case "o":
		return s, s.openOutput()
	case " ", "space":
		s.toggleMark()
		return s, nil
	}
	return s, nil
}

// notMinionRow is 1 when the session has hosts Salt does not manage.
func (s *SaltState) notMinionRow() int {
	if len(s.notMinion) > 0 {
		return 1
	}
	return 0
}

// rowCount is the number of rows: one per group, then the "not a minion" row.
func (s *SaltState) rowCount() int { return len(s.groups) + s.notMinionRow() }

// cursorPos is the cursor of the current mode.
func (s *SaltState) cursorPos() int {
	if s.members {
		return s.memberCursor
	}
	return s.cursor
}

// setCursor moves the cursor of the current mode.
func (s *SaltState) setCursor(n int) {
	if s.members {
		s.memberCursor = n
	} else {
		s.cursor = n
	}
	s.clampCursor()
}

// moveCursor moves the cursor of the current mode by delta.
func (s *SaltState) moveCursor(delta int) { s.setCursor(s.cursorPos() + delta) }

// clampCursor keeps the cursor inside the rows of the current mode.
func (s *SaltState) clampCursor() {
	last := s.rowCount() - 1
	if s.members {
		if g, ok := s.selectedGroup(); ok {
			last = len(g.Minions) - 1
		} else {
			last = 0
		}
	}
	if last < 0 {
		s.cursor, s.memberCursor = 0, 0
		return
	}
	if s.cursorPos() > last {
		s.setCursor(last)
		return
	}
	if s.cursorPos() < 0 {
		s.setCursor(0)
	}
}

// selectedGroup returns the group under the cursor, when the cursor is on one.
func (s *SaltState) selectedGroup() (salt.StateGroup, bool) {
	if s.cursor < 0 || s.cursor >= len(s.groups) {
		return salt.StateGroup{}, false
	}
	return s.groups[s.cursor], true
}

// open drills in: a group lists its states and minions, the "not a minion" row
// lists the hosts Salt does not manage.
func (s *SaltState) open() tea.Cmd {
	if _, ok := s.selectedGroup(); ok {
		s.members = true
		s.memberCursor = 0
		return nil
	}
	if s.notMinionRow() > 0 && s.cursor == len(s.groups) {
		return s.openNotMinion()
	}
	return tui.Flash(tui.FlashInfo, "no rows yet")
}

// openNotMinion shows the hosts of the session that Salt does not manage.
func (s *SaltState) openNotMinion() tea.Cmd {
	lines := make([]string, 0, len(s.notMinion))
	for _, h := range s.notMinion {
		lines = append(lines, string(h)+"  not a minion")
	}
	if len(lines) == 0 {
		lines = []string{"(none)"}
	}
	return tui.Push(NewText(s.env, fmt.Sprintf("not a minion · %d hosts", len(lines)), lines))
}

// cleanGroup is the group every other group is diffed against: the first clean
// one, else the largest.
func (s *SaltState) cleanGroup() (salt.StateGroup, bool) {
	for _, g := range s.groups {
		if g.Clean() {
			return g, true
		}
	}
	if len(s.groups) > 0 {
		return s.groups[0], true
	}
	return salt.StateGroup{}, false
}

// diffCmd opens the normalized text of the selected group diffed against the
// clean (or majority) group.
func (s *SaltState) diffCmd() tea.Cmd {
	g, ok := s.selectedGroup()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a group to diff")
	}
	base, ok := s.cleanGroup()
	if !ok {
		return tui.Flash(tui.FlashInfo, "no groups yet")
	}
	if base.Key == g.Key {
		return tui.Flash(tui.FlashWarn, "this is the group everything else is compared with; pick another")
	}
	diff := group.UnifiedDiff("baseline", base.Key,
		fmt.Sprintf("%d minions", len(g.Minions)), g.Key)
	if strings.TrimSpace(diff) == "" {
		return tui.Flash(tui.FlashInfo, "the group has the same normalized text as the baseline")
	}
	var t *theme.Theme
	if s.env != nil {
		t = s.env.Theme
	}
	return func() tea.Msg {
		return tui.OpenDialogMsg{Dialog: tui.NewDiffDialog(t, "diff against clean group", diff)}
	}
}

// openOutput shows the selected group's full normalized text.
func (s *SaltState) openOutput() tea.Cmd {
	g, ok := s.selectedGroup()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a group to show its text")
	}
	lines := outputLines(g.Normalized())
	if len(lines) == 0 {
		lines = []string{"(no output)"}
	}
	lines = append(lines, "", fmt.Sprintf("minions (%d):", len(g.Minions)))
	lines = append(lines, g.Minions...)
	crumb := fmt.Sprintf("state · %d minions · %s", len(g.Minions), g.SummaryLine())
	return tui.Push(NewText(s.env, crumb, lines))
}

// toggleMark marks or unmarks the group under the cursor.
func (s *SaltState) toggleMark() {
	g, ok := s.selectedGroup()
	if !ok {
		return
	}
	if s.marks == nil {
		s.marks = make(map[string]bool)
	}
	if s.marks[g.Key] {
		delete(s.marks, g.Key)
		return
	}
	s.marks[g.Key] = true
}

// markedMinions is the union of the marked groups' minions, sorted.
func (s *SaltState) markedMinions() []string {
	if len(s.marks) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, g := range s.groups {
		if !s.marks[g.Key] {
			continue
		}
		for _, m := range g.Minions {
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// selectionResources builds one salt-minion row per minion, so the actions menu
// sees the same resources the minion table would give it.
func selectionResources(minions []string) []resource.Resource {
	out := make([]resource.Resource, 0, len(minions))
	for _, m := range minions {
		out = append(out, resource.Resource{
			Ref:    resource.Ref{Backend: "salt", Kind: salt.KindMinion, Name: m},
			Fields: map[string]any{"minion": m},
		})
	}
	return out
}

// actionsCmd asks the root for the actions menu over the selected group's
// minions, or over every marked group when there are marks.
func (s *SaltState) actionsCmd() tea.Cmd {
	minions := s.markedMinions()
	if len(minions) == 0 {
		g, ok := s.selectedGroup()
		if !ok || len(g.Minions) == 0 {
			return tui.Flash(tui.FlashInfo, "no group selected")
		}
		minions = g.Minions
	}
	sel := selectionResources(minions)
	return func() tea.Msg {
		return tui.ActionsRequestMsg{Kind: salt.KindMinion, Selection: sel}
	}
}

// markedTargetsCmd answers `:targets marked`: the marked groups' minions
// become the session target set through their inventory hosts.
func (s *SaltState) markedTargetsCmd() tea.Cmd {
	minions := s.markedMinions()
	if len(minions) == 0 {
		return tui.Flash(tui.FlashInfo, "no marked groups")
	}
	hosts := make([]resource.HostID, 0, len(minions))
	for _, m := range minions {
		if h, ok := s.minionHost(m); ok {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return tui.Flash(tui.FlashInfo, "the marked minions have no inventory host")
	}
	set := targets.FromHosts(fmt.Sprintf("%d marked", len(hosts)), hosts)
	return func() tea.Msg { return tui.TargetsChangedMsg{Set: set} }
}

// minionHost maps a minion onto its inventory host, when the environment has an
// inventory.
func (s *SaltState) minionHost(minion string) (resource.HostID, bool) {
	if s.env == nil || s.env.Inventory == nil || minion == "" {
		return "", false
	}
	h, ok := s.env.Inventory.Resolve(minion)
	if !ok {
		return "", false
	}
	return h.ID, true
}

// Render draws the banner, the header and the visible rows, exactly w × h.
func (s *SaltState) Render(w, h int) string {
	s.w, s.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{s.banner()}
	if s.members {
		lines = append(lines, s.detailLines(w, h-1)...)
	} else if h > 1 {
		lines = append(lines, s.header(w))
		if visible := h - 2; visible > 0 {
			start := 0
			if s.cursor >= visible {
				start = s.cursor - visible + 1
			}
			for i := start; i < start+visible && i < s.rowCount(); i++ {
				lines = append(lines, s.rowLine(i, w))
			}
		}
	}
	return render.FitWith(s.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner is the summary line: the per-minion tally, the compiled target and,
// once finished, how long the dry run took.
func (s *SaltState) banner() string {
	var text string
	switch {
	case s.loadErr != nil:
		text = "error: " + s.loadErr.Error()
	case !s.started:
		text = "dry run " + s.crumbTarget() + " " + string(s.glyphs.Ellipsis)
	case s.done:
		text = s.counts.Summary() + " · " + render.Duration(s.elapsed)
	default:
		text = s.counts.Summary() + " " + string(s.glyphs.Ellipsis)
	}
	if s.started && s.loadErr == nil {
		if s.compiled.Explain != "" {
			text += " · " + s.compiled.Explain
		}
		if s.notMinionRow() > 0 {
			text += fmt.Sprintf(" · %d not a minion", len(s.notMinion))
		}
	}
	if s.env != nil && s.env.Theme != nil {
		text = s.env.Theme.Dim.Render(text)
	}
	return text
}

// crumbTarget names what is being applied.
func (s *SaltState) crumbTarget() string {
	if s.sls == "" {
		return "highstate"
	}
	return s.sls
}

// header is the column title row of the group list.
func (s *SaltState) header(w int) string {
	line := " " + " " +
		render.PadLeft("MINIONS", saltStateMinionsWidth) + " " +
		render.PadRight("SUMMARY", saltStateSummaryWidth) + " " +
		"FIRST CHANGE"
	line = render.Truncate(line, w)
	if s.env != nil && s.env.Theme != nil {
		line = s.env.Theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// rowLine renders one group row, or the "not a minion" row.
func (s *SaltState) rowLine(i, w int) string {
	if i < len(s.groups) {
		return s.groupLine(i, s.groups[i], w)
	}
	if s.notMinionRow() == 0 {
		return render.PadRight("", w)
	}
	line := " " + " " +
		render.PadLeft(strconv.Itoa(len(s.notMinion)), saltStateMinionsWidth) + " " +
		render.PadRight("not a minion", saltStateSummaryWidth) + " " +
		firstHost(s.notMinion)
	line = render.PadRight(render.Truncate(line, w), w)
	if s.env != nil && s.env.Theme != nil {
		if i == s.cursor {
			return s.env.Theme.Selected.Render(line)
		}
		return s.env.Theme.Warn.Render(line)
	}
	return line
}

// groupLine renders one group: the mark gutter, the minion count, the summary
// and the first changed state id.
func (s *SaltState) groupLine(i int, g salt.StateGroup, w int) string {
	line := s.markCell(g) + " " +
		render.PadLeft(strconv.Itoa(len(g.Minions)), saltStateMinionsWidth) + " " +
		render.PadRight(g.SummaryLine(), saltStateSummaryWidth) + " " +
		g.FirstChange()
	line = render.PadRight(render.Truncate(line, w), w)
	if s.env != nil && s.env.Theme != nil {
		if i == s.cursor && !s.members {
			return s.env.Theme.Selected.Render(line)
		}
		if g.Clean() {
			return s.env.Theme.OK.Render(line)
		}
		return s.env.Theme.Warn.Render(line)
	}
	return line
}

// markCell is the one-cell gutter: the mark glyph for a marked group.
func (s *SaltState) markCell(g salt.StateGroup) string {
	if s.marks[g.Key] {
		if s.glyphs.ASCII() {
			return ">"
		}
		return "▸"
	}
	return " "
}

// detailLines renders the group detail: per-state rows of a representative
// minion, then the member list.
func (s *SaltState) detailLines(w, h int) []string {
	g, ok := s.selectedGroup()
	if !ok {
		return []string{render.PadRight("", w)}
	}
	lines := []string{s.detailTitle(g, w)}
	rows := salt.RenderRows(g.Run)
	lines = append(lines, s.detailHeader(w))
	for _, row := range rows {
		lines = append(lines, s.detailRow(row, w))
	}
	if len(rows) == 0 {
		lines = append(lines, render.PadRight("(no states)", w))
	}
	lines = append(lines, "", render.PadRight(fmt.Sprintf("minions (%d) · space to mark, x for actions", len(g.Minions)), w))
	for _, m := range g.Minions {
		lines = append(lines, render.PadRight(m, w))
	}
	if len(lines) > h && h > 0 {
		lines = lines[:h]
	}
	return lines
}

// detailTitle is the group the detail belongs to.
func (s *SaltState) detailTitle(g salt.StateGroup, w int) string {
	text := fmt.Sprintf("%d minions · %s · %s", len(g.Minions), g.SummaryLine(), s.crumbTarget())
	if g.FirstChange() != "" {
		text += " · first change " + g.FirstChange()
	}
	text = render.Truncate(text, w)
	if s.env != nil && s.env.Theme != nil {
		text = s.env.Theme.Dim.Render(text)
	}
	return render.PadRight(text, w)
}

// detailHeader is the column title row of the per-state table.
func (s *SaltState) detailHeader(w int) string {
	line := render.Truncate("SLS        ID                   FUNCTION        RESULT        COMMENT", w)
	if s.env != nil && s.env.Theme != nil {
		line = s.env.Theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// detailRow renders one state row: sls, id, function, result and the first
// comment line.
func (s *SaltState) detailRow(cells []string, w int) string {
	line := render.Truncate(strings.Join(cells, "  "), w)
	line = render.PadRight(line, w)
	if s.env != nil && s.env.Theme != nil {
		if len(cells) >= 4 && (cells[3] == "failed" || cells[3] == "would change") {
			return s.env.Theme.Warn.Render(line)
		}
		return s.env.Theme.Base.Render(line)
	}
	return line
}

// crumb is "state <sls>" or "state highstate".
func (s *SaltState) Crumb() string { return "state " + s.crumbTarget() }

// Hints are the keys the view answers.
func (s *SaltState) Hints() []tui.KeyHint {
	if s.members {
		return []tui.KeyHint{
			{Key: firstKey(s.keys.Diff), Text: "diff"},
			{Key: "o", Text: "text"},
			{Key: "space", Text: "mark"},
			{Key: firstKey(s.keys.Actions), Text: "actions"},
			{Key: firstKey(s.keys.Back), Text: "groups"},
		}
	}
	return []tui.KeyHint{
		{Key: firstKey(s.keys.Open), Text: "states"},
		{Key: firstKey(s.keys.Diff), Text: "diff"},
		{Key: "o", Text: "text"},
		{Key: "space", Text: "mark"},
		{Key: firstKey(s.keys.Actions), Text: "actions"},
		{Key: firstKey(s.keys.Refresh), Text: "re-run"},
		{Key: firstKey(s.keys.Back), Text: "back"},
	}
}

// Subject is every minion the dry run covers.
func (s *SaltState) Subject() tui.Subject {
	hosts := make([]resource.HostID, 0, len(s.minions))
	for _, m := range s.minions {
		hosts = append(hosts, resource.HostID(m))
	}
	return tui.Subject{Hosts: hosts, Kind: salt.KindMinion}
}

// Capturing is always false: the state view has no input of its own.
func (s *SaltState) Capturing() bool { return false }

// CommandLine is the `:` command line that reopens this screen.
func (s *SaltState) CommandLine() string {
	if s.sls == "" {
		return "salt-state"
	}
	return "salt-state --arg sls=" + s.sls
}

// CopyLines returns the plain text of the view: the summary, then every
// group's minions and normalized text.
func (s *SaltState) CopyLines() []string {
	lines := []string{s.counts.Summary()}
	if s.compiled.Explain != "" {
		lines = append(lines, "target: "+s.compiled.Explain)
	}
	for _, g := range s.groups {
		lines = append(lines, fmt.Sprintf("%s  %d minions", g.SummaryLine(), len(g.Minions)))
		lines = append(lines, strings.Join(g.Minions, ", "))
		lines = append(lines, outputLines(g.Normalized())...)
	}
	if len(s.notMinion) > 0 {
		lines = append(lines, fmt.Sprintf("not a minion  %d hosts", len(s.notMinion)))
		for _, h := range s.notMinion {
			lines = append(lines, string(h))
		}
	}
	return lines
}

// firstHost is the first host of a list, for one-line rows.
func firstHost(hosts []resource.HostID) string {
	if len(hosts) == 0 {
		return ""
	}
	return string(hosts[0])
}
