package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

func helpSections() []HelpSection {
	var hs []KeyHint
	for i := 0; i < 40; i++ {
		hs = append(hs, KeyHint{Key: "k", Text: "do thing"})
	}
	return []HelpSection{{Title: "Global", Hints: hs}, {Title: "Hosts", Hints: []KeyHint{{"enter", "open"}}}}
}

func TestDialogCentredExactSize(t *testing.T) {
	th := theme.Dark()
	bg := strings.TrimRight(strings.Repeat(th.Accent.Render(strings.Repeat("▒", 80))+"\n", 24), "\n")
	d := NewHelpDialog(th, "Help", helpSections())
	for _, sz := range [][2]int{{80, 24}, {40, 10}, {120, 40}, {8, 4}} {
		out := RenderDialog(th, d, sz[0], sz[1], bg)
		lines := strings.Split(out, "\n")
		if len(lines) != sz[1] {
			t.Fatalf("%v: %d lines", sz, len(lines))
		}
		for i, l := range lines {
			if render.Width(l) != sz[0] {
				t.Fatalf("%v line %d width %d", sz, i, render.Width(l))
			}
		}
	}
	out := ansiStrip(RenderDialog(th, d, 80, 24, bg))
	lines := strings.Split(out, "\n")
	if !strings.Contains(out, "Help") || !strings.Contains(out, "╭") || !strings.Contains(out, "╯") {
		t.Errorf("no frame:\n%s", out)
	}
	r := []rune(lines[12])
	li, ri := -1, -1
	for i, c := range r {
		if c == '│' {
			if li < 0 {
				li = i
			}
			ri = i
		}
	}
	if li < 1 || ri <= li {
		t.Fatalf("no border on middle row: %q", lines[12])
	}
	if lm, rm := li, len(r)-1-ri; lm-rm > 1 || rm-lm > 1 {
		t.Errorf("not centred: left %d right %d", lm, rm)
	}
	if !strings.Contains(lines[0], "▒") || !strings.Contains(lines[23], "▒") {
		t.Errorf("background lost outside dialog")
	}
}

func TestHelpDialogCloses(t *testing.T) {
	th := theme.Dark()
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune("q")}, {Type: tea.KeyRunes, Runes: []rune("?")}} {
		d := NewHelpDialog(th, "Help", helpSections())
		_, cmd := d.Update(k)
		if cmd == nil {
			t.Fatalf("%v: no cmd", k)
		}
		if _, ok := cmd().(CloseDialogMsg); !ok {
			t.Errorf("%v: not CloseDialogMsg", k)
		}
	}
}

func TestHelpDialogScrolls(t *testing.T) {
	th := theme.Dark()
	d := NewHelpDialog(th, "Help", helpSections())
	first := ansiStrip(d.View(40, 8))
	d, cmd := d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if cmd != nil {
		t.Error("scroll must not emit a command")
	}
	second := ansiStrip(d.View(40, 8))
	if first == second {
		t.Error("down did not scroll")
	}
	for i := 0; i < 500; i++ {
		d, _ = d.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if end := ansiStrip(d.View(40, 8)); !strings.Contains(end, "open") {
		t.Errorf("end not reached:\n%s", end)
	}
	for i := 0; i < 500; i++ {
		d, _ = d.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	if ansiStrip(d.View(40, 8)) != first {
		t.Error("up did not return to top")
	}
}
