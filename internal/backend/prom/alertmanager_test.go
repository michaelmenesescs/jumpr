package prom

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport/httpx"
)

// amTestdata is where the Alertmanager fixtures live.
const amTestdata = "testdata/alertmanager"

// amNow is the clock every Alertmanager test runs with.
var amNow = time.Date(2026, 11, 12, 22, 30, 0, 0, time.UTC)

// amRequest is one request the fixture server received.
type amRequest struct {
	Method string
	Path   string
	Body   []byte
}

// amFixture is an Alertmanager that answers from the recorded fixtures and
// remembers what it was asked.
type amFixture struct {
	*httptest.Server

	mu       sync.Mutex
	reqs     []amRequest
	statuses map[string]int // "METHOD path" → status to force
}

// amRoutes maps a request onto the fixture file that answers it.
var amRoutes = map[string]string{
	http.MethodGet + " " + pathAMAlerts:     "alerts.json",
	http.MethodGet + " " + pathAMSilences:   "silences.json",
	http.MethodPost + " " + pathAMSilences:  "silence_created.json",
	http.MethodDelete + " " + pathAMSilence: "",
	http.MethodGet + " " + pathAMHealthy:    "",
}

// newAMFixture starts the fixture Alertmanager.
func newAMFixture(t *testing.T) *amFixture {
	t.Helper()
	f := &amFixture{statuses: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, amRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		f.mu.Unlock()

		key := r.Method + " " + r.URL.Path
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, pathAMSilence) {
			key = http.MethodDelete + " " + pathAMSilence
		}
		f.mu.Lock()
		status, forced := f.statuses[key]
		f.mu.Unlock()
		if forced {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"fixture failure"}`))
			return
		}
		if key == http.MethodGet+" "+pathAMHealthy {
			_, _ = w.Write([]byte("OK\n"))
			return
		}
		name, ok := amRoutes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no fixture for ` + key + `"}`))
			return
		}
		if name == "" || key == http.MethodDelete+" "+pathAMSilence {
			w.WriteHeader(http.StatusOK)
			return
		}
		data, err := os.ReadFile(filepath.Join(amTestdata, name))
		if err != nil {
			t.Errorf("read fixture %s: %v", name, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(f.Close)
	return f
}

// forceStatus makes the fixture answer key with status.
func (f *amFixture) forceStatus(key string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[key] = status
}

// requests returns every request the fixture received.
func (f *amFixture) requests() []amRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]amRequest(nil), f.reqs...)
}

// last returns the last request with method and path.
func (f *amFixture) last(method, path string) (amRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].Method == method && f.reqs[i].Path == path {
			return f.reqs[i], true
		}
	}
	return amRequest{}, false
}

// count counts the requests with method and path.
func (f *amFixture) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// newTestAMClient builds an Alertmanager client over fs the way the backend
// builds one.
func newTestAMClient(t *testing.T, fs *amFixture, name string) *AlertmanagerClient {
	t.Helper()
	prof := &httpx.Profile{Name: "am-test"}
	hc, err := prof.Client(httpx.Deps{})
	if err != nil {
		t.Fatalf("httpx client: %v", err)
	}
	c, err := NewAlertmanagerClient(AlertmanagerClientOptions{
		URL:    fs.URL,
		Client: hc,
		Name:   name,
	})
	if err != nil {
		t.Fatalf("NewAlertmanagerClient: %v", err)
	}
	return c
}

// newTestAMBackend builds an Alertmanager backend over fs.
func newTestAMBackend(t *testing.T, fs *amFixture, name string) *AlertmanagerBackend {
	t.Helper()
	return NewAlertmanager(AlertmanagerOptions{
		Servers: []AlertmanagerServer{{Name: name, Client: newTestAMClient(t, fs, name)}},
		Now:     func() time.Time { return amNow },
	})
}

// listAM drains a List stream of the Alertmanager backend.
func listAM(t *testing.T, b *AlertmanagerBackend, q resource.Query) []backend.Result {
	t.Helper()
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List(%v): %v", q.Kind, err)
	}
	return collect(t, ch)
}

// amRows returns every row of one kind from one server.
func amRows(t *testing.T, b *AlertmanagerBackend, kind resource.KindName) []resource.Resource {
	t.Helper()
	var out []resource.Resource
	for _, res := range listAM(t, b, resource.Query{Kind: kind}) {
		if res.Err != nil {
			t.Fatalf("%s result error: %v", kind, res.Err)
		}
		out = append(out, res.Items...)
	}
	return out
}

func TestAlertmanagerClientAlerts(t *testing.T) {
	fs := newAMFixture(t)
	c := newTestAMClient(t, fs, "eu")

	alerts, err := c.Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 4 {
		t.Fatalf("got %d alerts, want 4", len(alerts))
	}
	if got, want := alerts[0].Labels["alertname"], "DiskFillingUp"; got != want {
		t.Errorf("alertname = %q, want %q", got, want)
	}
	if got, want := alerts[0].Fingerprint, "0f1e2d3c4b5a6978"; got != want {
		t.Errorf("fingerprint = %q, want %q", got, want)
	}
	if !alerts[0].StartsAt.Equal(time.Date(2026, 11, 12, 21, 30, 0, 0, time.UTC)) {
		t.Errorf("startsAt = %s", alerts[0].StartsAt)
	}
	if alerts[2].Silenced() != true || alerts[2].Inhibited() != false {
		t.Errorf("HighLatency silenced/inhibited = %v/%v, want true/false", alerts[2].Silenced(), alerts[2].Inhibited())
	}
	if alerts[3].Inhibited() != true || alerts[3].Silenced() != false {
		t.Errorf("NodeDown silenced/inhibited = %v/%v, want false/true", alerts[3].Silenced(), alerts[3].Inhibited())
	}
	if got, want := fs.count(http.MethodGet, pathAMAlerts), 1; got != want {
		t.Errorf("alerts requests = %d, want %d", got, want)
	}
}

func TestAlertmanagerClientSilences(t *testing.T) {
	fs := newAMFixture(t)
	c := newTestAMClient(t, fs, "eu")

	silences, err := c.Silences(context.Background())
	if err != nil {
		t.Fatalf("Silences: %v", err)
	}
	if len(silences) != 2 {
		t.Fatalf("got %d silences, want 2", len(silences))
	}
	first := silences[0]
	if got, want := first.ID, "c3a4b5d6-1111-4000-8000-000000000001"; got != want {
		t.Errorf("id = %q, want %q", got, want)
	}
	if got, want := first.CreatedBy, "michael"; got != want {
		t.Errorf("createdBy = %q, want %q", got, want)
	}
	if got, want := len(first.Matchers), 2; got != want {
		t.Fatalf("matchers = %d, want %d", got, want)
	}
	if m := first.Matchers[1]; m.Name != "service" || m.Value != "checkout" || m.IsRegex || !m.IsEqual {
		t.Errorf("matcher = %+v", m)
	}
	if got, want := first.State(amNow), "active"; got != want {
		t.Errorf("state = %q, want %q", got, want)
	}
	if got, want := silences[1].State(amNow), "expired"; got != want {
		t.Errorf("second state = %q, want %q", got, want)
	}
}

func TestAlertmanagerClientErrors(t *testing.T) {
	fs := newAMFixture(t)
	fs.forceStatus(http.MethodGet+" "+pathAMAlerts, http.StatusInternalServerError)
	c := newTestAMClient(t, fs, "eu")

	_, err := c.Alerts(context.Background())
	if err == nil {
		t.Fatal("Alerts on a 500 returned no error")
	}
	if got, want := errs.KindOf(err), errs.Remote; got != want {
		t.Errorf("kind = %s, want %s (%v)", got, want, err)
	}
}

func TestAlertmanagerClientCreateSilence(t *testing.T) {
	fs := newAMFixture(t)
	c := newTestAMClient(t, fs, "eu")

	id, err := c.CreateSilence(context.Background(), SilenceRequest{
		Matchers:  []AMMatcher{{Name: "alertname", Value: "HighErrorRate", IsEqual: true}},
		StartsAt:  "2026-11-12T22:30:00Z",
		EndsAt:    "2026-11-13T00:30:00Z",
		CreatedBy: "michael",
		Comment:   "maintenance",
	})
	if err != nil {
		t.Fatalf("CreateSilence: %v", err)
	}
	if got, want := id, "e5c6d7e8-3333-4000-8000-000000000003"; got != want {
		t.Errorf("silence id = %q, want %q", got, want)
	}
	req, ok := fs.last(http.MethodPost, pathAMSilences)
	if !ok {
		t.Fatal("no POST /api/v2/silences")
	}
	var sent map[string]any
	if err := json.Unmarshal(req.Body, &sent); err != nil {
		t.Fatalf("request body %q: %v", req.Body, err)
	}
	if got, want := sent["createdBy"], "michael"; got != want {
		t.Errorf("createdBy = %v, want %v", got, want)
	}
	if got, want := sent["endsAt"], "2026-11-13T00:30:00Z"; got != want {
		t.Errorf("endsAt = %v, want %v", got, want)
	}
}

func TestAlertmanagerBackendKinds(t *testing.T) {
	b := NewAlertmanager(AlertmanagerOptions{})
	if got, want := b.Name(), "am"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	specs := b.Kinds()
	if len(specs) != 2 {
		t.Fatalf("got %d kinds, want 2", len(specs))
	}
	byName := map[resource.KindName]resource.KindSpec{}
	for _, s := range specs {
		byName[s.Name] = s
		if s.Scope != resource.ScopeGlobal {
			t.Errorf("%s scope = %v, want global", s.Name, s.Scope)
		}
	}
	alert, ok := byName[KindAlert]
	if !ok {
		t.Fatalf("kind %q missing from %v", KindAlert, byName)
	}
	if got, want := strings.Join(alert.Aliases, ","), "alerts,al"; got != want {
		t.Errorf("alert aliases = %q, want %q", got, want)
	}
	var alertCols []string
	for _, c := range alert.Columns {
		alertCols = append(alertCols, c.ID)
	}
	if got, want := strings.Join(alertCols, ","), "sev,alert,labels,source,start,state"; got != want {
		t.Errorf("alert columns = %q, want %q", got, want)
	}
	sil, ok := byName[KindSilence]
	if !ok {
		t.Fatalf("kind %q missing from %v", KindSilence, byName)
	}
	if got, want := strings.Join(sil.Aliases, ","), "silences,si"; got != want {
		t.Errorf("silence aliases = %q, want %q", got, want)
	}
	var silCols []string
	for _, c := range sil.Columns {
		silCols = append(silCols, c.ID)
	}
	if got, want := strings.Join(silCols, ","), "id,matchers,creator,comment,starts,ends,state"; got != want {
		t.Errorf("silence columns = %q, want %q", got, want)
	}
}

func TestAlertmanagerBackendAlerts(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")

	rows := amRows(t, b, KindAlert)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	// Firing first, then severity, then start.
	want := []string{"HighErrorRate", "DiskFillingUp", "HighLatency", "NodeDown"}
	for i, name := range want {
		if got := rows[i].Str("alert"); got != name {
			t.Errorf("row %d alert = %q, want %q (order %v)", i, got, name, rowAlerts(rows))
		}
	}
	first := rows[0]
	if got, want := first.Ref.Backend, "am"; got != want {
		t.Errorf("ref backend = %q, want %q", got, want)
	}
	if got, want := first.Ref.Kind, KindAlert; got != want {
		t.Errorf("ref kind = %q, want %q", got, want)
	}
	if got, want := first.Ref.Namespace, "eu"; got != want {
		t.Errorf("ref namespace = %q, want %q", got, want)
	}
	if got, want := first.Ref.Name, "1a2b3c4d5e6f7a8b"; got != want {
		t.Errorf("ref name = %q, want %q", got, want)
	}
	if got, want := first.Str("sev"), "critical"; got != want {
		t.Errorf("sev = %q, want %q", got, want)
	}
	if got, want := first.Str("state"), "firing"; got != want {
		t.Errorf("state = %q, want %q", got, want)
	}
	if got, want := first.Str("source"), "alertmanager:eu"; got != want {
		t.Errorf("source = %q, want %q", got, want)
	}
	if got, want := first.Str("labels"), `dc="ewr", service="api"`; got != want {
		t.Errorf("labels = %q, want %q", got, want)
	}
	if start, ok := first.Fields["start"].(time.Time); !ok || !start.Equal(time.Date(2026, 11, 12, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", first.Fields["start"])
	}
	if first.Status != resource.StatusCrit {
		t.Errorf("critical firing status = %v, want Crit", first.Status)
	}
	if got, want := rows[2].Str("state"), "silenced"; got != want {
		t.Errorf("HighLatency state = %q, want %q", got, want)
	}
	if got, want := rows[3].Str("state"), "inhibited"; got != want {
		t.Errorf("NodeDown state = %q, want %q", got, want)
	}
	if got, want := rows[3].Str("sev"), "critical"; got != want {
		t.Errorf("NodeDown sev = %q, want %q", got, want)
	}
	if got, want := first.Labels["service"], "api"; got != want {
		t.Errorf("full labels kept in Labels: service = %q, want %q", got, want)
	}
}

// rowAlerts is the alertname column of every row, for failure messages.
func rowAlerts(rows []resource.Resource) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Str("alert"))
	}
	return out
}

func TestAlertmanagerBackendSilences(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")

	rows := amRows(t, b, KindSilence)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	active := rows[0]
	if got, want := active.Str("state"), "active"; got != want {
		t.Errorf("state = %q, want %q", got, want)
	}
	if got, want := active.Str("id"), "c3a4b5d6"; got != want {
		t.Errorf("id = %q, want %q", got, want)
	}
	if got, want := active.Ref.Name, "c3a4b5d6-1111-4000-8000-000000000001"; got != want {
		t.Errorf("ref name = %q, want %q", got, want)
	}
	if got, want := active.Str("matchers"), `alertname="HighLatency", service="checkout"`; got != want {
		t.Errorf("matchers = %q, want %q", got, want)
	}
	if got, want := active.Str("creator"), "michael"; got != want {
		t.Errorf("creator = %q, want %q", got, want)
	}
	if got, want := active.Str("comment"), "maintenance window on checkout"; got != want {
		t.Errorf("comment = %q, want %q", got, want)
	}
	if ends, ok := active.Fields["ends"].(time.Time); !ok || !ends.Equal(time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("ends = %v", active.Fields["ends"])
	}
	if got, want := rows[1].Str("state"), "expired"; got != want {
		t.Errorf("second state = %q, want %q", got, want)
	}
	raw, ok := active.Raw.(AMSilence)
	if !ok {
		t.Fatalf("Raw = %T, want AMSilence", active.Raw)
	}
	if got, want := len(raw.Matchers), 2; got != want {
		t.Errorf("raw matchers = %d, want %d", got, want)
	}
}

func TestAlertmanagerBackendServerFilter(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")

	res := listAM(t, b, resource.Query{Kind: KindAlert, Args: map[string]string{ArgServer: "eu"}})
	if len(res) != 1 || len(res[0].Items) != 4 {
		t.Fatalf("filtered list = %+v", res)
	}
	if _, err := b.List(context.Background(), resource.Query{Kind: KindAlert, Args: map[string]string{ArgServer: "xx"}}); err == nil {
		t.Error("unknown server returned no error")
	} else if got, want := errs.KindOf(err), errs.Config; got != want {
		t.Errorf("kind = %s, want %s", got, want)
	}
	if _, err := NewAlertmanager(AlertmanagerOptions{}).List(context.Background(), resource.Query{Kind: KindAlert}); err == nil {
		t.Error("unconfigured backend returned no error")
	}
	if _, err := b.List(context.Background(), resource.Query{Kind: KindTarget}); err == nil {
		t.Error("a prometheus kind was accepted by the alertmanager backend")
	}
}

func TestAlertmanagerBackendGet(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")

	row, err := b.Get(context.Background(), resource.Ref{
		Backend: "am", Kind: KindAlert, Namespace: "eu", Name: "1a2b3c4d5e6f7a8b",
	})
	if err != nil {
		t.Fatalf("Get alert: %v", err)
	}
	if got, want := row.Str("alert"), "HighErrorRate"; got != want {
		t.Errorf("alert = %q, want %q", got, want)
	}
	sil, err := b.Get(context.Background(), resource.Ref{
		Backend: "am", Kind: KindSilence, Namespace: "eu", Name: "d4b5c6d7-2222-4000-8000-000000000002",
	})
	if err != nil {
		t.Fatalf("Get silence: %v", err)
	}
	if got, want := sil.Str("creator"), "ops"; got != want {
		t.Errorf("creator = %q, want %q", got, want)
	}
	if _, err := b.Get(context.Background(), resource.Ref{Kind: KindAlert, Namespace: "eu", Name: "nope"}); err == nil {
		t.Error("a missing ref returned no error")
	} else if got, want := errs.KindOf(err), errs.NotAvailable; got != want {
		t.Errorf("kind = %s, want %s", got, want)
	}
	if _, err := b.Get(context.Background(), resource.Ref{Kind: KindTarget, Namespace: "eu", Name: "x"}); err == nil {
		t.Error("an unknown kind returned no error")
	}
}

func TestAlertmanagerBackendHealth(t *testing.T) {
	if got, want := NewAlertmanager(AlertmanagerOptions{}).Health(context.Background()).State, backend.HealthUnconfigured; got != want {
		t.Errorf("unconfigured state = %v, want %v", got, want)
	}

	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")
	h := b.Health(context.Background())
	if h.State != backend.HealthOK {
		t.Errorf("state = %v (%s), want OK", h.State, h.Detail)
	}
	status := b.Status(context.Background())
	if len(status) != 1 || status[0].Name != "eu" || status[0].Health.State != backend.HealthOK {
		t.Errorf("status = %+v", status)
	}

	fs.forceStatus(http.MethodGet+" "+pathAMHealthy, http.StatusInternalServerError)
	b2 := newTestAMBackend(t, fs, "eu")
	if h := b2.Health(context.Background()); h.State != backend.HealthDown {
		t.Errorf("state with a failing probe = %v (%s), want Down", h.State, h.Detail)
	}
}

func TestAlertmanagerBackendLinks(t *testing.T) {
	b := NewAlertmanager(AlertmanagerOptions{})
	if got := b.Links(resource.Resource{Ref: resource.Ref{Kind: KindAlert}}); got != nil {
		t.Errorf("Links = %v, want nil", got)
	}
}

// newAMPlanner builds the planner over fs with a fixed clock and creator.
func newAMPlanner(t *testing.T, fs *amFixture, name string) *AlertmanagerPlanner {
	t.Helper()
	return NewAlertmanagerPlanner(AlertmanagerPlannerOptions{
		Servers: []AlertmanagerServer{{Name: name, Client: newTestAMClient(t, fs, name)}},
		Creator: "michael",
		Now:     func() time.Time { return amNow },
	})
}

// silenceReq is a decoded CreateSilence body.
type silenceReq struct {
	Matchers []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		IsRegex bool   `json:"isRegex"`
		IsEqual bool   `json:"isEqual"`
	} `json:"matchers"`
	StartsAt  string `json:"startsAt"`
	EndsAt    string `json:"endsAt"`
	CreatedBy string `json:"createdBy"`
	Comment   string `json:"comment"`
}

// postedSilence decodes the last POST /api/v2/silences body.
func postedSilence(t *testing.T, fs *amFixture) silenceReq {
	t.Helper()
	req, ok := fs.last(http.MethodPost, pathAMSilences)
	if !ok {
		t.Fatal("no POST /api/v2/silences")
	}
	var out silenceReq
	if err := json.Unmarshal(req.Body, &out); err != nil {
		t.Fatalf("body %q: %v", req.Body, err)
	}
	return out
}

func TestAlertmanagerPlannerActions(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")
	p := newAMPlanner(t, fs, "eu")

	alerts := amRows(t, b, KindAlert)
	specs := p.Actions(alerts[:1])
	if len(specs) != 1 || specs[0].ID != SpecSilenceCreate {
		t.Fatalf("alert actions = %+v", specs)
	}
	if specs[0].Risk != action.RiskNormal {
		t.Errorf("create risk = %v, want normal", specs[0].Risk)
	}
	var names []string
	for _, prm := range specs[0].Params {
		names = append(names, prm.Name)
	}
	if got, want := strings.Join(names, ","), ParamDuration+","+ParamComment; got != want {
		t.Errorf("params = %q, want %q", got, want)
	}

	silences := amRows(t, b, KindSilence)
	specs = p.Actions(silences)
	var ids []string
	for _, s := range specs {
		ids = append(ids, s.ID)
	}
	if got, want := strings.Join(ids, ","), SpecSilenceExpire+","+SpecSilenceExtend; got != want {
		t.Errorf("silence actions = %q, want %q", got, want)
	}
	if specs[0].Risk != action.RiskNormal {
		t.Errorf("expire risk = %v, want normal", specs[0].Risk)
	}
	// An expired silence cannot be expired again, but can be extended.
	specs = p.Actions(silences[1:])
	ids = nil
	for _, s := range specs {
		ids = append(ids, s.ID)
	}
	if got, want := strings.Join(ids, ","), SpecSilenceExtend; got != want {
		t.Errorf("expired silence actions = %q, want %q", got, want)
	}
	if got := p.Actions(append(alerts[:1:1], silences[:1]...)); got != nil {
		t.Errorf("mixed selection actions = %+v, want nil", got)
	}
	if got := p.Actions(nil); got != nil {
		t.Errorf("empty selection actions = %+v, want nil", got)
	}
	if !p.Owns(SpecSilenceCreate) || p.Owns("linux.systemctl.restart") {
		t.Error("Owns does not claim the alertmanager prefix")
	}
}

func TestAlertmanagerPlannerCreateSilence(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")
	p := newAMPlanner(t, fs, "eu")

	alert := amRows(t, b, KindAlert)[0] // HighErrorRate, critical, dc=ewr, service=api
	plan, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceCreate,
		Targets: []action.Target{{Ref: alert.Ref, Labels: alert.Labels}},
		Params:  map[string]string{ParamComment: "maintenance on api", ParamDuration: "90m"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got, want := len(plan.Steps), 1; got != want {
		t.Fatalf("steps = %d, want %d", got, want)
	}
	if !strings.Contains(plan.Title, "HighErrorRate") {
		t.Errorf("title = %q, want it to name the alert", plan.Title)
	}
	wantDisplay := `POST ` + fs.URL + pathAMSilences + ` {`
	if !strings.HasPrefix(plan.Steps[0].Display, wantDisplay) {
		t.Errorf("display = %q, want the POST call %q", plan.Steps[0].Display, wantDisplay)
	}
	if !strings.Contains(plan.Steps[0].Display, `"comment":"maintenance on api"`) {
		t.Errorf("display = %q, want the comment in the body", plan.Steps[0].Display)
	}

	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("step error: %v", res.Err)
	}
	if res.RC != 0 {
		t.Errorf("rc = %d, want 0", res.RC)
	}
	if !strings.Contains(res.Output, "e5c6d7e8-3333-4000-8000-000000000003") {
		t.Errorf("output = %q, want the new silence id", res.Output)
	}
	body := postedSilence(t, fs)
	want := []struct{ name, value string }{
		{"alertname", "HighErrorRate"},
		{"dc", "ewr"},
		{"service", "api"},
	}
	if len(body.Matchers) != len(want) {
		t.Fatalf("matchers = %+v, want %v", body.Matchers, want)
	}
	for i, w := range want {
		m := body.Matchers[i]
		if m.Name != w.name || m.Value != w.value || m.IsRegex || !m.IsEqual {
			t.Errorf("matcher %d = %+v, want %+v", i, m, w)
		}
	}
	if got, want := body.StartsAt, "2026-11-12T22:30:00Z"; got != want {
		t.Errorf("startsAt = %q, want %q", got, want)
	}
	if got, want := body.EndsAt, "2026-11-13T00:00:00Z"; got != want {
		t.Errorf("endsAt = %q, want %q", got, want)
	}
	if got, want := body.CreatedBy, "michael"; got != want {
		t.Errorf("createdBy = %q, want %q", got, want)
	}
	if got, want := body.Comment, "maintenance on api"; got != want {
		t.Errorf("comment = %q, want %q", got, want)
	}
}

func TestAlertmanagerPlannerCreateDefaultsDuration(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")
	p := newAMPlanner(t, fs, "eu")

	alert := amRows(t, b, KindAlert)[0]
	plan, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceCreate,
		Targets: []action.Target{{Ref: alert.Ref, Labels: alert.Labels}},
		Params:  map[string]string{ParamComment: "maintenance"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := plan.Steps[0].Run(context.Background()).Err; err != nil {
		t.Fatalf("step: %v", err)
	}
	if got, want := postedSilence(t, fs).EndsAt, "2026-11-13T00:30:00Z"; got != want {
		t.Errorf("endsAt = %q, want now+2h = %q", got, want)
	}
}

func TestAlertmanagerPlannerCreateRejectsBadInput(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")
	p := newAMPlanner(t, fs, "eu")
	alert := amRows(t, b, KindAlert)[0]

	cases := []struct {
		name   string
		req    action.Request
		server string
	}{
		{
			name: "no comment",
			req: action.Request{
				SpecID:  SpecSilenceCreate,
				Targets: []action.Target{{Ref: alert.Ref, Labels: alert.Labels}},
			},
		},
		{
			name: "bad duration",
			req: action.Request{
				SpecID:  SpecSilenceCreate,
				Targets: []action.Target{{Ref: alert.Ref, Labels: alert.Labels}},
				Params:  map[string]string{ParamComment: "x", ParamDuration: "soon"},
			},
		},
		{
			name: "unknown spec",
			req: action.Request{
				SpecID:  "alertmanager.silence.nope",
				Targets: []action.Target{{Ref: alert.Ref, Labels: alert.Labels}},
				Params:  map[string]string{ParamComment: "x"},
			},
		},
		{
			name:   "no creator",
			server: "anon",
			req: action.Request{
				SpecID:  SpecSilenceCreate,
				Targets: []action.Target{{Ref: alert.Ref, Labels: alert.Labels}},
				Params:  map[string]string{ParamComment: "x"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pl := p
			if tc.server != "" {
				pl = NewAlertmanagerPlanner(AlertmanagerPlannerOptions{
					Servers: []AlertmanagerServer{{Name: "eu", Client: newTestAMClient(t, fs, "eu")}},
					Now:     func() time.Time { return amNow },
				})
			}
			_, err := pl.Plan(context.Background(), tc.req)
			if err == nil {
				t.Fatal("Plan returned no error")
			}
			if got, want := errs.KindOf(err), errs.Config; got != want {
				t.Errorf("kind = %s, want %s (%v)", got, want, err)
			}
		})
	}

	// An alert whose server is not configured is a mistake, not a no-op.
	_, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceCreate,
		Targets: []action.Target{{Ref: resource.Ref{Kind: KindAlert, Namespace: "xx", Name: "x"}, Labels: alert.Labels}},
		Params:  map[string]string{ParamComment: "x"},
	})
	if err == nil {
		t.Error("an unknown server returned no error")
	}
}

func TestAlertmanagerPlannerExpireSilence(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")
	p := newAMPlanner(t, fs, "eu")

	sil := amRows(t, b, KindSilence)[0]
	plan, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceExpire,
		Targets: []action.Target{{Ref: sil.Ref}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantDisplay := "DELETE " + fs.URL + pathAMSilence + "c3a4b5d6-1111-4000-8000-000000000001"
	if got := plan.Steps[0].Display; got != wantDisplay {
		t.Errorf("display = %q, want %q", got, wantDisplay)
	}
	res := plan.Steps[0].Run(context.Background())
	if res.Err != nil {
		t.Fatalf("step error: %v", res.Err)
	}
	if _, ok := fs.last(http.MethodDelete, pathAMSilence+"c3a4b5d6-1111-4000-8000-000000000001"); !ok {
		t.Errorf("no DELETE for the silence; requests = %+v", fs.requests())
	}
	if !strings.Contains(res.Output, "c3a4b5d6") {
		t.Errorf("output = %q, want the silence id", res.Output)
	}
}

func TestAlertmanagerPlannerExtendSilence(t *testing.T) {
	fs := newAMFixture(t)
	b := newTestAMBackend(t, fs, "eu")
	p := newAMPlanner(t, fs, "eu")

	sil := amRows(t, b, KindSilence)[0]
	plan, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceExtend,
		Targets: []action.Target{{Ref: sil.Ref}},
		Params:  map[string]string{ParamDuration: "2h"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.HasPrefix(plan.Steps[0].Display, "POST "+fs.URL+pathAMSilences) {
		t.Errorf("display = %q, want the POST call", plan.Steps[0].Display)
	}
	if res := plan.Steps[0].Run(context.Background()); res.Err != nil {
		t.Fatalf("step error: %v", res.Err)
	}
	body := postedSilence(t, fs)
	// The silence ended at 23:00; two more hours make it 01:00.
	if got, want := body.EndsAt, "2026-11-13T01:00:00Z"; got != want {
		t.Errorf("endsAt = %q, want %q", got, want)
	}
	if got, want := body.StartsAt, "2026-11-12T20:00:00Z"; got != want {
		t.Errorf("startsAt = %q, want the original start %q", got, want)
	}
	if got, want := body.Comment, "maintenance window on checkout"; got != want {
		t.Errorf("comment = %q, want the original comment %q", got, want)
	}
	if got, want := body.CreatedBy, "michael"; got != want {
		t.Errorf("createdBy = %q, want %q", got, want)
	}
	if got, want := len(body.Matchers), 2; got != want {
		t.Errorf("matchers = %d, want the original %d", got, want)
	}

	// An expired silence is extended from now, not from its old end.
	expired := amRows(t, b, KindSilence)[1]
	plan, err = p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceExtend,
		Targets: []action.Target{{Ref: expired.Ref}},
		Params:  map[string]string{ParamDuration: "1h", ParamComment: "again"},
	})
	if err != nil {
		t.Fatalf("Plan expired: %v", err)
	}
	if res := plan.Steps[0].Run(context.Background()); res.Err != nil {
		t.Fatalf("step error: %v", res.Err)
	}
	body = postedSilence(t, fs)
	if got, want := body.EndsAt, "2026-11-12T23:30:00Z"; got != want {
		t.Errorf("expired endsAt = %q, want now+1h = %q", got, want)
	}
	if got, want := body.Comment, "again"; got != want {
		t.Errorf("comment = %q, want %q", got, want)
	}
}

func TestAlertmanagerPlannerExtendNeedsSilence(t *testing.T) {
	fs := newAMFixture(t)
	p := newAMPlanner(t, fs, "eu")
	_, err := p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceExtend,
		Targets: []action.Target{{Ref: resource.Ref{Kind: KindSilence, Namespace: "eu", Name: "abc"}}},
	})
	if err == nil {
		t.Fatal("extend without the silence returned no error")
	}
	if got, want := errs.KindOf(err), errs.NotAvailable; got != want {
		t.Errorf("kind = %s, want %s (%v)", got, want, err)
	}
	// A target without an id is a caller mistake.
	_, err = p.Plan(context.Background(), action.Request{
		SpecID:  SpecSilenceExtend,
		Targets: []action.Target{{Ref: resource.Ref{Kind: KindSilence, Namespace: "eu"}}},
	})
	if err == nil {
		t.Fatal("extend without an id returned no error")
	}
	if got, want := errs.KindOf(err), errs.Config; got != want {
		t.Errorf("kind = %s, want %s (%v)", got, want, err)
	}
}

func TestSilenceDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 2 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"2h", 2 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"2w", 14 * 24 * time.Hour, true},
		{"0", 0, false},
		{"-1h", 0, false},
		{"soon", 0, false},
	}
	for _, tc := range cases {
		got, err := parseSilenceDuration(tc.in)
		if tc.ok && err != nil {
			t.Errorf("parseSilenceDuration(%q): %v", tc.in, err)
			continue
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("parseSilenceDuration(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("parseSilenceDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSilenceMatcherText(t *testing.T) {
	got := matcherText([]AMMatcher{
		{Name: "alertname", Value: "HighLatency", IsEqual: true},
		{Name: "service", Value: "check.*", IsRegex: true, IsEqual: true},
		{Name: "dc", Value: "ord", IsEqual: false},
		{Name: "pod", Value: "x.*", IsRegex: true, IsEqual: false},
	})
	want := `alertname="HighLatency", service=~"check.*", dc!="ord", pod!~"x.*"`
	if got != want {
		t.Errorf("matcherText = %q, want %q", got, want)
	}
}

func TestSilenceShortID(t *testing.T) {
	if got, want := shortSilenceID("c3a4b5d6-1111-4000-8000-000000000001"), "c3a4b5d6"; got != want {
		t.Errorf("shortSilenceID = %q, want %q", got, want)
	}
	if got, want := shortSilenceID("abc"), "abc"; got != want {
		t.Errorf("shortSilenceID(short) = %q, want %q", got, want)
	}
	if got := shortSilenceID(""); got != "" {
		t.Errorf("shortSilenceID(empty) = %q, want empty", got)
	}
}

// guard: the planner is what the bus registers.
var _ action.Planner = (*AlertmanagerPlanner)(nil)
var _ backend.Backend = (*AlertmanagerBackend)(nil)
