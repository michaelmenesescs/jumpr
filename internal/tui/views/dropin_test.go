package views

import (
	"context"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/investigation"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/tui"
)

// dropInClip is a clipboard that remembers what it was given.
type dropInClip struct {
	mu   sync.Mutex
	text []string
}

func (c *dropInClip) Copy(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = append(c.text, text)
	return nil
}

func (c *dropInClip) copied() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.text...)
}

// dropInExec is an Executor that remembers every command it was handed. A
// drop-in preview must never hand it one.
type dropInExec struct {
	mu    sync.Mutex
	calls []transport.Command
}

func (e *dropInExec) Name() string { return "drop-in-recorder" }

func (e *dropInExec) Exec(_ context.Context, _ resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, cmd)
	return transport.ExecResult{}, nil
}

func (e *dropInExec) commands() []transport.Command {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]transport.Command(nil), e.calls...)
}

// dropInKey is the key message a terminal delivers for key.
func dropInKey(key string) tea.KeyMsg {
	switch key {
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	}
	return keyMsg(key)
}

// dropInSend routes one key to the view and returns the command it asked for.
func dropInSend(t *testing.T, d *DropIn, key string) tea.Cmd {
	t.Helper()
	v, cmd := d.Update(dropInKey(key))
	if _, ok := v.(*DropIn); !ok {
		t.Fatalf("Update returned %T, want *DropIn", v)
	}
	return cmd
}

// dropInRun executes a command and everything it batches, returning the
// messages the view would receive.
func dropInRun(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var msgs []tea.Msg
	for i := 0; cmd != nil; i++ {
		if i > 100 {
			t.Fatal("commands did not settle")
		}
		msg := cmd()
		if msg == nil {
			return msgs
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c != nil {
					msgs = append(msgs, dropInRun(t, c)...)
				}
			}
			return msgs
		}
		msgs = append(msgs, msg)
		return msgs
	}
	return msgs
}

// dropInCat is a `systemctl cat` output with an existing override.
const dropInCat = `# /usr/lib/systemd/system/nginx.service
[Unit]
Description=nginx

# /etc/systemd/system/nginx.service.d/override.conf
[Service]
LimitNOFILE=65535`

// newDropInView builds the editor over the fixture's cat output.
func newDropInView(t *testing.T) *DropIn {
	t.Helper()
	return NewDropIn(testEnv(), "web-01", "nginx.service", dropInCat)
}

// preview builds the preview and returns the messages the commands produced.
func preview(t *testing.T, d *DropIn) []tea.Msg {
	t.Helper()
	if cmd := dropInSend(t, d, "ctrl+s"); cmd != nil {
		return dropInRun(t, cmd)
	}
	return nil
}

// TestDropInPrefillsExistingOverride checks the editor starts from the
// override.conf the cat output already carries, and from [Service] otherwise.
func TestDropInPrefillsExistingOverride(t *testing.T) {
	d := newDropInView(t)
	got := d.Value()
	if !strings.HasPrefix(got, "[Service]") {
		t.Errorf("draft = %q, want it to start with [Service]", got)
	}
	if !strings.Contains(got, "LimitNOFILE=65535") {
		t.Errorf("draft = %q, want the existing override", got)
	}
	if !d.existing {
		t.Error("existing = false, the cat output has the drop-in")
	}

	fresh := NewDropIn(testEnv(), "web-01", "nginx.service", "# /usr/lib/systemd/system/nginx.service\n[Unit]\nDescription=nginx")
	if got := fresh.Value(); strings.TrimSpace(got) != strings.TrimSpace(dropInDefault) {
		t.Errorf("draft of a unit without an override = %q, want %q", got, dropInDefault)
	}
	if fresh.existing {
		t.Error("existing = true for a unit without a drop-in")
	}
}

// TestDropInPreviewShowsDiffAndBanner is the acceptance behaviour: ctrl+s shows
// the banner, the diff against `systemctl cat` and the block to apply by hand.
func TestDropInPreviewShowsDiffAndBanner(t *testing.T) {
	d := newDropInView(t)
	d.SetValue("[Service]\nLimitNOFILE=65535\nNice=5\n")
	preview(t, d)

	if !d.Previewing() {
		t.Fatal("ctrl+s did not switch to the preview")
	}
	frame := stripHostANSI(d.Render(110, 40))
	for _, want := range []string{
		dropInBanner,
		"nginx.service.d/override.conf",
		"warnings",
		"+Nice=5",
		"@@",
		"to apply by hand",
		"sudo systemctl edit nginx.service",
		"sudo systemctl daemon-reload",
		"sudo systemctl restart nginx.service",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("preview does not contain %q:\n%s", want, frame)
		}
	}
	if p := d.Preview(); p.Unit != "nginx.service" {
		t.Errorf("preview is for %q", p.Unit)
	}
}

// TestDropInPreviewWarns checks the warnings reach the screen.
func TestDropInPreviewWarns(t *testing.T) {
	d := newDropInView(t)
	d.SetValue("[Servce]\nNice 5\n")
	preview(t, d)

	frame := stripHostANSI(d.Render(110, 30))
	for _, want := range []string{"unknown section [Servce]", "not a key=value line"} {
		if !strings.Contains(frame, want) {
			t.Errorf("preview does not warn %q:\n%s", want, frame)
		}
	}
}

// TestDropInYankCopiesApplyBlock checks `y` hands the whole block to the
// clipboard.
func TestDropInYankCopiesApplyBlock(t *testing.T) {
	d := newDropInView(t)
	clip := &dropInClip{}
	d.SetClipboard(clip)
	d.SetValue("[Service]\nNice=5\n")
	preview(t, d)

	msgs := dropInRun(t, dropInSend(t, d, "y"))
	if len(clip.copied()) != 1 {
		t.Fatalf("clipboard got %d copies, want 1", len(clip.copied()))
	}
	block := clip.copied()[0]
	for _, want := range []string{
		"to apply by hand",
		"sudo systemctl edit nginx.service",
		"sudo tee /etc/systemd/system/nginx.service.d/override.conf >/dev/null <<'EOF'",
		"sudo systemctl daemon-reload",
		"sudo systemctl restart nginx.service",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("copied block does not contain %q:\n%s", want, block)
		}
	}
	if len(msgs) != 1 {
		t.Fatalf("y produced %d messages, want one flash", len(msgs))
	}
	if flash, ok := msgs[0].(tui.FlashMsg); !ok || flash.Level != tui.FlashOK {
		t.Errorf("y produced %#v, want an OK flash", msgs[0])
	}

	// Without a clipboard it flashes instead of panicking.
	bare := newDropInView(t)
	preview(t, bare)
	msgs = dropInRun(t, dropInSend(t, bare, "y"))
	if flash, ok := msgs[0].(tui.FlashMsg); !ok || flash.Level != tui.FlashWarn {
		t.Errorf("y without a clipboard produced %#v, want a warning flash", msgs)
	}
}

// TestDropInRecordsNote checks the preview is recorded as a note entry with the
// draft and the diff attached, and only while an investigation is open.
func TestDropInRecordsNote(t *testing.T) {
	rec := &fakeRecorder{open: true}
	env := testEnv()
	env.Recorder = rec
	d := NewDropIn(env, "web-01", "nginx.service", dropInCat)
	d.SetValue("[Service]\nLimitNOFILE=65535\nNice=5\n")
	preview(t, d)

	got := rec.seen()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Kind != investigation.EntryNote {
		t.Errorf("kind = %q, want note", got[0].Kind)
	}
	if !strings.Contains(got[0].Text, "nginx.service") {
		t.Errorf("text = %q, want the unit named", got[0].Text)
	}
	if !strings.Contains(got[0].Output, "+Nice=5") || !strings.Contains(got[0].Output, "LimitNOFILE=65535") {
		t.Errorf("output does not attach the draft and the diff:\n%s", got[0].Output)
	}

	closed := &fakeRecorder{}
	env2 := testEnv()
	env2.Recorder = closed
	d2 := NewDropIn(env2, "web-01", "nginx.service", dropInCat)
	d2.SetValue("[Service]\nNice=5\n")
	preview(t, d2)
	if n := len(closed.seen()); n != 0 {
		t.Errorf("entries = %d, want none while no investigation is open", n)
	}
}

// TestDropInEscGoesBack checks esc returns to the editor and, from the editor,
// closes the view.
func TestDropInEscGoesBack(t *testing.T) {
	d := newDropInView(t)
	preview(t, d)
	dropInSend(t, d, "esc")
	if d.Previewing() {
		t.Fatal("esc did not return to the editor")
	}
	msgs := dropInRun(t, dropInSend(t, d, "esc"))
	if len(msgs) != 1 {
		t.Fatalf("esc produced %d messages, want one", len(msgs))
	}
	if _, ok := msgs[0].(tui.PopViewMsg); !ok {
		t.Errorf("esc from the editor produced %#v, want PopViewMsg", msgs[0])
	}
}

// TestDropInNeverRunsACommand is the acceptance's second half: editing and
// previewing hands the executor nothing, so nothing write-like can happen.
func TestDropInNeverRunsACommand(t *testing.T) {
	exec := &dropInExec{}
	env := testEnv()
	env.Exec = exec
	d := NewDropIn(env, "web-01", "nginx.service", dropInCat)
	d.SetClipboard(&dropInClip{})

	d.SetValue("[Service]\nNice=5\n")
	preview(t, d)
	dropInRun(t, dropInSend(t, d, "y"))
	dropInSend(t, d, "esc")

	if calls := exec.commands(); len(calls) != 0 {
		t.Fatalf("the view ran %d commands, want none: %+v", len(calls), calls)
	}
}

// TestDropInOpensFromTheCatTab checks `E` on the cat tab pushes the editor, and
// that the other tabs keep their own keys.
func TestDropInOpensFromTheCatTab(t *testing.T) {
	u, _ := unitFixture(t)

	// On the status tab `E` is not ours.
	if cmd := sendUnitKey(t, u, "E"); cmd != nil {
		t.Errorf("E on the status tab returned a command")
	}

	sendUnitKey(t, u, "3")
	cmds := dropInRun(t, sendUnitKey(t, u, "E"))
	if len(cmds) != 1 {
		t.Fatalf("E produced %d messages, want one", len(cmds))
	}
	push, ok := cmds[0].(tui.PushViewMsg)
	if !ok {
		t.Fatalf("E produced %#v, want PushViewMsg", cmds[0])
	}
	d, ok := push.View.(*DropIn)
	if !ok {
		t.Fatalf("pushed %T, want *DropIn", push.View)
	}
	if !strings.Contains(d.Value(), "LimitNOFILE=65535") {
		t.Errorf("the editor did not start from the loaded cat output: %q", d.Value())
	}
	if d.Subject().Unit != "nginx.service" || d.Crumb() != "drop-in nginx.service" {
		t.Errorf("crumb/subject = %q / %+v", d.Crumb(), d.Subject())
	}
}
