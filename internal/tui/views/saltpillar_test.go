package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/salt"
	"jumper/internal/investigation"
	"jumper/internal/tui"
)

// ---- fixtures ----

// fakePillar is a Salt pillar without a master.
type fakePillar struct {
	tree     salt.PillarTree
	treeErr  error
	value    string
	valueErr error

	minion string
	path   string
	reads  int
}

func (f *fakePillar) PillarKeys(_ context.Context, minion string) (salt.PillarTree, error) {
	f.minion = minion
	f.reads++
	return f.tree, f.treeErr
}

func (f *fakePillar) PillarValue(_ context.Context, minion, keyPath string) (string, error) {
	f.minion, f.path = minion, keyPath
	return f.value, f.valueErr
}

// pillarFixture is web-01's pillar: a nested secret, a password and a list.
func pillarFixture() *fakePillar {
	return &fakePillar{
		tree: salt.PillarTree{Minion: "web-01", Nodes: []salt.PillarNode{
			{Key: "nginx", Type: "dict", Children: []salt.PillarNode{
				{Key: "nginx:tls", Type: "dict", Children: []salt.PillarNode{
					{Key: "nginx:tls:key", Type: "str", Len: 1704},
					{Key: "nginx:tls:cert", Type: "str", Len: 2048},
				}},
				{Key: "nginx:workers", Type: "int"},
			}},
			{Key: "postgres", Type: "dict", Children: []salt.PillarNode{
				{Key: "postgres:password", Type: "str", Len: 12},
			}},
			{Key: "roles", Type: "list", Len: 3},
		}},
		value: "s3cr3t-value",
	}
}

// pillarEnv is a session with a timeline open and a redactor.
type pillarRecorder struct {
	open    bool
	entries []investigation.Entry
}

func (r *pillarRecorder) Append(e investigation.Entry) error {
	r.entries = append(r.entries, e)
	return nil
}

func (r *pillarRecorder) Current() (investigation.Meta, bool) {
	if !r.open {
		return investigation.Meta{}, false
	}
	return investigation.Meta{ID: "2026-11-13-pillar-test", Title: "pillar test", Status: "open"}, true
}

// newPillarEnv builds the environment the pillar screen runs in.
func newPillarEnv(rec *pillarRecorder) *tui.Env {
	env := &tui.Env{
		Session:  &tui.SessionState{Name: "test"},
		Now:      func() time.Time { return time.Unix(1730000000, 0) },
		Recorder: rec,
	}
	return env
}

// drivePillar runs a view command until nothing is left to do, giving every
// message the view itself emits back to it.
func drivePillar(t *testing.T, v *SaltPillar, cmd tea.Cmd) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for cmd != nil {
		if time.Now().After(deadline) {
			t.Fatal("the view did not settle")
		}
		msg := cmd()
		if _, next := v.Update(msg); next != nil {
			cmd = next
			continue
		}
		return
	}
}

// newPillarView builds the view and settles its first load.
func newPillarView(t *testing.T, f *fakePillar, env *tui.Env) *SaltPillar {
	t.Helper()
	if env == nil {
		env = newPillarEnv(&pillarRecorder{open: true})
	}
	v := NewSaltPillar(env, f, "web-01")
	drivePillar(t, v, v.Init())
	return v
}

// pressPillar sends one key press to the view.
func pressPillar(v *SaltPillar, key string) tea.Cmd {
	var msg tea.KeyMsg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := v.Update(msg)
	return cmd
}

// typePillar types text into the filter, one key at a time.
func typePillar(v *SaltPillar, text string) tea.Cmd {
	var cmd tea.Cmd
	for _, r := range text {
		_, cmd = v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return cmd
}

// selectPillar puts the cursor on the row whose key is path.
func selectPillar(t *testing.T, v *SaltPillar, path string) {
	t.Helper()
	for i, row := range v.rows {
		if row.Node.Key == path {
			v.cursor = i
			return
		}
	}
	t.Fatalf("no row %q in %d rows", path, len(v.rows))
}

// openDialog returns the dialog a command asks the root for.
func openDialog(t *testing.T, cmd tea.Cmd) tui.Dialog {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command to run")
	}
	msg := cmd()
	open, ok := msg.(tui.OpenDialogMsg)
	if !ok {
		t.Fatalf("message = %#v, want OpenDialogMsg", msg)
	}
	if open.Dialog == nil {
		t.Fatal("the message carries no dialog")
	}
	return open.Dialog
}

// confirm answers a y/N dialog the way the root does: it runs the command the
// dialog returns and hands back every message that comes out of it.
func confirm(t *testing.T, d tui.Dialog, key string) []tea.Msg {
	t.Helper()
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	if cmd == nil {
		t.Fatal("the confirm dialog answered nothing")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		if msg == nil {
			return nil
		}
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range batch {
		if c == nil {
			continue
		}
		if m := c(); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// ---- tests ----

// TestPillarTreeRendersTypesAndLengths is requirement 2: the tree shows keys,
// types and lengths, and no value.
func TestPillarTreeRendersTypesAndLengths(t *testing.T) {
	f := pillarFixture()
	v := newPillarView(t, f, nil)

	if f.minion != "web-01" {
		t.Errorf("PillarKeys asked for %q, want web-01", f.minion)
	}
	frame := v.Render(90, 20)
	for _, want := range []string{
		"nginx:",
		"tls:",
		"‹dict, 2 keys›",
		"‹dict, 2 keys›",
		"‹int›",
		"postgres:",
		"roles",
		"‹list, 3 items›",
		"values hidden",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "s3cr3t-value") {
		t.Errorf("the frame leaks a value:\n%s", frame)
	}

	// One level down: expanding tls shows the secret's length, not the secret.
	selectPillar(t, v, "nginx:tls")
	pressPillar(v, "enter")
	frame = v.Render(90, 20)
	if !strings.Contains(frame, "key") || !strings.Contains(frame, "‹str, 1,704 chars›") {
		t.Errorf("the expanded tree does not show the length:\n%s", frame)
	}
	if !strings.Contains(frame, "‹str, 2,048 chars›") {
		t.Errorf("the second leaf is missing:\n%s", frame)
	}
	if strings.Contains(frame, "s3cr3t-value") {
		t.Errorf("the expanded tree leaks a value:\n%s", frame)
	}
}

// TestPillarCopyLinesNeverCarryValues is requirement 3's copy rule.
func TestPillarCopyLinesNeverCarryValues(t *testing.T) {
	f := pillarFixture()
	v := newPillarView(t, f, nil)
	selectPillar(t, v, "nginx:tls")
	pressPillar(v, "enter")

	lines := strings.Join(v.CopyLines(), "\n")
	if !strings.Contains(lines, "nginx:tls:key") || !strings.Contains(lines, "‹str, 1,704 chars›") {
		t.Errorf("CopyLines does not describe the key:\n%s", lines)
	}
	for _, secret := range []string{"s3cr3t-value", f.value} {
		if secret != "" && strings.Contains(lines, secret) {
			t.Errorf("CopyLines carries %q:\n%s", secret, lines)
		}
	}
}

// TestPillarRevealFlow is requirement 3: R asks, the answer is shown in a
// dialog only, and the timeline records the path without the value.
func TestPillarRevealFlow(t *testing.T) {
	rec := &pillarRecorder{open: true}
	f := pillarFixture()
	v := newPillarView(t, f, newPillarEnv(rec))
	selectPillar(t, v, "nginx:tls")
	pressPillar(v, "enter")
	selectPillar(t, v, "nginx:tls:key")

	ask := openDialog(t, pressPillar(v, "R"))
	text := ask.View(110, 6)
	for _, want := range []string{
		"reveal pillar value nginx:tls:key on web-01?",
		"it will be shown on screen",
		"job cache",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the question does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "s3cr3t-value") {
		t.Errorf("the question shows the value before the answer:\n%s", text)
	}

	// Answer yes: the confirm dialog runs the reveal, which reads one key.
	var valueMsg tea.Msg
	for _, msg := range confirm(t, ask, "y") {
		if _, ok := msg.(pillarValueMsg); ok {
			valueMsg = msg
		}
	}
	if valueMsg == nil {
		t.Fatal("answering yes did not read a value")
	}
	if f.path != "nginx:tls:key" {
		t.Errorf("pillar.get asked for %q, want nginx:tls:key", f.path)
	}

	_, cmd := v.Update(valueMsg)
	shown := openDialog(t, cmd)
	if body := shown.View(80, 10); !strings.Contains(body, "s3cr3t-value") {
		t.Errorf("the dialog does not show the value:\n%s", body)
	}
	if !strings.Contains(shown.Title(), "nginx:tls:key") {
		t.Errorf("the dialog title %q does not name the path", shown.Title())
	}

	// The view itself still holds nothing: neither the screen nor copy mode.
	if frame := v.Render(90, 20); strings.Contains(frame, "s3cr3t-value") {
		t.Errorf("the screen shows a value after the dialog:\n%s", frame)
	}
	if lines := strings.Join(v.CopyLines(), "\n"); strings.Contains(lines, "s3cr3t-value") {
		t.Errorf("copy mode carries the revealed value:\n%s", lines)
	}

	// The timeline entry names the path and nothing else.
	if len(rec.entries) != 1 {
		t.Fatalf("recorded %d entries, want 1: %+v", len(rec.entries), rec.entries)
	}
	if got := rec.entries[0].Text; got != "pillar value revealed: nginx:tls:key" {
		t.Errorf("entry = %q, want the path only", got)
	}
	if strings.Contains(rec.entries[0].Text, "s3cr3t-value") {
		t.Error("the timeline entry carries the value")
	}
}

// TestPillarRevealRefusedInReadOnly is requirement 3's last clause.
func TestPillarRevealRefusedInReadOnly(t *testing.T) {
	f := pillarFixture()
	env := newPillarEnv(&pillarRecorder{open: true})
	env.Session.ReadOnly = true
	v := newPillarView(t, f, env)
	selectPillar(t, v, "nginx:tls")
	pressPillar(v, "enter")
	selectPillar(t, v, "nginx:tls:key")

	cmd := pressPillar(v, "R")
	if cmd == nil {
		t.Fatal("R answered nothing")
	}
	msg, ok := cmd().(tui.FlashMsg)
	if !ok {
		t.Fatalf("message = %#v, want a flash", cmd())
	}
	if !strings.Contains(msg.Text, "read-only") {
		t.Errorf("flash = %q, want a read-only refusal", msg.Text)
	}
	if f.path != "" {
		t.Errorf("a read-only session read %q", f.path)
	}
}

// TestPillarRevealOnlyOnLeaves: a dict is not a value to reveal.
func TestPillarRevealOnlyOnLeaves(t *testing.T) {
	v := newPillarView(t, pillarFixture(), nil)
	selectPillar(t, v, "nginx")
	cmd := pressPillar(v, "R")
	if cmd == nil {
		t.Fatal("R answered nothing")
	}
	msg, ok := cmd().(tui.FlashMsg)
	if !ok {
		t.Fatalf("message = %#v, want a flash", msg)
	}
	if !strings.Contains(msg.Text, "leaf") {
		t.Errorf("flash = %q, want a leaf-key hint", msg.Text)
	}
}

// TestPillarFilterByKeyPath is requirement 2's `/`.
func TestPillarFilterByKeyPath(t *testing.T) {
	v := newPillarView(t, pillarFixture(), nil)
	if len(v.rows) == 0 {
		t.Fatal("no rows")
	}
	pressPillar(v, "/")
	if !v.Capturing() {
		t.Fatal("the filter does not capture the keyboard")
	}
	typePillar(v, "tls")
	for _, row := range v.rows {
		if !strings.Contains(row.Node.Key, "tls") {
			t.Errorf("row %q does not match the filter", row.Node.Key)
		}
	}
	if len(v.rows) != 3 {
		t.Errorf("rows = %d, want 3 (tls and its two leaves)", len(v.rows))
	}
	pressPillar(v, "esc")
	if v.Capturing() {
		t.Error("esc did not leave the filter")
	}
	if len(v.rows) == 3 {
		t.Errorf("the filter was not cleared: %d rows", len(v.rows))
	}
}

// TestPillarLoadError: a failed read is shown, not hidden.
func TestPillarLoadError(t *testing.T) {
	f := pillarFixture()
	f.treeErr = context.DeadlineExceeded
	v := newPillarView(t, f, nil)
	if v.loadErr == nil {
		t.Fatal("a failed read set no error")
	}
	if frame := v.Render(80, 10); !strings.Contains(frame, "error:") {
		t.Errorf("the banner does not show the error:\n%s", frame)
	}
}
