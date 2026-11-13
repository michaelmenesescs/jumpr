package keymap

import (
	"fmt"
	"strings"

	"jumper/internal/errs"
)

// Binding is one action and the Bubble Tea keys that trigger it.
type Binding struct {
	Keys []string // Bubble Tea key strings: "enter", "esc", "ctrl+a", "u", "?"
	Help string   // one-line description for the hint bar and help view
}

// Matches reports whether key, as delivered by Bubble Tea, is bound.
func (b Binding) Matches(key string) bool {
	for _, k := range b.Keys {
		if k == key {
			return true
		}
	}
	return false
}

// Map holds the global bindings. Views read these instead of hard-coding keys.
type Map struct {
	Up         Binding
	Down       Binding
	PageUp     Binding
	PageDown   Binding
	Top        Binding
	Bottom     Binding
	Open       Binding
	Back       Binding
	Command    Binding
	Filter     Binding
	Help       Binding
	Refresh    Binding
	Quit       Binding
	Actions    Binding
	Note       Binding
	Finding    Binding
	Knowledge  Binding
	Timeline   Binding
	Yank       Binding
	Follow     Binding
	TimeWindow Binding
	Diff       Binding
	Errors     Binding
}

// Default returns the built-in key map.
func Default() *Map {
	return &Map{
		Up:         Binding{Keys: []string{"k", "up"}, Help: "up"},
		Down:       Binding{Keys: []string{"j", "down"}, Help: "down"},
		PageUp:     Binding{Keys: []string{"pgup", "ctrl+u"}, Help: "page up"},
		PageDown:   Binding{Keys: []string{"pgdown", "ctrl+d"}, Help: "page down"},
		Top:        Binding{Keys: []string{"g", "home"}, Help: "top"},
		Bottom:     Binding{Keys: []string{"G", "end"}, Help: "bottom"},
		Open:       Binding{Keys: []string{"enter"}, Help: "open"},
		Back:       Binding{Keys: []string{"esc"}, Help: "back"},
		Command:    Binding{Keys: []string{":"}, Help: "command"},
		Filter:     Binding{Keys: []string{"/"}, Help: "filter"},
		Help:       Binding{Keys: []string{"?"}, Help: "help"},
		Refresh:    Binding{Keys: []string{"r"}, Help: "refresh"},
		Quit:       Binding{Keys: []string{"q"}, Help: "quit"},
		Actions:    Binding{Keys: []string{"x"}, Help: "actions"},
		Note:       Binding{Keys: []string{"n"}, Help: "note"},
		Finding:    Binding{Keys: []string{"F"}, Help: "finding"},
		Knowledge:  Binding{Keys: []string{"K"}, Help: "knowledge"},
		Timeline:   Binding{Keys: []string{"T"}, Help: "timeline"},
		Yank:       Binding{Keys: []string{"y"}, Help: "yank"},
		Follow:     Binding{Keys: []string{"w"}, Help: "follow"},
		TimeWindow: Binding{Keys: []string{"t"}, Help: "time window"},
		Diff:       Binding{Keys: []string{"d"}, Help: "diff"},
		Errors:     Binding{Keys: []string{"e"}, Help: "errors"},
	}
}

// ParsePrefix converts a config-form prefix (`C-a`) into the Bubble Tea key
// string (`ctrl+a`). Only control keys with a Bubble Tea name are accepted:
// `C-a`..`C-z`, `C-@`, `C-\`, `C-]`, `C-^` and `C-_`.
func ParsePrefix(s string) (string, error) {
	if len(s) == 3 && s[0] == 'C' && s[1] == '-' {
		c := s[2]
		switch {
		case c >= 'a' && c <= 'z':
			return "ctrl+" + string(c), nil
		case c == '@' || c == '\\' || c == ']' || c == '^' || c == '_':
			return "ctrl+" + string(c), nil
		}
	}
	return "", errs.New(errs.Config, "keymap.ParsePrefix",
		fmt.Sprintf("prefix %q is not C-<a-z>, C-@, C-\\, C-], C-^ or C-_", s))
}

// DisplayKey renders a Bubble Tea key string for humans: `ctrl+a` becomes
// `C-a`; every other key is returned unchanged.
func DisplayKey(teaKey string) string {
	if rest, ok := strings.CutPrefix(teaKey, "ctrl+"); ok && len([]rune(rest)) == 1 {
		return "C-" + rest
	}
	return teaKey
}
