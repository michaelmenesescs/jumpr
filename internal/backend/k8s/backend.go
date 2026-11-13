package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

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

// Backend implements backend.Backend and backend.Watcher for Kubernetes. It
// carries the client, the session namespace, the health probe and the
// informers behind every kind.
type Backend struct {
	client *Client
	kinds  []resource.KindSpec
	now    func() time.Time
	// log receives the panic reports of the goroutines this backend starts
	// (safe.Go); nil means the reports are dropped.
	log *slog.Logger

	// typed and meta inject the client pair, which is what the tests do
	// instead of building a kubeconfig client; namespace is the session
	// namespace then. Empty means "read them from client".
	typed     kubernetes.Interface
	meta      metadata.Interface
	namespace string
	// syncTimeout shortens the cache-sync wait; 0 uses syncTimeout. The tests
	// use it so a forbidden kind fails fast.
	syncTimeout time.Duration

	// mu guards set, the informers of the current context and namespace, the
	// hook a context switch calls, and the node ↔ host pivot.
	mu       sync.Mutex
	set      *informerSet
	onSwitch func(context string)
	// pivot is the node ↔ host configuration (task 111); the zero value leaves
	// every node unmatched.
	pivot pivotState
}

var (
	_ backend.Backend = (*Backend)(nil)
	_ backend.Watcher = (*Backend)(nil)
)

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
	b.log = o.Log
	b.now = o.Now
	if b.now == nil {
		b.now = time.Now
	}
	return b, nil
}

// NewWithClient builds a backend around an existing client, which is what the
// tests use.
func NewWithClient(c *Client) *Backend {
	b := &Backend{client: c, kinds: kindSpecs(), now: time.Now}
	if c != nil {
		// A context switch must not leave an informer watching the old
		// cluster (SPEC §8.2).
		c.SetOnSwitch(b.switched)
	}
	return b
}

// switched runs after a context switch: the informers of the old cluster go
// first, then the hook internal/app installed.
func (b *Backend) switched(name string) {
	b.resetInformers()
	b.mu.Lock()
	hook := b.onSwitch
	b.mu.Unlock()
	if hook != nil {
		hook(name)
	}
}

// Name returns "k8s".
func (b *Backend) Name() string { return backendName }

// Kinds returns the kinds the backend serves (SPEC §8.2).
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

// SetOnSwitch installs the hook a context switch calls, after the informers
// of the old context have been stopped.
func (b *Backend) SetOnSwitch(fn func(context string)) {
	b.mu.Lock()
	b.onSwitch = fn
	b.mu.Unlock()
	if b.client != nil {
		b.client.SetOnSwitch(b.switched)
	}
}

// Close stops the informers. internal/app calls it when the backend goes away;
// the tests call it instead of leaving goroutines behind.
func (b *Backend) Close() { b.resetInformers() }

// List streams one batch: the informer cache of q.Kind, as it stands.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	return b.list(ctx, q)
}

// Watch delivers add/modify/delete events of q.Kind until ctx ends.
func (b *Backend) Watch(ctx context.Context, q resource.Query) (<-chan backend.Event, error) {
	return b.watch(ctx, q)
}

// Get returns one resource from the informer cache.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	return b.get(ctx, ref)
}

// Links returns the pivots from r: a pod or a node row opens the host overview
// of the node it is on, the units on that host and its journal (task 111).
func (b *Backend) Links(r resource.Resource) []resource.Link { return b.links(r) }

// ExtraLinks returns the pivots a host row has when it is a Kubernetes node
// (task 111): a backend.LinkProvider, so the root and the Linux backend can
// ask without importing this package.
func (b *Backend) ExtraLinks(r resource.Resource) []resource.Link { return b.extraLinks(r) }

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
