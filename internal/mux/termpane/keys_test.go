package termpane

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyEncodeTable(t *testing.T) {
	key := func(t tea.KeyType, runes ...rune) tea.KeyMsg {
		return tea.KeyMsg{Type: t, Runes: runes}
	}
	cases := []struct {
		name string
		msg  tea.KeyMsg
		want string
	}{
		{"runes", key(tea.KeyRunes, 'a', 'b'), "ab"},
		{"utf8", key(tea.KeyRunes, 'é'), "é"},
		{"enter", key(tea.KeyEnter), "\r"},
		{"tab", key(tea.KeyTab), "\t"},
		{"backspace", key(tea.KeyBackspace), "\x7f"},
		{"esc", key(tea.KeyEsc), "\x1b"},
		{"ctrl+a", key(tea.KeyCtrlA), "\x01"},
		{"ctrl+c", key(tea.KeyCtrlC), "\x03"},
		{"ctrl+z", key(tea.KeyCtrlZ), "\x1a"},
		{"ctrl+@", key(tea.KeyCtrlAt), "\x00"},
		{"ctrl+space", key(tea.KeyCtrlAt), "\x00"},
		{"ctrl+backslash", key(tea.KeyCtrlBackslash), "\x1c"},
		{"ctrl+]", key(tea.KeyCtrlCloseBracket), "\x1d"},
		{"ctrl+^", key(tea.KeyCtrlCaret), "\x1e"},
		{"ctrl+_", key(tea.KeyCtrlUnderscore), "\x1f"},
		{"up", key(tea.KeyUp), "\x1b[A"},
		{"down", key(tea.KeyDown), "\x1b[B"},
		{"right", key(tea.KeyRight), "\x1b[C"},
		{"left", key(tea.KeyLeft), "\x1b[D"},
		{"home", key(tea.KeyHome), "\x1b[H"},
		{"end", key(tea.KeyEnd), "\x1b[F"},
		{"pgup", key(tea.KeyPgUp), "\x1b[5~"},
		{"pgdown", key(tea.KeyPgDown), "\x1b[6~"},
		{"delete", key(tea.KeyDelete), "\x1b[3~"},
		{"insert", key(tea.KeyInsert), "\x1b[2~"},
		{"f1", key(tea.KeyF1), "\x1bOP"},
		{"f2", key(tea.KeyF2), "\x1bOQ"},
		{"f3", key(tea.KeyF3), "\x1bOR"},
		{"f4", key(tea.KeyF4), "\x1bOS"},
		{"f5", key(tea.KeyF5), "\x1b[15~"},
		{"f6", key(tea.KeyF6), "\x1b[17~"},
		{"f7", key(tea.KeyF7), "\x1b[18~"},
		{"f8", key(tea.KeyF8), "\x1b[19~"},
		{"f9", key(tea.KeyF9), "\x1b[20~"},
		{"f10", key(tea.KeyF10), "\x1b[21~"},
		{"f11", key(tea.KeyF11), "\x1b[23~"},
		{"f12", key(tea.KeyF12), "\x1b[24~"},
		{"shift+tab", key(tea.KeyShiftTab), "\x1b[Z"},
		{"alt+rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x"), Alt: true}, "\x1bx"},
		{"paste", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pasted text"), Paste: true}, "pasted text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(Encode(c.msg)); got != c.want {
				t.Fatalf("Encode(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestEncodeIgnoresUnknownKeys(t *testing.T) {
	if got := Encode(tea.KeyMsg{Type: tea.KeyF13}); got != nil {
		t.Fatalf("Encode(f13) = %q, want nil", got)
	}
}
