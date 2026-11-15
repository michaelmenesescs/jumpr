package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/backend/k8s"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// nodeOnlyBackend is a backend that knows one Kubernetes node and nothing else.
type nodeOnlyBackend struct {
	status k8s.NodeStatus
	node   bool
	err    error
	calls  int
}

func (b *nodeOnlyBackend) Name() string                            { return "k8s" }
func (b *nodeOnlyBackend) Kinds() []resource.KindSpec              { return nil }
func (b *nodeOnlyBackend) Links(resource.Resource) []resource.Link { return nil }
func (b *nodeOnlyBackend) Health(context.Context) backend.Health   { return backend.Health{} }

func (b *nodeOnlyBackend) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result)
	close(ch)
	return ch, nil
}

func (b *nodeOnlyBackend) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, nil
}

func (b *nodeOnlyBackend) NodeStatusOf(ctx context.Context, host resource.HostID) (k8s.NodeStatus, bool, error) {
	b.calls++
	return b.status, b.node, b.err
}

// newNodeHost builds the overview of k3s-1 over a backend that knows its node.
func newNodeHost(t *testing.T, b *nodeOnlyBackend) *Host {
	t.Helper()
	reg := backend.NewRegistry()
	if err := reg.Register(b); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	env := &tui.Env{
		Registry: reg,
		Session:  &tui.SessionState{Name: "main"},
		Now:      func() time.Time { return hostTestNow },
	}
	return NewHost(env, b, "k3s-1.lab.example.internal")
}

// TestHostKubernetesLine is requirement 3: the host overview of a node shows its
// ready state, its pod count and how many pods are not ready.
func TestHostKubernetesLine(t *testing.T) {
	b := &nodeOnlyBackend{node: true, status: k8s.NodeStatus{
		Name:         "k3s-1",
		Status:       "Ready",
		Ready:        true,
		Pods:         42,
		NotReadyPods: 2,
	}}
	h := newNodeHost(t, b)

	cmd := h.loadNode()
	if cmd == nil {
		t.Fatal("loadNode returned no command for a registered k8s backend")
	}
	if _, next := h.Update(cmd()); next != nil {
		t.Fatalf("Update(node msg) returned %v, want no follow-up", next)
	}
	if b.calls != 1 {
		t.Fatalf("NodeStatusOf calls = %d, want 1", b.calls)
	}

	screen := h.Render(120, 30)
	if !strings.Contains(screen, "KUBERNETES") {
		t.Fatalf("overview has no KUBERNETES section:\n%s", screen)
	}
	for _, want := range []string{"Ready", "42 pods", "2 not ready"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("KUBERNETES line is missing %q:\n%s", want, screen)
		}
	}
}

// TestHostKubernetesLineAbsent: a host that is not a node, or a build without a
// k8s backend, renders no KUBERNETES section.
func TestHostKubernetesLineAbsent(t *testing.T) {
	notNode := newNodeHost(t, &nodeOnlyBackend{node: false})
	if cmd := notNode.loadNode(); cmd != nil {
		notNode.Update(cmd())
	}
	if screen := notNode.Render(120, 30); strings.Contains(screen, "KUBERNETES") {
		t.Fatalf("a host that is not a node has a KUBERNETES section:\n%s", screen)
	}

	plain := NewHost(&tui.Env{Session: &tui.SessionState{Name: "main"}}, nil, "web-01")
	if cmd := plain.loadNode(); cmd != nil {
		t.Fatalf("loadNode without a k8s backend returned a command")
	}
	if screen := plain.Render(120, 30); strings.Contains(screen, "KUBERNETES") {
		t.Fatalf("a host without a k8s backend has a KUBERNETES section:\n%s", screen)
	}
}

// TestHostNodeErrorShowsKind: a node read that fails shows the error kind in the
// section instead of a blank line.
func TestHostNodeErrorShowsKind(t *testing.T) {
	b := &nodeOnlyBackend{node: true, err: errs.New(errs.Auth, "k8s.nodes", "forbidden")}
	h := newNodeHost(t, b)
	if cmd := h.loadNode(); cmd != nil {
		h.Update(cmd())
	}
	screen := h.Render(120, 30)
	if !strings.Contains(screen, "KUBERNETES") || !strings.Contains(screen, "auth") {
		t.Fatalf("KUBERNETES line does not show the error kind:\n%s", screen)
	}
}
