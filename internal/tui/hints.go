package tui

import (
	"strings"

	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

func renderHint(t *theme.Theme, h KeyHint) (string, int) {
	s := t.HintKey.Render("<"+h.Key+">") + " " + t.HintText.Render(h.Text)
	return s, render.Width("<" + h.Key + "> " + h.Text)
}

// RenderHints renders the hint bar exactly w cells wide. Hints are dropped from
// the right until they fit; `<:> cmd  <?> help` always ends the line if it fits.
func RenderHints(t *theme.Theme, hints []KeyHint, w int) string {
	if w <= 0 {
		return ""
	}
	tail, tailW := renderHint(t, KeyHint{Key: ":", Text: "cmd"})
	h2, h2W := renderHint(t, KeyHint{Key: "?", Text: "help"})
	tail += "  " + h2
	tailW += 2 + h2W

	avail := w
	if tailW <= w {
		avail = w - tailW - 2
	}
	var parts []string
	cur := 0
	for _, h := range hints {
		s, sw := renderHint(t, h)
		need := sw
		if len(parts) > 0 {
			need += 2
		}
		if cur+need > avail {
			break
		}
		parts = append(parts, s)
		cur += need
	}
	line := strings.Join(parts, "  ")
	if tailW <= w {
		if line != "" {
			line += "  "
		}
		line += tail
	}
	return render.PadRight(line, w)
}

// RenderFlash renders one flash line exactly w cells wide.
func RenderFlash(t *theme.Theme, m FlashMsg, w int) string {
	if w <= 0 {
		return ""
	}
	st := t.Flash.Info
	switch m.Level {
	case FlashOK:
		st = t.Flash.OK
	case FlashWarn:
		st = t.Flash.Warn
	case FlashError:
		st = t.Flash.Error
	}
	text := strings.ReplaceAll(m.Text, "\n", " ")
	return render.PadRight(st.Render(render.Truncate(text, w)), w)
}
