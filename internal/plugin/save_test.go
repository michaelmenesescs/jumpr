package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveWritesALoadablePlugin is requirement 4: `S` writes
// plugins/<name>.yaml with the command, target host, parse raw and the grouped
// view, and the loader accepts it unchanged.
func TestSaveWritesALoadablePlugin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins")
	command := "nginx -T 2>/dev/null | grep listen\nss -Htan | wc -l"
	path, err := Save(dir, "my-nginx", command, true)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if path != filepath.Join(dir, "my-nginx.yaml") {
		t.Fatalf("path = %q", path)
	}

	defs, problems := LoadDir(dir)
	if len(problems) > 0 {
		t.Fatalf("saved plugin does not load: %v", problems)
	}
	if len(defs) != 1 {
		t.Fatalf("loaded %d plugins, want 1", len(defs))
	}
	def := defs[0]
	if def.Name != "my-nginx" {
		t.Fatalf("name = %q", def.Name)
	}
	if def.Target != TargetHost {
		t.Fatalf("target = %q, want host", def.Target)
	}
	if def.Command != command {
		t.Fatalf("command = %q, want %q", def.Command, command)
	}
	if def.Parse.Type != ParseRaw {
		t.Fatalf("parse type = %q, want raw", def.Parse.Type)
	}
	if !def.View.GroupByOutput {
		t.Fatal("view.group_by_output is not set")
	}
	if !def.Privileged {
		t.Fatal("privileged is not set for a --sudo run")
	}
}

// TestSaveWithoutSudoLeavesPrivilegedOff checks the other half of the sudo rule.
func TestSaveWithoutSudoLeavesPrivilegedOff(t *testing.T) {
	dir := t.TempDir()
	path, err := Save(dir, "plain", "uptime", false)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "privileged") {
		t.Fatalf("privileged written without --sudo:\n%s", data)
	}
}

// TestSaveNeverOverwrites is requirement 4's last line: the file of that name
// belongs to an earlier save and is left alone.
func TestSaveNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path, err := Save(dir, "twice", "uptime", false)
	if err != nil {
		t.Fatalf("first Save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := Save(dir, "twice", "date", false); err == nil {
		t.Fatal("a second save overwrote the first")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the refused save changed the file")
	}
}

// TestSaveRejectsBadInput checks the errors Save hands back to the prompt.
func TestSaveRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, plugin, command string }{
		{"bad name", "Not A Name", "uptime"},
		{"empty name", "", "uptime"},
		{"empty command", "empty-command", "   "},
		{"no dir", "no-dir", "uptime"},
	}
	for _, c := range cases {
		useDir := dir
		if c.name == "no dir" {
			useDir = ""
		}
		if _, err := Save(useDir, c.plugin, c.command, false); err == nil {
			t.Errorf("%s: Save accepted it", c.name)
		}
	}
}

// TestSaveRoundTripsHostileCommand checks that a command containing quotes,
// colons and an indented body still loads back exactly.
func TestSaveRoundTripsHostileCommand(t *testing.T) {
	dir := t.TempDir()
	command := "sh -c 'echo \"a: b\"' | awk '{print $1}'\n  indented line\n\nlast"
	if _, err := Save(dir, "hostile", command, false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	defs, problems := LoadDir(dir)
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(defs) != 1 {
		t.Fatalf("loaded %d definitions", len(defs))
	}
	if defs[0].Command != command {
		t.Fatalf("command round-trip:\n got %q\nwant %q", defs[0].Command, command)
	}
}

// TestSaveSuggestName checks the name the prompt starts with.
func TestSaveSuggestName(t *testing.T) {
	cases := map[string]string{
		"run ss -s on 12 hosts": "run-ss-s-on-12-hosts",
		"Unit health":           "unit-health",
		"  --sudo uptime":       "sudo-uptime",
		"42":                    "run-42",
		"":                      "run",
	}
	for in, want := range cases {
		if got := SuggestName(in); got != want {
			t.Errorf("SuggestName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SuggestName(strings.Repeat("a", 100)); len(got) > saveNameMax {
		t.Errorf("SuggestName kept %d characters, want <= %d", len(got), saveNameMax)
	}
}
