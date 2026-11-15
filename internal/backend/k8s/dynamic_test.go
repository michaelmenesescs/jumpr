package k8s

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// dynamicNow is the fixed clock of every test in this file.
var dynamicNow = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

// widgetsGVR is the CRD the tests discover.
var widgetsGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}

// tableCall records one Table request a test source saw.
type tableCall struct {
	gvr        schema.GroupVersionResource
	namespaced bool
	namespace  string
	limit      int
}

// fakeSource is a tableSource with no API server behind it.
type fakeSource struct {
	mu        sync.Mutex
	resources []apiResource
	tables    map[schema.GroupVersionResource]*metav1.Table
	calls     []tableCall
	discovers int
	resErr    error
	tableErr  map[schema.GroupVersionResource]error
}

func newFakeSource(resources []apiResource) *fakeSource {
	return &fakeSource{
		resources: resources,
		tables:    make(map[schema.GroupVersionResource]*metav1.Table),
		tableErr:  make(map[schema.GroupVersionResource]error),
	}
}

func (f *fakeSource) Resources(context.Context) ([]apiResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discovers++
	if f.resErr != nil {
		return nil, f.resErr
	}
	out := make([]apiResource, len(f.resources))
	copy(out, f.resources)
	return out, nil
}

func (f *fakeSource) Table(_ context.Context, gvr schema.GroupVersionResource, namespaced bool, namespace string, limit int) (*metav1.Table, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tableCall{gvr: gvr, namespaced: namespaced, namespace: namespace, limit: limit})
	if err := f.tableErr[gvr]; err != nil {
		return nil, err
	}
	table, ok := f.tables[gvr]
	if !ok {
		return nil, errs.New(errs.NotAvailable, opDiscover, "no table for "+gvr.Resource)
	}
	cp := *table
	return &cp, nil
}

// callCount is how many Table requests the source saw.
func (f *fakeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// lastCall is the last Table request.
func (f *fakeSource) lastCall(t *testing.T) tableCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the source saw no Table request")
	}
	return f.calls[len(f.calls)-1]
}

// runs is how many discovery passes the source saw.
func (f *fakeSource) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discovers
}

// widgetResource is the CRD the tests serve.
func widgetResource() apiResource {
	return apiResource{
		Group:      "example.com",
		Version:    "v1",
		Resource:   "widgets",
		Kind:       "Widget",
		Singular:   "widget",
		ShortNames: []string{"wd"},
		Namespaced: true,
	}
}

// widgetTable is a Table the way a CRD with printer columns answers it.
func widgetTable() *metav1.Table {
	return &metav1.Table{
		TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "Table"},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Size", Type: "integer"},
			{Name: "Ready", Type: "boolean"},
			{Name: "Phase", Type: "string"},
			{Name: "Age", Type: "string"},
		},
		Rows: []metav1.TableRow{
			tableRow("prod", "widget-a", "0f0a1a1e-0000-4000-8000-00000000000a", float64(3), true, "Running", "5m"),
			tableRow("prod", "widget-b", "0f0a1a1e-0000-4000-8000-00000000000b", float64(2.5), false, "Pending", "5m"),
		},
	}
}

// tableRow is one Table row with its object metadata.
func tableRow(namespace, name, uid string, cells ...any) metav1.TableRow {
	object := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"uid":       uid,
			"labels":    map[string]any{"app": name},
		},
	}
	raw, _ := json.Marshal(object)
	return metav1.TableRow{
		Cells:  append([]any{name}, cells...),
		Object: runtime.RawExtension{Raw: raw},
	}
}

// dynamicBackend builds a Dynamic over a fake source, with the fixed clock.
func dynamicBackend(t *testing.T, src tableSource) *Dynamic {
	t.Helper()
	b := NewWithClient(nil)
	b.now = func() time.Time { return dynamicNow }
	d := newDynamicWithBackend(b)
	d.SetSource(src)
	return d
}

// refresh runs one discovery pass or fails.
func refresh(t *testing.T, d *Dynamic) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.RefreshKinds(ctx); err != nil {
		t.Fatalf("RefreshKinds: %v", err)
	}
}

// specByName finds a kind spec.
func specByName(t *testing.T, specs []resource.KindSpec, name string) resource.KindSpec {
	t.Helper()
	for _, s := range specs {
		if string(s.Name) == name {
			return s
		}
	}
	t.Fatalf("kind %q not in %v", name, kindNames(specs))
	return resource.KindSpec{}
}

// kindNames is the name list of specs.
func kindNames(specs []resource.KindSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = string(s.Name)
	}
	return out
}

// TestDynamicDiscoveryKinds is requirement 1: every listable resource becomes a
// kind named by its plural, with the CRD's short and singular names as aliases,
// the Table's columns, and a built-in conflict skipped.
func TestDynamicDiscoveryKinds(t *testing.T) {
	src := newFakeSource([]apiResource{
		// A built-in the typed kinds already serve: it must not be shadowed.
		{Group: "", Version: "v1", Resource: "pods", Kind: "Pod", Singular: "pod", ShortNames: []string{"po"}, Namespaced: true},
		widgetResource(),
		// Cluster-scoped, to prove the scope mapping.
		{Group: "example.com", Version: "v1", Resource: "clusters", Kind: "Cluster", Singular: "cluster", Namespaced: false},
	})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	refresh(t, d)

	specs := d.Kinds()
	if got := countKind(specs, "pods"); got != 0 {
		t.Fatalf("pods appears %d times in %v; the typed pods kind must win", got, kindNames(specs))
	}
	if got := countKind(specs, "pod"); got != 1 {
		t.Fatalf("the typed pod kind appears %d times, want 1", got)
	}
	w := specByName(t, specs, "widgets")
	if strings.Join(w.Aliases, ",") != "wd,widget" {
		t.Errorf("widgets aliases = %v, want [wd widget]", w.Aliases)
	}
	if !strings.Contains(w.Title, "example.com/v1") {
		t.Errorf("widgets title = %q, want the API group in it", w.Title)
	}
	if w.Scope != resource.ScopeNamespace || w.Watchable {
		t.Errorf("widgets scope/watchable = %v/%v, want namespace/not watchable", w.Scope, w.Watchable)
	}
	ids := make([]string, len(w.Columns))
	for i, c := range w.Columns {
		ids[i] = c.ID
	}
	if strings.Join(ids, ",") != "name,size,ready,phase,age" {
		t.Errorf("widgets columns = %v", ids)
	}
	if size := w.Columns[1]; size.Align != resource.AlignRight || size.Kind != resource.ColNumber {
		t.Errorf("size column = %+v, want a right-aligned number", size)
	}
	cl := specByName(t, specs, "clusters")
	if cl.Scope != resource.ScopeGlobal {
		t.Errorf("clusters scope = %v, want global", cl.Scope)
	}
	// The probe asked for one row, cluster-wide, for the columns only.
	if calls := src.callCount(); calls != 2 {
		t.Errorf("Table probes = %d, want 2 (one per discovered kind)", calls)
	}
	if call := src.lastCall(t); call.limit != dynamicProbeLimit {
		t.Errorf("probe limit = %d, want %d", call.limit, dynamicProbeLimit)
	}
}

// TestDynamicTableJSON is the real shape of the data: the API server's Table
// JSON (mixed cell types, a Metadata object per row) decodes into the same rows
// a fake source produces.
func TestDynamicTableJSON(t *testing.T) {
	const body = `{
      "apiVersion": "meta.k8s.io/v1",
      "kind": "Table",
      "metadata": {"resourceVersion": "12"},
      "columnDefinitions": [
        {"name": "Name", "type": "string", "format": "name"},
        {"name": "Replicas", "type": "integer"},
        {"name": "Ready", "type": "boolean"},
        {"name": "Age", "type": "string"}
      ],
      "rows": [
        {"cells": ["widget-a", 3, true, "2h"],
         "object": {"apiVersion": "example.com/v1", "kind": "Widget",
                    "metadata": {"name": "widget-a", "namespace": "prod",
                                 "labels": {"app": "a"}}}}
      ]
    }`
	var table metav1.Table
	if err := json.Unmarshal([]byte(body), &table); err != nil {
		t.Fatalf("decode Table: %v", err)
	}
	api := widgetResource()
	def := kindDef{spec: resource.KindSpec{Name: "widgets", Columns: columnsFromTable(table.ColumnDefinitions, true)}, gvr: api.gvr(), namespaced: true}
	r := dynamicRow(def, table.Rows[0], dynamicNow)
	if r.Ref.Namespace != "prod" || r.Ref.Name != "widget-a" {
		t.Errorf("Ref = %+v", r.Ref)
	}
	if n, ok := r.Fields["replicas"].(int64); !ok || n != 3 {
		t.Errorf("replicas = %#v, want int64(3)", r.Fields["replicas"])
	}
	if ready, ok := r.Fields["ready"].(bool); !ok || !ready {
		t.Errorf("ready = %#v", r.Fields["ready"])
	}
	if r.Fields["age"] != "2h" {
		t.Errorf("age = %#v", r.Fields["age"])
	}
	if r.Labels["app"] != "a" {
		t.Errorf("labels = %v", r.Labels)
	}
}

// TestDynamicTableURL is the request the Table API needs: the media type is
// passed as Accept, and the namespace is in the path only for a namespaced kind.
func TestDynamicTableURL(t *testing.T) {
	cases := []struct {
		name  string
		gvr   schema.GroupVersionResource
		ns    bool
		space string
		want  string
	}{
		{"core namespaced", schema.GroupVersionResource{Version: "v1", Resource: "pods"}, true, "prod",
			"https://api:6443/api/v1/namespaces/prod/pods?includeObject=Metadata&limit=500"},
		{"core all namespaces", schema.GroupVersionResource{Version: "v1", Resource: "pods"}, true, "",
			"https://api:6443/api/v1/pods?includeObject=Metadata&limit=500"},
		{"group cluster-scoped", widgetsGVR, false, "prod",
			"https://api:6443/apis/example.com/v1/widgets?includeObject=Metadata&limit=500"},
	}
	for _, tc := range cases {
		if got := tableURL("https://api:6443/", tc.gvr, tc.ns, tc.space, 500); got != tc.want {
			t.Errorf("%s: tableURL = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// countKind is how many specs carry name.
func countKind(specs []resource.KindSpec, name string) int {
	n := 0
	for _, s := range specs {
		if string(s.Name) == name {
			n++
		}
	}
	return n
}

// TestDynamicDiscoveryConflictingAliases is requirement 1: an alias a built-in
// kind already claims is dropped, not registered twice.
func TestDynamicDiscoveryConflictingAliases(t *testing.T) {
	// "po" is pods' alias; "wd" is free.
	widget := widgetResource()
	widget.ShortNames = []string{"po", "wd"}
	src := newFakeSource([]apiResource{widget})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	refresh(t, d)

	specs := d.Kinds()
	keys := map[string]int{}
	for _, s := range specs {
		keys[strings.ToLower(string(s.Name))]++
		for _, a := range s.Aliases {
			keys[strings.ToLower(a)]++
		}
	}
	if keys["po"] != 1 {
		t.Errorf("alias po is claimed %d times, want once (by pods only)", keys["po"])
	}
	if keys["wd"] != 1 {
		t.Errorf("alias wd is claimed %d times, want once", keys["wd"])
	}
}

// TestDynamicDiscoveryCache is requirement 1's ten-minute cache: a second pass
// within the window is not made, one after it is, and a failure keeps the kinds.
func TestDynamicDiscoveryCache(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	b := NewWithClient(nil)
	clock := dynamicNow
	b.now = func() time.Time { return clock }
	d := newDynamicWithBackend(b)
	d.SetSource(src)
	refresh(t, d)
	if runs := src.runs(); runs != 1 {
		t.Fatalf("discovery passes = %d, want 1", runs)
	}

	// Inside the ten-minute window: refresh() still runs when asked
	// explicitly, but ensure()/Kinds() do not.
	clock = dynamicNow.Add(9 * time.Minute)
	if err := d.cache.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if runs := src.runs(); runs != 1 {
		t.Fatalf("discovery passes after 9m = %d, want 1 (cached)", runs)
	}

	// Past it: ensure refreshes.
	clock = dynamicNow.Add(dynamicTTL + time.Minute)
	if err := d.cache.ensure(context.Background()); err != nil {
		t.Fatalf("ensure after the TTL: %v", err)
	}
	if runs := src.runs(); runs != 2 {
		t.Fatalf("discovery passes after the TTL = %d, want 2", runs)
	}

	// A failing refresh keeps the kinds that were already known.
	src.mu.Lock()
	src.resErr = errs.New(errs.Unreachable, "test", "api server is down")
	src.mu.Unlock()
	clock = clock.Add(2 * dynamicTTL)
	if err := d.cache.ensure(context.Background()); err != nil {
		t.Fatalf("ensure with a failing discovery returned %v, want the cached kinds", err)
	}
	if _, ok := d.cache.lookup("widgets"); !ok {
		t.Error("the discovered kinds were dropped after a failed refresh")
	}
}

// TestDynamicListCells is requirement 1's mixed-cell rule: a Table cell is
// rendered by its type, numbers are numbers and a null is a missing value.
func TestDynamicListCells(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	table := widgetTable()
	table.Rows = append(table.Rows, metav1.TableRow{
		Cells:  []any{"widget-c", float64(1), true, nil, "1h"},
		Object: runtime.RawExtension{Raw: mustJSON(t, map[string]any{"metadata": map[string]any{"name": "widget-c", "namespace": "prod", "labels": map[string]any{"tier": "batch"}}})},
	})
	src.tables[widgetsGVR] = table
	d := dynamicBackend(t, src)
	refresh(t, d)

	rows := listDynamicRows(t, d, resource.Query{Kind: "widgets", Args: map[string]string{"namespace": "prod"}})
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	a := rows[0]
	if a.Ref.Backend != backendName || a.Ref.Kind != "widgets" || a.Ref.Namespace != "prod" || a.Ref.Name != "widget-a" {
		t.Errorf("Ref = %+v", a.Ref)
	}
	if got, ok := a.Fields["size"].(int64); !ok || got != 3 {
		t.Errorf("size = %#v, want int64(3)", a.Fields["size"])
	}
	if got, ok := a.Fields["ready"].(bool); !ok || !got {
		t.Errorf("ready = %#v, want true", a.Fields["ready"])
	}
	if a.Fields["phase"] != "Running" || a.Fields["age"] != "5m" || a.Fields["name"] != "widget-a" {
		t.Errorf("text fields = %v", a.Fields)
	}
	if a.Labels["app"] != "widget-a" {
		t.Errorf("labels = %v, want app=widget-a", a.Labels)
	}
	if got, ok := rows[1].Fields["size"].(float64); !ok || got != 2.5 {
		t.Errorf("fractional size = %#v, want float64(2.5)", rows[1].Fields["size"])
	}
	c := rows[2]
	if _, ok := c.Fields["phase"]; ok {
		t.Errorf("null cell became %#v, want the field absent", c.Fields["phase"])
	}
	if c.Fields["age"] != "1h" {
		t.Errorf("age = %#v", c.Fields["age"])
	}
	if call := src.lastCall(t); call.namespace != "prod" || !call.namespaced || call.limit != dynamicListLimit {
		t.Errorf("Table call = %+v, want namespace prod and limit %d", call, dynamicListLimit)
	}
}

// listDynamicRows drains a dynamic List.
func listDynamicRows(t *testing.T, d *Dynamic, q resource.Query) []resource.Resource {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := d.List(ctx, q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var rows []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("List result: %v", res.Err)
		}
		rows = append(rows, res.Items...)
	}
	return rows
}

// testDynamic wraps a dynamic client and records the PatchOptions of every
// patch, which is how a test tells a dry run from a write.
type recordingDyn struct {
	dynamic.Interface
	onPatch func(metav1.PatchOptions)
}

// Resource returns the recorder-wrapped resource of gvr.
func (r *recordingDyn) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordingNS{NamespaceableResourceInterface: r.Interface.Resource(gvr), parent: r}
}

// recordingNS is a cluster-scoped handle: it records and keeps the recorder on
// a namespaced handle.
type recordingNS struct {
	dynamic.NamespaceableResourceInterface
	parent *recordingDyn
}

// Namespace keeps the recorder on a namespaced handle.
func (r *recordingNS) Namespace(ns string) dynamic.ResourceInterface {
	return &recordingRI{ResourceInterface: r.NamespaceableResourceInterface.Namespace(ns), parent: r.parent}
}

// Patch records the options, then patches.
func (r *recordingNS) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	r.parent.record(opts)
	return r.NamespaceableResourceInterface.Patch(ctx, name, pt, data, opts, subresources...)
}

// recordingRI records the patch options and delegates everything else.
type recordingRI struct {
	dynamic.ResourceInterface
	parent *recordingDyn
}

// Patch records the options, then patches.
func (r *recordingRI) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	r.parent.record(opts)
	return r.ResourceInterface.Patch(ctx, name, pt, data, opts, subresources...)
}

// record hands one patch's options to the test's callback.
func (r *recordingDyn) record(opts metav1.PatchOptions) {
	if r.onPatch != nil {
		r.onPatch(opts)
	}
}

// mustJSON marshals v or fails.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestDynamicListFilter is requirement 1 plus the selector rules: session
// namespace, `all` and a label selector all narrow a discovered list.
func TestDynamicListFilter(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	b := NewWithClient(nil)
	b.now = func() time.Time { return dynamicNow }
	b.namespace = "team-a"
	d := newDynamicWithBackend(b)
	d.SetSource(src)
	refresh(t, d)

	rows := listDynamicRows(t, d, resource.Query{Kind: "widgets"})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want both rows of the session namespace", len(rows))
	}
	if call := src.lastCall(t); call.namespace != "team-a" {
		t.Errorf("Table namespace = %q, want the session namespace", call.namespace)
	}

	listDynamicRows(t, d, resource.Query{Kind: "widgets", Args: map[string]string{"namespace": "all"}})
	if call := src.lastCall(t); call.namespace != "" {
		t.Errorf("Table namespace for all = %q, want every namespace", call.namespace)
	}

	sel, err := resource.ParseSelector("app=widget-b")
	if err != nil {
		t.Fatalf("ParseSelector: %v", err)
	}
	rows = listDynamicRows(t, d, resource.Query{Kind: "widgets", Selector: sel, Args: map[string]string{"namespace": "all"}})
	if len(rows) != 1 || rows[0].Ref.Name != "widget-b" {
		t.Fatalf("rows = %v, want widget-b only", rows)
	}
}

// TestDynamicGet is Get over a discovered kind: the row comes from the Table so
// it keeps its cells, and an absent name is `not-available`.
func TestDynamicGet(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	refresh(t, d)

	ctx := context.Background()
	r, err := d.Get(ctx, resource.Ref{Backend: backendName, Kind: "widgets", Namespace: "prod", Name: "widget-b"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.Ref.Name != "widget-b" || r.Fields["phase"] != "Pending" {
		t.Errorf("row = %+v, want widget-b/Pending", r)
	}
	if _, err := d.Get(ctx, resource.Ref{Kind: "widgets", Namespace: "prod", Name: "nope"}); errs.KindOf(err) != errs.NotAvailable {
		t.Errorf("Get(absent) kind = %q, want not-available (%v)", errs.KindOf(err), err)
	}
}

// TestDynamicWatchNotAvailable is requirement 1: a discovered kind is a
// snapshot, so watching it is refused, not silently empty.
func TestDynamicWatchNotAvailable(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	refresh(t, d)

	if _, err := d.Watch(context.Background(), resource.Query{Kind: "widgets"}); errs.KindOf(err) != errs.NotAvailable {
		t.Fatalf("Watch kind = %q, want not-available (%v)", errs.KindOf(err), err)
	}
	if spec := specByName(t, d.Kinds(), "widgets"); spec.Watchable {
		t.Error("widgets is watchable, want not watchable")
	}
}

// TestDynamicReadObject is requirement 1's "describe/YAML via 108": a
// discovered object reads through the same reader, with its own GVR.
func TestDynamicReadObject(t *testing.T) {
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"name":              "widget-a",
			"namespace":         "prod",
			"uid":               "0f0a1a1e-0000-4000-8000-000000000001",
			"managedFields":     []any{map[string]any{"manager": "kubectl"}},
			"creationTimestamp": "2026-11-11T10:00:00Z",
		},
		"spec": map[string]any{"size": float64(3)},
	}}
	reader := describeReaderFor(t, widgetsGVR, "WidgetList", widget)

	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	d.SetReader(reader)
	refresh(t, d)

	detail, err := d.ReadObject(context.Background(), resource.Ref{Kind: "widgets", Namespace: "prod", Name: "widget-a"}, false)
	if err != nil {
		t.Fatalf("ReadObject: %v", err)
	}
	if detail.Ref.Kind != "widgets" || detail.Kind != "Widget" {
		t.Errorf("detail = %+v, want kind widgets/Widget", detail)
	}
	if !strings.Contains(detail.YAML, "apiVersion: example.com/v1") || !strings.Contains(detail.YAML, "kind: Widget") {
		t.Errorf("YAML misses apiVersion/kind:\n%s", detail.YAML)
	}
	if strings.Contains(detail.YAML, "managedFields") {
		t.Errorf("YAML keeps managedFields:\n%s", detail.YAML)
	}
	if !strings.Contains(detail.Describe, "Name:") {
		t.Errorf("describe = %q, want the generic summary", detail.Describe)
	}
}

// describeReaderFor builds an ObjectReader over a fake dynamic client that knows
// one custom resource.
func describeReaderFor(t *testing.T, gvr schema.GroupVersionResource, listKind string, objs ...runtime.Object) *ObjectReader {
	t.Helper()
	scheme := runtime.NewScheme()
	lists := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "events"}: "EventList",
		gvr: listKind,
	}
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, lists, objs...)
	return NewObjectReader(cli, func() time.Time { return dynamicNow })
}

// patchPlan builds a plan for one widget.
func patchPlan(t *testing.T, p *PatchPlanner, patch string) action.Plan {
	t.Helper()
	req := action.Request{
		SpecID: SpecApplyPatch,
		Targets: []action.Target{{
			Ref: resource.Ref{Backend: backendName, Kind: "widgets", Namespace: "prod", Name: "widget-a"},
		}},
		Params: map[string]string{ParamPatch: patch},
		Actor:  action.Human,
	}
	plan, err := p.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

// TestDynamicPatchPlan is requirement 2: the plan shows the server-side dry-run
// diff, the step names the equivalent kubectl patch, and the write happens only
// when the step runs.
func TestDynamicPatchPlan(t *testing.T) {
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "widget-a", "namespace": "prod"},
		"spec":       map[string]any{"size": float64(3)},
	}}
	cli := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{widgetsGVR: "WidgetList", {Group: "", Version: "v1", Resource: "events"}: "EventList"},
		widget)

	var sweeps []metav1.PatchOptions
	dyn := &recordingDyn{Interface: cli, onPatch: func(o metav1.PatchOptions) { sweeps = append(sweeps, o) }}

	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	refresh(t, d)
	p := d.PatchPlanner()
	p.dyn = dyn

	plan := patchPlan(t, p, `{"spec":{"size":4}}`)
	if len(sweeps) != 1 {
		t.Fatalf("patches during Plan = %d, want 1 (the dry run)", len(sweeps))
	}
	if len(sweeps[0].DryRun) == 0 {
		t.Error("the plan's patch was not a dry run")
	}
	if plan.Preview == "" || !strings.Contains(plan.Preview, "@@") {
		t.Errorf("preview = %q, want a unified diff", plan.Preview)
	}
	if !strings.Contains(plan.Steps[0].Display, "patch") || !strings.Contains(plan.Steps[0].Display, "widgets") {
		t.Errorf("display = %q, want the kubectl patch line", plan.Steps[0].Display)
	}
	if plan.Title != "patch widgets prod/widget-a" {
		t.Errorf("title = %q", plan.Title)
	}

	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("step: %v", res.Err)
	}
	if len(sweeps) != 2 || len(sweeps[1].DryRun) != 0 {
		t.Fatalf("after the step patches = %d opts = %v, want a real patch last", len(sweeps), sweeps)
	}
	if !strings.Contains(res.Output, "patched prod/widget-a") {
		t.Errorf("output = %q", res.Output)
	}
}

// TestDynamicPatchValidation is requirement 2's input rule: the pasted text
// must be a JSON object, and the spec ID must be this planner's.
func TestDynamicPatchValidation(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	refresh(t, d)
	p := d.PatchPlanner()

	for _, patch := range []string{"", "   ", "not json", "[1,2]", `"text"`} {
		req := action.Request{
			SpecID:  SpecApplyPatch,
			Targets: []action.Target{{Ref: resource.Ref{Kind: "widgets", Namespace: "prod", Name: "widget-a"}}},
			Params:  map[string]string{ParamPatch: patch},
		}
		_, err := p.Plan(context.Background(), req)
		if errs.KindOf(err) != errs.Config {
			t.Errorf("Plan(%q) kind = %q, want config (%v)", patch, errs.KindOf(err), err)
		}
	}
	req := action.Request{SpecID: "k8s.delete", Targets: []action.Target{{Ref: resource.Ref{Kind: "widgets", Name: "x"}}}}
	if _, err := p.Plan(context.Background(), req); errs.KindOf(err) != errs.Config {
		t.Errorf("Plan(other spec) kind = %q, want config", errs.KindOf(err))
	}
	if _, err := p.Plan(context.Background(), action.Request{SpecID: SpecApplyPatch}); errs.KindOf(err) != errs.Config {
		t.Errorf("Plan(no targets) kind = %q, want config", errs.KindOf(err))
	}
}

// TestDynamicPatchActions is requirement 2's menu rule: the edit flow is offered
// for this backend's rows only, and owns exactly its spec ID.
func TestDynamicPatchActions(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	src.tables[widgetsGVR] = widgetTable()
	d := dynamicBackend(t, src)
	refresh(t, d)
	p := d.PatchPlanner()

	specs := p.Actions([]resource.Resource{
		{Ref: resource.Ref{Backend: backendName, Kind: "widgets", Namespace: "prod", Name: "widget-a"}},
	})
	if len(specs) != 1 || specs[0].ID != SpecApplyPatch {
		t.Fatalf("Actions = %+v, want one %s", specs, SpecApplyPatch)
	}
	if !specs[0].HasDryRun {
		t.Error("apply-patch does not advertise its dry run")
	}
	if got := specs[0].Params; len(got) != 1 || got[0].Name != ParamPatch || !got[0].Required {
		t.Errorf("params = %+v, want a required %s", got, ParamPatch)
	}
	if got := p.Actions([]resource.Resource{{Ref: resource.Ref{Backend: "linux", Kind: "host", Name: "web-01"}}}); got != nil {
		t.Errorf("Actions(other backend) = %+v, want nil", got)
	}
	if !p.Owns(SpecApplyPatch) || p.Owns("k8s.delete") {
		t.Error("Owns does not claim exactly the apply-patch spec")
	}
}

// TestDynamicKindsShortNamesProbe checks the probe path and column fallback:
// when the Table probe fails, the kind still exists with NAME.
func TestDynamicKindsProbeFailure(t *testing.T) {
	src := newFakeSource([]apiResource{widgetResource()})
	src.tableErr[widgetsGVR] = errs.New(errs.Auth, opDiscover, "forbidden")
	d := dynamicBackend(t, src)
	refresh(t, d)

	w := specByName(t, d.Kinds(), "widgets")
	if len(w.Columns) < 2 || w.Columns[0].ID != "namespace" || w.Columns[1].ID != nameColumnID {
		t.Fatalf("columns = %+v, want the namespace/name fallback", w.Columns)
	}
}
