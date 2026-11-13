package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

const (
	// syncTimeout bounds how long a List or a Get waits for an informer's
	// first cache sync. What is in the cache after that is served as Stale.
	syncTimeout = 10 * time.Second
	// idleTimeout is how long an informer lives once nobody has listed it and
	// no watcher is subscribed.
	idleTimeout = 5 * time.Minute
	// watchBuffer is how many events one watcher may fall behind before the
	// informer drops them: a slow view never blocks an informer.
	watchBuffer = 256
	// minSweep and maxSweep bound the janitor interval, which is otherwise a
	// quarter of idleTimeout (tests use a short idleTimeout).
	minSweep = 5 * time.Millisecond
	maxSweep = 30 * time.Second
)

// api is the client pair and the namespace one informer set serves. A context
// switch or a `:ns` change produces a new api, and the set is rebuilt, so no
// informer ever outlives the client it was built from.
type api struct {
	typed     kubernetes.Interface
	meta      metadata.Interface
	namespace string
	context   string
}

// listOptions is what an informer lists and watches with: one namespace, the
// server-side label selector and the field selector.
type listOptions struct {
	namespace string
	labels    string
	fields    string
}

// apply puts the selectors on the options client-go hands us, keeping the
// resourceVersion and timeout of a watch.
func (o listOptions) apply(opts metav1.ListOptions) metav1.ListOptions {
	opts.LabelSelector = o.labels
	opts.FieldSelector = o.fields
	return opts
}

// errNoWatchList tells a reflector that the watch-list initial-sync mode is
// not used: every informer here starts with a plain list followed by a watch,
// the sequence every API server supports. The reflector treats the error as
// "not supported" and lists instead, which is the behaviour this cache is
// built and tested against.
var errNoWatchList = errors.New("k8s: watch-list mode is not used")

// watchOK refuses the watch-list mode and otherwise lets the request through.
func watchOK(opts metav1.ListOptions) error {
	if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
		return errNoWatchList
	}
	return nil
}

// errRec records the first list or watch error of an informer, so a List can
// report `auth` for a forbidden kind instead of an empty table that looks like
// an empty cluster.
type errRec struct {
	mu  sync.Mutex
	err error
}

// set keeps the first error seen.
func (r *errRec) set(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

// get returns the first error seen, if any.
func (r *errRec) get() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// newLW builds a ListWatch around one typed resource, stamping the selectors
// on every call and recording the first error.
func newLW[T runtime.Object](
	list func(ctx context.Context, opts metav1.ListOptions) (T, error),
	watchFn func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error),
	o listOptions,
	rec *errRec,
) *cache.ListWatch {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			obj, err := list(ctx, o.apply(opts))
			rec.set(err)
			return obj, err
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			if err := watchOK(opts); err != nil {
				return nil, err
			}
			w, err := watchFn(ctx, o.apply(opts))
			rec.set(err)
			return w, err
		},
	}
}

// typedLW is the ListWatch of a kind served by the typed clientset.
func typedLW(cli kubernetes.Interface, def kindDef, o listOptions, rec *errRec) *cache.ListWatch {
	ns := o.namespace
	if !def.namespaced {
		ns = ""
	}
	core := cli.CoreV1()
	apps := cli.AppsV1()
	batch := cli.BatchV1()
	switch def.gvr.Resource {
	case "pods":
		c := core.Pods(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "deployments":
		c := apps.Deployments(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "statefulsets":
		c := apps.StatefulSets(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "daemonsets":
		c := apps.DaemonSets(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "jobs":
		c := batch.Jobs(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "cronjobs":
		c := batch.CronJobs(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "nodes":
		c := core.Nodes()
		return newLW(c.List, c.Watch, o, rec)
	case "services":
		c := core.Services(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "endpoints":
		c := core.Endpoints(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "ingresses":
		c := cli.NetworkingV1().Ingresses(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "configmaps":
		c := core.ConfigMaps(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "events":
		c := core.Events(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "namespaces":
		c := core.Namespaces()
		return newLW(c.List, c.Watch, o, rec)
	case "persistentvolumeclaims":
		c := core.PersistentVolumeClaims(ns)
		return newLW(c.List, c.Watch, o, rec)
	case "persistentvolumes":
		c := core.PersistentVolumes()
		return newLW(c.List, c.Watch, o, rec)
	}
	return nil
}

// metadataLW is the ListWatch of a kind served by the metadata client: the API
// server sends the ObjectMeta only, so a secret's values never reach this
// process.
func metadataLW(mc metadata.Interface, def kindDef, o listOptions, rec *errRec) *cache.ListWatch {
	get := mc.Resource(def.gvr)
	var ri metadata.ResourceInterface = get
	if def.namespaced {
		ri = get.Namespace(o.namespace)
	}
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			obj, err := ri.List(ctx, o.apply(opts))
			rec.set(err)
			return obj, err
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			if err := watchOK(opts); err != nil {
				return nil, err
			}
			w, err := ri.Watch(ctx, o.apply(opts))
			rec.set(err)
			return w, err
		},
	}
}

// objectKind is the kind name of a metadata-served resource, which the
// PartialObjectMetadata prototype needs.
func objectKind(def kindDef) string {
	switch def.gvr.Resource {
	case "secrets":
		return "Secret"
	case "configmaps":
		return "ConfigMap"
	}
	return strings.TrimSuffix(def.gvr.Resource, "s")
}

// watcher is one Watch subscriber.
type watcher struct {
	out chan backend.Event
}

// kindInformer is one informer and its subscribers.
type kindInformer struct {
	def  kindDef
	inf  cache.SharedIndexInformer
	rec  *errRec
	opts listOptions
	stop chan struct{}

	mu      sync.Mutex
	subs    map[int]*watcher
	nextID  int
	lastUse time.Time
	stopped bool
}

// handler builds the informer callbacks: every add, update and delete becomes
// an Event for each subscriber, with the row built the same way a listing
// builds it.
func (ki *kindInformer) handler(now func() time.Time) cache.ResourceEventHandlerFuncs {
	emit := func(t backend.EventType, obj any) {
		r := ki.def.row(obj, now())
		if r.Ref.Name == "" {
			return
		}
		ki.mu.Lock()
		for _, w := range ki.subs {
			select {
			case w.out <- backend.Event{Type: t, Resource: r}:
			default: // a slow subscriber loses events; the informer never blocks
			}
		}
		ki.lastUse = now()
		ki.mu.Unlock()
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { emit(backend.EventAdded, obj) },
		UpdateFunc: func(_, obj any) { emit(backend.EventModified, obj) },
		DeleteFunc: func(obj any) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			emit(backend.EventDeleted, obj)
		},
	}
}

// touch records that the informer was just used.
func (ki *kindInformer) touch(now time.Time) {
	ki.mu.Lock()
	ki.lastUse = now
	ki.mu.Unlock()
}

// idle reports whether the informer has no subscriber and has not been used
// for at least idle.
func (ki *kindInformer) idle(now time.Time, idle time.Duration) bool {
	ki.mu.Lock()
	defer ki.mu.Unlock()
	return len(ki.subs) == 0 && !ki.lastUse.Add(idle).After(now)
}

// shutdown stops the informer. It is safe to call twice.
func (ki *kindInformer) shutdown() {
	ki.mu.Lock()
	defer ki.mu.Unlock()
	if ki.stopped {
		return
	}
	ki.stopped = true
	close(ki.stop)
}

// informerSet owns every informer of one (context, namespace) pair. It is
// created lazily by Backend and replaced when either changes.
type informerSet struct {
	typed     kubernetes.Interface
	meta      metadata.Interface
	context   string
	namespace string
	idle      time.Duration
	sync      time.Duration
	now       func() time.Time

	mu     sync.Mutex
	kinds  map[string]*kindInformer
	closed bool
	done   chan struct{}
}

// newInformerSet starts a set and its janitor. close it when the context or
// the namespace changes.
func newInformerSet(typed kubernetes.Interface, meta metadata.Interface, context, namespace string, idle, sync time.Duration, now func() time.Time) *informerSet {
	if idle <= 0 {
		idle = idleTimeout
	}
	if sync <= 0 {
		sync = syncTimeout
	}
	if now == nil {
		now = time.Now
	}
	s := &informerSet{
		typed:     typed,
		meta:      meta,
		context:   context,
		namespace: namespace,
		idle:      idle,
		sync:      sync,
		now:       now,
		kinds:     make(map[string]*kindInformer),
		done:      make(chan struct{}),
	}
	go s.janitor()
	return s
}

// janitor sweeps idle informers until the set is closed.
func (s *informerSet) janitor() {
	every := s.idle / 4
	if every < minSweep {
		every = minSweep
	}
	if every > maxSweep {
		every = maxSweep
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

// key identifies one informer: kind, namespace and the selectors it was built
// with, so a query with a label selector or another namespace does not reuse
// the unfiltered cache.
func informerKey(def kindDef, o listOptions) string {
	return string(def.spec.Name) + "\x00" + o.namespace + "\x00" + o.labels + "\x00" + o.fields
}

// get returns the informer for def and o, starting it on first use.
func (s *informerSet) get(def kindDef, o listOptions) (*kindInformer, error) {
	key := informerKey(def, o)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errs.New(errs.Canceled, "k8s.list", "the informers are stopped")
	}
	if ki, ok := s.kinds[key]; ok {
		ki.touch(s.now())
		return ki, nil
	}
	ki, err := s.start(def, o)
	if err != nil {
		return nil, err
	}
	s.kinds[key] = ki
	return ki, nil
}

// start builds and runs one informer. The caller holds s.mu.
func (s *informerSet) start(def kindDef, o listOptions) (*kindInformer, error) {
	rec := &errRec{}
	var lw *cache.ListWatch
	var proto runtime.Object
	switch {
	case def.object == nil:
		if s.meta == nil {
			return nil, errs.New(errs.Config, "k8s.list",
				fmt.Sprintf("%s needs a metadata client", def.spec.Name))
		}
		lw = metadataLW(s.meta, def, o, rec)
		proto = &metav1.PartialObjectMetadata{
			TypeMeta: metav1.TypeMeta{APIVersion: def.gvr.GroupVersion().String(), Kind: objectKind(def)},
		}
	default:
		if s.typed == nil {
			return nil, errs.New(errs.Config, "k8s.list",
				fmt.Sprintf("%s needs a typed client", def.spec.Name))
		}
		lw = typedLW(s.typed, def, o, rec)
		proto = def.object
	}
	inf := cache.NewSharedIndexInformer(lw, proto, 0, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	ki := &kindInformer{
		def:     def,
		inf:     inf,
		rec:     rec,
		opts:    o,
		stop:    make(chan struct{}),
		subs:    make(map[int]*watcher),
		lastUse: s.now(),
	}
	inf.AddEventHandler(ki.handler(s.now))
	go inf.Run(ki.stop)
	return ki, nil
}

// snapshot returns the cache of def as rows, filtered by the parts of the
// selector Kubernetes cannot apply. stale is true when the cache had not
// synced within syncTimeout: the caller serves what is there. A permission
// failure is different: it is returned, so a forbidden kind shows `auth`
// instead of an empty table.
func (s *informerSet) snapshot(ctx context.Context, def kindDef, f filter) ([]resource.Resource, bool, error) {
	ki, err := s.get(def, f.opts)
	if err != nil {
		return nil, false, err
	}
	stale := false
	if !ki.inf.HasSynced() {
		tctx, cancel := context.WithTimeout(ctx, s.sync)
		synced := cache.WaitForCacheSync(tctx.Done(), ki.inf.HasSynced)
		cancel()
		if !synced {
			if lerr := ki.rec.get(); lerr != nil && Classify(lerr) == errs.Auth {
				return nil, false, listError(lerr)
			}
			stale = true
		}
	}
	objs := ki.inf.GetStore().List()
	now := s.now()
	rows := make([]resource.Resource, 0, len(objs))
	for _, obj := range objs {
		r := def.row(obj, now)
		if r.Ref.Name == "" {
			continue
		}
		if !f.matches(r) {
			continue
		}
		if stale {
			r.Stale = true
		}
		rows = append(rows, r)
	}
	ki.touch(now)
	return rows, stale, nil
}

// watch subscribes to one kind's add/modify/delete stream.
func (s *informerSet) watch(ctx context.Context, def kindDef, f filter) (<-chan backend.Event, error) {
	ki, err := s.get(def, f.opts)
	if err != nil {
		return nil, err
	}
	ki.mu.Lock()
	ki.nextID++
	id := ki.nextID
	w := &watcher{out: make(chan backend.Event, watchBuffer)}
	ki.subs[id] = w
	ki.lastUse = s.now()
	ki.mu.Unlock()

	out := make(chan backend.Event, watchBuffer)
	go func() {
		defer close(out)
		defer func() { s.release(ki, id) }()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ki.stop:
				return
			case <-s.done:
				return
			case ev := <-w.out:
				if !f.matches(ev.Resource) {
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// release drops a subscriber. The watcher channel is never closed: the emitter
// may still hold it, and a send on a closed channel would panic.
func (s *informerSet) release(ki *kindInformer, id int) {
	ki.mu.Lock()
	delete(ki.subs, id)
	ki.lastUse = s.now()
	ki.mu.Unlock()
}

// sweep stops every informer that has gone idle.
func (s *informerSet) sweep() {
	now := s.now()
	s.mu.Lock()
	var dead []*kindInformer
	for key, ki := range s.kinds {
		if ki.idle(now, s.idle) {
			dead = append(dead, ki)
			delete(s.kinds, key)
		}
	}
	s.mu.Unlock()
	for _, ki := range dead {
		ki.shutdown()
	}
}

// running is how many informers the set holds; the tests assert that an idle
// one has been stopped.
func (s *informerSet) running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.kinds)
}

// close stops every informer and the janitor.
func (s *informerSet) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	kinds := s.kinds
	s.kinds = make(map[string]*kindInformer)
	s.mu.Unlock()
	for _, ki := range kinds {
		ki.shutdown()
	}
}

// listError turns an informer's list error into an *errs.Error: a 403 becomes
// `auth`, which the table shows for that kind only.
func listError(err error) error {
	return &errs.Error{Kind: Classify(err), Op: "k8s.list", Msg: oneLine(err.Error()), Err: err}
}

// filter is the resolved filter of one Query: what the informer asks the API
// server for and what the backend applies to the rows itself.
type filter struct {
	opts listOptions
	sel  resource.Selector
	// node is Args["node"]: the field selector spec.nodeName, also applied
	// client-side because not every API server (nor the fake clientset)
	// honours a field selector.
	node string
	// name is Args["name"]: one object by name, applied client-side. It is how
	// a pivot narrows a list to the row it came from (task 111).
	name string
}

// matches reports whether a row passes the parts of the filter Kubernetes was
// asked to apply but may not have.
func (f filter) matches(r resource.Resource) bool {
	if f.node != "" {
		if got, _ := r.Fields["node"].(string); got != f.node {
			return false
		}
	}
	if f.name != "" {
		got, _ := r.Fields["name"].(string)
		if got == "" {
			got = r.Ref.Name
		}
		if got != f.name {
			return false
		}
	}
	return f.sel.IsZero() || f.sel.Matches(r.Labels)
}

// filterFor resolves q against the session namespace and the kinds the
// backend can filter server-side.
func filterFor(q resource.Query, sessionNS string, def kindDef) (filter, error) {
	ns := strings.TrimSpace(q.Args["namespace"])
	switch {
	case ns == "":
		ns = sessionNS
	case strings.EqualFold(ns, "all"):
		ns = ""
	}
	if !def.namespaced {
		ns = ""
	}
	lab, err := serverSelector(q.Selector)
	if err != nil {
		return filter{}, err
	}
	f := filter{opts: listOptions{namespace: ns, labels: lab}, sel: q.Selector}
	if name := strings.TrimSpace(q.Args["name"]); name != "" {
		f.name = name
	}
	if node := strings.TrimSpace(q.Args["node"]); node != "" {
		if !def.nodeArg {
			return filter{}, errs.New(errs.Config, "k8s.query",
				fmt.Sprintf("kind %q has no node filter", def.spec.Name))
		}
		f.node = node
		f.opts.fields = "spec.nodeName=" + node
	}
	return f, nil
}

// serverSelector is the part of a resource.Selector Kubernetes can filter on
// itself: =, !=, key and !key. The regex forms stay client-side.
func serverSelector(sel resource.Selector) (string, error) {
	reqs := make([]labels.Requirement, 0, len(sel.Terms))
	for _, t := range sel.Terms {
		var op selection.Operator
		switch t.Op {
		case resource.OpEq:
			op = selection.Equals
		case resource.OpNotEq:
			op = selection.NotEquals
		case resource.OpExists:
			op = selection.Exists
		case resource.OpNotExists:
			op = selection.DoesNotExist
		default:
			continue // ~ and !~ are regexes: client-side only
		}
		var vals []string
		switch op {
		case selection.Equals, selection.NotEquals:
			vals = []string{t.Value}
		}
		req, err := labels.NewRequirement(t.Key, op, vals)
		if err != nil {
			return "", errs.New(errs.Config, "k8s.selector",
				fmt.Sprintf("%s: %v", t.Key, err))
		}
		reqs = append(reqs, *req)
	}
	if len(reqs) == 0 {
		return "", nil
	}
	return labels.NewSelector().Add(reqs...).String(), nil
}

// informers returns the informer set of the current context and namespace,
// rebuilding it when either changed: a context switch or `:ns` must never be
// answered from the old cluster's cache.
func (b *Backend) informers() (*informerSet, error) {
	ap, err := b.api()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.set != nil && b.set.context == ap.context && b.set.namespace == ap.namespace {
		return b.set, nil
	}
	old := b.set
	b.set = newInformerSet(ap.typed, ap.meta, ap.context, ap.namespace, idleTimeout, b.syncDelay(), b.now)
	if old != nil {
		old.close()
	}
	return b.set, nil
}

// syncDelay is how long List and Get wait for a cache sync.
func (b *Backend) syncDelay() time.Duration {
	if b.syncTimeout > 0 {
		return b.syncTimeout
	}
	return syncTimeout
}

// resetInformers drops the set, which is what a context switch calls.
func (b *Backend) resetInformers() {
	b.mu.Lock()
	old := b.set
	b.set = nil
	b.mu.Unlock()
	if old != nil {
		old.close()
	}
}

// api returns the clients and the namespace the informers should serve. Tests
// set the typed and meta fields instead of a kubeconfig client.
func (b *Backend) api() (api, error) {
	if b.typed != nil {
		return api{typed: b.typed, meta: b.meta, namespace: b.namespace, context: "test"}, nil
	}
	typed, err := b.client.Clientset()
	if err != nil {
		return api{}, err
	}
	cfg, err := b.client.RESTConfig()
	if err != nil {
		return api{}, err
	}
	mc, err := metadata.NewForConfig(cfg)
	if err != nil {
		return api{}, errs.Wrap(errs.Config, "k8s.metadata", err)
	}
	return api{typed: typed, meta: mc, namespace: b.client.Namespace(), context: b.client.CurrentContext()}, nil
}

// list serves one kind from the informer cache as a single Result.
func (b *Backend) list(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	def, ok := defFor(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "k8s.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	set, err := b.informers()
	if err != nil {
		return nil, err
	}
	f, err := filterFor(q, set.namespace, def)
	if err != nil {
		return nil, err
	}

	ch := make(chan backend.Result, 1)
	go func() {
		defer close(ch)
		rows, _, err := set.snapshot(ctx, def, f)
		res := backend.Result{Done: 1, Total: 1}
		if err != nil {
			res.Err = err
		} else {
			res.Items = rows
		}
		select {
		case ch <- res:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// get returns one resource, from the cache when it has synced.
func (b *Backend) get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	def, ok := defFor(ref.Kind)
	if !ok {
		return resource.Resource{}, errs.New(errs.Config, "k8s.Get",
			fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	set, err := b.informers()
	if err != nil {
		return resource.Resource{}, err
	}
	ns := ref.Namespace
	if ns == "" || !def.namespaced {
		ns = set.namespace
	}
	if !def.namespaced {
		ns = ""
	}
	ki, err := set.get(def, listOptions{namespace: ns})
	if err != nil {
		return resource.Resource{}, err
	}
	if !ki.inf.HasSynced() {
		tctx, cancel := context.WithTimeout(ctx, b.syncDelay())
		synced := cache.WaitForCacheSync(tctx.Done(), ki.inf.HasSynced)
		cancel()
		if !synced {
			if lerr := ki.rec.get(); lerr != nil {
				return resource.Resource{}, listError(lerr)
			}
		}
	}
	obj, exists, err := ki.inf.GetStore().GetByKey(def.storeKey(ns, ref.Name))
	if err != nil {
		return resource.Resource{}, errs.Wrap(errs.Internal, "k8s.get", err)
	}
	if !exists {
		return resource.Resource{}, errs.New(errs.NotAvailable, "k8s.Get",
			fmt.Sprintf("%s %s not found", def.spec.Name, strings.TrimPrefix(ns+"/"+ref.Name, "/")))
	}
	return def.row(obj, b.now()), nil
}

// watch subscribes to one kind's add/modify/delete stream.
func (b *Backend) watch(ctx context.Context, q resource.Query) (<-chan backend.Event, error) {
	def, ok := defFor(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "k8s.Watch", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	set, err := b.informers()
	if err != nil {
		return nil, err
	}
	f, err := filterFor(q, set.namespace, def)
	if err != nil {
		return nil, err
	}
	return set.watch(ctx, def, f)
}
