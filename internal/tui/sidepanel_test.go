package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumpr/internal/investigation"
	"jumpr/internal/mux"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// kbRecorder is an investigation recorder that also knows the Markdown root,
// which is what the knowledge panel derives its base from. It records nothing.
type kbRecorder struct {
	root string
	meta investigation.Meta
	open bool
}

var _ investigation.Log = (*kbRecorder)(nil)

func (r *kbRecorder) Append(investigation.Entry) error { return nil }

func (r *kbRecorder) Current() (investigation.Meta, bool) { return r.meta, r.open }

func (r *kbRecorder) Root() string { return r.root }

// kbNote writes one note into a knowledge base.
func kbNote(t *testing.T, root, rel, body string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

// newKBEnv builds a knowledge base and the env that points at it.
func newKBEnv(t *testing.T) (string, *Env) {
	t.Helper()
	theme.SetProfile("none")
	root := t.TempDir()
	kbNote(t, root, "knowledge/hosts/web-ewr-007.md",
		"# web-ewr-007\n\nRebuilt after a failed root disk in September.\n")
	kbNote(t, root, "knowledge/services/nginx.md",
		"---\nservices: [nginx]\n---\n\n# nginx restarts under load\n\nworker_connections is too low for the fleet.\n")
	kbNote(t, root, "knowledge/alerts/api-5xx.md",
		"---\nalerts: [api-5xx]\n---\n\n# api-5xx alert\n\nThe usual cause is the edge cache failing warm-up.\n")
	env := &Env{
		Keys:     keymap.Default(),
		Theme:    theme.Dark(),
		Recorder: &kbRecorder{root: root},
	}
	return root, env
}

// pump runs cmd and feeds every message it, its batches and the pane's answers
// produce back into the pane, so one call drives a whole index build, debounce
// and match.
func pump(t *testing.T, p *ViewPane, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 200 {
			t.Fatal("pump did not settle")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if pm, ok := msg.(mux.PaneMsg); ok {
			msg = pm.Msg
		}
		_, next := p.Update(msg)
		if next != nil {
			queue = append(queue, next)
		}
	}
}

// kbPane builds a view pane over the knowledge base whose root view is a table
// row about subject.
func kbPane(t *testing.T, env *Env, subject Subject) (*ViewPane, *paneView) {
	t.Helper()
	root := &paneView{crumb: "hosts", line: "host", subject: subject}
	p := NewViewPane(env, 1, context.Background(), root)
	t.Cleanup(func() { _ = p.Close() })
	// These tests are about the panel, which is off until K asks for it.
	p.panelOn = true
	pump(t, p, p.Init())
	return p, root
}

// renderLines checks the frame is exactly w cells by h lines.
func renderLines(t *testing.T, out string, w, h int) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Fatalf("frame has %d lines, want %d", len(lines), h)
	}
	for i, ln := range lines {
		if got := render.Width(ln); got != w {
			t.Fatalf("line %d is %d cells wide, want %d: %q", i, got, w, ln)
		}
	}
	return lines
}

func TestSidePanelShowsHitsForSubject(t *testing.T) {
	root, env := newKBEnv(t)
	p, _ := kbPane(t, env, Subject{Hosts: []resource.HostID{"web-ewr-007"}})

	const w, h = 110, 16
	lines := renderLines(t, p.View(w, h), w, h)
	frame := strings.Join(lines, "\n")
	if !strings.Contains(frame, "Knowledge (1)") {
		t.Fatalf("panel title missing:\n%s", frame)
	}
	if !strings.Contains(frame, "knowledge/hosts/web-ewr-007.md") {
		t.Fatalf("hit path missing:\n%s", frame)
	}
	if !strings.Contains(frame, "host") {
		t.Fatalf("hit reason missing:\n%s", frame)
	}
	if !strings.Contains(frame, "Rebuilt after a failed root disk") {
		t.Fatalf("hit snippet missing:\n%s", frame)
	}
	// The panel occupies the rightmost panelWidth cells of every body line.
	for i, ln := range lines[1:] {
		left := ansi.Truncate(ln, w-panelWidth, "")
		if got := render.Width(strings.TrimPrefix(ln, left)); got != panelWidth {
			t.Fatalf("body line %d has no %d-cell panel: %q", i+1, panelWidth, ln)
		}
	}

	// A different subject is matched after the debounce and replaces the hits.
	p.stack.Top().(*paneView).subject = Subject{Unit: "nginx.service"}
	_, subjCmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	pump(t, p, subjCmd)
	frame = p.View(w, h)
	if !strings.Contains(frame, "knowledge/services/nginx.md") || !strings.Contains(frame, "unit nginx.service") {
		t.Fatalf("panel did not follow the new subject:\n%s", frame)
	}
	if strings.Contains(frame, "knowledge/hosts/web-ewr-007.md") {
		t.Fatalf("panel still shows the old subject's hits:\n%s", frame)
	}

	// The index is what the root holds.
	if got := len(p.panel.ix.Match(investigation.Subject{Alert: "api-5xx"})); got != 1 {
		t.Fatalf("alert hits = %d, want 1 (root %s)", got, root)
	}
}

func TestSidePanelHiddenWhenNarrow(t *testing.T) {
	_, env := newKBEnv(t)
	p, _ := kbPane(t, env, Subject{Hosts: []resource.HostID{"web-ewr-007"}})

	// 89 cells is one short of the 90-cell minimum.
	for _, w := range []int{40, panelMinWidth - 1} {
		out := p.View(w, 12)
		if strings.Contains(out, "Knowledge (") {
			t.Fatalf("panel shown at width %d:\n%s", w, out)
		}
		renderLines(t, out, w, 12)
	}
	// 90 is enough.
	out := p.View(panelMinWidth, 12)
	if !strings.Contains(out, "Knowledge (") {
		t.Fatalf("panel hidden at width %d:\n%s", panelMinWidth, out)
	}
	renderLines(t, out, panelMinWidth, 12)

	// A pane without a knowledge root never has a panel, however wide it is.
	plain := NewViewPane(&Env{Keys: keymap.Default()}, 2, context.Background(), &paneView{crumb: "hosts"})
	defer plain.Close()
	if out := plain.View(200, 10); strings.Contains(out, "Knowledge (") {
		t.Fatalf("a pane without a knowledge root showed a panel:\n%s", out)
	}
}

func TestSidePanelToggleK(t *testing.T) {
	_, env := newKBEnv(t)
	p, _ := kbPane(t, env, Subject{Hosts: []resource.HostID{"web-ewr-007"}})

	const w, h = 110, 12
	if !strings.Contains(p.View(w, h), "Knowledge (") {
		t.Fatal("the panel is not on by default")
	}

	// K turns it off, and the flash says so.
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	msg := runPaneCmd(t, cmd)
	flash, ok := msg.(FlashMsg)
	if !ok || !strings.Contains(flash.Text, "knowledge panel off") {
		t.Fatalf("K produced %#v, want a flash about the panel", msg)
	}
	if strings.Contains(p.View(w, h), "Knowledge (") {
		t.Fatal("the panel did not hide")
	}

	// K turns it back on.
	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	if msg := runPaneCmd(t, cmd); msg != nil {
		if f, ok := msg.(FlashMsg); ok && !strings.Contains(f.Text, "knowledge panel on") {
			t.Fatalf("flash = %q, want the panel on", f.Text)
		}
	}
	if !strings.Contains(p.View(w, h), "Knowledge (") {
		t.Fatal("the panel did not come back")
	}

	// A view that is capturing the keyboard keeps its K.
	p.stack.Top().(*paneView).capturing = true
	if _, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")}); cmd != nil {
		t.Fatal("K was taken from a capturing view")
	}
	if !strings.Contains(p.View(w, h), "Knowledge (") {
		t.Fatal("a capturing view toggled the panel")
	}
}

func TestSidePanelTabFocusOpensNote(t *testing.T) {
	_, env := newKBEnv(t)
	p, _ := kbPane(t, env, Subject{Hosts: []resource.HostID{"web-ewr-007"}})

	const w, h = 110, 16
	p.View(w, h) // the panel has to be drawn once before tab can find it

	// K hides the panel; K again shows it and moves the keyboard into it.
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	if !p.panel.focused {
		t.Fatal("K did not move focus into the panel")
	}

	// While focused, the panel owns the keys: a movement key never reaches the
	// view underneath.
	called := false
	p.stack.Top().(*paneView).onUpdate = func(tea.Msg) tea.Cmd { called = true; return nil }
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if called {
		t.Fatal("a key reached the view while the panel had focus")
	}

	// enter opens the note in a text view.
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	msg := runPaneCmd(t, cmd)
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf("enter produced %#v, want a PushViewMsg", msg)
	}
	if _, ok := push.View.(*noteView); !ok {
		t.Fatalf("enter opened %T, want a note text view", push.View)
	}
	pump(t, p, func() tea.Msg { return push })
	if p.stack.Len() != 2 {
		t.Fatalf("stack length = %d, want the note on top", p.stack.Len())
	}
	if out := p.View(w, h); !strings.Contains(out, "Rebuilt after a failed root disk") {
		t.Fatalf("the note is not shown:\n%s", out)
	}

	// esc goes back to the view; no focus was left behind in the panel.
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pump(t, p, func() tea.Msg { return PopViewMsg{} })
	if p.stack.Len() != 1 {
		t.Fatalf("stack length = %d, want the view back", p.stack.Len())
	}

	// K, K in; tab out.
	p.View(w, h)
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	if !p.panel.focused {
		t.Fatal("K did not focus the panel again")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyTab})
	if p.panel.focused {
		t.Fatal("a second tab did not leave the panel")
	}
}

func TestKnowledgeNewCommand(t *testing.T) {
	root, env := newKBEnv(t)
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	if err := RegisterInvestigationCommands(cmds); err != nil {
		t.Fatalf("register: %v", err)
	}

	msg := runCmd(t, cmds.Exec(env, "kb new alert api-timeout"))
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf(":kb new produced %#v, want a PushViewMsg", msg)
	}
	view, ok := push.View.(*noteView)
	if !ok {
		t.Fatalf(":kb new opened %T, want a note text view", push.View)
	}
	if !strings.Contains(view.flash, "created knowledge/alerts/api-timeout.md") {
		t.Fatalf("flash = %q, want the created path", view.flash)
	}
	data, err := os.ReadFile(filepath.Join(root, "knowledge", "alerts", "api-timeout.md"))
	if err != nil {
		t.Fatalf("note not created: %v", err)
	}
	if !strings.Contains(string(data), "alerts: [api-timeout]") {
		t.Fatalf("note has no frontmatter:\n%s", data)
	}

	// Creating it again opens it and says so.
	msg = runCmd(t, cmds.Exec(env, "kb new alert api-timeout"))
	push, ok = msg.(PushViewMsg)
	if !ok {
		t.Fatalf(":kb new again produced %#v", msg)
	}
	if v := push.View.(*noteView); !strings.Contains(v.flash, "opened ") {
		t.Fatalf("flash = %q, want it to say the note was opened", v.flash)
	}

	// The command line is validated, and a missing knowledge base is a warning.
	for _, line := range []string{"kb new", "kb new host", "kb wat x", "kb"} {
		if out := runCmd(t, cmds.Exec(env, line)); out == nil {
			t.Fatalf("%q produced no message", line)
		}
	}
	if out := runCmd(t, cmds.Exec(&Env{Keys: keymap.Default()}, "kb new host web-01")); out == nil {
		t.Fatal(":kb new without a knowledge root produced no message")
	}
}

func TestKnowledgeReindexCommand(t *testing.T) {
	root, env := newKBEnv(t)
	p, _ := kbPane(t, env, Subject{Hosts: []resource.HostID{"web-ewr-007"}})
	if got := p.panel.ix.Len(); got != 3 {
		t.Fatalf("index holds %d notes, want 3", got)
	}

	kbNote(t, root, "knowledge/notes/fresh.md", "---\ntags: [fresh]\n---\n\n# fresh note\n\nA new one.\n")

	msg := runCmd(t, runKB(env, "reindex"))
	if _, ok := msg.(knowledgeReindexMsg); !ok {
		t.Fatalf(":kb reindex produced %#v", msg)
	}
	pump(t, p, func() tea.Msg { return msg })
	if got := p.panel.ix.Len(); got != 4 {
		t.Fatalf("index holds %d notes after reindex, want 4", got)
	}
}

func TestKnowledgeResolveOffersNote(t *testing.T) {
	root, env := newKBEnv(t)
	rec := env.Recorder.(*kbRecorder)
	rec.meta = investigation.Meta{ID: "2026-11-12-api-5xx", Status: "open", Alerts: []string{"api-5xx"}}
	rec.open = true

	p, _ := kbPane(t, env, Subject{Hosts: []resource.HostID{"web-ewr-007"}})
	const w, h = 110, 12
	p.View(w, h)

	// The resolve command asks every pane to offer the note (see
	// resolveWith); the pane turns that into a y/N dialog.
	_, cmd := p.Update(knowledgeRememberMsg{Cause: "varnish warm-up missed the build", Alerts: []string{"api-5xx"}})
	msg := runPaneCmd(t, cmd)
	open, ok := msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf("resolve offer produced %#v, want an OpenDialogMsg", msg)
	}
	if open.Dialog == nil || open.Dialog.Title() != "remember this?" {
		t.Fatalf("dialog = %#v, want the y/N offer", open.Dialog)
	}
	if body := open.Dialog.View(60, 10); !strings.Contains(body, "knowledge/alerts/api-5xx.md") {
		t.Fatalf("dialog does not name the note:\n%s", body)
	}

	// y files the root cause in the alert's note.
	_, ycmd := open.Dialog.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	var note knowledgeNoteMsg
	found := false
	for _, m := range flatten(t, ycmd) {
		if n, ok := m.(knowledgeNoteMsg); ok {
			note, found = n, true
		}
	}
	if !found {
		t.Fatalf("y produced no knowledgeNoteMsg: %v", flatten(t, ycmd))
	}
	if note.Err != nil {
		t.Fatalf("filing the root cause: %v", note.Err)
	}
	data, err := os.ReadFile(filepath.Join(root, "knowledge", "alerts", "api-5xx.md"))
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	if !strings.Contains(string(data), "- varnish warm-up missed the build") {
		t.Fatalf("root cause not filed:\n%s", data)
	}

	// The pane re-indexes the note and flashes its path.
	_, follow := p.Update(note)
	frame := ""
	for _, c := range flatten(t, follow) {
		if m, ok := c.(FlashMsg); ok {
			frame = m.Text
		}
	}
	if !strings.Contains(frame, "knowledge: knowledge/alerts/api-5xx.md") {
		t.Fatalf("flash = %q, want the note path", frame)
	}
	if hits := p.panel.ix.Match(investigation.Subject{Tags: []string{"warm-up"}}); len(hits) != 1 {
		t.Fatalf("the filed root cause is not indexed: %v", hits)
	}
}

// runPaneCmd unwraps a command for pane 1 and returns its message.
func runPaneCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if pm, ok := msg.(mux.PaneMsg); ok {
		return pm.Msg
	}
	return msg
}

// flatten runs a command tree and returns every message it produces.
func flatten(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 50 {
			t.Fatal("flatten did not settle")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch m := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, m...)
		default:
			if pm, ok := m.(mux.PaneMsg); ok {
				m = pm.Msg
			}
			out = append(out, m)
		}
	}
	return out
}
