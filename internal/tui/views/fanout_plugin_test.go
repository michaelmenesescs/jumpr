package views

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/plugin"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui"
)

// TestFanoutSaveAsPluginPromptsAndWrites is requirement 4: `S` in a run result
// view asks for a name, writes plugins/<name>.yaml and flashes the path.
func TestFanoutSaveAsPluginPromptsAndWrites(t *testing.T) {
	dir := t.TempDir()
	env := fanoutEnv(transporttest.New())
	env.PluginDir = dir
	f := NewFanout(env, "probe failed-units", transport.Command{Script: "ss -s", Sudo: true}, fanoutHostIDs()[:1], nil)

	msg := f.handleSaveKey(t)
	open, ok := msg.(tui.OpenDialogMsg)
	if !ok {
		t.Fatalf("S returned %T, want a dialog", msg)
	}
	if open.Dialog == nil {
		t.Fatal("S opened no dialog")
	}
	if got := open.Dialog.Title(); got != "save as plugin" {
		t.Fatalf("dialog title = %q", got)
	}
	// The prompt's submit hook is what writes the file; drive it through the
	// dialog keys, exactly as the root would, then run the command it returns.
	dialog := open.Dialog
	for _, r := range "saved-run" {
		dialog, _ = dialog.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	_, submit := dialog.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if submit == nil {
		t.Fatal("enter on the prompt returned no command")
	}
	for _, msg := range runCommands(submit) {
		if flash, ok := msg.(tui.FlashMsg); ok {
			if flash.Level != tui.FlashOK || !strings.Contains(flash.Text, ".yaml") {
				t.Fatalf("flash = %+v, want the saved path", flash)
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read plugin dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote %d files, want 1", len(entries))
	}
	defs, problems := plugin.LoadDir(dir)
	if len(problems) > 0 {
		t.Fatalf("the saved plugin does not load: %v", problems)
	}
	if len(defs) != 1 || defs[0].Command != "ss -s" {
		t.Fatalf("definitions = %+v", defs)
	}
	if !defs[0].Privileged {
		t.Fatal("privileged was not saved for a --sudo run")
	}
	if name := entries[0].Name(); !strings.HasSuffix(name, ".yaml") {
		t.Fatalf("file name = %q", name)
	}
	if !strings.HasPrefix(entries[0].Name(), "probe-failed-units") {
		t.Fatalf("the prompt did not start from the suggested name: %q", entries[0].Name())
	}
}

// TestFanoutSaveAsPluginWithoutCommandExplains checks the after-the-fact view:
// a grouped result built from a finished rollout has no command to save.
func TestFanoutSaveAsPluginWithoutCommandExplains(t *testing.T) {
	env := fanoutEnv(transporttest.New())
	env.PluginDir = t.TempDir()
	f := NewFanoutFromResults(env, "run ss -s", nil)
	msg := f.handleSaveKey(t)
	flash, ok := msg.(tui.FlashMsg)
	if !ok {
		t.Fatalf("S returned %T, want a flash", msg)
	}
	if !strings.Contains(flash.Text, "no command") {
		t.Fatalf("flash = %q", flash.Text)
	}
}

// TestFanoutSaveAsPluginWithoutDirRefuses checks that saving is refused when no
// plugin directory is configured.
func TestFanoutSaveAsPluginWithoutDirRefuses(t *testing.T) {
	env := fanoutEnv(transporttest.New())
	f := NewFanout(env, "probe", transport.Command{Script: "uptime"}, fanoutHostIDs()[:1], nil)
	msg := f.handleSaveKey(t)
	flash, ok := msg.(tui.FlashMsg)
	if !ok {
		t.Fatalf("S returned %T, want a flash", msg)
	}
	if flash.Level != tui.FlashError || !strings.Contains(flash.Text, "plugin directory") {
		t.Fatalf("flash = %+v", flash)
	}
}

// handleSaveKey presses S and runs the tea.Cmd it returns once, so a dialog or a
// flash is what the test sees.
func (f *Fanout) handleSaveKey(t *testing.T) tea.Msg {
	t.Helper()
	_, cmd := f.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")})
	if cmd == nil {
		t.Fatal("S returned no command")
	}
	return cmd()
}

// runCommands runs a tea.Cmd and, when it batches or sequences others, runs
// those too, returning every message. It is how a test reaches the command a
// dialog returned without a whole tea.Program.
func runCommands(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCommands(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}
