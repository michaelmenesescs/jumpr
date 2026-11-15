package views

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
)

const (
	// maxTextLines caps a text view so a runaway stream cannot exhaust memory;
	// the oldest lines are dropped first.
	maxTextLines = 50000
	// textTabSize is the number of spaces a tab expands to on input.
	textTabSize = 4
	// textHStep is the number of cells one left/right key scrolls.
	textHStep = 8
	// textPageFallback is the page size before the first Render.
	textPageFallback = 10
)

// Text is a scrollable, searchable plain-text pane: describe output, unit
// files, timelines, help, and anything that is not a table. It is a tui.View;
// it never starts streams itself, the caller feeds it with SetLines, Append or
// a tui.LinesMsg.
type Text struct {
	env   *tui.Env
	crumb string
	keys  *keymap.Map

	lines []string // tab-expanded, may contain ANSI styling

	top  int  // first source line shown
	left int  // horizontal scroll offset in cells (wrapless only)
	wrap bool // wrap long lines instead of scrolling horizontally
	// follow pins the view to the bottom while lines are appended.
	follow bool

	// search state. While searching is true the view owns the keyboard and
	// every key edits the pattern.
	search    string
	searching bool
	matches   []int // source lines containing at least one match
	matchIdx  int

	seq int
	w   int
	h   int
}

var _ tui.View = (*Text)(nil)

// NewText builds a text view. Tabs in lines are expanded to four spaces.
func NewText(env *tui.Env, crumb string, lines []string) *Text {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	t := &Text{env: env, crumb: crumb, keys: keys}
	t.lines = normalizeLines(lines)
	if d := len(t.lines) - maxTextLines; d > 0 {
		t.lines = append(t.lines[:0], t.lines[d:]...)
	}
	return t
}

// NewKV builds a text view of aligned `key  value` lines, the key rendered in
// the theme's dim style. It is the detail pane for anything that is naturally
// a set of fields.
func NewKV(env *tui.Env, crumb string, pairs [][2]string) *Text {
	keys := make([]string, len(pairs))
	width := 0
	for i, p := range pairs {
		keys[i] = p[0]
		if w := render.Width(p[0]); w > width {
			width = w
		}
	}
	lines := make([]string, len(pairs))
	for i, p := range pairs {
		key := p[0] + strings.Repeat(" ", width-render.Width(p[0]))
		lines[i] = key
		if env != nil && env.Theme != nil {
			lines[i] = env.Theme.Dim.Render(key)
		}
		lines[i] += "  " + p[1]
	}
	return NewText(env, crumb, lines)
}

// normalizeLines expands tabs and copies the input so the caller cannot mutate
// the view's storage.
func normalizeLines(in []string) []string {
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = expandTabs(l)
	}
	return out
}

// expandTabs replaces every tab with textTabSize spaces.
func expandTabs(s string) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	return strings.ReplaceAll(s, "\t", strings.Repeat(" ", textTabSize))
}

// SetLines replaces the content and drops any scroll or search position that
// no longer exists.
func (t *Text) SetLines(lines []string) {
	t.lines = normalizeLines(lines)
	if d := len(t.lines) - maxTextLines; d > 0 {
		t.lines = append(t.lines[:0], t.lines[d:]...)
	}
	t.top = 0
	t.left = 0
	t.matches = nil
	t.matchIdx = 0
	if t.search != "" {
		t.rescan(0)
	}
}

// Append adds lines to the bottom, keeping the view pinned to the bottom when
// Follow is on and dropping the oldest lines past the cap.
func (t *Text) Append(lines ...string) {
	if len(lines) == 0 {
		return
	}
	start := len(t.lines)
	for _, l := range lines {
		t.lines = append(t.lines, expandTabs(l))
	}
	if t.search != "" {
		t.rescan(start)
	}
	t.trim()
}

// SetSeq sets the sequence number this view expects on tui.LinesMsg, so a
// stream started for an older view can never write into this one.
func (t *Text) SetSeq(seq int) { t.seq = seq }

// Seq returns the sequence number SetSeq installed.
func (t *Text) Seq() int { return t.seq }

// trim enforces the line cap by dropping the oldest lines and shifting every
// index that points into the document.
func (t *Text) trim() {
	d := len(t.lines) - maxTextLines
	if d <= 0 {
		return
	}
	t.lines = append(t.lines[:0], t.lines[d:]...)
	t.top -= d
	if t.top < 0 {
		t.top = 0
	}
	out := t.matches[:0]
	for _, m := range t.matches {
		if m -= d; m >= 0 {
			out = append(out, m)
		}
	}
	t.matches = out
	if t.matchIdx >= len(t.matches) {
		t.matchIdx = 0
	}
}

// Init does nothing: the caller owns the stream.
func (t *Text) Init() tea.Cmd { return nil }

// Update handles stream batches and keys.
func (t *Text) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case tui.LinesMsg:
		if m.Seq != t.seq {
			return t, nil
		}
		if len(m.Lines) > 0 {
			t.Append(m.Lines...)
		}
		if m.Closed && m.Err != nil {
			t.AppendError(string(errs.KindOf(m.Err)))
		}
		return t, nil
	case tea.KeyMsg:
		return t.handleKey(m)
	}
	return t, nil
}

// AppendError adds the final, Crit-styled line of a stream that failed.
func (t *Text) AppendError(kind string) {
	line := "stream ended: " + kind
	if t.env != nil && t.env.Theme != nil {
		line = t.env.Theme.Crit.Render(line)
	}
	t.Append(line)
}

// handleKey routes one key press. While searching, every key belongs to the
// pattern (j, k, q and : are text too).
func (t *Text) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	if t.searching {
		return t, t.searchKey(m)
	}

	key := m.String()
	switch {
	case t.keys.Filter.Matches(key):
		t.beginSearch()
		return t, nil
	case t.keys.Back.Matches(key):
		if t.search != "" {
			t.clearSearch()
			return t, nil
		}
		return t, tui.Pop()
	case t.keys.Follow.Matches(key):
		t.follow = !t.follow
		return t, nil
	case t.keys.Up.Matches(key):
		t.scroll(-1)
		return t, nil
	case t.keys.Down.Matches(key):
		t.scroll(1)
		return t, nil
	case t.keys.PageUp.Matches(key):
		t.scroll(-t.pageSize())
		return t, nil
	case t.keys.PageDown.Matches(key):
		t.scroll(t.pageSize())
		return t, nil
	case t.keys.Top.Matches(key):
		t.top = 0
		return t, nil
	case t.keys.Bottom.Matches(key):
		t.top = t.maxTop()
		return t, nil
	}

	switch key {
	case "left":
		if !t.wrap {
			t.left -= textHStep
			if t.left < 0 {
				t.left = 0
			}
		}
		return t, nil
	case "right":
		if !t.wrap {
			t.left += textHStep
		}
		return t, nil
	case "W":
		t.wrap = !t.wrap
		if t.wrap {
			t.left = 0
		}
		return t, nil
	case "n":
		if t.search != "" {
			t.jumpMatch(1)
		}
		return t, nil
	case "N":
		if t.search != "" {
			t.jumpMatch(-1)
		}
		return t, nil
	}
	return t, nil
}

// searchKey edits the search pattern, then commits it with Enter or throws it
// away with Esc.
func (t *Text) searchKey(m tea.KeyMsg) tea.Cmd {
	switch m.Type {
	case tea.KeyEnter:
		t.searching = false
		if t.search != "" {
			t.jumpFromTop()
		}
		return nil
	case tea.KeyEsc:
		t.searching = false
		t.clearSearch()
		return nil
	case tea.KeyBackspace:
		if r := []rune(t.search); len(r) > 0 {
			t.setSearch(string(r[:len(r)-1]))
		}
		return nil
	case tea.KeyCtrlU:
		t.setSearch("")
		return nil
	case tea.KeyRunes:
		t.setSearch(t.search + string(m.Runes))
		return nil
	}
	return nil
}

// beginSearch starts a fresh pattern capture.
func (t *Text) beginSearch() {
	t.searching = true
	t.search = ""
	t.matches = nil
	t.matchIdx = 0
}

// clearSearch drops the pattern and its highlights.
func (t *Text) clearSearch() {
	t.search = ""
	t.matches = nil
	t.matchIdx = 0
}

// setSearch installs a pattern and rebuilds the match list.
func (t *Text) setSearch(s string) {
	t.search = s
	t.matches = nil
	t.matchIdx = 0
	if s == "" {
		return
	}
	t.rescan(0)
	t.jumpFromTop()
}

// rescan records the lines containing the pattern, starting at line from. It
// is the incremental form used by Append.
func (t *Text) rescan(from int) {
	if from < 0 {
		from = 0
	}
	pat := []rune(strings.ToLower(t.search))
	for i := from; i < len(t.lines); i++ {
		if containsFold(t.plain(i), pat) {
			t.matches = append(t.matches, i)
		}
	}
}

// plain is the ANSI-free text of one source line.
func (t *Text) plain(i int) string {
	return ansi.Strip(t.lines[i])
}

// containsFold reports whether the lower-cased runes of s contain pat.
func containsFold(s string, pat []rune) bool {
	if len(pat) == 0 {
		return false
	}
	rs := []rune(s)
	for i := 0; i+len(pat) <= len(rs); i++ {
		if strings.EqualFold(string(rs[i:i+len(pat)]), string(pat)) {
			return true
		}
	}
	return false
}

// jumpFromTop selects the first match at or below the current top line, and
// scrolls to it when it is off screen.
func (t *Text) jumpFromTop() {
	if len(t.matches) == 0 {
		return
	}
	for i, m := range t.matches {
		if m >= t.top {
			t.matchIdx = i
			t.reveal(m)
			return
		}
	}
	t.matchIdx = 0
	t.reveal(t.matches[0])
}

// jumpMatch cycles through the matches in direction dir (wrapping), and
// scrolls to the selected one.
func (t *Text) jumpMatch(dir int) {
	if len(t.matches) == 0 {
		return
	}
	t.matchIdx = (t.matchIdx + dir + len(t.matches)) % len(t.matches)
	t.reveal(t.matches[t.matchIdx])
}

// reveal scrolls so that line is the top of the body.
func (t *Text) reveal(line int) {
	t.top = line
	if mx := t.maxTop(); t.top > mx {
		t.top = mx
	}
	if t.top < 0 {
		t.top = 0
	}
}

// scroll moves the top line by delta and clamps it.
func (t *Text) scroll(delta int) {
	t.top += delta
	if t.top < 0 {
		t.top = 0
	}
	if mx := t.maxTop(); t.top > mx {
		t.top = mx
	}
}

// bodyHeight is the number of content lines: everything but the footer.
func (t *Text) bodyHeight() int {
	if t.h > 1 {
		return t.h - 1
	}
	if t.h == 1 {
		return 0
	}
	return textPageFallback
}

// pageSize is the number of lines one page key moves.
func (t *Text) pageSize() int {
	if body := t.bodyHeight(); body > 0 {
		return body
	}
	return textPageFallback
}

// maxTop is the largest valid top line: the bottom of the document sits at the
// bottom of the body. In wrap mode a source line may span many rows, so the
// scroll position is still a source line and G stops on the last one.
func (t *Text) maxTop() int {
	if len(t.lines) == 0 {
		return 0
	}
	if t.wrap {
		return len(t.lines) - 1
	}
	if body := t.bodyHeight(); len(t.lines) > body {
		return len(t.lines) - body
	}
	return 0
}

// matchWord spells "match" for one and "matches" for the rest.
func matchWord(n int) string {
	if n == 1 {
		return "match"
	}
	return "matches"
}

// Render returns exactly h lines of exactly w cells: the visible lines and a
// footer describing the position and the search.
func (t *Text) Render(w, h int) string {
	t.w, t.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	body := h - 1
	if t.follow {
		t.top = t.maxTop()
	}
	if mx := t.maxTop(); t.top > mx {
		t.top = mx
	}
	if t.top < 0 {
		t.top = 0
	}

	rows := make([]string, 0, body)
	for i := t.top; i < len(t.lines) && len(rows) < body; i++ {
		line := t.renderLine(i)
		if !t.wrap {
			rows = append(rows, line)
			continue
		}
		for _, row := range strings.Split(ansi.Wrap(line, w, ""), "\n") {
			if len(rows) >= body {
				break
			}
			rows = append(rows, row)
		}
	}
	// Keep the footer on the last line even when the document is short.
	for len(rows) < body {
		rows = append(rows, "")
	}
	rows = append(rows, t.footer(w))
	return render.Fit(strings.Join(rows, "\n"), w, h)
}

// renderLine prepares one source line for display: search matches are
// highlighted first, then the horizontal offset is cut, both ANSI-aware.
func (t *Text) renderLine(i int) string {
	line := t.lines[i]
	if t.search != "" {
		line = t.highlight(line, t.search)
	}
	if !t.wrap && t.left > 0 {
		line = ansi.TruncateLeft(line, t.left, "")
	}
	return line
}

// highlight wraps every occurrence of pattern in the line with the selected
// style. Positions are computed on the plain text and applied with cell-based
// cuts, so escape sequences in the line survive.
func (t *Text) highlight(line, pattern string) string {
	if t.env == nil || t.env.Theme == nil {
		return line
	}
	plain := ansi.Strip(line)
	rs := []rune(plain)
	pr := []rune(strings.ToLower(pattern))
	if len(pr) == 0 || len(pr) > len(rs) {
		return line
	}

	type span struct{ start, end int }
	var spans []span
	for i := 0; i+len(pr) <= len(rs); {
		if strings.EqualFold(string(rs[i:i+len(pr)]), string(pr)) {
			spans = append(spans, span{i, i + len(pr)})
			i += len(pr)
			continue
		}
		i++
	}
	if len(spans) == 0 {
		return line
	}

	var b strings.Builder
	last := 0
	for _, s := range spans {
		cellStart := ansi.StringWidth(string(rs[:s.start]))
		cellEnd := ansi.StringWidth(string(rs[:s.end]))
		if cellStart > last {
			b.WriteString(ansi.Cut(line, last, cellStart))
		}
		b.WriteString(t.env.Theme.Selected.Render(ansi.Cut(line, cellStart, cellEnd)))
		last = cellEnd
	}
	if tok := ansi.StringWidth(plain); last < tok {
		b.WriteString(ansi.Cut(line, last, tok))
	}
	return b.String()
}

// footer renders the in-view status line:
// `line 120/3,400 · follow · /pattern (12 matches)`.
func (t *Text) footer(w int) string {
	var b strings.Builder
	if len(t.lines) == 0 {
		b.WriteString("line 0/0")
	} else {
		fmt.Fprintf(&b, "line %s/%s", render.Int(int64(t.top+1)), render.Int(int64(len(t.lines))))
	}
	if t.follow {
		b.WriteString(" · follow")
	}
	if t.wrap {
		b.WriteString(" · wrap")
	}
	if t.left > 0 {
		fmt.Fprintf(&b, " · +%d", t.left)
	}
	if t.search != "" {
		fmt.Fprintf(&b, " · /%s (%d %s)", t.search, len(t.matches), matchWord(len(t.matches)))
	}
	if t.env != nil && t.env.Theme != nil {
		return t.env.Theme.Dim.Render(render.Truncate(b.String(), w))
	}
	return render.Truncate(b.String(), w)
}

// Crumb is the caller-supplied breadcrumb text.
func (t *Text) Crumb() string { return t.crumb }

// Hints lists scrolling, search, follow, wrap and back.
func (t *Text) Hints() []tui.KeyHint {
	return []tui.KeyHint{
		{Key: firstKey(t.keys.Up) + "/" + firstKey(t.keys.Down), Text: "scroll"},
		{Key: firstKey(t.keys.Filter), Text: "search"},
		{Key: firstKey(t.keys.Follow), Text: "follow"},
		{Key: "W", Text: "wrap"},
		{Key: firstKey(t.keys.Back), Text: "back"},
	}
}

// Subject is empty: a text view is not about a resource.
func (t *Text) Subject() tui.Subject { return tui.Subject{} }

// Capturing is true while the search pattern owns the keyboard.
func (t *Text) Capturing() bool { return t.searching }

// CopyLines returns the full document as plain text, without styling.
func (t *Text) CopyLines() []string {
	out := make([]string, len(t.lines))
	for i, l := range t.lines {
		out[i] = ansi.Strip(l)
	}
	return out
}
