package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

// pluginYAML is one valid plugin file. name and body let a test swap the
// description (a harmless edit) or break the file.
func pluginYAML(name, description string) string {
	return `apiVersion: jumpr/v1
kind: Plugin
name: ` + name + `
description: ` + description + `
target: host
command: uptime
parse:
  type: lines
view:
  columns: [line]
`
}

// writePlugin writes body as dir/name.yaml.
func writePlugin(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// newReloadManager builds a manager over dir with an empty registry.
func newReloadManager(t *testing.T, dir string) (*Manager, *backend.Registry) {
	t.Helper()
	reg := backend.NewRegistry()
	m, err := NewManager(ManagerOptions{
		Dir:      dir,
		Registry: reg,
		Exec:     transporttest.New(),
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, reg
}

// TestReloadRegistersEachPlugin checks that a reload turns every host plugin
// into a kind of the registry, named after the plugin.
func TestReloadRegistersEachPlugin(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "nginx-status", pluginYAML("nginx-status", "nginx summary"))
	writePlugin(t, dir, "cert-expiry", pluginYAML("cert-expiry", "cert days"))
	m, reg := newReloadManager(t, dir)

	if problems := m.Load(); len(problems) > 0 {
		t.Fatalf("load problems: %v", problems)
	}
	for _, kind := range []string{"nginx-status", "cert-expiry"} {
		b, spec, ok := reg.Lookup(kind)
		if !ok {
			t.Fatalf("kind %q is not registered", kind)
		}
		if string(spec.Name) != resourceKind(kind) {
			t.Fatalf("kind %q resolved to %q", kind, spec.Name)
		}
		if b.Name() != kind {
			t.Fatalf("backend name = %q, want %q", b.Name(), kind)
		}
	}
	if got := len(m.Plugins()); got != 2 {
		t.Fatalf("Plugins() = %d entries, want 2", got)
	}
}

// TestReloadSwapsAChangedPlugin checks requirement 3's happy path: an edited
// file is re-registered through Registry.Replace, so the old version is gone.
func TestReloadSwapsAChangedPlugin(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "nginx-status", pluginYAML("nginx-status", "first"))
	m, reg := newReloadManager(t, dir)
	if problems := m.Load(); len(problems) > 0 {
		t.Fatalf("load problems: %v", problems)
	}

	writePlugin(t, dir, "nginx-status", pluginYAML("nginx-status", "second"))
	if problems := m.Reload(); len(problems) > 0 {
		t.Fatalf("reload problems: %v", problems)
	}
	b, _, ok := reg.Lookup("nginx-status")
	if !ok {
		t.Fatal("the kind disappeared after a reload")
	}
	pb, ok := b.(*Backend)
	if !ok {
		t.Fatalf("backend is %T", b)
	}
	if pb.Definition().Description != "second" {
		t.Fatalf("description = %q, want the reloaded one", pb.Definition().Description)
	}
}

// TestReloadKeepsLastGoodVersionOnABrokenFile is requirement 3's safety rule: a
// broken edit is reported with file and line, and the version that worked stays
// registered.
func TestReloadKeepsLastGoodVersionOnABrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := writePlugin(t, dir, "nginx-status", pluginYAML("nginx-status", "first"))
	m, reg := newReloadManager(t, dir)
	if problems := m.Load(); len(problems) > 0 {
		t.Fatalf("load problems: %v", problems)
	}

	// An unknown field on line 3: the classic hand-edit mistake.
	writePlugin(t, dir, "nginx-status", `apiVersion: jumpr/v1
kind: Plugin
nmae: nginx-status
target: host
command: uptime
parse:
  type: lines
`)
	problems := m.Reload()
	if len(problems) == 0 {
		t.Fatal("a broken file reloaded without a problem")
	}
	first := problems[0]
	if first.File != path {
		t.Fatalf("problem file = %q, want %q", first.File, path)
	}
	if first.Line < 1 {
		t.Fatalf("problem line = %d, want the line of the mistake", first.Line)
	}

	b, _, ok := reg.Lookup("nginx-status")
	if !ok {
		t.Fatal("a broken edit unregistered the kind")
	}
	pb, ok := b.(*Backend)
	if !ok || pb.Definition().Description != "first" {
		t.Fatalf("the last good version is not the one serving: %+v", b)
	}

	entries := m.Plugins()
	if len(entries) != 1 {
		t.Fatalf("Plugins() = %+v, want the one broken file", entries)
	}
	if entries[0].State != StateError {
		t.Fatalf("state = %q, want %q", entries[0].State, StateError)
	}
	if len(entries[0].Problems) == 0 || entries[0].Problems[0].Line < 1 {
		t.Fatalf("the listing lost the problems: %+v", entries[0])
	}
}

// TestReloadListsAFileThatNeverLoaded checks that a plugin added with a mistake
// is still visible, with its path and line.
func TestReloadListsAFileThatNeverLoaded(t *testing.T) {
	dir := t.TempDir()
	path := writePlugin(t, dir, "half-written", `apiVersion: jumpr/v1
kind: Plugin
name: half-written
target: host
`)
	m, _ := newReloadManager(t, dir)
	problems := m.Load()
	if len(problems) == 0 {
		t.Fatal("an incomplete plugin loaded")
	}
	entries := m.Plugins()
	if len(entries) != 1 {
		t.Fatalf("Plugins() = %+v, want one entry", entries)
	}
	if entries[0].Name != "half-written" {
		t.Fatalf("name = %q", entries[0].Name)
	}
	if entries[0].Path != path {
		t.Fatalf("path = %q, want %q", entries[0].Path, path)
	}
	if entries[0].State != StateError {
		t.Fatalf("state = %q", entries[0].State)
	}
}

// TestReloadKeepsADeletedPluginServing documents the choice: a plugin whose file
// is removed keeps serving until the session ends, because the registry has no
// Unregister.
func TestReloadKeepsADeletedPluginServing(t *testing.T) {
	dir := t.TempDir()
	path := writePlugin(t, dir, "nginx-status", pluginYAML("nginx-status", "first"))
	m, reg := newReloadManager(t, dir)
	if problems := m.Load(); len(problems) > 0 {
		t.Fatalf("load problems: %v", problems)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if problems := m.Reload(); len(problems) > 0 {
		t.Fatalf("reload problems: %v", problems)
	}
	if _, _, ok := reg.Lookup("nginx-status"); !ok {
		t.Fatal("a removed file unregistered the kind")
	}
	if len(m.Plugins()) != 1 {
		t.Fatalf("Plugins() = %+v", m.Plugins())
	}
}

// TestReloadReportsANameCollision checks that a plugin named like a built-in
// kind is reported rather than taking the kind over.
func TestReloadReportsANameCollision(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "uptime", pluginYAML("uptime", "collides with a host kind"))
	reg := backend.NewRegistry()
	if err := reg.Register(fakeHostBackend{}); err != nil {
		t.Fatalf("register built-in: %v", err)
	}
	m, err := NewManager(ManagerOptions{Dir: dir, Registry: reg, Exec: transporttest.New()})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	problems := m.Load()
	if len(problems) == 0 {
		t.Fatal("a plugin colliding with a built-in kind loaded")
	}
	if !strings.Contains(problems[0].Msg, "already registered") {
		t.Fatalf("problem = %q", problems[0].Msg)
	}
}

// TestReloadSkipsNonHostPlugins checks that an HTTP or inventory plugin is
// listed but claims no host kind (that path belongs to its own track).
func TestReloadSkipsNonHostPlugins(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "lb-pool-members", `apiVersion: jumpr/v1
kind: Plugin
name: lb-pool-members
description: load balancer members
target: http
request:
  method: GET
  url: https://lb-api.example.internal/v1/pools/web/members
parse:
  type: json
view:
  columns: [host]
emits_hosts: host
`)
	m, reg := newReloadManager(t, dir)
	if problems := m.Load(); len(problems) > 0 {
		t.Fatalf("a http plugin is not a problem: %v", problems)
	}
	if _, _, ok := reg.Lookup("lb-pool-members"); ok {
		t.Fatal("a http plugin registered a host kind")
	}
	entries := m.Plugins()
	if len(entries) != 1 || entries[0].State != StateNotKind {
		t.Fatalf("Plugins() = %+v", entries)
	}
}

// resourceKind is the kind name of a plugin, as a string for the assertions.
func resourceKind(name string) string { return name }

// fakeHostBackend is a built-in backend that owns the "uptime" kind, so the
// collision test has something to collide with.
type fakeHostBackend struct{}

func (fakeHostBackend) Name() string { return "builtin" }
func (fakeHostBackend) Kinds() []resource.KindSpec {
	return []resource.KindSpec{{Name: "uptime", Title: "uptime", Scope: resource.ScopeGlobal}}
}
func (fakeHostBackend) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result)
	close(ch)
	return ch, nil
}
func (fakeHostBackend) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, nil
}
func (fakeHostBackend) Links(resource.Resource) []resource.Link { return nil }
func (fakeHostBackend) Health(context.Context) backend.Health   { return backend.Health{} }
