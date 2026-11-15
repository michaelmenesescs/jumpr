package app

import "strings"

// resolveColor turns ui.color into a concrete theme profile name. An explicit
// setting wins. "auto" is decided from the terminal's environment rather than
// from this process's stdout: the session server is a detached process with no
// TTY, so stdout-based detection would always pick "no colour" there, while the
// environment it inherits from the client still names the real terminal.
func resolveColor(setting string, getenv func(string) string) string {
	if setting != "auto" && setting != "" {
		return setting
	}
	if getenv("NO_COLOR") != "" {
		return "none"
	}
	colorTerm := strings.ToLower(getenv("COLORTERM"))
	term := strings.ToLower(getenv("TERM"))
	switch {
	case colorTerm == "truecolor" || colorTerm == "24bit":
		return "truecolor"
	case term == "" || term == "dumb":
		return "none"
	case strings.Contains(term, "ghostty"), strings.Contains(term, "kitty"),
		strings.Contains(term, "wezterm"), strings.Contains(term, "alacritty"),
		strings.Contains(term, "direct"):
		return "truecolor"
	case strings.Contains(term, "256color"):
		return "256"
	default:
		return "16"
	}
}
