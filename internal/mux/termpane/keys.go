package termpane

import tea "github.com/charmbracelet/bubbletea"

// esc is the escape byte every escape sequence starts with.
const esc = 0x1b

// fKeys are the xterm sequences of the function keys.
var fKeys = map[tea.KeyType]string{
	tea.KeyF1:  "\x1bOP",
	tea.KeyF2:  "\x1bOQ",
	tea.KeyF3:  "\x1bOR",
	tea.KeyF4:  "\x1bOS",
	tea.KeyF5:  "\x1b[15~",
	tea.KeyF6:  "\x1b[17~",
	tea.KeyF7:  "\x1b[18~",
	tea.KeyF8:  "\x1b[19~",
	tea.KeyF9:  "\x1b[20~",
	tea.KeyF10: "\x1b[21~",
	tea.KeyF11: "\x1b[23~",
	tea.KeyF12: "\x1b[24~",
}

// Encode turns one key press into the bytes a remote PTY expects. It returns
// nil for keys the table does not cover; unknown keys are dropped, never
// guessed at.
func Encode(msg tea.KeyMsg) []byte {
	if msg.Paste {
		// A bracketed paste is sent as its runes, exactly as typed.
		return []byte(string(msg.Runes))
	}
	if msg.Type == tea.KeyRunes {
		runes := []byte(string(msg.Runes))
		if msg.Alt {
			return append([]byte{esc}, runes...)
		}
		return runes
	}
	seq, ok := encodeKey(msg.Type)
	if !ok {
		return nil
	}
	if msg.Alt {
		return append([]byte{esc}, seq...)
	}
	return seq
}

// encodeKey encodes one non-rune key type.
func encodeKey(t tea.KeyType) ([]byte, bool) {
	switch t {
	case tea.KeyEnter:
		return []byte("\r"), true
	case tea.KeyTab:
		return []byte("\t"), true
	case tea.KeyBackspace:
		return []byte{0x7f}, true
	case tea.KeyEsc:
		return []byte{esc}, true
	case tea.KeySpace:
		return []byte(" "), true
	case tea.KeyUp:
		return []byte("\x1b[A"), true
	case tea.KeyDown:
		return []byte("\x1b[B"), true
	case tea.KeyRight:
		return []byte("\x1b[C"), true
	case tea.KeyLeft:
		return []byte("\x1b[D"), true
	case tea.KeyHome:
		return []byte("\x1b[H"), true
	case tea.KeyEnd:
		return []byte("\x1b[F"), true
	case tea.KeyPgUp:
		return []byte("\x1b[5~"), true
	case tea.KeyPgDown:
		return []byte("\x1b[6~"), true
	case tea.KeyDelete:
		return []byte("\x1b[3~"), true
	case tea.KeyInsert:
		return []byte("\x1b[2~"), true
	case tea.KeyShiftTab:
		return []byte("\x1b[Z"), true
	case tea.KeyCtrlAt:
		// ctrl+@ and ctrl+space.
		return []byte{0x00}, true
	}
	// ctrl+<letter> is 0x01-0x1a; ctrl+\, ctrl+], ctrl+^ and ctrl+_ follow
	// consecutively as 0x1c-0x1f. ctrl+m and ctrl+i arrive as enter and tab
	// and were handled above with the same bytes.
	if t >= tea.KeyCtrlA && t <= tea.KeyCtrlZ {
		return []byte{byte(t)}, true
	}
	if t >= tea.KeyCtrlBackslash && t <= tea.KeyCtrlUnderscore {
		return []byte{byte(t)}, true
	}
	if seq, ok := fKeys[t]; ok {
		return []byte(seq), true
	}
	return nil, false
}
