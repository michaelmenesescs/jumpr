package views

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/tui"
)

// newText builds a text view over the given lines with the test environment.
func newText(lines ...string) *Text {
	return NewText(testEnv(), "text", lines)
}

// numbered returns n lines "line 001", "line 002", …
func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %03d", i+1)
	}
	return out
}

// renderText renders and splits the frame into its lines.
func renderText(t *testing.T, txt *Text, w, h int) []string {
	t.Helper()
	frame := txt.Render(w, h)
	lines := strings.Split(frame, "\n")
	if len(lines) != h {
		t.Fatalf("Render(%d,%d) returned %d lines, want %d", w, h, len(lines), h)
	}
	for i, l := range lines {
		if got := render.Width(l); got != w {
			t.Fatalf("Render(%d,%d) line %d is %d cells wide, want %d", w, h, i, got, w)
		}
	}
	return lines
}

// sendKey delivers one Bubble Tea key string to the view.
func sendKey(t *testing.T, txt *Text, key string) *Text {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		msg = tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		msg = tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+d":
		msg = tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+u":
		msg = tea.KeyMsg{Type: tea.KeyCtrlU}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	view, _ := txt.Update(msg)
	next, ok := view.(*Text)
	if !ok {
		t.Fatalf("Update returned %T, want *Text", view)
	}
	return next
}

// typeText sends every rune of s as one key message each.
func typeText(t *testing.T, txt *Text, s string) *Text {
	t.Helper()
	for _, r := range s {
		txt = sendKey(t, txt, string(r))
	}
	return txt
}

func TestTextRenderSize(t *testing.T) {
	txt := newText(numbered(100)...)
	lines := renderText(t, txt, 40, 10)

	if !strings.Contains(lines[0], "line 001") {
		t.Fatalf("first body line = %q, want line 001", lines[0])
	}
	if !strings.Contains(lines[8], "line 009") {
		t.Fatalf("ninth body line = %q, want line 009", lines[8])
	}
	if !strings.Contains(lines[9], "line 1/100") {
		t.Fatalf("footer = %q, want the position", lines[9])
	}

	// A frame smaller than the content and one taller than it both keep the
	// exact-size guarantee.
	renderText(t, txt, 1, 1)
	renderText(t, txt, 200, 200)

	// Empty documents render too.
	empty := newText()
	el := renderText(t, empty, 20, 3)
	if !strings.Contains(el[2], "line 0/0") {
		t.Fatalf("empty footer = %q, want line 0/0", el[2])
	}
}

func TestTextScrollBounds(t *testing.T) {
	txt := newText(numbered(100)...)
	renderText(t, txt, 20, 10) // body = 9 lines

	sendKey(t, txt, "up")
	if txt.top != 0 {
		t.Fatalf("top after up at the start = %d, want 0", txt.top)
	}

	txt = sendKey(t, txt, "G")
	if want := 100 - 9; txt.top != want {
		t.Fatalf("top after bottom = %d, want %d", txt.top, want)
	}
	lines := renderText(t, txt, 20, 10)
	if !strings.Contains(lines[8], "line 100") {
		t.Fatalf("last body line = %q, want line 100", lines[8])
	}
	sendKey(t, txt, "down")
	if txt.top != 91 {
		t.Fatalf("top after down at the end = %d, want 91", txt.top)
	}

	txt = sendKey(t, txt, "g")
	if txt.top != 0 {
		t.Fatalf("top after top = %d, want 0", txt.top)
	}

	sendKey(t, txt, "ctrl+d") // page down
	if txt.top != 9 {
		t.Fatalf("top after page down = %d, want 9", txt.top)
	}
	txt = sendKey(t, txt, "ctrl+u") // page up
	if txt.top != 0 {
		t.Fatalf("top after page up = %d, want 0", txt.top)
	}

	// A document shorter than the body never scrolls.
	short := newText(numbered(3)...)
	renderText(t, short, 20, 10)
	short = sendKey(t, short, "G")
	if short.top != 0 {
		t.Fatalf("short document top = %d, want 0", short.top)
	}
}

func TestTextSearchNextPrev(t *testing.T) {
	lines := numbered(20)
	lines[2] = "foo here" // line 3
	lines[7] = "also foo" // line 8
	txt := newText(lines...)
	renderText(t, txt, 30, 10)

	txt = sendKey(t, txt, "/")
	if !txt.Capturing() {
		t.Fatal("search should capture the keyboard")
	}
	txt = typeText(t, txt, "foo")
	if len(txt.matches) != 2 {
		t.Fatalf("matches = %v, want 2", txt.matches)
	}
	txt = sendKey(t, txt, "enter")
	if txt.Capturing() {
		t.Fatal("enter should end capture")
	}
	if txt.top != 2 {
		t.Fatalf("top after enter = %d, want the first match at 2", txt.top)
	}
	frame := strings.Join(renderText(t, txt, 30, 10), "\n")
	if !strings.Contains(frame, "(2 matches)") {
		t.Fatalf("footer = %q, want (2 matches)", frame)
	}

	txt = sendKey(t, txt, "n")
	if txt.top != 7 {
		t.Fatalf("top after n = %d, want 7", txt.top)
	}
	txt = sendKey(t, txt, "N")
	if txt.top != 2 {
		t.Fatalf("top after N = %d, want 2", txt.top)
	}

	// n wraps forward, N wraps back.
	txt = sendKey(t, txt, "n")
	txt = sendKey(t, txt, "n")
	if txt.top != 2 {
		t.Fatalf("n should wrap to the first match, top = %d", txt.top)
	}
	txt = sendKey(t, txt, "N")
	if txt.top != 7 {
		t.Fatalf("N should wrap to the last match, top = %d", txt.top)
	}

	// Esc clears the search; a second Esc pops.
	view, cmd := txt.Update(tea.KeyMsg{Type: tea.KeyEsc})
	txt = view.(*Text)
	if txt.search != "" || len(txt.matches) != 0 {
		t.Fatalf("esc left search %q with %d matches", txt.search, len(txt.matches))
	}
	if cmd != nil {
		t.Fatal("clearing the search should not pop")
	}
	_, cmd = txt.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc with no search should pop")
	}
	if _, ok := cmd().(tui.PopViewMsg); !ok {
		t.Fatalf("esc emitted %T, want PopViewMsg", cmd())
	}

	// A pattern that matches nothing keeps the view where it is.
	txt = sendKey(t, txt, "/")
	txt = typeText(t, txt, "zzz")
	txt = sendKey(t, txt, "enter")
	if len(txt.matches) != 0 {
		t.Fatalf("matches = %v, want none", txt.matches)
	}
}

func TestTextFollowPinsBottom(t *testing.T) {
	txt := NewText(testEnv(), "logs", numbered(50))
	renderText(t, txt, 20, 10)
	if txt.top != 0 {
		t.Fatalf("top before follow = %d, want 0", txt.top)
	}

	txt = sendKey(t, txt, "w")
	if !txt.follow {
		t.Fatal("w should enable follow")
	}
	txt.Append("line 051", "line 052")
	lines := renderText(t, txt, 20, 10)
	if want := 52 - 9; txt.top != want {
		t.Fatalf("top with follow = %d, want %d", txt.top, want)
	}
	if !strings.Contains(lines[8], "line 052") {
		t.Fatalf("last body line = %q, want line 052", lines[8])
	}
	if !strings.Contains(lines[9], "follow") {
		t.Fatalf("footer = %q, want follow", lines[9])
	}

	// Scrolling away is undone on the next append while follow is on.
	txt = sendKey(t, txt, "g")
	txt.Append("line 053")
	lines = renderText(t, txt, 20, 10)
	if !strings.Contains(lines[8], "line 053") {
		t.Fatalf("last body line after append = %q, want line 053", lines[8])
	}

	txt = sendKey(t, txt, "w")
	if txt.follow {
		t.Fatal("w should disable follow")
	}
}

func TestTextCapsLines(t *testing.T) {
	txt := NewText(testEnv(), "logs", numbered(maxTextLines+5))
	if got := len(txt.lines); got != maxTextLines {
		t.Fatalf("len(lines) = %d, want %d", got, maxTextLines)
	}
	if got := txt.CopyLines()[0]; got != "line 006" {
		t.Fatalf("oldest line = %q, want line 006", got)
	}

	// Appending past the cap drops from the front and keeps the newest.
	txt.Append("tail")
	if got := len(txt.lines); got != maxTextLines {
		t.Fatalf("len(lines) after append = %d, want %d", got, maxTextLines)
	}
	if got := txt.CopyLines()[maxTextLines-1]; got != "tail" {
		t.Fatalf("newest line = %q, want tail", got)
	}
	if got := txt.CopyLines()[0]; got != "line 007" {
		t.Fatalf("oldest line after append = %q, want line 007", got)
	}

	// A search survives the shift: its match indices move with the document.
	txt = sendKey(t, txt, "/")
	txt = typeText(t, txt, "line 010")
	txt = sendKey(t, txt, "enter")
	if len(txt.matches) != 1 || txt.matches[0] != 3 {
		t.Fatalf("matches = %v, want [3]", txt.matches)
	}
	txt.Append("line 010 again")
	txt.Append("line 010 once more")
	if len(txt.matches) != 3 {
		t.Fatalf("matches after appends = %v, want 3", txt.matches)
	}
	if txt.matches[0] != 1 {
		t.Fatalf("first match = %d, want 1 after two lines were dropped", txt.matches[0])
	}
}

func TestTextHorizontalScrollWithANSI(t *testing.T) {
	line := "\x1b[31m" + strings.Repeat("x", 40) + "\x1b[0m END"
	txt := newText(line, "second")
	lines := renderText(t, txt, 50, 5)

	if got, want := ansi.Strip(lines[0]), render.PadRight(strings.Repeat("x", 40)+" END", 50); got != want {
		t.Fatalf("first body line = %q, want %q", got, want)
	}

	txt = sendKey(t, txt, "right")
	if txt.left != textHStep {
		t.Fatalf("left after right = %d, want %d", txt.left, textHStep)
	}
	lines = renderText(t, txt, 50, 5)
	if got, want := ansi.Strip(lines[0]), render.PadRight(strings.Repeat("x", 32)+" END", 50); got != want {
		t.Fatalf("scrolled body line = %q, want %q", got, want)
	}
	if render.Width(lines[0]) != 50 {
		t.Fatalf("scrolled line is %d cells, want 50", render.Width(lines[0]))
	}

	// Four more steps reach the tail of the line.
	for i := 0; i < 4; i++ {
		txt = sendKey(t, txt, "right")
	}
	lines = renderText(t, txt, 50, 5)
	if got, want := ansi.Strip(lines[0]), render.PadRight(" END", 50); got != want {
		t.Fatalf("line at left=%d = %q, want %q", txt.left, got, want)
	}

	// Left floors at zero.
	for i := 0; i < 10; i++ {
		txt = sendKey(t, txt, "left")
	}
	if txt.left != 0 {
		t.Fatalf("left = %d, want 0", txt.left)
	}

	// Wrap mode resets the horizontal offset and keeps the content in view.
	txt = sendKey(t, txt, "right")
	txt = sendKey(t, txt, "W")
	if !txt.wrap || txt.left != 0 {
		t.Fatalf("wrap = %v, left = %d, want true/0", txt.wrap, txt.left)
	}
	lines = renderText(t, txt, 20, 5)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(ansi.Strip(joined), "END") {
		t.Fatalf("wrapped frame = %q, want END visible", joined)
	}
}

func TestTextLinesMsg(t *testing.T) {
	txt := NewText(testEnv(), "logs", nil)
	txt.SetSeq(7)

	// A batch from another sequence is ignored.
	view, _ := txt.Update(tui.LinesMsg{Seq: 6, Lines: []string{"stale"}})
	txt = view.(*Text)
	if len(txt.lines) != 0 {
		t.Fatalf("stale batch appended %v", txt.lines)
	}

	view, _ = txt.Update(tui.LinesMsg{Seq: 7, Lines: []string{"one", "two"}})
	txt = view.(*Text)
	if got := strings.Join(txt.CopyLines(), ","); got != "one,two" {
		t.Fatalf("lines = %q, want one,two", got)
	}

	// A closed stream with an error appends one final Crit line naming the kind.
	err := errs.New(errs.Unreachable, "list", "connection reset")
	view, _ = txt.Update(tui.LinesMsg{Seq: 7, Closed: true, Err: err})
	txt = view.(*Text)
	lines := txt.CopyLines()
	if len(lines) != 3 {
		t.Fatalf("lines = %v, want 3", lines)
	}
	if want := "stream ended: unreachable"; lines[2] != want {
		t.Fatalf("last line = %q, want %q", lines[2], want)
	}

	// Tabs are expanded on input.
	txt.SetLines([]string{"a\tb"})
	if got := txt.CopyLines()[0]; got != "a    b" {
		t.Fatalf("tab expansion = %q, want %q", got, "a    b")
	}
}

func TestTextKV(t *testing.T) {
	txt := NewKV(testEnv(), "detail", [][2]string{
		{"host", "web-ewr-007"},
		{"long-key", "value"},
	})
	lines := txt.CopyLines()
	if lines[0] != "host      web-ewr-007" {
		t.Fatalf("first line = %q, want aligned key/value", lines[0])
	}
	if lines[1] != "long-key  value" {
		t.Fatalf("second line = %q, want aligned key/value", lines[1])
	}
	renderText(t, txt, 30, 5)

	if got := txt.Crumb(); got != "detail" {
		t.Fatalf("Crumb = %q, want detail", got)
	}
	if txt.Capturing() || len(txt.Hints()) == 0 {
		t.Fatalf("unexpected state: capturing=%v hints=%v", txt.Capturing(), txt.Hints())
	}
}
