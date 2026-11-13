package tui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/investigation"
	"jumper/internal/render"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// The knowledge side panel answers "have I seen this before?" for whatever the
// top view is about (SPEC §10, task 060). It sits on the right of a view pane,
// 34 cells wide, when the pane interior is at least 90 wide and the user has
// not turned it off with K.
//
// The index is built once at startup in a tea.Cmd, never on the first frame,
// and matching runs in a tea.Cmd after a 150 ms debounce whenever the top
// view's Subject changes.
const (
	// panelWidth is the panel's width in cells, separator included.
	panelWidth = 34
	// panelMinWidth is the narrowest pane interior that still shows a panel.
	panelMinWidth = 90
	// panelDebounce is how long the panel waits for the subject to settle.
	panelDebounce = 150 * time.Millisecond
	// panelSnippetLines is how many wrapped lines a snippet may use.
	panelSnippetLines = 2
	// panelSeparator is the leading column that divides panel from view.
	panelSeparator = "│ "
)

// knowledgeRooter is implemented by a recorder that knows where the Markdown
// records live. internal/investigation.FileLog.Root is the root of both
// investigations/ and knowledge/, so one value serves the panel and `:kb`.
type knowledgeRooter interface{ Root() string }

// knowledgeRoot is the session's knowledge root, or "" when no recorder is
// wired (a demo, or a unit test without one).
func knowledgeRoot(env *Env) string {
	if env == nil || env.Recorder == nil {
		return ""
	}
	r, ok := env.Recorder.(knowledgeRooter)
	if !ok {
		return ""
	}
	return strings.TrimSpace(r.Root())
}

// knowledgeSubject converts the UI's Subject into the index's. A row that
// names a unit or alert kind lends its name to the matching field, because the
// table fills Ref rather than Unit.
func knowledgeSubject(s Subject) investigation.Subject {
	hosts := make([]string, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		if h != "" {
			hosts = append(hosts, string(h))
		}
	}
	out := investigation.Subject{Hosts: hosts, Unit: s.Unit, Alert: s.Alert}
	if s.Ref.Name != "" {
		kind := strings.ToLower(string(s.Ref.Kind))
		if out.Unit == "" && strings.Contains(kind, "unit") {
			out.Unit = s.Ref.Name
		}
		if out.Alert == "" && strings.Contains(kind, "alert") {
			out.Alert = s.Ref.Name
		}
	}
	return out
}

// sameSubject reports whether two index subjects ask the same question.
func sameSubject(a, b investigation.Subject) bool {
	if a.Unit != b.Unit || a.Alert != b.Alert {
		return false
	}
	if len(a.Hosts) != len(b.Hosts) || len(a.Tags) != len(b.Tags) {
		return false
	}
	for i := range a.Hosts {
		if a.Hosts[i] != b.Hosts[i] {
			return false
		}
	}
	for i := range a.Tags {
		if a.Tags[i] != b.Tags[i] {
			return false
		}
	}
	return true
}

// emptySubject reports whether s asks nothing.
func emptySubject(s investigation.Subject) bool {
	return len(s.Hosts) == 0 && s.Unit == "" && s.Alert == "" && len(s.Tags) == 0
}

// Messages of the knowledge panel. Index building and matching run in commands,
// so each result comes back as a message like every other stream.

type knowledgeIndexMsg struct {
	IX  *investigation.Index
	Err error
}

type knowledgeDebounceMsg struct {
	Seq     int
	Subject investigation.Subject
}

type knowledgeMatchMsg struct {
	Seq  int
	Hits []investigation.Hit
}

// knowledgeReindexMsg asks every pane to rebuild its index. `:kb reindex`
// returns it and the root broadcasts it.
type knowledgeReindexMsg struct{}

// knowledgePathMsg is the result of re-indexing one file after an append.
type knowledgePathMsg struct {
	Path string
	Err  error
}

// knowledgeNoteMsg is the result of filing a root cause into a note.
type knowledgeNoteMsg struct {
	Path string
	Err  error
}

// sidePanel is one pane's knowledge panel: the hits for the current subject,
// the cursor into them, and the focus flag.
type sidePanel struct {
	env  *Env
	root string

	ix      *investigation.Index
	hits    []investigation.Hit
	cursor  int
	focused bool
	loading bool
	err     error

	subject investigation.Subject
	seq     int
}

// newSidePanel builds a panel over root's knowledge base. When internal/app
// already built the index at startup the panel starts from it; a reindex still
// rebuilds from disk.
func newSidePanel(env *Env, root string) *sidePanel {
	p := &sidePanel{env: env, root: root, loading: true}
	if env != nil && env.Knowledge != nil {
		p.ix = env.Knowledge
		p.loading = false
	}
	return p
}

// Init starts the index build.
func (p *sidePanel) Init() tea.Cmd { return p.buildCmd() }

// buildCmd reads the whole knowledge base off the UI goroutine.
func (p *sidePanel) buildCmd() tea.Cmd {
	root := p.root
	return func() tea.Msg {
		ix, err := investigation.BuildIndex(root)
		return knowledgeIndexMsg{IX: ix, Err: err}
	}
}

// handleMsg handles the panel's own messages. handled is true when msg
// belonged to the panel and must not reach the view stack.
func (p *sidePanel) handleMsg(msg tea.Msg) (cmd tea.Cmd, handled bool) {
	if p == nil {
		return nil, false
	}
	switch m := msg.(type) {
	case knowledgeReindexMsg:
		p.loading = true
		return p.buildCmd(), true
	case knowledgeIndexMsg:
		p.ix = m.IX
		p.err = m.Err
		p.loading = false
		if emptySubject(p.subject) {
			return nil, true
		}
		return p.matchCmd(p.subject), true
	case knowledgeDebounceMsg:
		if m.Seq != p.seq {
			return nil, true
		}
		return p.matchCmd(m.Subject), true
	case knowledgeMatchMsg:
		if m.Seq != p.seq {
			return nil, true
		}
		p.hits = m.Hits
		p.clamp()
		return nil, true
	case knowledgePathMsg:
		p.err = m.Err
		return nil, true
	case knowledgeNoteMsg:
		return p.savedCmd(m), true
	}
	return nil, false
}

// savedCmd reports a filed root cause and re-indexes the note it landed in.
func (p *sidePanel) savedCmd(m knowledgeNoteMsg) tea.Cmd {
	if m.Err != nil {
		return Flash(FlashError, "knowledge: %v", m.Err)
	}
	cmds := []tea.Cmd{Flash(FlashOK, "knowledge: %s", p.relPath(m.Path))}
	if up := p.updateCmd(m.Path); up != nil {
		cmds = append(cmds, up)
	}
	return tea.Batch(cmds...)
}

// relPath renders path relative to the knowledge root.
func (p *sidePanel) relPath(path string) string {
	if rel, err := filepath.Rel(p.root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}

// rememberCmd offers to file an investigation's root cause in a knowledge note:
// the alert's note when the investigation has an alert, else the unit's. It
// returns nil when there is nothing to attach it to or no knowledge base.
func (p *ViewPane) rememberCmd(m knowledgeRememberMsg) tea.Cmd {
	root := knowledgeRoot(p.env)
	cause := strings.TrimSpace(m.Cause)
	if root == "" || cause == "" {
		return nil
	}
	kind, name := "", ""
	if len(m.Alerts) > 0 {
		kind, name = investigation.NoteKindAlert, strings.TrimSpace(m.Alerts[0])
	}
	if kind == "" {
		if subj := p.Subject(); subj.Unit != "" {
			kind = investigation.NoteKindService
			name = strings.TrimSuffix(strings.TrimSpace(subj.Unit), ".service")
		}
	}
	if kind == "" || name == "" {
		return nil
	}
	path, err := investigation.NotePath(root, kind, name)
	if err != nil {
		return nil
	}
	if p.env != nil && p.env.Redact != nil {
		cause = p.env.Redact.String(cause)
	}
	rel := relToRoot(root, path)
	yes := func() tea.Msg {
		saved, rerr := investigation.RecordRootCause(root, kind, name, cause)
		return knowledgeNoteMsg{Path: saved, Err: rerr}
	}
	dlg := NewConfirmDialog(themeOf(p.env), "remember this?", "append the root cause to "+rel+"?", yes)
	return func() tea.Msg { return OpenDialogMsg{Dialog: dlg} }
}

// relToRoot renders path relative to root, or path itself when they share
// nothing.
func relToRoot(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}

// trackSubject remembers the top view's subject and, when it changed, returns
// the debounce command that ends in a match.
func (p *sidePanel) trackSubject(s investigation.Subject) tea.Cmd {
	if p == nil {
		return nil
	}
	if sameSubject(s, p.subject) {
		return nil
	}
	p.subject = s
	p.seq++
	seq := p.seq
	if emptySubject(s) {
		p.hits = nil
		p.cursor = 0
		return nil
	}
	return tea.Tick(panelDebounce, func(time.Time) tea.Msg {
		return knowledgeDebounceMsg{Seq: seq, Subject: s}
	})
}

// matchCmd runs the match off the UI goroutine.
func (p *sidePanel) matchCmd(s investigation.Subject) tea.Cmd {
	ix := p.ix
	if ix == nil {
		return nil
	}
	seq := p.seq
	return func() tea.Msg { return knowledgeMatchMsg{Seq: seq, Hits: ix.Match(s)} }
}

// updateCmd re-reads one file after an append or a resolve.
func (p *sidePanel) updateCmd(path string) tea.Cmd {
	ix := p.ix
	if ix == nil || path == "" {
		return nil
	}
	return func() tea.Msg { return knowledgePathMsg{Path: path, Err: ix.Update(path)} }
}

// handleKey handles one key while the panel has focus. handled is true when
// the key belonged to the panel; the panel consumes every key while focused,
// so a stray letter cannot reach the view underneath.
func (p *sidePanel) handleKey(k tea.KeyMsg) (cmd tea.Cmd, handled bool) {
	if p == nil || !p.focused {
		return nil, false
	}
	key := k.String()
	if key == "tab" || key == "esc" {
		p.focused = false
		return nil, true
	}
	keys := keymap.Default()
	if p.env != nil && p.env.Keys != nil {
		keys = p.env.Keys
	}
	switch {
	case keys.Up.Matches(key):
		p.cursor--
		p.clamp()
	case keys.Down.Matches(key):
		p.cursor++
		p.clamp()
	case keys.Top.Matches(key):
		p.cursor = 0
	case keys.Bottom.Matches(key):
		p.cursor = len(p.hits) - 1
		p.clamp()
	case keys.Open.Matches(key):
		return p.openCmd(), true
	}
	return nil, true
}

// openCmd opens the note under the cursor in a text view.
func (p *sidePanel) openCmd() tea.Cmd {
	if p.cursor < 0 || p.cursor >= len(p.hits) {
		return nil
	}
	hit := p.hits[p.cursor]
	env := p.env
	path := hit.Path
	return func() tea.Msg {
		return PushViewMsg{View: newNoteView(env, path, "")}
	}
}

// clamp keeps the cursor on a hit.
func (p *sidePanel) clamp() {
	if len(p.hits) == 0 {
		p.cursor = 0
		return
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(p.hits) {
		p.cursor = len(p.hits) - 1
	}
}

// title is the panel heading: Knowledge (N).
func (p *sidePanel) title() string {
	return "Knowledge (" + strconv.Itoa(len(p.hits)) + ")"
}

// styles returns the theme, or nil when the panel has none.
func (p *sidePanel) theme() *theme.Theme {
	if p.env == nil {
		return nil
	}
	return p.env.Theme
}

// Render draws the panel as exactly h lines of exactly w cells, the separator
// column included.
func (p *sidePanel) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	content := w - render.Width(panelSeparator)
	if content < 1 {
		content = 1
	}
	t := p.theme()

	lines := make([]string, 0, h)
	title := p.title()
	if t != nil {
		title = t.Header.Render(title)
	}
	lines = append(lines, title)

	switch {
	case p.err != nil:
		lines = append(lines, "", dimStyle(t, p.err.Error()))
	case p.loading:
		lines = append(lines, "", dimStyle(t, "indexing"+string(glyphsFor(p.env).Ellipsis)))
	case len(p.hits) == 0:
		lines = append(lines, "", dimStyle(t, "nothing about this yet"))
	default:
		lines = append(lines, "")
		for i, hit := range p.hits {
			lines = append(lines, p.hitLines(t, i, hit, content)...)
		}
	}

	body := render.Fit(strings.Join(lines, "\n"), content, h)
	rows := strings.Split(body, "\n")
	for i, row := range rows {
		rows[i] = panelSeparator + row
	}
	return strings.Join(rows, "\n")
}

// hitLines renders one hit: the path in the accent colour, the reason dim, and
// the snippet wrapped over at most two lines.
func (p *sidePanel) hitLines(t *theme.Theme, i int, hit investigation.Hit, width int) []string {
	path := hit.Path
	if rel, err := filepath.Rel(p.root, path); err == nil && !strings.HasPrefix(rel, "..") {
		path = filepath.ToSlash(rel)
	}
	if t != nil {
		path = t.Accent.Render(path)
	}
	why := hit.Why
	if hit.Investigation && hit.Status != "" {
		why += " · " + hit.Status
	}
	why = dimStyle(t, why)

	out := []string{path, why}
	for _, ln := range wrapText(hit.Snippet, width, panelSnippetLines) {
		out = append(out, ln)
	}
	if p.focused && i == p.cursor && t != nil {
		for j := range out {
			out[j] = t.Selected.Render(out[j])
		}
	}
	return out
}

// dimStyle renders text in the dim style when there is a theme.
func dimStyle(t *theme.Theme, text string) string {
	if t == nil {
		return text
	}
	return t.Dim.Render(text)
}

// wrapText wraps s to at most width cells per line, keeping at most max lines;
// the last line ends in an ellipsis when text is left over.
func wrapText(s string, width, max int) []string {
	if width < 1 || max < 1 {
		return nil
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	cur := ""
	cut := false
	for _, word := range words {
		if cur == "" {
			cur = word
			continue
		}
		if render.Width(cur)+1+render.Width(word) <= width {
			cur += " " + word
			continue
		}
		if len(out) == max-1 {
			cut = true
			break
		}
		out = append(out, render.Truncate(cur, width))
		cur = word
	}
	switch {
	case cur == "":
	case len(out) == max:
		cut = true
	default:
		out = append(out, render.Truncate(cur, width))
	}
	if cut && len(out) > 0 {
		out[len(out)-1] = withEllipsis(out[len(out)-1], width)
	}
	return out
}

// withEllipsis ends s in an ellipsis inside width cells.
func withEllipsis(s string, width int) string {
	s = strings.TrimSuffix(s, "…")
	if render.Width(s)+1 > width {
		if width < 2 {
			return render.Truncate("…", width)
		}
		s = render.Truncate(s, width-1)
	}
	return s + "…"
}

// noteLinesMsg is the loaded content of a note.
type noteLinesMsg struct {
	Path  string
	Lines []string
	Err   error
}

// noteView is the read-only text view a knowledge note opens in. It exists
// here rather than in internal/tui/views because that package imports this
// one; it is the same text-pane behaviour (scroll, esc back) in miniature, and
// editing stays in the user's editor outside jumper.
type noteView struct {
	env   *Env
	path  string
	crumb string

	lines  []string
	err    error
	top    int
	flash  string
	loaded bool
}

var _ View = (*noteView)(nil)

// newNoteView builds the view for one note file. flash, when set, is shown
// once the file has been read.
func newNoteView(env *Env, path, flash string) *noteView {
	return &noteView{
		env:   env,
		path:  path,
		crumb: noteCrumb(env, path),
		flash: flash,
	}
}

// noteCrumb is the note's path relative to the knowledge root, or its base
// name when they share nothing.
func noteCrumb(env *Env, path string) string {
	if root := knowledgeRoot(env); root != "" {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
}

// Init reads the file and reports the path: the flash says whether the note was
// created or opened, and re-indexing keeps the panel in step.
func (v *noteView) Init() tea.Cmd {
	path := v.path
	load := func() tea.Msg {
		data, err := os.ReadFile(path)
		if err != nil {
			return noteLinesMsg{Path: path, Err: err}
		}
		return noteLinesMsg{Path: path, Lines: strings.Split(strings.TrimRight(string(data), "\n"), "\n")}
	}
	cmds := []tea.Cmd{load, func() tea.Msg { return knowledgePathMsg{Path: path} }}
	if v.flash != "" {
		cmds = append(cmds, Flash(FlashInfo, "%s", v.flash))
	}
	return tea.Batch(cmds...)
}

// Update stores the loaded lines and scrolls.
func (v *noteView) Update(msg tea.Msg) (View, tea.Cmd) {
	switch m := msg.(type) {
	case noteLinesMsg:
		if m.Path != v.path {
			return v, nil
		}
		v.loaded = true
		v.err = m.Err
		v.lines = m.Lines
		return v, nil
	case tea.KeyMsg:
		return v, v.handleKey(m)
	}
	return v, nil
}

// handleKey scrolls the note and pops on esc.
func (v *noteView) handleKey(k tea.KeyMsg) tea.Cmd {
	keys := keymap.Default()
	if v.env != nil && v.env.Keys != nil {
		keys = v.env.Keys
	}
	key := k.String()
	switch {
	case keys.Up.Matches(key):
		v.top--
	case keys.Down.Matches(key):
		v.top++
	case keys.PageUp.Matches(key):
		v.top -= 10
	case keys.PageDown.Matches(key):
		v.top += 10
	case keys.Top.Matches(key):
		v.top = 0
	case keys.Bottom.Matches(key):
		v.top = len(v.lines)
	case keys.Back.Matches(key):
		return Pop()
	}
	if maxTop := len(v.lines) - 1; v.top > maxTop {
		v.top = maxTop
	}
	if v.top < 0 {
		v.top = 0
	}
	return nil
}

// Render draws the visible part of the note.
func (v *noteView) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	switch {
	case !v.loaded:
		return render.Fit("loading", w, h)
	case v.err != nil:
		return render.Fit(v.err.Error(), w, h)
	case len(v.lines) == 0:
		return render.Fit("(empty note)", w, h)
	}
	visible := v.lines
	if v.top > 0 && v.top < len(visible) {
		visible = visible[v.top:]
	}
	return render.Fit(strings.Join(visible, "\n"), w, h)
}

// Crumb is the note's path.
func (v *noteView) Crumb() string { return v.crumb }

// Hints lists the scrolling keys.
func (v *noteView) Hints() []KeyHint {
	return []KeyHint{
		{Key: "j/k", Text: "scroll"},
		{Key: "esc", Text: "back"},
	}
}

// Subject is empty: a note is not about one host or unit.
func (v *noteView) Subject() Subject { return Subject{} }

// Capturing is false: the note view owns no text input.
func (v *noteView) Capturing() bool { return false }

// CopyLines is the note's plain text.
func (v *noteView) CopyLines() []string { return v.lines }
