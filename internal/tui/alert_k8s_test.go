package tui_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/investigation"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// k8sAlertRow is one alert whose labels name a Kubernetes namespace and pod.
func k8sAlertRow() resource.Resource {
	return resource.Resource{
		Ref: resource.Ref{Backend: "am", Kind: "alert", Name: "PodCrashLooping"},
		Fields: map[string]any{
			"alert":  "PodCrashLooping",
			"start":  alertNow.Add(-10 * time.Minute),
			"state":  "firing",
			"labels": "namespace=prod pod=api-7d9f",
		},
		Labels: map[string]string{
			"alertname": "PodCrashLooping",
			"namespace": "prod",
			"pod":       "api-7d9f",
			"severity":  "critical",
		},
	}
}

// alertBackend serves the one Kubernetes alert.
type alertBackend struct{}

func (alertBackend) Name() string { return "am" }

func (alertBackend) Kinds() []resource.KindSpec {
	return []resource.KindSpec{{
		Name:    "alert",
		Aliases: []string{"al"},
		Title:   "Alerts",
		Columns: []resource.Column{
			{ID: "alert", Title: "ALERT"},
			{ID: "state", Title: "STATE", Width: 8},
		},
		DefaultSort: "alert",
	}}
}

func (alertBackend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result, 1)
	ch <- backend.Result{Items: []resource.Resource{k8sAlertRow()}, Done: 1, Total: 1}
	close(ch)
	return ch, nil
}

func (alertBackend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	return k8sAlertRow(), nil
}

func (alertBackend) Links(resource.Resource) []resource.Link { return nil }

func (alertBackend) Health(context.Context) backend.Health {
	return backend.Health{State: backend.HealthOK}
}

// k8sAlertBackend is the Kubernetes backend as the alert flow sees it: it lists
// the pods of a query and resolves their nodes to inventory hosts.
type k8sAlertBackend struct {
	alertBackend
	resolved [][]resource.HostID
	mu       sync.Mutex
	queries  []resource.Query
}

// seenQueries is the queries AlertNodes was called with.
func (b *k8sAlertBackend) seenQueries() []resource.Query {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]resource.Query(nil), b.queries...)
}

func (b *k8sAlertBackend) Name() string { return "k8s" }

func (b *k8sAlertBackend) Kinds() []resource.KindSpec {
	return []resource.KindSpec{{
		Name:    "pod",
		Aliases: []string{"po"},
		Title:   "Pods",
		Scope:   resource.ScopeNamespace,
		Columns: []resource.Column{
			{ID: "namespace", Title: "NAMESPACE", Width: 12},
			{ID: "name", Title: "NAME"},
			{ID: "node", Title: "NODE", Width: 10},
		},
		DefaultSort: "name",
	}}
}

func (b *k8sAlertBackend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	pod := resource.Resource{
		Ref:    resource.Ref{Backend: "k8s", Kind: "pod", Namespace: "prod", Name: "api-7d9f"},
		Fields: map[string]any{"namespace": "prod", "name": "api-7d9f", "node": "k3s-1"},
	}
	ch := make(chan backend.Result, 1)
	ch <- backend.Result{Items: []resource.Resource{pod}, Done: 1, Total: 1}
	close(ch)
	return ch, nil
}

func (b *k8sAlertBackend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	ch, err := b.List(ctx, resource.Query{Kind: ref.Kind})
	if err != nil {
		return resource.Resource{}, err
	}
	for res := range ch {
		return res.Items[0], nil
	}
	return resource.Resource{}, nil
}

// AlertNodes is requirement 4: the pods of the query resolve to the nodes they
// run on, as inventory hosts.
func (b *k8sAlertBackend) AlertNodes(ctx context.Context, q resource.Query) ([]resource.HostID, string, error) {
	b.mu.Lock()
	b.queries = append(b.queries, q)
	b.mu.Unlock()
	if len(b.resolved) == 0 {
		return nil, "the alert's pods are not running", nil
	}
	return b.resolved[0], "", nil
}

// alertK8sHarness is a root over the two fake backends, a one-node inventory and
// a real investigation record on disk.
type alertK8sHarness struct {
	root *tui.Root
	env  *tui.Env
	k8s  *k8sAlertBackend
	dir  string
}

func newAlertK8sHarness(t *testing.T) *alertK8sHarness {
	t.Helper()
	theme.SetProfile("none")
	dir := t.TempDir()
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	log := investigation.NewLog(dir, red, func() time.Time { return alertNow })

	reg := backend.NewRegistry()
	if err := reg.Register(alertBackend{}); err != nil {
		t.Fatalf("register alert backend: %v", err)
	}
	k8sBackend := &k8sAlertBackend{resolved: [][]resource.HostID{{"k3s-1.lab.example.internal"}}}
	if err := reg.Register(k8sBackend); err != nil {
		t.Fatalf("register k8s backend: %v", err)
	}
	env := &tui.Env{
		Registry:  reg,
		Inventory: newAlertInventory(),
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       func() time.Time { return alertNow },
		Factories: make(map[resource.KindName]tui.Factory),
		Recorder:  log,
	}
	for _, spec := range reg.Kinds() {
		env.Factories[spec.Name] = tableFactory
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	if err := tui.RegisterInvestigationCommands(cmds); err != nil {
		t.Fatalf("register investigation commands: %v", err)
	}
	root := tui.NewRoot(env, cmds, tableFactory, resource.Query{Kind: "alert"}, "ctrl+a")
	return &alertK8sHarness{root: root, env: env, k8s: k8sBackend, dir: dir}
}

// TestAlertK8sOpensPodsAndNodes is requirement 4: an alert with namespace and
// pod labels opens the pods view filtered to them, still creates the
// investigation, and the investigation targets are the nodes of those pods.
func TestAlertK8sOpensPodsAndNodes(t *testing.T) {
	h := newAlertK8sHarness(t)
	tm := start(t, h.root)
	waitFor(t, tm, "PodCrashLooping")

	openAlertRow(t, tm, "PodCrashLooping")
	waitFor(t, tm, "api-7d9f", "k3s-1", "inv:")

	paths, err := filepath.Glob(filepath.Join(h.dir, "investigations", "*.md"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("investigation files = %v, %v; want exactly one", paths, err)
	}
	file := waitFile(t, paths[0], "PodCrashLooping", "k3s-1.lab.example.internal")
	if !strings.Contains(file, "**open**") {
		t.Fatalf("record has no open entry:\n%s", file)
	}

	screen := finish(t, tm)
	for _, want := range []string{"api-7d9f", "inv:", "targets:1"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("final screen is missing %q:\n%s", want, screen)
		}
	}

	// The pods were read once, for the alert's namespace and pod.
	if queries := h.k8s.seenQueries(); len(queries) != 1 {
		t.Fatalf("AlertNodes calls = %d, want 1 (the pods are read once)", len(queries))
	} else if q := queries[0]; q.Kind != "pod" || q.Args["namespace"] != "prod" || q.Args["name"] != "api-7d9f" {
		t.Fatalf("query = %+v, want the prod pods named api-7d9f", q)
	}

	// The program is stopped, so the session state is safe to read here.
	cur := h.env.Session.Investigation
	if cur == nil {
		t.Fatal("no investigation was opened")
	}
	if len(cur.Targets) != 1 || cur.Targets[0] != "k3s-1.lab.example.internal" {
		t.Fatalf("investigation targets = %v, want the node's host", cur.Targets)
	}
	if got := h.env.Session.Targets; len(got.Hosts) != 1 || got.Hosts[0] != "k3s-1.lab.example.internal" {
		t.Fatalf("session targets = %+v, want the node's host", got)
	}
	if h.env.Session.Targets.Label != "PodCrashLooping" {
		t.Fatalf("target label = %q, want the alert name", h.env.Session.Targets.Label)
	}
}

// TestAlertK8sNoNodesStillInvestigates: when no node resolves, the record still
// opens with no targets. The pods view still opens.
func TestAlertK8sNoNodesStillInvestigates(t *testing.T) {
	h := newAlertK8sHarness(t)
	h.k8s.resolved = nil
	tm := start(t, h.root)
	waitFor(t, tm, "PodCrashLooping")

	openAlertRow(t, tm, "PodCrashLooping")
	waitFor(t, tm, "inv:", "targets:0", "api-7d9f")

	paths, err := filepath.Glob(filepath.Join(h.dir, "investigations", "*.md"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("investigation files = %v, %v; want exactly one", paths, err)
	}
	file := waitFile(t, paths[0], "PodCrashLooping", "**open**")
	if !strings.Contains(file, "targets: []") {
		t.Fatalf("record claims targets without a resolvable node:\n%s", file)
	}
	_ = finish(t, tm)
}
