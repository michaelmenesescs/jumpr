// Generic Kubernetes resources (task 112): every listable API resource the
// cluster serves becomes a kind, with the columns its own Table response
// declares, so a CRD's printer columns show up exactly as kubectl prints them.
//
// Discovery is cached for ten minutes and dropped on a context switch. The
// typed kinds of Backend stay the source of truth for pods, nodes and the rest;
// a resource whose plural, singular or short name collides with one of those is
// skipped, so a CRD can never shadow a built-in kind.
//
// describe and YAML reuse task 108's reader: the same fetch, event lookup,
// generic describe and sigs.k8s.io/yaml marshalling, over a kindDef built from
// discovery.
//
// The edit flow is v1: there are no local editor panes, so `e` offers
// k8s.apply-patch. The operator pastes a JSON merge patch into the action
// dialog's text parameter; Plan shows the server-side dry-run result as a
// unified diff and nothing is written until the bus runs the step.
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

const (
	// dynamicTTL is how long a discovery pass is reused.
	dynamicTTL = 10 * time.Minute
	// dynamicProbeTimeout bounds one Table probe during discovery, and
	// dynamicDiscoverTimeout the whole pass.
	dynamicProbeTimeout    = 5 * time.Second
	dynamicDiscoverTimeout = 30 * time.Second
	// dynamicListTimeout bounds one Table list.
	dynamicListTimeout = 30 * time.Second
	// dynamicListLimit is the page size a list asks for. kubectl uses 500.
	dynamicListLimit = 500
	// dynamicProbeLimit is the page size of the discovery probe: one row is
	// enough to learn the columns, and cheap on a big cluster.
	dynamicProbeLimit = 1
	// tableMediaType is the Table API's Accept value. The server answers with
	// the printer columns the resource author declared.
	tableMediaType = "application/json;as=Table;v=v1;g=meta.k8s.io"
	// tableIncludeObject asks for metadata only: no object body reaches this
	// process for a list (the draw of the Table API).
	tableIncludeObject = "Metadata"

	// SpecApplyPatch is the spec ID of the v1 edit flow (task 112).
	SpecApplyPatch = "k8s.apply-patch"
	// ParamPatch is the parameter holding the JSON merge patch text.
	ParamPatch = "patch"

	// opDiscover, opPatch and opDynamicObject are the error operations of
	// discovery, the patch planner and the dynamic object read.
	opDiscover      = "k8s.discovery"
	opDynamicList   = "k8s.list"
	opPatch         = "k8s.patch"
	opDynamicObject = "k8s.object"

	// nameColumnID is the field and column ID every kind has.
	nameColumnID = "name"
	// namespaceColumnID is the namespace column of a namespaced kind.
	namespaceColumnID = "namespace"
)

// apiResource is one listable API resource discovery reported.
type apiResource struct {
	Group      string
	Version    string
	Resource   string
	Kind       string
	Singular   string
	ShortNames []string
	Namespaced bool
}

// gvr is the resource's group-version-resource.
func (r apiResource) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource}
}

// groupVersion is the resource's group/version, for messages and titles.
func (r apiResource) groupVersion() schema.GroupVersion {
	return schema.GroupVersion{Group: r.Group, Version: r.Version}
}

// tableSource is what the dynamic cache reads: the cluster's listable
// resources and one resource's Table. clientSource implements it over
// client-go; the tests inject a fake, so no test needs an API server.
type tableSource interface {
	Resources(ctx context.Context) ([]apiResource, error)
	Table(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, namespace string, limit int) (*metav1.Table, error)
}

// apiServer is the real tableSource: discovery plus one HTTP client built from
// the context's rest config, so every request carries the same credentials,
// timeouts and proxies a generated client would use.
type apiServer struct {
	client *Client
}

// Resources lists the preferred version of every group's listable resources.
// A group that fails (an aggregated API server that is down) does not hide the
// rest: the partial result is used and the error is dropped, as kubectl does.
func (s apiServer) Resources(ctx context.Context) ([]apiResource, error) {
	disc, err := s.client.Discovery()
	if err != nil {
		return nil, err
	}
	lists, err := disc.ServerPreferredResources()
	if err != nil {
		var partial *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &partial) || len(lists) == 0 {
			return nil, &errs.Error{Kind: Classify(err), Op: opDiscover,
				Msg: "discovery: " + oneLine(err.Error()), Err: err}
		}
	}
	out := make([]apiResource, 0, 64)
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, gerr := schema.ParseGroupVersion(list.GroupVersion)
		if gerr != nil {
			continue // a group we cannot parse is one we cannot list
		}
		for _, api := range list.APIResources {
			if !listable(api) {
				continue
			}
			out = append(out, apiResource{
				Group:      gv.Group,
				Version:    gv.Version,
				Resource:   api.Name,
				Kind:       api.Kind,
				Singular:   api.SingularName,
				ShortNames: append([]string(nil), api.ShortNames...),
				Namespaced: api.Namespaced,
			})
		}
	}
	return out, nil
}

// listable reports whether discovery's entry is a resource this backend can
// list: not a subresource, and one the server says supports `list`.
func listable(api metav1.APIResource) bool {
	if api.Name == "" || strings.Contains(api.Name, "/") {
		return false
	}
	for _, verb := range api.Verbs {
		if verb == "list" {
			return true
		}
	}
	return false
}

// Table asks the server for one resource's Table.
func (s apiServer) Table(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, namespace string, limit int) (*metav1.Table, error) {
	cfg, err := s.client.RESTConfig()
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, errs.Wrap(errs.Config, opDiscover, err)
	}
	u := tableURL(cfg.Host, gvr, namespaced, namespace, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, opDiscover, err)
	}
	req.Header.Set("Accept", tableMediaType)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &errs.Error{Kind: errs.KindOf(err), Op: opDiscover, Msg: oneLine(err.Error()), Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, errs.Wrap(errs.Remote, opDiscover, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, body, gvr)
	}
	var table metav1.Table
	if err := json.Unmarshal(body, &table); err != nil {
		return nil, &errs.Error{Kind: errs.Parse, Op: opDiscover,
			Msg: fmt.Sprintf("%s did not parse as a Table", gvr.Resource), Err: err}
	}
	return &table, nil
}

// tableURL is the request URL of one Table: /api/v1 or /apis/<group>/<version>,
// then the namespace when the kind is namespaced and one is selected.
func tableURL(host string, gvr schema.GroupVersionResource, namespaced bool, namespace string, limit int) string {
	var b strings.Builder
	b.WriteString(strings.TrimSuffix(host, "/"))
	if gvr.Group == "" {
		b.WriteString("/api/")
	} else {
		b.WriteString("/apis/")
		b.WriteString(gvr.Group)
		b.WriteString("/")
	}
	b.WriteString(gvr.Version)
	if namespaced && namespace != "" {
		b.WriteString("/namespaces/")
		b.WriteString(namespace)
	}
	b.WriteString("/")
	b.WriteString(gvr.Resource)
	b.WriteString(fmt.Sprintf("?includeObject=%s&limit=%d", tableIncludeObject, limit))
	return b.String()
}

// statusError turns a non-200 Table response into an *errs.Error with the kind
// the error rules of BUILD-PLAN §7 name.
func statusError(code int, body []byte, gvr schema.GroupVersionResource) error {
	msg := oneLine(string(body))
	var status metav1.Status
	if err := json.Unmarshal(body, &status); err == nil && status.Message != "" {
		msg = oneLine(status.Message)
	}
	kind := errs.Remote
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = errs.Auth
	case http.StatusNotFound:
		kind = errs.NotAvailable
	case http.StatusTooManyRequests:
		kind = errs.Remote
	}
	return &errs.Error{Kind: kind, Op: opDiscover,
		Msg: fmt.Sprintf("%s: HTTP %d: %s", gvr.Resource, code, msg)}
}

// dynamicCache is the discovery result and its ten-minute lifetime. Every
// method is safe for concurrent use; nothing here touches the UI, and a stale
// cache is refreshed in the background so Kinds never blocks.
type dynamicCache struct {
	src tableSource
	now func() time.Time
	ttl time.Duration

	mu         sync.Mutex
	loaded     bool
	ok         bool // a pass finished successfully
	at         time.Time
	defs       []kindDef
	specs      []resource.KindSpec
	index      map[string]int
	refreshing bool
}

// newDynamicCache builds a cache over src.
func newDynamicCache(src tableSource, now func() time.Time, ttl time.Duration) *dynamicCache {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = dynamicTTL
	}
	return &dynamicCache{src: src, now: now, ttl: ttl, index: make(map[string]int)}
}

// kindSpecs is a copy of the discovered kinds.
func (c *dynamicCache) kindSpecs() []resource.KindSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]resource.KindSpec, len(c.specs))
	copy(out, c.specs)
	return out
}

// lookup resolves a discovered kind name or alias.
func (c *dynamicCache) lookup(name resource.KindName) (kindDef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, ok := c.index[strings.ToLower(strings.TrimSpace(string(name)))]
	if !ok {
		return kindDef{}, false
	}
	return c.defs[i], true
}

// stale reports whether the cache needs a refresh.
func (c *dynamicCache) stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.loaded || !c.now().Before(c.at.Add(c.ttl))
}

// ensure refreshes the cache when it is stale, blocking on the caller's
// context. A failed pass is reported only when no earlier pass succeeded: with
// a result in hand the rows stay usable.
func (c *dynamicCache) ensure(ctx context.Context) error {
	if !c.stale() {
		return nil
	}
	if err := c.refresh(ctx); err != nil && !c.hasResult() {
		return err
	}
	return nil
}

// hasResult reports whether a discovery pass ever succeeded.
func (c *dynamicCache) hasResult() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ok
}

// refreshInBackground starts a refresh when the cache is stale and none is
// running. Kinds calls it, so the first `:kinds` after startup fills in without
// the UI ever waiting on the API server.
func (c *dynamicCache) refreshInBackground() {
	c.mu.Lock()
	if c.refreshing || (c.loaded && c.now().Before(c.at.Add(c.ttl))) {
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dynamicDiscoverTimeout)
		defer cancel()
		_ = c.refresh(ctx)
	}()
}

// refresh runs one discovery pass and swaps the result in. A failure keeps the
// previous kinds and is returned for the caller to report.
func (c *dynamicCache) refresh(ctx context.Context) error {
	c.mu.Lock()
	if c.refreshing {
		// Someone else is already discovering; wait for the result rather
		// than starting a second pass against the API server.
		c.mu.Unlock()
		return nil
	}
	c.refreshing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()

	if c.src == nil {
		return errs.New(errs.Config, opDiscover, "no Kubernetes client is configured")
	}
	dctx, cancel := context.WithTimeout(ctx, dynamicDiscoverTimeout)
	defer cancel()
	resources, err := c.src.Resources(dctx)
	if err != nil {
		c.fail(err)
		return err
	}
	taken := staticKeys()
	served := staticGVRs()
	defs := make([]kindDef, 0, len(resources))
	specs := make([]resource.KindSpec, 0, len(resources))
	index := make(map[string]int, len(resources)*3)
	sort.SliceStable(resources, func(i, j int) bool {
		if resources[i].Resource != resources[j].Resource {
			return resources[i].Resource < resources[j].Resource
		}
		return resources[i].groupVersion().String() < resources[j].groupVersion().String()
	})
	for _, api := range resources {
		name := strings.ToLower(strings.TrimSpace(api.Resource))
		if name == "" || taken[name] || served[api.gvr()] {
			continue
		}
		aliases := aliasesFor(api, name, taken)
		taken[name] = true
		for _, a := range aliases {
			taken[a] = true
		}
		cols := c.columns(dctx, api)
		spec := resource.KindSpec{
			Name:        resource.KindName(name),
			Aliases:     aliases,
			Title:       kindTitle(api),
			Scope:       scopeOf(api.Namespaced),
			Columns:     cols,
			DefaultSort: nameColumnID,
			Watchable:   false,
		}
		def := kindDef{
			spec:       spec,
			gvr:        api.gvr(),
			namespaced: api.Namespaced,
		}
		index[name] = len(defs)
		for _, a := range aliases {
			if _, exists := index[a]; !exists {
				index[a] = len(defs)
			}
		}
		defs = append(defs, def)
		specs = append(specs, spec)
	}
	c.mu.Lock()
	c.loaded = true
	c.at = c.now()
	c.defs = defs
	c.specs = specs
	c.index = index
	c.ok = true
	c.mu.Unlock()
	return nil
}

// fail records a discovery error: the previous result, if any, is kept and
// the next attempt waits a whole TTL.
func (c *dynamicCache) fail(error) {
	c.mu.Lock()
	c.loaded = true
	c.at = c.now()
	c.mu.Unlock()
}

// columns learns one kind's columns from a one-row Table probe. A probe that
// fails leaves the minimum: NAME and, for a namespaced resource, NAMESPACE.
func (c *dynamicCache) columns(ctx context.Context, api apiResource) []resource.Column {
	fallback := minimalColumns(api.Namespaced)
	if c.src == nil {
		return fallback
	}
	pctx, cancel := context.WithTimeout(ctx, dynamicProbeTimeout)
	defer cancel()
	table, err := c.src.Table(pctx, api.gvr(), api.Namespaced, "", dynamicProbeLimit)
	if err != nil || table == nil || len(table.ColumnDefinitions) == 0 {
		return fallback
	}
	return columnsFromTable(table.ColumnDefinitions, api.Namespaced)
}

// reset drops the cached result, which is what a context switch calls.
func (c *dynamicCache) reset() {
	c.mu.Lock()
	c.loaded = false
	c.at = time.Time{}
	c.defs = nil
	c.specs = nil
	c.index = make(map[string]int)
	c.ok = false
	c.mu.Unlock()
}

// staticGVRs is every resource the typed kinds already serve. A resource whose
// GVR is in here is skipped: the typed kind is the better row builder, and two
// kinds for one resource would collide in the registry.
func staticGVRs() map[schema.GroupVersionResource]bool {
	out := make(map[schema.GroupVersionResource]bool, 16)
	for _, def := range kindDefs() {
		out[def.gvr] = true
	}
	return out
}

// staticKeys is every kind name and alias the typed kinds already claim.
func staticKeys() map[string]bool {
	taken := make(map[string]bool, 64)
	for _, spec := range kindSpecs() {
		taken[strings.ToLower(string(spec.Name))] = true
		for _, alias := range spec.Aliases {
			taken[strings.ToLower(alias)] = true
		}
	}
	return taken
}

// aliasCandidates is a resource's alias list: its short names and its singular
// name, without the plural and without duplicates.
func aliasCandidates(api apiResource, name string) []string {
	out := make([]string, 0, len(api.ShortNames)+1)
	seen := map[string]bool{name: true}
	for _, candidate := range append(append([]string(nil), api.ShortNames...), api.Singular) {
		a := strings.ToLower(strings.TrimSpace(candidate))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// aliasesFor is the alias list of a resource whose plural is free.
func aliasesFor(api apiResource, name string, taken map[string]bool) []string {
	return freeAliases(aliasCandidates(api, name), taken)
}

// freeAliases drops the aliases another kind already claims.
func freeAliases(candidates []string, taken map[string]bool) []string {
	out := make([]string, 0, len(candidates))
	for _, a := range candidates {
		if !taken[a] {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// kindTitle is the kind's title, with its group and version so `:kinds` shows
// which API group a resource belongs to (task 112 requirement 1).
func kindTitle(api apiResource) string {
	title := strings.TrimSpace(api.Kind)
	if title == "" {
		title = api.Resource
	}
	if !strings.HasSuffix(title, "s") {
		title += "s"
	}
	return title + " · " + api.groupVersion().String()
}

// scopeOf maps the API's namespaced flag onto the resource scope.
func scopeOf(namespaced bool) resource.Scope {
	if namespaced {
		return resource.ScopeNamespace
	}
	return resource.ScopeGlobal
}

// minimalColumns is the fallback column list of a kind whose Table probe
// failed: the two columns every object has.
func minimalColumns(namespaced bool) []resource.Column {
	cols := make([]resource.Column, 0, 2)
	if namespaced {
		cols = append(cols, resource.Column{ID: namespaceColumnID, Title: "NAMESPACE", Width: 14})
	}
	return append(cols, resource.Column{ID: nameColumnID, Title: "NAME"})
}

// columnsFromTable turns the server's column definitions into jumper columns.
// A number is right-aligned (task 112, Pitfalls); a `date` format is a time.
func columnsFromTable(defs []metav1.TableColumnDefinition, namespaced bool) []resource.Column {
	cols := make([]resource.Column, 0, len(defs)+1)
	seen := make(map[string]int, len(defs))
	hasName := false
	for i, d := range defs {
		id := columnID(d.Name)
		if id == "" {
			id = fmt.Sprintf("col_%d", i)
		}
		if n, ok := seen[id]; ok {
			seen[id] = n + 1
			id = fmt.Sprintf("%s_%d", id, n+1)
		} else {
			seen[id] = 1
		}
		col := resource.Column{ID: id, Title: strings.ToUpper(strings.TrimSpace(d.Name))}
		switch {
		case strings.EqualFold(d.Format, "date"):
			col.Kind = resource.ColTime
			col.Align = resource.AlignRight
		case strings.EqualFold(d.Type, "integer"), strings.EqualFold(d.Type, "number"):
			col.Kind = resource.ColNumber
			col.Align = resource.AlignRight
		}
		if id == nameColumnID {
			hasName = true
		}
		cols = append(cols, col)
	}
	if !hasName {
		// The Table API always sends NAME first; a custom printer that omits
		// it still needs one for the table view to key rows by name.
		head := []resource.Column{{ID: nameColumnID, Title: "NAME"}}
		if namespaced && !hasColumn(cols, namespaceColumnID) {
			head = append(head, resource.Column{ID: namespaceColumnID, Title: "NAMESPACE", Width: 14})
		}
		cols = append(head, cols...)
	}
	return cols
}

// hasColumn reports whether cols holds id.
func hasColumn(cols []resource.Column, id string) bool {
	for _, c := range cols {
		if c.ID == id {
			return true
		}
	}
	return false
}

// columnID is the field ID of a column title: lower case, words joined by an
// underscore, punctuation dropped ("UP-TO-DATE" → "up_to_date").
func columnID(title string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
		default:
			gap = true
		}
	}
	return b.String()
}

// cellValue turns one Table cell into a field value. A number becomes an int64
// when it is integral, so the table renders it without decimals; anything the
// Table API sends that is not a number, string, bool or null is rendered with
// fmt.Sprint (task 112, Pitfalls).
func cellValue(v any) (any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case string:
		if x == "" {
			return nil, false
		}
		return x, true
	case bool:
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		if x == math.Trunc(x) && x >= math.MinInt64 && x <= math.MaxInt64 {
			return int64(x), true
		}
		return x, true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
		if f, err := x.Float64(); err == nil {
			return f, true
		}
		return x.String(), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case map[string]any, []any:
		return fmt.Sprint(x), true
	}
	return fmt.Sprint(v), true
}

// dynamicRow builds one row of a discovered kind from a Table row.
func dynamicRow(def kindDef, row metav1.TableRow, now time.Time) resource.Resource {
	meta := tableRowMeta(row)
	r := newRow(def, &meta, meta.Labels, now)
	if def.namespaced && meta.Namespace != "" {
		r.Fields[namespaceColumnID] = meta.Namespace
	}
	if name := strings.TrimSpace(meta.Name); name != "" {
		r.Fields[nameColumnID] = name
	}
	cols := def.spec.Columns
	for i, cell := range row.Cells {
		if i >= len(cols) {
			break
		}
		if v, ok := cellValue(cell); ok {
			r.Fields[cols[i].ID] = v
		}
	}
	r.Raw = meta
	return r
}

// tableRowMeta decodes a Table row's object metadata. A server that sent no
// object leaves the metadata empty; the row's name is then whatever the NAME
// cell holds.
func tableRowMeta(row metav1.TableRow) metav1.PartialObjectMetadata {
	var meta metav1.PartialObjectMetadata
	if len(row.Object.Raw) > 0 {
		_ = json.Unmarshal(row.Object.Raw, &meta)
	}
	return meta
}

// Dynamic is the Kubernetes backend as the registry sees it: the typed kinds of
// Backend plus every other listable resource the cluster serves (task 112).
// List, Get, Watch and ReadObject route a discovered kind to the Table API and
// the dynamic client; everything else is the embedded backend's.
type Dynamic struct {
	*Backend
	cache  *dynamicCache
	reader *ObjectReader // injected in tests; nil reads through the client
}

var (
	_ backend.Backend = (*Dynamic)(nil)
	_ backend.Watcher = (*Dynamic)(nil)
	_ action.Planner  = (*PatchPlanner)(nil)
)

// NewDynamic builds the backend of task 112 over Options.
func NewDynamic(o Options) (*Dynamic, error) {
	b, err := New(o)
	if err != nil {
		return nil, err
	}
	return newDynamicWithBackend(b), nil
}

// NewDynamicWithClient builds the backend around an existing client, which is
// what the tests use.
func NewDynamicWithClient(c *Client) *Dynamic {
	return newDynamicWithBackend(NewWithClient(c))
}

// newDynamicWithBackend wraps a backend and its discovery cache.
func newDynamicWithBackend(b *Backend) *Dynamic {
	d := &Dynamic{Backend: b, cache: newDynamicCache(clientSource{b.client}, b.now, dynamicTTL)}
	return d
}

// clientSource is the tableSource of a real cluster. It resolves the discovery
// client and the rest config on every call, so a context switch is picked up
// without rebuilding the cache's source.
type clientSource struct{ client *Client }

// Resources lists the cluster's listable resources.
func (s clientSource) Resources(ctx context.Context) ([]apiResource, error) {
	if s.client == nil {
		return nil, errs.New(errs.Config, opDiscover, "no Kubernetes client is configured")
	}
	return apiServer{client: s.client}.Resources(ctx)
}

// Table asks the cluster for one resource's Table.
func (s clientSource) Table(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, namespace string, limit int) (*metav1.Table, error) {
	if s.client == nil {
		return nil, errs.New(errs.Config, opDiscover, "no Kubernetes client is configured")
	}
	return apiServer{client: s.client}.Table(ctx, gvr, namespaced, namespace, limit)
}

// SetSource replaces the cache's source. It exists for tests and for a caller
// that injects its own discovery.
func (d *Dynamic) SetSource(src tableSource) {
	d.cache = newDynamicCache(src, d.now, dynamicTTL)
	d.cache.loaded = true
	d.cache.at = d.now()
}

// SetReader replaces the object reader describe and YAML use. It exists for
// tests, which have no kubeconfig.
func (d *Dynamic) SetReader(r *ObjectReader) { d.reader = r }

// objectReader is the reader of describe and YAML.
func (d *Dynamic) objectReader() (*ObjectReader, error) {
	if d.reader != nil {
		return d.reader, nil
	}
	return d.Backend.objectReader()
}

// Kinds returns the typed kinds followed by the discovered ones. It never
// blocks: a stale cache refreshes in the background.
func (d *Dynamic) Kinds() []resource.KindSpec {
	d.cache.refreshInBackground()
	base := d.Backend.Kinds()
	extra := d.cache.kindSpecs()
	out := make([]resource.KindSpec, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// RefreshKinds runs one discovery pass now, so a caller can key the registry
// with every kind before it registers this backend.
func (d *Dynamic) RefreshKinds(ctx context.Context) error {
	return d.cache.refresh(ctx)
}

// defFor resolves a kind through the typed kinds and then the discovered ones.
func (d *Dynamic) defFor(name resource.KindName) (kindDef, bool) {
	if def, ok := defFor(name); ok {
		return def, true
	}
	return d.cache.lookup(name)
}

// dynamicOnly reports whether name is served by discovery only.
func (d *Dynamic) dynamicOnly(name resource.KindName) bool {
	if _, ok := defFor(name); ok {
		return false
	}
	_, ok := d.cache.lookup(name)
	return ok
}

// List serves a discovered kind from the Table API and everything else from
// the informer cache.
func (d *Dynamic) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	def, ok := d.kind(ctx, q.Kind)
	if !ok {
		return d.Backend.List(ctx, q)
	}
	return d.listDynamic(ctx, q, def)
}

// kind resolves a discovered kind, giving a stale cache one chance to load
// first: the first query of a kind can arrive before the background discovery
// of `:kinds` finished. A failed discovery is not reported here; the caller's
// kind lookup is what decides, and the typed kinds still work.
func (d *Dynamic) kind(ctx context.Context, name resource.KindName) (kindDef, bool) {
	if def, ok := d.cache.lookup(name); ok {
		return def, true
	}
	if !d.cache.stale() {
		return kindDef{}, false
	}
	_ = d.cache.ensure(ctx)
	return d.cache.lookup(name)
}

// listDynamic is one Table request, streamed as one Result.
func (d *Dynamic) listDynamic(ctx context.Context, q resource.Query, def kindDef) (<-chan backend.Result, error) {
	if err := d.cache.ensure(ctx); err != nil {
		return nil, err
	}
	f := filter{sel: q.Selector, name: strings.TrimSpace(q.Args["name"])}
	ns := namespaceFor(q, d.sessionNamespace(), def)
	ch := make(chan backend.Result, 1)
	go func() {
		defer close(ch)
		res := backend.Result{Done: 1, Total: 1}
		rows, err := d.tableRows(ctx, def, ns, q)
		if err != nil {
			res.Err = err
		} else {
			items := make([]resource.Resource, 0, len(rows))
			for _, r := range rows {
				if !f.matches(r) {
					continue
				}
				items = append(items, r)
			}
			res.Items = items
		}
		select {
		case ch <- res:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// tableRows fetches one page of a discovered kind's Table as rows.
func (d *Dynamic) tableRows(ctx context.Context, def kindDef, ns string, q resource.Query) ([]resource.Resource, error) {
	if d.cache.src == nil {
		return nil, errs.New(errs.Config, opDynamicList, "no Kubernetes client is configured")
	}
	lctx, cancel := context.WithTimeout(ctx, dynamicListTimeout)
	defer cancel()
	table, err := d.cache.src.Table(lctx, def.gvr, def.namespaced, ns, dynamicListLimit)
	if err != nil {
		return nil, err
	}
	now := d.now()
	rows := make([]resource.Resource, 0, len(table.Rows))
	for _, row := range table.Rows {
		r := dynamicRow(def, row, now)
		if r.Ref.Name == "" {
			continue
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// Get returns one discovered object by listing its Table narrowed by
// metadata.name, which is what keeps the row's cells: the dynamic client's
// object alone does not carry the printed columns.
func (d *Dynamic) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	def, ok := d.kind(ctx, ref.Kind)
	if !ok {
		return d.Backend.Get(ctx, ref)
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return resource.Resource{}, errs.New(errs.Config, "k8s.Get", "the object needs a name")
	}
	ns := ref.Namespace
	if def.namespaced {
		if ns == "" {
			ns = d.sessionNamespace()
		}
	} else {
		ns = ""
	}
	rows, err := d.tableRows(ctx, def, ns, resource.Query{})
	if err != nil {
		return resource.Resource{}, err
	}
	for _, r := range rows {
		if r.Ref.Name == name {
			return r, nil
		}
	}
	return resource.Resource{}, errs.New(errs.NotAvailable, "k8s.Get",
		fmt.Sprintf("%s %s not found", def.spec.Name, strings.TrimPrefix(ns+"/"+name, "/")))
}

// Watch is not available for a discovered kind: the Table API is a snapshot,
// and Kinds marks those kinds not watchable.
func (d *Dynamic) Watch(ctx context.Context, q resource.Query) (<-chan backend.Event, error) {
	if d.dynamicOnly(q.Kind) {
		return nil, errs.New(errs.NotAvailable, "k8s.Watch",
			fmt.Sprintf("%s is not watchable: it is served by the Table API", q.Kind))
	}
	return d.Backend.Watch(ctx, q)
}

// SetOnSwitch drops the discovery cache after a context switch, then calls fn.
func (d *Dynamic) SetOnSwitch(fn func(context string)) {
	d.Backend.SetOnSwitch(func(name string) {
		d.cache.reset()
		if fn != nil {
			fn(name)
		}
	})
}

// ReadObject reads describe, YAML and events for any object: the typed kinds go
// through task 108's reader unchanged, a discovered kind through the same
// reader over its discovered definition.
func (d *Dynamic) ReadObject(ctx context.Context, ref resource.Ref, reveal bool) (ObjectDetail, error) {
	if !d.dynamicOnly(ref.Kind) {
		return d.Backend.ReadObject(ctx, ref, reveal)
	}
	def, ok := d.cache.lookup(ref.Kind)
	if !ok {
		return ObjectDetail{}, errs.New(errs.Config, opDynamicObject,
			fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return ObjectDetail{}, errs.New(errs.Config, opDynamicObject, "the object needs a name")
	}
	ref.Name = name
	ref.Kind = def.spec.Name
	if def.namespaced {
		if ref.Namespace == "" {
			ref.Namespace = defaultNamespace
		}
	} else {
		ref.Namespace = ""
	}
	reader, err := d.objectReader()
	if err != nil {
		return ObjectDetail{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	obj, err := reader.get(rctx, def, ref)
	if err != nil {
		return ObjectDetail{}, err
	}
	detail := ObjectDetail{Ref: ref, Kind: objectKindName(def)}
	if kind := strings.TrimSpace(obj.GetKind()); kind != "" {
		detail.Kind = kind
	}
	events, eventErr := reader.events(rctx, def, ref, obj.GetUID())
	detail.Events, detail.EventErr = events, eventErr
	detail.Describe, err = reader.describe(rctx, def, obj, events, eventErr)
	if err != nil {
		return ObjectDetail{}, err
	}
	detail.YAML, detail.Redacted, err = marshalYAML(def, obj, false)
	if err != nil {
		return ObjectDetail{}, err
	}
	return detail, nil
}

// PatchPlanner is the v1 edit flow (task 112 requirement 2): a JSON merge patch
// typed into the action dialog's text parameter, previewed with a server-side
// dry run and applied through the bus.
type PatchPlanner struct {
	d   *Dynamic
	dyn dynamic.Interface // injected in tests
	now func() time.Time
}

// PatchPlanner returns the planner of k8s.apply-patch.
func (d *Dynamic) PatchPlanner() *PatchPlanner {
	return &PatchPlanner{d: d, now: d.now}
}

// Owns claims exactly the apply-patch spec: it is registered under its own
// name, not under the "k8s" prefix the backend's other specs use.
func (p *PatchPlanner) Owns(specID string) bool { return specID == SpecApplyPatch }

// patchSpec is the action menu entry of the edit flow.
func patchSpec() action.Spec {
	return action.Spec{
		ID:        SpecApplyPatch,
		Title:     "edit (apply JSON merge patch)",
		Risk:      action.RiskNormal,
		HasDryRun: true,
		Params: []action.Param{{
			Name:     ParamPatch,
			Title:    "JSON merge patch",
			Required: true,
		}},
	}
}

// Actions offers apply-patch for a selection of this backend's rows. It is
// pure: it only reads the rows.
func (p *PatchPlanner) Actions(selection []resource.Resource) []action.Spec {
	if p == nil || p.d == nil || len(selection) == 0 {
		return nil
	}
	for _, r := range selection {
		if r.Ref.Backend != backendName {
			return nil
		}
		if _, ok := p.d.defFor(r.Ref.Kind); !ok {
			return nil
		}
	}
	return []action.Spec{patchSpec()}
}

// Plan validates the patch, runs one server-side dry-run patch per target and
// returns the steps that apply it for real. Nothing is written here.
func (p *PatchPlanner) Plan(ctx context.Context, req action.Request) (action.Plan, error) {
	if req.SpecID != SpecApplyPatch {
		return action.Plan{}, errs.New(errs.Config, opPlan, fmt.Sprintf("unknown spec %q", req.SpecID))
	}
	if len(req.Targets) == 0 {
		return action.Plan{}, errs.New(errs.Config, opPlan, "no targets")
	}
	patch, err := parsePatch(req.Params[ParamPatch])
	if err != nil {
		return action.Plan{}, err
	}
	dyn, err := p.client()
	if err != nil {
		return action.Plan{}, err
	}
	planned := make([]patchTarget, 0, len(req.Targets))
	var previews []string
	for _, t := range req.Targets {
		pt, perr := p.resolve(t)
		if perr != nil {
			return action.Plan{}, perr
		}
		diff, derr := p.dryRun(ctx, dyn, pt, patch)
		if derr != nil {
			return action.Plan{}, derr
		}
		if diff != "" {
			previews = append(previews, diff)
		}
		planned = append(planned, pt)
	}

	plan := action.Plan{
		Spec:     patchSpec(),
		Request:  req,
		Title:    patchTitle(planned),
		Via:      p.d.via(),
		Preview:  strings.Join(previews, "\n"),
		Warnings: nil,
	}
	for _, pt := range planned {
		pt := pt
		plan.Steps = append(plan.Steps, action.Step{
			Target:  pt.target,
			Display: p.display(pt, patch),
			Run: func(ctx context.Context) action.StepResult {
				return p.apply(ctx, dyn, pt, patch)
			},
		})
	}
	if plan.Preview == "" {
		plan.Preview = "no change: the patch leaves every object as it is"
	}
	return plan, nil
}

// patchTarget is one object a plan patches.
type patchTarget struct {
	target action.Target
	def    kindDef
	ns     string
	name   string
}

// resolve turns a request target into the object a step patches.
func (p *PatchPlanner) resolve(t action.Target) (patchTarget, error) {
	def, ok := p.d.defFor(t.Ref.Kind)
	if !ok {
		return patchTarget{}, errs.New(errs.Config, opPlan,
			fmt.Sprintf("unknown kind %q", t.Ref.Kind))
	}
	name := strings.TrimSpace(t.Ref.Name)
	if name == "" {
		return patchTarget{}, errs.New(errs.Config, opPlan, "target has no name")
	}
	ns := strings.TrimSpace(t.Ref.Namespace)
	if def.namespaced && ns == "" {
		ns = p.d.sessionNamespace()
	}
	if def.namespaced && ns == "" {
		return patchTarget{}, errs.New(errs.Config, opPlan,
			fmt.Sprintf("%s %s has no namespace", def.spec.Name, name))
	}
	if !def.namespaced {
		ns = ""
	}
	if t.Ref.Backend != "" && t.Ref.Backend != backendName {
		return patchTarget{}, errs.New(errs.Config, opPlan,
			fmt.Sprintf("%s is not this backend's object", t.Ref.String()))
	}
	ref := t.Ref
	ref.Backend = backendName
	ref.Kind = def.spec.Name
	ref.Namespace = ns
	ref.Name = name
	target := t
	target.Ref = ref
	return patchTarget{target: target, def: def, ns: ns, name: name}, nil
}

// parsePatch validates the pasted text: it must be a JSON object, because a
// merge patch of anything else would replace the whole object.
func parsePatch(text string) ([]byte, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errs.New(errs.Config, opPlan, "the patch is empty: type or paste a JSON merge patch")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil, &errs.Error{Kind: errs.Config, Op: opPlan,
			Msg: "the patch is not a JSON object", Err: err}
	}
	return []byte(trimmed), nil
}

// client is the dynamic client the steps use.
func (p *PatchPlanner) client() (dynamic.Interface, error) {
	if p.dyn != nil {
		return p.dyn, nil
	}
	if p.d == nil || p.d.client == nil {
		return nil, errs.New(errs.Config, opPatch, "no Kubernetes client is configured")
	}
	return p.d.client.Dynamic()
}

// resource is the dynamic client scoped to one target.
func (p *PatchPlanner) resource(dyn dynamic.Interface, pt patchTarget) dynamic.ResourceInterface {
	if pt.def.namespaced {
		return dyn.Resource(pt.def.gvr).Namespace(pt.ns)
	}
	return dyn.Resource(pt.def.gvr)
}

// dryRun patches pt with DryRun=All and returns the unified diff between the
// object before and after. The API server validates the patch and the object
// that would result from it, so a forbidden field fails here, not in a step.
func (p *PatchPlanner) dryRun(ctx context.Context, dyn dynamic.Interface, pt patchTarget, patch []byte) (string, error) {
	ri := p.resource(dyn, pt)
	before, err := ri.Get(ctx, pt.name, metav1.GetOptions{})
	if err != nil {
		return "", patchError(err, pt)
	}
	after, err := ri.Patch(ctx, pt.name, types.MergePatchType, patch,
		metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return "", patchError(err, pt)
	}
	oldYAML, _, err := marshalYAML(pt.def, before, false)
	if err != nil {
		return "", err
	}
	newYAML, _, err := marshalYAML(pt.def, after, false)
	if err != nil {
		return "", err
	}
	return unifiedDiff(pt, oldYAML, newYAML)
}

// apply performs the patch for real. It runs inside a step, after the bus has
// the operator's confirmation.
func (p *PatchPlanner) apply(ctx context.Context, dyn dynamic.Interface, pt patchTarget, patch []byte) action.StepResult {
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	_, err := p.resource(dyn, pt).Patch(sctx, pt.name, types.MergePatchType, patch,
		metav1.PatchOptions{})
	res := action.StepResult{Target: pt.target, Duration: time.Since(start)}
	if err != nil {
		res.RC = 1
		res.Err = patchError(err, pt)
		res.Output = res.Err.Error()
		return res
	}
	res.Output = "patched " + strings.TrimPrefix(pt.ns+"/"+pt.name, "/")
	return res
}

// patchError is the *errs.Error of a patch or get failure.
func patchError(err error, pt patchTarget) error {
	kind := Classify(err)
	msg := fmt.Sprintf("%s %s: %s", pt.def.spec.Name, strings.TrimPrefix(pt.ns+"/"+pt.name, "/"),
		oneLine(err.Error()))
	if apierrors.IsNotFound(err) {
		kind = errs.NotAvailable
		msg = fmt.Sprintf("%s %s not found", pt.def.spec.Name, strings.TrimPrefix(pt.ns+"/"+pt.name, "/"))
	}
	return &errs.Error{Kind: kind, Op: opPatch, Msg: msg, Err: err}
}

// display is the kubectl line equivalent to a step.
func (p *PatchPlanner) display(pt patchTarget, patch []byte) string {
	return p.d.kubectlLine(pt.ns, "patch", string(pt.def.spec.Name), pt.name,
		"--type=merge", "-p", string(patch))
}

// patchTitle is the plan dialog's one-line title.
func patchTitle(planned []patchTarget) string {
	if len(planned) == 1 {
		pt := planned[0]
		return fmt.Sprintf("patch %s %s", pt.def.spec.Name, strings.TrimPrefix(pt.ns+"/"+pt.name, "/"))
	}
	return fmt.Sprintf("patch %d objects", len(planned))
}

// unifiedDiff renders before and after as a unified diff, empty when they are
// identical.
func unifiedDiff(pt patchTarget, before, after string) (string, error) {
	if before == after {
		return "", nil
	}
	label := strings.TrimPrefix(pt.ns+"/"+pt.name, "/")
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(before),
		B:        difflib.SplitLines(after),
		FromFile: label + " (current)",
		ToFile:   label + " (patched)",
		Context:  3,
	})
	if err != nil {
		return "", errs.Wrap(errs.Internal, opPatch, err)
	}
	return out, nil
}

// namespaceFor is the namespace a Table request uses: Args["namespace"], or the
// session's, and "" (every namespace) for a cluster-scoped kind or "all".
func namespaceFor(q resource.Query, sessionNS string, def kindDef) string {
	if !def.namespaced {
		return ""
	}
	ns := strings.TrimSpace(q.Args["namespace"])
	switch {
	case ns == "":
		ns = sessionNS
	case strings.EqualFold(ns, "all"):
		ns = ""
	}
	return ns
}
