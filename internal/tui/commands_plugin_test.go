package tui

import (
	"strings"
	"testing"

	"jumpr/internal/plugin"
)

// fakePluginSource scripts the plugin manager for `:plugins`.
type fakePluginSource struct {
	plugins  []plugin.Plugin
	problems []plugin.Problem
	reloads  int
	next     []plugin.Problem
}

func (f *fakePluginSource) Plugins() []plugin.Plugin { return f.plugins }
func (f *fakePluginSource) Problems() []plugin.Problem {
	return f.problems
}
func (f *fakePluginSource) Reload() []plugin.Problem {
	f.reloads++
	f.problems = f.next
	// A reload re-reads the files: the broken ones come back with problems.
	for i := range f.plugins {
		if len(f.next) > 0 && f.plugins[i].State == plugin.StateError {
			f.plugins[i].Problems = f.next
		}
	}
	return f.next
}

// pluginCommands is a registry with `:plugins` over src.
func pluginCommands(t *testing.T, src PluginSource) *Commands {
	t.Helper()
	c := newTestCommands(t)
	if err := RegisterPluginCommands(c, src); err != nil {
		t.Fatalf("RegisterPluginCommands: %v", err)
	}
	return c
}

// TestPluginsCommandLists is requirement 5: name, target, description, state
// and the file, plus the problems of a broken file with their file and line.
func TestPluginsCommandLists(t *testing.T) {
	src := &fakePluginSource{
		plugins: []plugin.Plugin{
			{Name: "cert-expiry", Target: "host", Description: "cert days", State: plugin.StateLoaded,
				Path: "/home/u/.config/jumpr/plugins/cert-expiry.yaml"},
			{Name: "nginx-status", Target: "host", Description: "nginx summary", State: plugin.StateError,
				Path: "/home/u/.config/jumpr/plugins/nginx-status.yaml",
				Problems: []plugin.Problem{{
					File: "/home/u/.config/jumpr/plugins/nginx-status.yaml",
					Line: 7, Field: "parse.type", Msg: "parse type \"xml\" must be one of json, jq, regex, columns, kv, lines, raw",
				}}},
		},
		problems: []plugin.Problem{{
			File: "/home/u/.config/jumpr/plugins/nginx-status.yaml",
			Line: 7, Field: "parse.type", Msg: "parse type \"xml\" must be one of json, jq, regex, columns, kv, lines, raw",
		}},
	}
	cmds := pluginCommands(t, src)
	cmd, ok := cmds.Lookup("plugins")
	if !ok {
		t.Fatal(":plugins is not registered")
	}
	msg := cmd.Run(newTestEnv(t), "")()
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf(":plugins returned %T, want PushViewMsg", msg)
	}
	text := strings.Join(push.View.CopyLines(), "\n")
	for _, want := range []string{
		"NAME", "TARGET", "STATE", "DESCRIPTION",
		"cert-expiry", "cert days", plugin.StateLoaded,
		"nginx-status", "nginx summary", plugin.StateError,
		"/home/u/.config/jumpr/plugins/nginx-status.yaml:7",
		"parse.type",
	} {
		if !strings.Contains(text, want) {
			t.Errorf(":plugins is missing %q:\n%s", want, text)
		}
	}
	if push.View.Crumb() != "plugins" {
		t.Errorf("crumb = %q, want plugins", push.View.Crumb())
	}
}

// TestPluginsCommandReload re-reads the directory off the UI goroutine and
// reports what happened.
func TestPluginsCommandReload(t *testing.T) {
	src := &fakePluginSource{
		plugins: []plugin.Plugin{{Name: "nginx-status", Target: "host", State: plugin.StateLoaded}},
		next: []plugin.Problem{{
			File: "/p/nginx-status.yaml", Line: 4, Field: "target", Msg: "target \"hostt\" must be one of host",
		}},
	}
	cmds := pluginCommands(t, src)
	cmd, _ := cmds.Lookup("plugins")
	msg := cmd.Run(newTestEnv(t), "reload")()
	flash, ok := msg.(FlashMsg)
	if !ok {
		t.Fatalf("reload returned %T, want FlashMsg", msg)
	}
	if src.reloads != 1 {
		t.Fatalf("reload ran %d times", src.reloads)
	}
	if flash.Level != FlashWarn {
		t.Fatalf("level = %v, want warn", flash.Level)
	}
	if !strings.Contains(flash.Text, "1 problem") {
		t.Fatalf("flash = %q", flash.Text)
	}
}

// TestPluginsCommandUsage checks the argument grammar and the empty case.
func TestPluginsCommandUsage(t *testing.T) {
	cmds := pluginCommands(t, &fakePluginSource{})
	cmd, _ := cmds.Lookup("plugins")
	msg := cmd.Run(newTestEnv(t), "banana")()
	flash, ok := msg.(FlashMsg)
	if !ok || flash.Level != FlashError {
		t.Fatalf("bad argument returned %T %+v", msg, msg)
	}
	if !strings.Contains(flash.Text, "usage: :plugins") {
		t.Fatalf("flash = %q", flash.Text)
	}
}

// TestPluginsCommandWithoutSource lists nothing rather than failing.
func TestPluginsCommandWithoutSource(t *testing.T) {
	cmds := pluginCommands(t, nil)
	cmd, _ := cmds.Lookup("plugins")
	msg := cmd.Run(newTestEnv(t), "")()
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf(":plugins returned %T, want PushViewMsg", msg)
	}
	text := strings.Join(push.View.CopyLines(), "\n")
	if !strings.Contains(text, "no plugin directory configured") {
		t.Fatalf("text = %q", text)
	}
	if msg := cmd.Run(newTestEnv(t), "reload")(); msg == nil {
		t.Fatal("reload without a source returned nil")
	}
}
