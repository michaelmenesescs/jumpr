package app

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"jumpr/internal/bus"
	"jumpr/internal/config"
)

// initFileNames are the files `jumpr init` is required to write.
var initFileNames = []string{"config.yaml", "connections.yaml", "policy.yaml", "keymap.yaml"}

// runInit writes the example configuration into home/.config/jumpr.
func runInit(t *testing.T, home string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := Init(Options{Home: home}, &out)
	return code, out.String()
}

// TestInitWritesLoadableConfig writes the templates and loads every one of
// them with the real loader: config, connection profiles and policy. The
// config that comes back must be exactly config.Default().
func TestInitWritesLoadableConfig(t *testing.T) {
	home := t.TempDir()
	code, out := runInit(t, home)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	dir := filepath.Join(home, ".config", "jumpr")
	for _, name := range initFileNames {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
		if !strings.Contains(out, "created  "+path) {
			t.Errorf("report does not mention %s:\n%s", path, out)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "plugins")); err != nil || !info.IsDir() {
		t.Fatalf("plugins/ was not created (err %v)", err)
	}

	// config.yaml: every key at its default, so a fresh install behaves like
	// the built-in defaults and the loader's strict decoding accepts the file.
	got, err := config.Load(filepath.Join(dir, "config.yaml"), home)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	want := config.Default()
	want.SSH.ConfigFiles = []string{filepath.Join(home, ".ssh", "config")}
	want.Knowledge.Root = filepath.Join(home, "jumpr")
	if gotYAML, wantYAML := marshalConfig(t, got), marshalConfig(t, want); gotYAML != wantYAML {
		t.Errorf("generated config.yaml differs from config.Default():\n--- got ---\n%s\n--- want ---\n%s",
			gotYAML, wantYAML)
	}

	// connections.yaml: no profiles, valid strict YAML.
	profiles, err := config.LoadConnections(filepath.Join(dir, "connections.yaml"), home)
	if err != nil {
		t.Fatalf("LoadConnections: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("generated connections.yaml has %d profiles, want 0", len(profiles))
	}

	// policy.yaml: loads, with the documented default shape.
	policy, err := bus.LoadPolicy(filepath.Join(dir, "policy.yaml"))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if policy.ReadOnly() {
		t.Error("generated policy is read-only")
	}
	if got := policy.SyncRaw(); got != "confirm" {
		t.Errorf("sync.raw = %q, want confirm", got)
	}

	// keymap.yaml: a map of action name to key list, with the built-in keys.
	data, err := os.ReadFile(filepath.Join(dir, "keymap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string][]string
	if err := yaml.Unmarshal(data, &keys); err != nil {
		t.Fatalf("keymap.yaml is not valid YAML: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("keymap.yaml has no bindings")
	}
	if got := keys["up"]; !reflect.DeepEqual(got, []string{"k", "up"}) {
		t.Errorf("keymap up = %v, want [k up]", got)
	}
}

// marshalConfig renders a Config as YAML for comparison.
func marshalConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return string(data)
}

// TestInitNeverOverwrites runs init twice and proves an existing file, and an
// existing edit, survive untouched.
func TestInitNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	if code, out := runInit(t, home); code != 0 {
		t.Fatalf("first run: exit code = %d\n%s", code, out)
	}
	dir := filepath.Join(home, ".config", "jumpr")
	configPath := filepath.Join(dir, "config.yaml")
	const mine = "ui:\n  color: 256\n"
	if err := os.WriteFile(configPath, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runInit(t, home)
	if code != 0 {
		t.Fatalf("second run: exit code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "skipped  "+configPath+" (already exists)") {
		t.Errorf("second run does not report the skipped file:\n%s", out)
	}
	if strings.Contains(out, "created  "+configPath) {
		t.Errorf("second run claims it created an existing file:\n%s", out)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != mine {
		t.Errorf("config.yaml was overwritten:\n%s", data)
	}
	for _, name := range initFileNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s disappeared: %v", name, err)
		}
	}
}

// TestInitConfigTemplateKeys compares the template's key paths against the
// keys of config.Default(): a key added to the defaults without the template
// (or the other way round) fails here, which is what keeps init honest.
func TestInitConfigTemplateKeys(t *testing.T) {
	template := readTemplate(t, "config.yaml")
	home := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, template, 0o600); err != nil {
		t.Fatal(err)
	}

	// A template with an unknown key fails the strict loader.
	if _, err := config.Load(path, home); err != nil {
		t.Fatalf("config.Load on the template: %v", err)
	}

	var gotTree, wantTree map[string]any
	if err := yaml.Unmarshal(template, &gotTree); err != nil {
		t.Fatalf("template is not valid YAML: %v", err)
	}
	wantYAML, err := yaml.Marshal(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(wantYAML, &wantTree); err != nil {
		t.Fatal(err)
	}

	// Only the sections of BUILD-PLAN §11 are required in the template; the
	// optional backend lists are documented as comments at the end.
	gotKeys := keyPaths(gotTree, "")
	wantKeys := make([]string, 0, 64)
	for _, k := range keyPaths(wantTree, "") {
		if isOptionalSection(k) {
			continue
		}
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(wantKeys)

	for _, k := range wantKeys {
		if !containsString(gotKeys, k) {
			t.Errorf("template is missing key %q", k)
		}
	}
	for _, k := range gotKeys {
		if isOptionalSection(k) || !containsString(wantKeys, k) {
			t.Errorf("template has key %q that config.Default() does not define", k)
		}
	}
}

// isOptionalSection reports whether a key path belongs to a section §11 does
// not list (the HTTP backends, added by later tasks).
func isOptionalSection(path string) bool {
	for _, name := range []string{"prometheus", "alertmanager", "grafana", "elasticsearch", "ssh.transports"} {
		if path == name || strings.HasPrefix(path, name+".") || strings.HasPrefix(path, name+"[") {
			return true
		}
	}
	return false
}

// keyPaths flattens a YAML tree into dotted key paths; list indices become
// "key[]" because only the shape matters here.
func keyPaths(tree map[string]any, prefix string) []string {
	var out []string
	for key, value := range tree {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		out = append(out, path)
		switch v := value.(type) {
		case map[string]any:
			out = append(out, keyPaths(v, path)...)
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					out = append(out, keyPaths(m, path+"[]")...)
				}
			}
		}
	}
	return out
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// readTemplate returns one embedded template.
func readTemplate(t *testing.T, name string) []byte {
	t.Helper()
	data, err := initTemplates.ReadFile("templates/" + name)
	if err != nil {
		t.Fatalf("template %s: %v", name, err)
	}
	return data
}
