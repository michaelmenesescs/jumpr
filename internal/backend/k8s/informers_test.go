package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/metadata"
	metadatafake "k8s.io/client-go/metadata/fake"
	ktesting "k8s.io/client-go/testing"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// testPod is one pod in ns on node.
func testPod(ns, name, node string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{containerRunning("app", 0)},
		},
	}
}

// testBackend builds a backend over a fake clientset, the way internal/app
// injects its own, with a short cache-sync wait.
func testBackend(t *testing.T, ns string, objs ...runtime.Object) (*Backend, *fake.Clientset) {
	t.Helper()
	cli := fake.NewSimpleClientset(objs...)
	b := NewWithClient(nil)
	b.typed = cli
	b.meta = metadatafake.NewSimpleMetadataClient(metadatafake.NewTestScheme())
	b.namespace = ns
	b.syncTimeout = 2 * time.Second
	t.Cleanup(b.Close)
	return b, cli
}

// collect drains a List into one slice.
func collect(t *testing.T, b *Backend, q resource.Query) ([]resource.Resource, []error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := b.List(ctx, q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var rows []resource.Resource
	var errsSeen []error
	for res := range ch {
		if res.Err != nil {
			errsSeen = append(errsSeen, res.Err)
			continue
		}
		rows = append(rows, res.Items...)
	}
	return rows, errsSeen
}

// names is the sorted name list of rows.
func names(rows []resource.Resource) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Ref.Namespace + "/" + r.Ref.Name
	}
	return out
}

// TestInformerListPodSnapshot: a List is one Result holding the whole cache.
func TestInformerListPodSnapshot(t *testing.T) {
	b, _ := testBackend(t, "prod",
		testPod("prod", "web-1", "node-a", nil),
		testPod("prod", "web-2", "node-b", nil),
		testPod("dev", "web-3", "node-a", nil),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := b.List(ctx, resource.Query{Kind: "pod"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var results []backend.Result
	for res := range ch {
		results = append(results, res)
	}
	if len(results) != 1 {
		t.Fatalf("got %d Results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("Result.Err = %v", results[0].Err)
	}
	if results[0].Done != 1 || results[0].Total != 1 {
		t.Errorf("Done/Total = %d/%d, want 1/1", results[0].Done, results[0].Total)
	}
	if got := len(results[0].Items); got != 2 {
		t.Fatalf("got %d rows, want the 2 pods of the session namespace: %v", got, names(results[0].Items))
	}
	for _, r := range results[0].Items {
		if r.Ref.Namespace != "prod" {
			t.Errorf("row %s is outside the session namespace", r.Ref)
		}
		if r.Fields["status"] != "Running" {
			t.Errorf("status = %v, want Running", r.Fields["status"])
		}
	}
}

// TestInformerNamespaceFilter: an empty session namespace lists everything,
// a named one only its own.
func TestInformerNamespaceFilter(t *testing.T) {
	objs := []runtime.Object{
		testPod("prod", "web-1", "node-a", nil),
		testPod("dev", "web-3", "node-a", nil),
	}
	b, _ := testBackend(t, "prod", objs...)
	rows, errsSeen := collect(t, b, resource.Query{Kind: "pod"})
	if len(errsSeen) != 0 {
		t.Fatalf("errors: %v", errsSeen)
	}
	if len(rows) != 1 || rows[0].Ref.Name != "web-1" {
		t.Fatalf("rows = %v, want the prod pod only", names(rows))
	}

	all := NewWithClient(nil)
	all.typed = fake.NewSimpleClientset(objs...)
	all.syncTimeout = 2 * time.Second
	t.Cleanup(all.Close)
	rows, _ = collect(t, all, resource.Query{Kind: "pod"})
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want both namespaces", names(rows))
	}
	// The query argument wins over the session namespace.
	rows, _ = collect(t, all, resource.Query{Kind: "pod", Args: map[string]string{"namespace": "dev"}})
	if len(rows) != 1 || rows[0].Ref.Name != "web-3" {
		t.Fatalf("rows = %v, want the dev pod only", names(rows))
	}
}

// TestInformerLabelSelector: =, != and existence terms go to the API server as
// a label selector, and the regex form is applied client-side.
func TestInformerLabelSelector(t *testing.T) {
	b, cli := testBackend(t, "prod",
		testPod("prod", "web-1", "node-a", map[string]string{"app": "web"}),
		testPod("prod", "api-1", "node-a", map[string]string{"app": "api"}),
		testPod("prod", "bare-1", "node-a", nil),
	)

	sel, err := resource.ParseSelector("app=web")
	if err != nil {
		t.Fatalf("ParseSelector: %v", err)
	}
	rows, errsSeen := collect(t, b, resource.Query{Kind: "pod", Selector: sel})
	if len(errsSeen) != 0 {
		t.Fatalf("errors: %v", errsSeen)
	}
	if len(rows) != 1 || rows[0].Ref.Name != "web-1" {
		t.Fatalf("rows = %v, want web-1 only", names(rows))
	}
	if got := listSelector(t, cli, "pods"); !strings.Contains(got, "app=web") {
		t.Errorf("server-side label selector = %q, want app=web", got)
	}

	// A regex term cannot be sent to the API server: it is applied to the rows.
	b2, cli2 := testBackend(t, "prod",
		testPod("prod", "web-1", "node-a", map[string]string{"app": "web"}),
		testPod("prod", "api-1", "node-a", map[string]string{"app": "api"}),
	)
	re, err := resource.ParseSelector("app~web|api")
	if err != nil {
		t.Fatalf("ParseSelector: %v", err)
	}
	rows, _ = collect(t, b2, resource.Query{Kind: "pod", Selector: re})
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want web-1 and api-1", names(rows))
	}
	if got := listSelector(t, cli2, "pods"); got != "" {
		t.Errorf("server-side label selector = %q, want none for a regex term", got)
	}
}

// listSelector is the label selector the fake clientset saw for one resource.
func listSelector(t *testing.T, cli *fake.Clientset, resourceName string) string {
	t.Helper()
	for _, action := range cli.Actions() {
		list, ok := action.(ktesting.ListAction)
		if !ok || action.GetResource().Resource != resourceName {
			continue
		}
		return list.GetListRestrictions().Labels.String()
	}
	return ""
}

// listFields is the field selector the fake clientset saw for one resource.
func listFields(t *testing.T, cli *fake.Clientset, resourceName string) string {
	t.Helper()
	for _, action := range cli.Actions() {
		list, ok := action.(ktesting.ListAction)
		if !ok || action.GetResource().Resource != resourceName {
			continue
		}
		return list.GetListRestrictions().Fields.String()
	}
	return ""
}

// TestInformerNodeFieldSelector: Args["node"] becomes the field selector
// spec.nodeName.
func TestInformerNodeFieldSelector(t *testing.T) {
	b, cli := testBackend(t, "prod",
		testPod("prod", "web-1", "node-a", nil),
		testPod("prod", "web-2", "node-b", nil),
	)
	rows, errsSeen := collect(t, b, resource.Query{Kind: "pod", Args: map[string]string{"node": "node-a"}})
	if len(errsSeen) != 0 {
		t.Fatalf("errors: %v", errsSeen)
	}
	if len(rows) != 1 || rows[0].Ref.Name != "web-1" {
		t.Fatalf("rows = %v, want the pod on node-a", names(rows))
	}
	if got := listFields(t, cli, "pods"); !strings.Contains(got, "spec.nodeName=node-a") {
		t.Errorf("field selector = %q, want spec.nodeName=node-a", got)
	}

	// Only pods can be filtered by node.
	if _, err := b.List(context.Background(), resource.Query{Kind: "deployment", Args: map[string]string{"node": "node-a"}}); err == nil {
		t.Error("a node filter on deployments must be refused")
	}
}

// TestInformerWatchEvents: add, modify and delete reach the Watch channel.
func TestInformerWatchEvents(t *testing.T) {
	b, cli := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := b.Watch(ctx, resource.Query{Kind: "pod"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// The first events are the initial sync adds; drain them.
	drainFor(t, events, 50*time.Millisecond)

	pod := testPod("prod", "web-2", "node-b", nil)
	if _, err := cli.CoreV1().Pods("prod").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ev := nextEvent(t, events)
	if ev.Type != backend.EventAdded || ev.Resource.Ref.Name != "web-2" {
		t.Fatalf("event = %+v, want an add of web-2", ev)
	}

	pod.ResourceVersion = ev.Resource.Raw.(*corev1.Pod).ResourceVersion
	pod.Labels = map[string]string{"app": "web"}
	if _, err := cli.CoreV1().Pods("prod").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	ev = nextEvent(t, events)
	if ev.Type != backend.EventModified || ev.Resource.Ref.Name != "web-2" {
		t.Fatalf("event = %+v, want a modify of web-2", ev)
	}
	if ev.Resource.Labels["app"] != "web" {
		t.Errorf("labels = %v, want app=web", ev.Resource.Labels)
	}

	if err := cli.CoreV1().Pods("prod").Delete(ctx, "web-2", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ev = nextEvent(t, events)
	if ev.Type != backend.EventDeleted || ev.Resource.Ref.Name != "web-2" {
		t.Fatalf("event = %+v, want a delete of web-2", ev)
	}
}

// drainFor reads whatever arrives within d.
func drainFor[T any](t *testing.T, ch <-chan T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

// nextEvent waits for one event, failing the test after two seconds.
func nextEvent(t *testing.T, ch <-chan backend.Event) backend.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the watch channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
	}
	return backend.Event{}
}

// TestInformerForbidden: a forbidden list is `auth` for that kind only.
func TestInformerForbidden(t *testing.T) {
	b, cli := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))
	cli.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("denied"))
	})

	rows, errsSeen := collect(t, b, resource.Query{Kind: "node"})
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
	if len(errsSeen) != 1 {
		t.Fatalf("errors = %v, want exactly one", errsSeen)
	}
	if kind := errs.KindOf(errsSeen[0]); kind != errs.Auth {
		t.Errorf("kind = %v, want auth", kind)
	}

	// Pods still work: the failure is per kind.
	rows, errsSeen = collect(t, b, resource.Query{Kind: "pod"})
	if len(errsSeen) != 0 {
		t.Fatalf("pod errors: %v", errsSeen)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %v, want the pod", names(rows))
	}
}

// TestInformerSecretsMetadataOnly: secrets come from a metadata client and
// carry no data at all.
func TestInformerSecretsMetadataOnly(t *testing.T) {
	scheme := metadatafake.NewTestScheme()
	metav1.AddMetaToScheme(scheme)
	secret := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "prod",
			Name:              "db-password",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
			Labels:            map[string]string{"app": "db"},
		},
	}
	meta := metadatafake.NewSimpleMetadataClient(scheme, secret)

	typed := fake.NewSimpleClientset()
	b := NewWithClient(nil)
	b.typed = typed
	b.meta = meta
	b.namespace = "prod"
	b.syncTimeout = 2 * time.Second
	t.Cleanup(b.Close)

	rows, errsSeen := collect(t, b, resource.Query{Kind: "secret"})
	if len(errsSeen) != 0 {
		t.Fatalf("errors: %v", errsSeen)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want the one secret", names(rows))
	}
	if rows[0].Ref.Name != "db-password" {
		t.Errorf("name = %q", rows[0].Ref.Name)
	}
	for id, v := range rows[0].Fields {
		if id != "namespace" && id != "name" && id != "age" {
			t.Errorf("field %q = %v on a secret row", id, v)
		}
	}
	// The typed clientset, which would have carried the values, was never
	// asked for a secret.
	for _, action := range typed.Actions() {
		if action.GetResource().Resource == "secrets" {
			t.Errorf("the typed client was used for secrets: %v", action)
		}
	}
	var _ metadata.Interface = meta
}

// TestInformerIdleStop: an informer without a subscriber stops after the idle
// timeout and starts again on the next List.
func TestInformerIdleStop(t *testing.T) {
	cli := fake.NewSimpleClientset(testPod("prod", "web-1", "node-a", nil))
	set := newInformerSet(cli, nil, "ctx", "prod", 200*time.Millisecond, time.Second, time.Now)
	t.Cleanup(set.close)

	if _, _, err := set.snapshot(context.Background(), mustDef("pod"), filter{opts: listOptions{namespace: "prod"}}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := set.running(); got != 1 {
		t.Fatalf("running = %d, want 1", got)
	}
	waitFor(t, 3*time.Second, func() bool { return set.running() == 0 })

	// The next List starts a fresh informer, not a stopped one.
	rows, _, err := set.snapshot(context.Background(), mustDef("pod"), filter{opts: listOptions{namespace: "prod"}})
	if err != nil {
		t.Fatalf("snapshot after idle: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want the pod again", names(rows))
	}
}

// TestInformerWatchKeepsAlive: a subscriber keeps the informer running past
// the idle timeout.
func TestInformerWatchKeepsAlive(t *testing.T) {
	cli := fake.NewSimpleClientset(testPod("prod", "web-1", "node-a", nil))
	set := newInformerSet(cli, nil, "ctx", "prod", 20*time.Millisecond, time.Second, time.Now)
	t.Cleanup(set.close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := set.watch(ctx, mustDef("pod"), filter{opts: listOptions{namespace: "prod"}})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	drainFor(t, events, 50*time.Millisecond)
	time.Sleep(60 * time.Millisecond) // past the idle timeout
	if got := set.running(); got != 1 {
		t.Fatalf("running = %d, want the subscribed informer to stay", got)
	}
	cancel()
	waitFor(t, time.Second, func() bool { return set.running() == 0 })
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestInformerGet: Get answers from the cache and reports a missing object as
// not-available.
func TestInformerGet(t *testing.T) {
	b, _ := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := b.Get(ctx, resource.Ref{Kind: "pod", Namespace: "prod", Name: "web-1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.Ref.Name != "web-1" || r.Fields["status"] != "Running" {
		t.Errorf("row = %+v", r)
	}

	_, err = b.Get(ctx, resource.Ref{Kind: "pod", Namespace: "prod", Name: "nope"})
	if err == nil {
		t.Fatal("Get of a missing pod must fail")
	}
	if kind := errs.KindOf(err); kind != errs.NotAvailable {
		t.Errorf("kind = %v, want not-available", kind)
	}
}

// TestInformerUnknownKind: an unknown kind is a config error, not a panic.
func TestInformerUnknownKind(t *testing.T) {
	b, _ := testBackend(t, "prod")
	if _, err := b.List(context.Background(), resource.Query{Kind: "widget"}); err == nil {
		t.Error("List of an unknown kind must fail")
	}
	if _, err := b.Watch(context.Background(), resource.Query{Kind: "widget"}); err == nil {
		t.Error("Watch of an unknown kind must fail")
	}
}

// TestInformerAliases: kubectl's short names resolve to the same kind.
func TestInformerAliases(t *testing.T) {
	b, _ := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))
	for _, alias := range []string{"po", "POD"} {
		rows, errsSeen := collect(t, b, resource.Query{Kind: resource.KindName(alias)})
		if len(errsSeen) != 0 {
			t.Fatalf("%s: errors %v", alias, errsSeen)
		}
		if len(rows) != 1 || rows[0].Ref.Kind != "pod" {
			t.Errorf("%s: rows = %v, want the pod", alias, names(rows))
		}
	}
}

// TestInformerRebuildOnNamespaceChange: a `:ns` change rebuilds the informers,
// so a listing never answers from the old namespace's cache.
func TestInformerRebuildOnNamespaceChange(t *testing.T) {
	b, _ := testBackend(t, "prod",
		testPod("prod", "web-1", "node-a", nil),
		testPod("dev", "dev-1", "node-a", nil),
	)
	rows, _ := collect(t, b, resource.Query{Kind: "pod"})
	if len(rows) != 1 || rows[0].Ref.Namespace != "prod" {
		t.Fatalf("rows = %v, want the prod pod", names(rows))
	}
	b.namespace = "dev"
	rows, _ = collect(t, b, resource.Query{Kind: "pod"})
	if len(rows) != 1 || rows[0].Ref.Namespace != "dev" {
		t.Fatalf("rows = %v, want the dev pod", names(rows))
	}
}

// TestInformerStaleSnapshot: a cache that never syncs is served with Stale
// set, and List does not block past the sync timeout.
func TestInformerStaleSnapshot(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("still trying")
	})
	set := newInformerSet(cli, nil, "ctx", "prod", time.Minute, 50*time.Millisecond, time.Now)
	t.Cleanup(set.close)

	start := time.Now()
	rows, stale, err := set.snapshot(context.Background(), mustDef("pod"), filter{opts: listOptions{namespace: "prod"}})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !stale {
		t.Error("stale = false, want true for an unsynced cache")
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none", names(rows))
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("snapshot waited %v, want no more than the sync timeout", waited)
	}
}

// TestInformerListCanceled: List returns nothing and leaves no goroutine when
// the caller's context is already done.
func TestInformerListCanceled(t *testing.T) {
	b, _ := testBackend(t, "prod", testPod("prod", "web-1", "node-a", nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch, err := b.List(ctx, resource.Query{Kind: "pod"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for range ch {
	}
}
