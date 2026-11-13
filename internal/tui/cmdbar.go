package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"sitrep/internal/render"
)

const (
	// cmdBarHistoryMax is the number of remembered command lines.
	cmdBarHistoryMax = 200
	// cmdBarGap separates the input from the completion candidates.
	cmdBarGap = "  "
)

// CmdBar is the one-line `:` prompt. It owns its history in memory; task 024
// persists it through History and SetHistory.
type CmdBar struct {
	env  *Env
	cmds *Commands

	input  textinput.Model
	inited bool
	active bool

	history []string
	histPos int    // len(history) while editing a fresh line
	draft   string // the fresh line, saved while walking back through history

	matches     []string // candidates from the last tab
	matchIdx    int
	matchBase   string // text before the completed word
	matchSuffix string // appended after the candidate (a space ends the first word)
	matchValue  string // input value after the last completion
}

// NewCmdBar builds a command bar completing against cmds and env's registry.
// Both may be nil; the bar then completes nothing but still runs history.
func NewCmdBar(env *Env, cmds *Commands) *CmdBar {
	b := &CmdBar{env: env, cmds: cmds}
	b.init()
	return b
}

func (b *CmdBar) init() {
	if b.inited {
		return
	}
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = ""
	b.input = in
	b.inited = true
	b.histPos = len(b.history)
}

// Open shows the bar with prefill already typed.
func (b *CmdBar) Open(prefill string) {
	b.init()
	b.input.Reset()
	b.input.SetValue(prefill)
	b.input.CursorEnd()
	b.input.Focus()
	b.active = true
	b.histPos = len(b.history)
	b.draft = ""
	b.clearMatches()
}

// Active reports whether the bar owns the keyboard.
func (b *CmdBar) Active() bool { return b.active }

// History returns a copy of the in-memory history, oldest first.
func (b *CmdBar) History() []string {
	return append([]string(nil), b.history...)
}

// SetHistory replaces the history, keeping at most the newest entries.
func (b *CmdBar) SetHistory(h []string) {
	b.history = append([]string(nil), h...)
	if len(b.history) > cmdBarHistoryMax {
		b.history = b.history[len(b.history)-cmdBarHistoryMax:]
	}
	b.histPos = len(b.history)
}

// Update handles one key while the bar is open. done reports that the bar
// closed; cmd is the RunCommandMsg emitter for enter.
func (b *CmdBar) Update(msg tea.KeyMsg) (cmd tea.Cmd, done bool) {
	if !b.active {
		return nil, false
	}
	switch msg.Type { //nolint:exhaustive // every other key goes to the input
	case tea.KeyEnter:
		line := strings.TrimSpace(b.input.Value())
		b.close()
		if line == "" {
			return nil, true
		}
		b.pushHistory(line)
		return func() tea.Msg { return RunCommandMsg{Line: line} }, true
	case tea.KeyEsc:
		b.close()
		return nil, true
	case tea.KeyUp:
		b.historyPrev()
		return nil, false
	case tea.KeyDown:
		b.historyNext()
		return nil, false
	case tea.KeyTab:
		b.complete()
		return nil, false
	}
	b.clearMatches()
	var next tea.Cmd
	b.input, next = b.input.Update(msg)
	return next, false
}

func (b *CmdBar) close() {
	b.active = false
	b.input.Blur()
	b.clearMatches()
}

// View renders exactly w cells: ":" + the input, followed by the completion
// candidates while more than one is shown.
func (b *CmdBar) View(w int) string {
	if w <= 0 {
		return ""
	}
	if !b.active {
		return strings.Repeat(" ", w)
	}
	var text string
	if len(b.matches) > 1 {
		text = strings.Join(b.matches, cmdBarGap)
		if b.env != nil && b.env.Theme != nil {
			text = b.env.Theme.Dim.Render(text)
		}
	}
	// The input scrolls inside the space the candidates leave it, so the
	// candidate list is never pushed off the line.
	width := w - 1
	if text != "" {
		width -= len(cmdBarGap) + render.Width(text)
	}
	b.input.Width = max(1, width)
	line := ":" + render.Truncate(b.input.View(), max(1, width))
	if text != "" {
		line += cmdBarGap + text
	}
	return render.PadRight(render.Truncate(line, w), w)
}

// pushHistory records a submitted line, skipping consecutive duplicates and
// keeping the newest cmdBarHistoryMax entries.
func (b *CmdBar) pushHistory(line string) {
	if n := len(b.history); n > 0 && b.history[n-1] == line {
		return
	}
	b.history = append(b.history, line)
	if len(b.history) > cmdBarHistoryMax {
		b.history = append([]string(nil), b.history[len(b.history)-cmdBarHistoryMax:]...)
	}
	b.histPos = len(b.history)
}

func (b *CmdBar) historyPrev() {
	if len(b.history) == 0 || b.histPos == 0 {
		return
	}
	if b.histPos == len(b.history) {
		b.draft = b.input.Value()
	}
	b.histPos--
	b.setValue(b.history[b.histPos])
}

func (b *CmdBar) historyNext() {
	if len(b.history) == 0 || b.histPos >= len(b.history) {
		return
	}
	b.histPos++
	if b.histPos == len(b.history) {
		b.setValue(b.draft)
		return
	}
	b.setValue(b.history[b.histPos])
}

func (b *CmdBar) setValue(s string) {
	b.input.SetValue(s)
	b.input.CursorEnd()
	b.clearMatches()
}

func (b *CmdBar) clearMatches() {
	b.matches = nil
	b.matchIdx = 0
	b.matchBase = ""
	b.matchSuffix = ""
	b.matchValue = ""
}

// complete completes the word under the cursor: the first word against command
// names and kind names (plus aliases), later words through Command.Complete. A
// single candidate is inserted; several are listed after the input and further
// tabs cycle through them.
func (b *CmdBar) complete() {
	value := b.input.Value()
	if b.matches != nil && value == b.matchValue {
		b.matchIdx = (b.matchIdx + 1) % len(b.matches)
		b.apply(b.matchBase, b.matches[b.matchIdx], b.matchSuffix)
		return
	}
	cands := b.candidates(value)
	if len(cands) == 0 {
		b.clearMatches()
		return
	}
	partial := partialWord(value)
	base := value[:len(value)-len(partial)]
	suffix := ""
	if !strings.Contains(value, " ") {
		suffix = " " // a completed command word is followed by its arguments
	}
	b.matches = cands
	b.matchIdx = 0
	b.matchBase = base
	b.matchSuffix = suffix
	b.apply(base, cands[0], suffix)
}

func (b *CmdBar) apply(base, candidate, suffix string) {
	b.matchValue = base + candidate + suffix
	b.input.SetValue(b.matchValue)
	b.input.CursorEnd()
}

// candidates returns the completion candidates for value, in stable order.
func (b *CmdBar) candidates(value string) []string {
	partial := partialWord(value)
	if i := strings.IndexAny(value, " \t"); i >= 0 {
		cmd, ok := b.cmds.Lookup(value[:i])
		if !ok || cmd.Complete == nil {
			return nil
		}
		return filterPrefix(cmd.Complete(b.env, strings.TrimSpace(value[i+1:])), partial)
	}

	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		key := strings.ToLower(s)
		if s == "" || seen[key] || !hasPrefixFold(s, partial) {
			return
		}
		seen[key] = true
		out = append(out, s)
	}
	if b.cmds != nil {
		for _, c := range b.cmds.All() {
			add(c.Name)
			for _, a := range c.Aliases {
				add(a)
			}
		}
	}
	if b.env != nil && b.env.Registry != nil {
		for _, k := range b.env.Registry.Kinds() {
			add(string(k.Name))
			for _, a := range k.Aliases {
				add(a)
			}
		}
	}
	sort.Strings(out)
	return out
}

// partialWord returns the last space- or tab-separated word of v.
func partialWord(v string) string {
	if i := strings.LastIndexAny(v, " \t"); i >= 0 {
		return v[i+1:]
	}
	return v
}

func filterPrefix(cands []string, partial string) []string {
	out := make([]string, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		key := strings.ToLower(c)
		if !hasPrefixFold(c, partial) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

func hasPrefixFold(s, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
}
