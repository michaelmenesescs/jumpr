// Alertmanager API v2 (SPEC §8.3): the active alerts, firing, silenced or
// inhibited, and the silences, read-only. Silences are written through the
// action bus; the planner lives in am_actions.go.
//
// The backend is named "am" so its health dot and its refs are distinguishable
// from the Prometheus backend's "prom". Alerts and silences are global kinds:
// one row per alert or silence of every configured Alertmanager.

package prom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport/httpx"
)

// alertmanagerName is the name every Alertmanager ref and health dot uses.
const alertmanagerName = "am"

// Alertmanager API v2 paths and the health probe.
const (
	pathAMAlerts   = "/api/v2/alerts"
	pathAMSilences = "/api/v2/silences"
	pathAMSilence  = "/api/v2/silence/"
	pathAMHealthy  = "/-/healthy"
)

// Silence states as derived from the silence's own window.
const (
	SilenceActive  = "active"
	SilencePending = "pending"
	SilenceExpired = "expired"
)

// Alert states as the alert row shows them.
const (
	AlertFiring    = "firing"
	AlertSilenced  = "silenced"
	AlertInhibited = "inhibited"
)

// amStepTimeout bounds one silence write.
const amStepTimeout = 30 * time.Second

// AMMatcher is one matcher of a silence. IsEqual distinguishes `=` from `!=`,
// IsRegex `=~` from `!~`.
type AMMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// AMAlert is one alert of /api/v2/alerts.
type AMAlert struct {
	Labels       map[string]string
	Annotations  map[string]string
	StartsAt     time.Time
	EndsAt       time.Time
	GeneratorURL string
	Fingerprint  string
	// State is the status Alertmanager reports: "active" or "suppressed".
	State       string
	SilencedBy  []string
	InhibitedBy []string
}

// Silenced reports whether a silence covers the alert.
func (a AMAlert) Silenced() bool { return len(a.SilencedBy) > 0 }

// Inhibited reports whether another alert inhibits it.
func (a AMAlert) Inhibited() bool { return len(a.InhibitedBy) > 0 }

// RowState is the state the alert row shows: silenced wins over inhibited,
// because that is what explains its absence from notifications.
func (a AMAlert) RowState() string {
	switch {
	case a.Silenced():
		return AlertSilenced
	case a.Inhibited():
		return AlertInhibited
	}
	return AlertFiring
}

// AMSilence is one silence of /api/v2/silences.
type AMSilence struct {
	ID        string
	Matchers  []AMMatcher
	StartsAt  time.Time
	EndsAt    time.Time
	UpdatedAt time.Time
	CreatedBy string
	Comment   string
	// Status is what Alertmanager reported; State derives the same value from
	// the window and the clock.
	Status string
}

// State derives a silence's state from its window: pending before it starts,
// active inside it, expired after it ends. An unknown window is reported as
// Alertmanager reported it.
func (s AMSilence) State(now time.Time) string {
	if s.StartsAt.IsZero() || s.EndsAt.IsZero() {
		return s.Status
	}
	switch {
	case now.Before(s.StartsAt):
		return SilencePending
	case now.After(s.EndsAt):
		return SilenceExpired
	}
	return SilenceActive
}

// SilenceRequest is the body of POST /api/v2/silences. Times are RFC 3339 in
// UTC; the comment is the user's free text and is encoded, never concatenated.
type SilenceRequest struct {
	Matchers  []AMMatcher `json:"matchers"`
	StartsAt  string      `json:"startsAt"`
	EndsAt    string      `json:"endsAt"`
	CreatedBy string      `json:"createdBy"`
	Comment   string      `json:"comment"`
}

// AlertmanagerClientOptions configures an AlertmanagerClient.
type AlertmanagerClientOptions struct {
	// URL is the server's base URL, e.g. https://alertmanager.example.internal.
	URL string
	// Client is the connection profile's HTTP client. Nil builds a plain one,
	// which is only useful in tests.
	Client *httpx.Client
	// Name is the configured server name, used in errors and warnings.
	Name string
}

// AlertmanagerClient is one Alertmanager server. It is safe for concurrent use:
// it holds only immutable state.
type AlertmanagerClient struct {
	base string
	name string
	hc   *httpx.Client
	http *http.Client
}

// NewAlertmanagerClient builds a client for o.URL. An empty or non-http(s) URL
// is an errs.Config error.
func NewAlertmanagerClient(o AlertmanagerClientOptions) (*AlertmanagerClient, error) {
	raw := strings.TrimSpace(o.URL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errs.New(errs.Config, "prom.am.client",
			fmt.Sprintf("server %q: %q is not an http(s) URL", o.Name, o.URL))
	}
	name := strings.TrimSpace(o.Name)
	if name == "" {
		name = u.Host
	}
	c := &AlertmanagerClient{
		base: strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"),
		name: name,
		hc:   o.Client,
	}
	if c.hc == nil {
		c.http = &http.Client{Timeout: httpx.DefaultTimeout}
	}
	return c, nil
}

// Name returns the configured server name.
func (c *AlertmanagerClient) Name() string { return c.name }

// BaseURL returns the server's base URL.
func (c *AlertmanagerClient) BaseURL() string { return c.base }

// Alerts returns the alerts Alertmanager currently holds, including the
// silenced and inhibited ones.
func (c *AlertmanagerClient) Alerts(ctx context.Context) ([]AMAlert, error) {
	body, err := c.request(ctx, "alerts", http.MethodGet, pathAMAlerts, nil)
	if err != nil {
		return nil, err
	}
	var raw []rawAMAlert
	if err := decodeAM(body, &raw); err != nil {
		return nil, c.parseError("alerts", err)
	}
	out := make([]AMAlert, 0, len(raw))
	for _, a := range raw {
		out = append(out, a.alert())
	}
	return out, nil
}

// Silences returns every silence Alertmanager holds, expired ones included.
func (c *AlertmanagerClient) Silences(ctx context.Context) ([]AMSilence, error) {
	body, err := c.request(ctx, "silences", http.MethodGet, pathAMSilences, nil)
	if err != nil {
		return nil, err
	}
	var raw []rawAMSilence
	if err := decodeAM(body, &raw); err != nil {
		return nil, c.parseError("silences", err)
	}
	out := make([]AMSilence, 0, len(raw))
	for _, s := range raw {
		out = append(out, s.silence())
	}
	return out, nil
}

// CreateSilence posts a silence and returns its ID.
func (c *AlertmanagerClient) CreateSilence(ctx context.Context, req SilenceRequest) (string, error) {
	body, err := c.request(ctx, "silence.create", http.MethodPost, pathAMSilences, req)
	if err != nil {
		return "", err
	}
	var created struct {
		SilenceID string `json:"silenceID"`
	}
	if err := decodeAM(body, &created); err != nil {
		return "", c.parseError("silence.create", err)
	}
	return created.SilenceID, nil
}

// ExpireSilence deletes a silence by its full ID. Alertmanager answers 200 with
// an empty body.
func (c *AlertmanagerClient) ExpireSilence(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return c.confError("silence.expire", "a silence needs an id")
	}
	_, err := c.request(ctx, "silence.expire", http.MethodDelete, pathAMSilence+url.PathEscape(id), nil)
	return err
}

// Healthy probes /-/healthy. Any answer is a ServerHealth, never an error.
func (c *AlertmanagerClient) Healthy(ctx context.Context) ServerHealth {
	h := ServerHealth{Name: c.name}
	ctx, cancel := context.WithTimeout(ctxOr(ctx), healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+pathAMHealthy, nil)
	if err != nil {
		h.Detail = string(errs.Config)
		return h
	}
	resp, err := c.send(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
		_ = resp.Body.Close()
	}
	if err != nil {
		h.Detail = string(errs.KindOf(err))
		return h
	}
	if resp.StatusCode != http.StatusOK {
		h.Detail = fmt.Sprintf("status %d", resp.StatusCode)
		return h
	}
	h.Health = true
	h.Detail = "healthy"
	return h
}

// request performs one API call and returns the response body. A status of 400
// or above becomes an errs.Remote error carrying Alertmanager's message.
func (c *AlertmanagerClient) request(ctx context.Context, op, method, path string, in any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, &errs.Error{Kind: errs.Internal, Op: "prom.am." + op, Host: c.name,
				Msg: "cannot encode the request body", Err: err}
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctxOr(ctx), method, c.base+path, body)
	if err != nil {
		return nil, c.confError(op, "invalid URL")
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.send(req)
	if resp == nil {
		if err == nil {
			err = c.confError(op, "no response")
		}
		return nil, err
	}
	defer resp.Body.Close()

	data, rerr := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxBodyBytes))
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, c.statusError(op, resp, data)
	}
	if err != nil {
		return nil, err
	}
	if rerr != nil {
		return nil, c.remoteError(op, "cannot read the response: "+rerr.Error())
	}
	return data, nil
}

// send sends the request through the connection profile's client when there is
// one.
func (c *AlertmanagerClient) send(req *http.Request) (*http.Response, error) {
	if c.hc != nil {
		return c.hc.Do(req.Context(), req)
	}
	return c.http.Do(req)
}

// statusError turns a failing status into an *errs.Error, keeping the message
// Alertmanager sent when the body carries one.
func (c *AlertmanagerClient) statusError(op string, resp *http.Response, body []byte) error {
	msg := ""
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		msg = strings.TrimSpace(payload.Error)
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	kind := errs.Remote
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = errs.Auth
	case http.StatusNotFound:
		kind = errs.NotAvailable
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		kind = errs.Timeout
	}
	return &errs.Error{Kind: kind, Op: "prom.am." + op, Host: c.name,
		Msg: fmt.Sprintf("status %d: %s", resp.StatusCode, msg)}
}

// confError is a caller mistake: an empty ID, a bad URL.
func (c *AlertmanagerClient) confError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Config, Op: "prom.am." + op, Host: c.name, Msg: msg}
}

// remoteError is Alertmanager failing the call.
func (c *AlertmanagerClient) remoteError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Remote, Op: "prom.am." + op, Host: c.name, Msg: msg}
}

// parseError is a response body jumper could not read.
func (c *AlertmanagerClient) parseError(op string, err error) *errs.Error {
	return &errs.Error{Kind: errs.Parse, Op: "prom.am." + op, Host: c.name,
		Msg: "the response did not parse", Err: err}
}

// decodeAM decodes a JSON body, mapping a failure onto the zero value.
func decodeAM[T any](body []byte, out *T) error {
	if len(body) == 0 {
		return errs.New(errs.Parse, "prom.am", "empty response body")
	}
	return json.Unmarshal(body, out)
}

// rawAMAlert is one alert of the API.
type rawAMAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	Status       struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
}

// rawAMSilence is one silence of the API.
type rawAMSilence struct {
	ID        string      `json:"id"`
	Matchers  []AMMatcher `json:"matchers"`
	StartsAt  time.Time   `json:"startsAt"`
	EndsAt    time.Time   `json:"endsAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
	CreatedBy string      `json:"createdBy"`
	Comment   string      `json:"comment"`
	Status    struct {
		State string `json:"state"`
	} `json:"status"`
}

// alert converts a decoded alert.
func (a rawAMAlert) alert() AMAlert {
	return AMAlert{
		Labels:       a.Labels,
		Annotations:  a.Annotations,
		StartsAt:     a.StartsAt,
		EndsAt:       a.EndsAt,
		GeneratorURL: a.GeneratorURL,
		Fingerprint:  a.Fingerprint,
		State:        a.Status.State,
		SilencedBy:   a.Status.SilencedBy,
		InhibitedBy:  a.Status.InhibitedBy,
	}
}

// silence converts a decoded silence.
func (s rawAMSilence) silence() AMSilence {
	return AMSilence{
		ID:        s.ID,
		Matchers:  s.Matchers,
		StartsAt:  s.StartsAt,
		EndsAt:    s.EndsAt,
		UpdatedAt: s.UpdatedAt,
		CreatedBy: s.CreatedBy,
		Comment:   s.Comment,
		Status:    s.Status.State,
	}
}

// AlertmanagerServer is one configured Alertmanager.
type AlertmanagerServer struct {
	// Name is the config entry's name. It scopes the rows the server returns
	// and labels its health.
	Name string
	// Client is the typed client for it. Nil makes the server unconfigured.
	Client *AlertmanagerClient
}

// AlertmanagerOptions configures an AlertmanagerBackend. An empty Servers list
// is a configured-but-unusable backend: every List fails with errs.Config and
// Health reports HealthUnconfigured.
type AlertmanagerOptions struct {
	Servers []AlertmanagerServer
	Now     func() time.Time
	Log     *slog.Logger
}

// AlertmanagerBackend implements backend.Backend for Alertmanager. Both kinds
// are global, so there is no per-host batching: one Result per configured
// server.
type AlertmanagerBackend struct {
	servers []AlertmanagerServer
	now     func() time.Time
	log     *slog.Logger

	mu     sync.Mutex
	health map[string]healthEntry
}

var _ backend.Backend = (*AlertmanagerBackend)(nil)

// NewAlertmanager builds an Alertmanager backend over the configured servers.
func NewAlertmanager(o AlertmanagerOptions) *AlertmanagerBackend {
	b := &AlertmanagerBackend{
		servers: append([]AlertmanagerServer{}, o.Servers...),
		now:     o.Now,
		log:     o.Log,
		health:  map[string]healthEntry{},
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.log == nil {
		b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return b
}

// Name returns "am": the health dot and the refs use it.
func (b *AlertmanagerBackend) Name() string { return alertmanagerName }

// Kinds returns the two Alertmanager kinds.
func (b *AlertmanagerBackend) Kinds() []resource.KindSpec { return amKindSpecs() }

// Servers returns the configured server names, sorted.
func (b *AlertmanagerBackend) Servers() []string {
	out := make([]string, 0, len(b.servers))
	for _, s := range b.servers {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// List streams one Result per configured server, in configured order.
func (b *AlertmanagerBackend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	kind, ok := normalizeAMKind(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "prom.am.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	if len(b.servers) == 0 {
		return nil, errs.New(errs.Config, "prom.am.List", "alertmanager backend not configured: no servers")
	}
	servers := b.selectServers(q.Args[ArgServer])
	if len(servers) == 0 {
		return nil, errs.New(errs.Config, "prom.am.List",
			fmt.Sprintf("unknown alertmanager server %q", q.Args[ArgServer]))
	}

	out := make(chan backend.Result, 1)
	go func() {
		defer close(out)
		for i, s := range servers {
			items, err := b.rows(ctx, s, kind)
			res := backend.Result{Items: items, Done: i + 1, Total: len(servers)}
			if err != nil {
				res.Items, res.Err = nil, err
			}
			select {
			case <-ctx.Done():
				return
			case out <- res:
			}
		}
	}()
	return out, nil
}

// selectServers returns the servers a query may use: all of them, or the one
// named in name.
func (b *AlertmanagerBackend) selectServers(name string) []AlertmanagerServer {
	name = strings.TrimSpace(name)
	if name == "" {
		return b.servers
	}
	for _, s := range b.servers {
		if s.Name == name {
			return []AlertmanagerServer{s}
		}
	}
	return nil
}

// rows fetches one server's rows for one kind.
func (b *AlertmanagerBackend) rows(ctx context.Context, s AlertmanagerServer, kind resource.KindName) ([]resource.Resource, error) {
	if s.Client == nil {
		return nil, errs.New(errs.Config, "prom.am.List", fmt.Sprintf("server %q has no client", s.Name))
	}
	now := b.now()
	switch kind {
	case KindAlert:
		alerts, err := s.Client.Alerts(ctx)
		if err != nil {
			return nil, err
		}
		return alertRows(s.Name, alerts, now), nil
	case KindSilence:
		silences, err := s.Client.Silences(ctx)
		if err != nil {
			return nil, err
		}
		return silenceRows(s.Name, silences, now), nil
	}
	return nil, errs.New(errs.Config, "prom.am.List", fmt.Sprintf("unknown kind %q", kind))
}

// Get returns the row a ref names.
func (b *AlertmanagerBackend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	kind, ok := normalizeAMKind(ref.Kind)
	if !ok {
		return resource.Resource{}, errs.New(errs.Config, "prom.am.Get", fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	if strings.TrimSpace(ref.Name) == "" {
		return resource.Resource{}, errs.New(errs.Config, "prom.am.Get", "ref needs a name")
	}
	ch, err := b.List(ctx, resource.Query{Kind: kind, Args: map[string]string{ArgServer: ref.Namespace}})
	if err != nil {
		return resource.Resource{}, err
	}
	var batchErr error
	for res := range ch {
		if res.Err != nil {
			batchErr = res.Err
			continue
		}
		for _, row := range res.Items {
			if row.Ref.Name == ref.Name {
				return row, nil
			}
		}
	}
	if batchErr != nil {
		return resource.Resource{}, batchErr
	}
	return resource.Resource{}, errs.New(errs.NotAvailable, "prom.am.Get", "no resource "+ref.String())
}

// Links returns no pivots: an alert's labels are turned into hosts by the
// investigation (task 104), not by a ref.
func (b *AlertmanagerBackend) Links(resource.Resource) []resource.Link { return nil }

// Health folds every configured server's probe into one answer.
func (b *AlertmanagerBackend) Health(ctx context.Context) backend.Health {
	if len(b.servers) == 0 {
		return backend.Health{State: backend.HealthUnconfigured, Detail: "alertmanager not configured", Checked: b.now()}
	}
	statuses := b.Status(ctx)
	h := backend.Health{Checked: b.now()}
	up := 0
	parts := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if st.Health.State == backend.HealthOK {
			up++
		} else {
			parts = append(parts, st.Name+": "+st.Health.Detail)
		}
	}
	switch {
	case up == len(statuses):
		h.State = backend.HealthOK
		h.Detail = fmt.Sprintf("%d/%d servers healthy", up, len(statuses))
	case up == 0:
		h.State = backend.HealthDown
	default:
		h.State = backend.HealthDegraded
	}
	if len(parts) > 0 {
		h.Detail = strings.Join(parts, "; ")
	}
	return h
}

// Status returns one health per configured server, named by config name.
func (b *AlertmanagerBackend) Status(ctx context.Context) []ServerStatus {
	out := make([]ServerStatus, 0, len(b.servers))
	for _, s := range b.servers {
		out = append(out, ServerStatus{Name: s.Name, Health: b.serverHealth(ctx, s)})
	}
	return out
}

// serverHealth probes one server, reusing an answer younger than healthTTL.
func (b *AlertmanagerBackend) serverHealth(ctx context.Context, s AlertmanagerServer) backend.Health {
	now := b.now()
	b.mu.Lock()
	if e, ok := b.health[s.Name]; ok && now.Sub(e.at) >= 0 && now.Sub(e.at) < healthTTL {
		b.mu.Unlock()
		return e.h
	}
	b.mu.Unlock()

	h := backend.Health{Checked: now}
	if s.Client == nil {
		h.State, h.Detail = backend.HealthUnconfigured, "no client"
		return h
	}
	probe := s.Client.Healthy(ctx)
	h.Checked = b.now()
	h.Detail = s.Name + ": " + probe.Detail
	if probe.Health {
		h.State = backend.HealthOK
	} else {
		h.State = backend.HealthDown
	}

	b.mu.Lock()
	b.health[s.Name] = healthEntry{h: h, at: h.Checked}
	b.mu.Unlock()
	return h
}

// alertRows maps alerts onto rows of the alert kind, firing first, then by
// severity (critical, warning, other), then by start.
func alertRows(server string, alerts []AMAlert, now time.Time) []resource.Resource {
	out := make([]resource.Resource, 0, len(alerts))
	for _, a := range alerts {
		sev := strings.TrimSpace(a.Labels["severity"])
		if sev == "" {
			sev = "none"
		}
		state := a.RowState()
		out = append(out, resource.Resource{
			Ref: resource.Ref{
				Backend:   alertmanagerName,
				Kind:      KindAlert,
				Namespace: server,
				Name:      alertRefName(a),
			},
			Fields: map[string]any{
				"sev":    sev,
				"alert":  a.Labels["alertname"],
				"labels": compactLabels(a.Labels, "alertname", "severity"),
				"source": "alertmanager:" + server,
				"start":  a.StartsAt,
				"state":  state,
			},
			Labels:    a.Labels,
			Status:    alertStatus(sev, state),
			Raw:       a,
			FetchedAt: now,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return alertLess(out[i], out[j]) })
	return out
}

// alertLess is the alert order: firing first, then severity, then start.
func alertLess(a, b resource.Resource) bool {
	if ra, rb := alertStateRank(a.Str("state")), alertStateRank(b.Str("state")); ra != rb {
		return ra < rb
	}
	if ra, rb := severityRank(a.Str("sev")), severityRank(b.Str("sev")); ra != rb {
		return ra < rb
	}
	at, _ := a.Fields["start"].(time.Time)
	bt, _ := b.Fields["start"].(time.Time)
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.Ref.Name < b.Ref.Name
}

// alertStateRank orders the states: firing, then the suppressed ones.
func alertStateRank(state string) int {
	if state == AlertFiring {
		return 0
	}
	return 1
}

// severityRank orders severities: critical, warning, everything else.
func severityRank(sev string) int {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical":
		return 0
	case "warning":
		return 1
	}
	return 2
}

// alertStatus colours an alert row.
func alertStatus(sev, state string) resource.Status {
	if state != AlertFiring {
		return resource.StatusNA
	}
	if strings.EqualFold(sev, "critical") {
		return resource.StatusCrit
	}
	return resource.StatusWarn
}

// alertRefName is the unique part of an alert's ref: its fingerprint, or the
// alertname plus its labels when Alertmanager sent no fingerprint.
func alertRefName(a AMAlert) string {
	if fp := strings.TrimSpace(a.Fingerprint); fp != "" {
		return fp
	}
	name := a.Labels["alertname"]
	if name == "" {
		return compactLabels(a.Labels, "severity")
	}
	return name + "/" + compactLabels(a.Labels, "alertname", "severity")
}

// silenceRows maps silences onto rows of the silence kind, the ones still in
// effect first.
func silenceRows(server string, silences []AMSilence, now time.Time) []resource.Resource {
	out := make([]resource.Resource, 0, len(silences))
	for _, s := range silences {
		out = append(out, resource.Resource{
			Ref: resource.Ref{
				Backend:   alertmanagerName,
				Kind:      KindSilence,
				Namespace: server,
				Name:      s.ID,
			},
			Fields: map[string]any{
				"id":       shortSilenceID(s.ID),
				"matchers": matcherText(s.Matchers),
				"creator":  s.CreatedBy,
				"comment":  s.Comment,
				"starts":   s.StartsAt,
				"ends":     s.EndsAt,
				"state":    s.State(now),
			},
			Labels:    matcherLabels(s.Matchers),
			Status:    silenceStatus(s.State(now)),
			Raw:       s,
			FetchedAt: now,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return silenceLess(out[i], out[j]) })
	return out
}

// silenceLess puts the silences still in effect first, then the latest start.
func silenceLess(a, b resource.Resource) bool {
	ra, rb := silenceStateRank(a.Str("state")), silenceStateRank(b.Str("state"))
	if ra != rb {
		return ra < rb
	}
	as, _ := a.Fields["starts"].(time.Time)
	bs, _ := b.Fields["starts"].(time.Time)
	if !as.Equal(bs) {
		return as.After(bs)
	}
	return a.Ref.Name < b.Ref.Name
}

// silenceStateRank orders active before pending before expired.
func silenceStateRank(state string) int {
	switch state {
	case SilenceActive:
		return 0
	case SilencePending:
		return 1
	}
	return 2
}

// silenceStatus colours a silence row: an expired silence is information, not
// a problem.
func silenceStatus(state string) resource.Status {
	switch state {
	case SilenceActive:
		return resource.StatusOK
	case SilencePending:
		return resource.StatusWarn
	}
	return resource.StatusNA
}

// matcherText renders matchers as Prometheus selectors, in the order
// Alertmanager sent them.
func matcherText(matchers []AMMatcher) string {
	parts := make([]string, 0, len(matchers))
	for _, m := range matchers {
		op := "="
		switch {
		case m.IsRegex && m.IsEqual:
			op = "=~"
		case m.IsRegex:
			op = "!~"
		case !m.IsEqual:
			op = "!="
		}
		parts = append(parts, m.Name+op+quoteMatcher(m.Value))
	}
	return strings.Join(parts, ", ")
}

// quoteMatcher quotes a matcher value the way a selector does.
func quoteMatcher(v string) string {
	if !strings.ContainsAny(v, `"\,\`) {
		return `"` + v + `"`
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// matcherLabels is a matcher set as a label map, keeping only the `=` matchers:
// they are the ones that name a resource.
func matcherLabels(matchers []AMMatcher) map[string]string {
	out := make(map[string]string, len(matchers))
	for _, m := range matchers {
		if m.IsEqual && !m.IsRegex {
			out[m.Name] = m.Value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// shortSilenceID is the short form a row shows: the first UUID group.
func shortSilenceID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// compactLabels renders a label set as `k="v", k2="v2"`, sorted and without the
// dropped keys.
func compactLabels(labels map[string]string, drop ...string) string {
	skip := make(map[string]bool, len(drop))
	for _, k := range drop {
		skip[k] = true
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+quoteMatcher(labels[k]))
	}
	return strings.Join(parts, ", ")
}

// matchersFromAlert builds the matchers a silence on an alert uses: every label
// except severity, which describes presentation rather than the alert's
// identity. Sorted, so the plan and the record are stable.
func matchersFromAlert(labels map[string]string) []AMMatcher {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "severity" || strings.TrimSpace(labels[k]) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]AMMatcher, 0, len(keys))
	for _, k := range keys {
		out = append(out, AMMatcher{Name: k, Value: labels[k], IsEqual: true})
	}
	return out
}
