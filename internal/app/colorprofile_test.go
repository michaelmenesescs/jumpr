package app

import "testing"

func TestResolveColor(t *testing.T) {
	cases := []struct {
		name    string
		setting string
		env     map[string]string
		want    string
	}{
		{"explicit wins", "256", map[string]string{"COLORTERM": "truecolor"}, "256"},
		{"explicit none", "none", map[string]string{"COLORTERM": "truecolor"}, "none"},
		{"colorterm truecolor", "auto", map[string]string{"TERM": "xterm-ghostty", "COLORTERM": "truecolor"}, "truecolor"},
		{"colorterm 24bit", "", map[string]string{"TERM": "xterm", "COLORTERM": "24bit"}, "truecolor"},
		{"ghostty without colorterm", "auto", map[string]string{"TERM": "xterm-ghostty"}, "truecolor"},
		{"tmux 256", "auto", map[string]string{"TERM": "tmux-256color"}, "256"},
		{"plain xterm", "auto", map[string]string{"TERM": "xterm"}, "16"},
		{"dumb", "auto", map[string]string{"TERM": "dumb"}, "none"},
		{"no term", "auto", map[string]string{}, "none"},
		{"NO_COLOR", "auto", map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "NO_COLOR": "1"}, "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveColor(c.setting, func(k string) string { return c.env[k] })
			if got != c.want {
				t.Fatalf("resolveColor(%q, %v) = %q, want %q", c.setting, c.env, got, c.want)
			}
		})
	}
}
