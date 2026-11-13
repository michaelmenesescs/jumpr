package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

func testStatus() StatusData {
	return StatusData{
		Session: "main", Prefix: "^A", Context: "prod-ewr", Investigation: "inc-42", Window: "last 1h",
		Targets: 12, ReadOnly: true, Sync: 3,
		Windows: []WindowTab{{0, "hosts", true}, {1, "units", false}, {2, "alerts", false}},
		Health: []HealthDot{
			{"salt", backend.HealthOK}, {"prom", backend.HealthOK}, {"grafana", backend.HealthDegraded},
		},
	}
}

func TestStatusExactWidth(t *testing.T) {
	th := theme.Dark()
	for _, w := range []int{40, 80, 132, 200} {
		got := RenderStatus(th, testStatus(), w)
		if render.Width(got) != w || strings.Contains(got, "\n") {
			t.Errorf("w=%d: width %d, multiline=%v", w, render.Width(got), strings.Contains(got, "\n"))
		}
	}
}

func TestStatusContent(t *testing.T) {
	got := ansiStrip(RenderStatus(theme.Dark(), testStatus(), 200))
	for _, want := range []string{"jumper", "main", "0:hosts*", "inv:inc-42", "window:last 1h", "targets:12", "RO", "SYNC ×3", "^A", "●salt", "●prom", "◐grafana"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestStatusDropsOrder(t *testing.T) {
	th := theme.Dark()
	d := testStatus()
	full := ansiStrip(RenderStatus(th, d, 200))
	if !strings.Contains(full, "prod-ewr") {
		t.Fatalf("context missing: %q", full)
	}
	// Find the widest width where each item is first gone, scanning downward.
	gone := map[string]int{}
	items := []string{"prod-ewr", "window:last 1h", "●salt", "1:units"}
	for w := 200; w >= 20; w-- {
		s := ansiStrip(RenderStatus(th, d, w))
		for _, it := range items {
			if _, ok := gone[it]; !ok && !strings.Contains(s, it) {
				gone[it] = w
			}
		}
	}
	for i := 0; i+1 < len(items); i++ {
		a, b := gone[items[i]], gone[items[i+1]]
		if a == 0 || b == 0 || a < b {
			t.Errorf("drop order wrong: %q at %d, %q at %d", items[i], a, items[i+1], b)
		}
	}
	narrow := ansiStrip(RenderStatus(th, d, 60))
	if !strings.Contains(narrow, "0:hosts*") || !strings.Contains(narrow, "●") {
		t.Errorf("active tab and dots must stay: %q", narrow)
	}
}

func TestHealthDotGlyphs(t *testing.T) {
	th := theme.Dark()
	want := map[backend.HealthState]string{
		backend.HealthOK: "●", backend.HealthDegraded: "◐", backend.HealthDown: "●", backend.HealthUnconfigured: "○",
	}
	for st, g := range want {
		got, styled := healthGlyph(th, st)
		if got != g || ansiStrip(styled) != g {
			t.Errorf("state %v: %q / %q, want %q", st, got, styled, g)
		}
	}
}

func TestHealthPoll(t *testing.T) {
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatal(err)
	}
	cmd := HealthPoll(reg, time.Second)
	msg := cmd()
	var msgs []tea.Msg
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			msgs = append(msgs, c())
		}
	} else {
		msgs = append(msgs, msg)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	h, ok := msgs[0].(HealthMsg)
	if !ok || h.Backend != "fake" || h.Health.State != backend.HealthOK {
		t.Errorf("msg = %#v", msgs[0])
	}
	if HealthPoll(backend.NewRegistry(), time.Second) != nil {
		t.Error("empty registry should return nil")
	}
}

func TestHintsFit(t *testing.T) {
	th := theme.Dark()
	hints := []KeyHint{{"enter", "open"}, {"u", "units"}, {"a", "alerts"}, {"l", "logs"}, {"s", "ssh"}}
	for w := 1; w <= 120; w++ {
		got := RenderHints(th, hints, w)
		if render.Width(got) != w {
			t.Fatalf("w=%d: width %d", w, render.Width(got))
		}
	}
	wide := ansiStrip(RenderHints(th, hints, 100))
	if !strings.HasPrefix(wide, "<enter> open  <u> units") || !strings.Contains(wide, "<s> ssh") {
		t.Errorf("wide: %q", wide)
	}
	if !strings.HasSuffix(strings.TrimRight(wide, " "), "<:> cmd  <?> help") {
		t.Errorf("tail missing: %q", wide)
	}
	mid := ansiStrip(RenderHints(th, hints, 40))
	if !strings.HasSuffix(strings.TrimRight(mid, " "), "<:> cmd  <?> help") || strings.Contains(mid, "<s> ssh") {
		t.Errorf("mid: %q", mid)
	}
}

func TestFlash(t *testing.T) {
	th := theme.Dark()
	for _, l := range []FlashLevel{FlashInfo, FlashOK, FlashWarn, FlashError} {
		got := RenderFlash(th, FlashMsg{Level: l, Text: "hello\nworld " + strings.Repeat("x", 100)}, 30)
		if render.Width(got) != 30 || strings.Contains(got, "\n") {
			t.Errorf("level %v width %d", l, render.Width(got))
		}
	}
}

func ansiStrip(s string) string {
	var sb strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			in = true
		case in && r == 'm':
			in = false
		case !in:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
