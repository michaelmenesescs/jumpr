package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// backendName is the name every Ref and registry lookup uses.
const backendName = "k8s"

// Options configures a Backend. Every value comes from internal/app, which owns
// the environment (BUILD-PLAN §6.1).
type Options struct {
	// Kubeconfig is kubernetes.kubeconfig: an explicit path, `~` allowed.
	Kubeconfig string
	// KubeconfigEnv is the $KUBECONFIG value internal/app read.
	KubeconfigEnv string
	// Home is the home directory, injected.
	Home string
	// Protected is kubernetes.contexts_protected.
	Protected []string
	// Log receives client-go's warnings and klog lines.
	Log *slog.Logger
	// Now is the clock. Nil uses the wall clock.
	Now func() time.Time
}

// Backend implements backend.Backend for Kubernetes. It carries the client, the
// session namespace and the health probe; the kinds arrive in task 106.
type Backend struct {
	client *Client
	kinds  []resource.KindSpec
	now    func() time.Time
}

var _ backend.Backend = (*Backend)(nil)

// New builds a Kubernetes backend over Options.
func New(o Options) (*Backend, error) {
	c, err := NewClient(ClientOptions{
		Kubeconfig:    o.Kubeconfig,
		KubeconfigEnv: o.KubeconfigEnv,
		Home:          o.Home,
		Protected:     o.Protected,
		Log:           o.Log,
	})
	if err != nil {
		return nil, err
	}
	b := NewWithClient(c)
	b.now = o.Now
	if b.now == nil {
		b.now = time.Now
	}
	return b, nil
}

// NewWithClient builds a backend around an existing client, which is what the
// tests use.
func NewWithClient(c *Client) *Backend {
	b := &Backend{client: c, now: time.Now}
	return b
}

// Name returns "k8s".
func (b *Backend) Name() string { return backendName }

// Kinds returns the kinds the backend serves. It is empty until task 106 adds
// pods, deployments and the rest.
func (b *Backend) Kinds() []resource.KindSpec { return b.kinds }

// Client returns the client the backend holds.
func (b *Backend) Client() *Client { return b.client }

// Contexts lists the kubeconfig's contexts (SPEC §8.2).
func (b *Backend) Contexts() ([]Context, error) { return b.client.Contexts() }

// CurrentContext is the selected context.
func (b *Backend) CurrentContext() string { return b.client.CurrentContext() }

// SwitchContext selects a context and rebuilds the client. protected reports
// whether the context is in kubernetes.contexts_protected, which makes the
// session read-only (SPEC §16).
func (b *Backend) SwitchContext(ctx context.Context, name string) (bool, error) {
	return b.client.SwitchContext(ctx, name)
}

// Namespace is the session namespace; "" means every namespace.
func (b *Backend) Namespace() string { return b.client.Namespace() }

// SetNamespace sets the session namespace: a name, or "all" for every one.
func (b *Backend) SetNamespace(namespace string) error { return b.client.SetNamespace(namespace) }

// Protected reports whether a context is listed in contexts_protected.
func (b *Backend) Protected(name string) bool { return b.client.Protected(name) }

// SetOnSwitch installs the teardown hook a context switch calls; task 106 stops
// its informer factory there.
func (b *Backend) SetOnSwitch(fn func(context string)) { b.client.SetOnSwitch(fn) }

// List streams resources of one kind. Until task 106 adds the kinds and their
// informers there is nothing to list, and saying so is better than an empty
// table that looks like an empty cluster.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	return nil, errs.New(errs.Config, "k8s.List",
		fmt.Sprintf("k8s does not serve kind %q yet", q.Kind))
}

// Get returns one resource. The kinds arrive in task 106.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, errs.New(errs.Config, "k8s.Get",
		fmt.Sprintf("k8s does not serve kind %q yet", ref.Kind))
}

// Links returns the pivots from r. The node ↔ host pivot arrives in task 111.
func (b *Backend) Links(r resource.Resource) []resource.Link { return nil }

// Health probes GET /version with a 5 s timeout (SPEC §8.2). A missing
// kubeconfig is unconfigured, not broken; an authentication failure, including
// a credential plugin that could not run, is `auth`.
func (b *Backend) Health(ctx context.Context) backend.Health {
	h := backend.Health{Checked: b.now()}
	version, err := b.client.ServerVersion(ctx)
	if err == nil {
		h.State = backend.HealthOK
		h.Detail = version
		if name := b.client.CurrentContext(); name != "" {
			h.Detail = fmt.Sprintf("%s (%s)", version, name)
		}
		return h
	}
	if Unconfigured(err) {
		h.State = backend.HealthUnconfigured
		h.Detail = oneLine(messageOf(err))
		return h
	}
	kind := Classify(err)
	h.State = backend.HealthDown
	h.Detail = string(kind) + ": " + oneLine(messageOf(err))
	return h
}

// messageOf is the user-facing part of err: an *errs.Error's message and cause
// without the operation prefix.
func messageOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		if e.Err != nil {
			return fmt.Sprintf("%s: %v", e.Msg, e.Err)
		}
		return e.Msg
	}
	return err.Error()
}
