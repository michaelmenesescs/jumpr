// Package grafana is the Grafana HTTP API backend (SPEC §8.4): dashboard
// search, dashboard definitions with their panel grid and template variables,
// Grafana alerting rows, and the /api/health probe.
//
// It is read-only and talks to Grafana through an internal/transport/httpx
// client, so proxies, TLS, auth helpers and retries are the connection
// profile's business, not a backend's. Panel rendering and the full
// /api/ds/query data-frame decoding belong to task 120; this package resolves
// simple label_values template variables through that endpoint and nothing
// more.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/transport/httpx"
)

// API paths.
const (
	pathSearch    = "/api/search"
	pathDashboard = "/api/dashboards/uid/"
	pathDSQuery   = "/api/ds/query"
	pathAlerts    = "/api/prometheus/grafana/api/v1/alerts"
	pathHealth    = "/api/health"
)

// healthTimeout bounds one /api/health probe (BUILD-PLAN §8: health checks 5 s).
const healthTimeout = 5 * time.Second

// searchLimit caps the dashboards one search returns; the default of Grafana's
// API is a thousand, which no table needs.
const searchLimit = 500

// SearchHit is one result of /api/search. Only dashboards are asked for
// (type=dash-db), but a folder hit is decoded too so a weird answer cannot
// fail the list.
type SearchHit struct {
	UID    string
	Title  string
	Folder string
	Tags   []string
	Type   string
	URL    string
}

// GrafanaAlert is one alert of /api/prometheus/grafana/api/v1/alerts. It shares
// the Prometheus alert shape because Grafana serves it from its alerting
// engine's Prometheus-compatible API.
type GrafanaAlert struct {
	Labels      map[string]string
	Annotations map[string]string
	State       string
	ActiveAt    time.Time
	Value       string
}

// ServerHealth is one Grafana server's answer to /api/health.
type ServerHealth struct {
	Name   string
	Health bool
	Detail string
}

// DSQuery is one query of a /api/ds/query request: a panel target or a template
// variable's query.
type DSQuery struct {
	RefID         string     `json:"refId"`
	Datasource    Datasource `json:"datasource"`
	Expr          string     `json:"expr,omitempty"`
	Query         string     `json:"query,omitempty"`
	Instant       bool       `json:"instant,omitempty"`
	Range         bool       `json:"range,omitempty"`
	Format        string     `json:"format,omitempty"`
	IntervalMs    int64      `json:"intervalMs,omitempty"`
	MaxDataPoints int64      `json:"maxDataPoints,omitempty"`
}

// DSRequest is the body of POST /api/ds/query. From and To are epoch
// milliseconds as strings, the form Grafana accepts.
type DSRequest struct {
	Queries []DSQuery `json:"queries"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
}

// DSResponse is the answer to a /api/ds/query request: one result per refId.
type DSResponse struct {
	Results map[string]DSResult `json:"results"`
}

// DSResult is one query's data frames or its error.
type DSResult struct {
	Frames []DSFrame `json:"frames"`
	Error  string    `json:"error"`
	Status int       `json:"status"`
}

// DSFrame is one data frame: typed fields and column-major values.
type DSFrame struct {
	Schema DSFrameSchema `json:"schema"`
	Data   DSFrameData   `json:"data"`
}

// DSFrameSchema names a frame's fields.
type DSFrameSchema struct {
	Fields []DSField `json:"fields"`
	RefID  string    `json:"refId"`
}

// DSField is one column of a frame.
type DSField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// DSFrameData holds one array per field.
type DSFrameData struct {
	Values []json.RawMessage `json:"values"`
}

// Strings returns the first field's values as strings: what a label_values
// variable needs.
func (r DSResult) Strings() []string {
	for _, f := range r.Frames {
		if len(f.Data.Values) == 0 {
			continue
		}
		var col []any
		if err := json.Unmarshal(f.Data.Values[0], &col); err != nil {
			continue
		}
		out := make([]string, 0, len(col))
		for _, v := range col {
			if s := scalar(v); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ClientOptions configures a Client.
type ClientOptions struct {
	// URL is the server's base URL, e.g. https://grafana.example.internal.
	URL string
	// Client is the connection profile's HTTP client. Nil builds a plain one,
	// which is only useful in tests.
	Client *httpx.Client
	// Name is the configured server name, used in errors and warnings.
	Name string
	// Warn receives non-fatal notes (a decoder that skipped something). Nil
	// discards.
	Warn func(msg string)
	// Now is the clock. Nil uses the wall clock.
	Now func() time.Time
}

// Client is one Grafana server. It is safe for concurrent use: it holds only
// immutable state.
type Client struct {
	base string
	name string
	hc   *httpx.Client
	http *http.Client
	warn func(string)
	now  func() time.Time
}

// NewClient builds a client for o.URL. An empty or non-http(s) URL is an
// errs.Config error.
func NewClient(o ClientOptions) (*Client, error) {
	raw := strings.TrimSpace(o.URL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errs.New(errs.Config, "grafana.client",
			fmt.Sprintf("server %q: %q is not an http(s) URL", o.Name, o.URL))
	}
	name := strings.TrimSpace(o.Name)
	if name == "" {
		name = u.Host
	}
	c := &Client{
		base: strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"),
		name: name,
		hc:   o.Client,
		warn: o.Warn,
		now:  o.Now,
	}
	if c.hc == nil {
		c.http = &http.Client{Timeout: httpx.DefaultTimeout}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.name }

// BaseURL returns the server's base URL.
func (c *Client) BaseURL() string { return c.base }

// Absolute turns a Grafana URL path into an absolute URL on this server.
func (c *Client) Absolute(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return c.base
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	return c.base + "/" + strings.TrimLeft(p, "/")
}

// warnf forwards one non-fatal note to the owner.
func (c *Client) warnf(format string, args ...any) {
	if c.warn == nil {
		return
	}
	c.warn(fmt.Sprintf(format, args...))
}

// Search lists dashboards matching query (an empty query lists all of them).
func (c *Client) Search(ctx context.Context, query string) ([]SearchHit, error) {
	params := url.Values{
		"type":  {"dash-db"},
		"query": {strings.TrimSpace(query)},
		"limit": {strconv.Itoa(searchLimit)},
	}
	var raw []rawSearchHit
	if err := c.get(ctx, "search", pathSearch, params, &raw); err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(raw))
	for _, h := range raw {
		out = append(out, h.hit())
	}
	return out, nil
}

// Dashboard fetches one dashboard by uid, with its panel grid, template
// variables and time defaults.
func (c *Client) Dashboard(ctx context.Context, uid string) (*Dashboard, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, c.confError("dashboard", "a dashboard needs a uid")
	}
	var raw struct {
		Dashboard json.RawMessage `json:"dashboard"`
		Meta      struct {
			Slug        string `json:"slug"`
			URL         string `json:"url"`
			FolderTitle string `json:"folderTitle"`
		} `json:"meta"`
	}
	if err := c.get(ctx, "dashboard", pathDashboard+url.PathEscape(uid), nil, &raw); err != nil {
		return nil, err
	}
	if len(raw.Dashboard) == 0 {
		return nil, &errs.Error{Kind: errs.Parse, Op: "grafana.dashboard", Host: c.name,
			Msg: "the response carried no dashboard"}
	}
	var d Dashboard
	if err := json.Unmarshal(raw.Dashboard, &d); err != nil {
		return nil, &errs.Error{Kind: errs.Parse, Op: "grafana.dashboard", Host: c.name,
			Msg: "the dashboard did not parse", Err: err}
	}
	if d.UID == "" {
		d.UID = uid
	}
	if d.Slug == "" {
		d.Slug = raw.Meta.Slug
	}
	d.Folder = raw.Meta.FolderTitle
	p := raw.Meta.URL
	if p == "" {
		p = DashboardPath(d.UID, d.Slug)
	}
	d.URL = c.Absolute(p)
	return &d, nil
}

// QueryData runs a /api/ds/query request. Some Grafana versions answer 200 with
// one result carrying an error string; that becomes an errs.Remote error.
func (c *Client) QueryData(ctx context.Context, req DSRequest) (DSResponse, error) {
	if len(req.Queries) == 0 {
		return DSResponse{}, c.confError("ds.query", "a query request needs at least one query")
	}
	var resp DSResponse
	if err := c.post(ctx, "ds.query", pathDSQuery, req, &resp); err != nil {
		return DSResponse{}, err
	}
	for ref, res := range resp.Results {
		if strings.TrimSpace(res.Error) != "" {
			return DSResponse{}, c.remoteError("ds.query", ref+": "+res.Error)
		}
	}
	return resp, nil
}

// ResolveLabelValues resolves a template variable's query through
// /api/ds/query. expr is the variable's simple label_values(form, label) text.
func (c *Client) ResolveLabelValues(ctx context.Context, ds Datasource, expr string) ([]string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, c.confError("ds.query", "a variable query needs an expression")
	}
	resp, err := c.QueryData(ctx, DSRequest{Queries: []DSQuery{{
		RefID:      "A",
		Datasource: ds,
		Expr:       expr,
		Instant:    true,
		Format:     "table",
	}}})
	if err != nil {
		return nil, err
	}
	return resp.Results["A"].Strings(), nil
}

// Alerts returns Grafana's active alerts.
func (c *Client) Alerts(ctx context.Context) ([]GrafanaAlert, error) {
	var env struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Alerts []rawAlert `json:"alerts"`
		} `json:"data"`
	}
	if err := c.get(ctx, "alerts", pathAlerts, nil, &env); err != nil {
		return nil, err
	}
	if env.Status != "" && !strings.EqualFold(env.Status, "success") && env.Error != "" {
		return nil, c.remoteError("alerts", env.Error)
	}
	out := make([]GrafanaAlert, 0, len(env.Data.Alerts))
	for _, a := range env.Data.Alerts {
		out = append(out, a.alert())
	}
	return out, nil
}

// Healthy probes /api/health. Any answer is a Health, never an error.
func (c *Client) Healthy(ctx context.Context) ServerHealth {
	h := ServerHealth{Name: c.name}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+pathHealth, nil)
	if err != nil {
		h.Detail = string(errs.Config)
		return h
	}
	resp, err := c.do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		h.Detail = string(errs.KindOf(err))
		return h
	}
	if resp.StatusCode != http.StatusOK {
		h.Detail = fmt.Sprintf("status %d", resp.StatusCode)
		return h
	}
	var body struct {
		Database string `json:"database"`
		Version  string `json:"version"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
	if db := strings.TrimSpace(body.Database); db != "" && !strings.EqualFold(db, "ok") {
		h.Detail = "database " + db
		return h
	}
	h.Health = true
	h.Detail = "healthy"
	if v := strings.TrimSpace(body.Version); v != "" {
		h.Detail = v + " healthy"
	}
	return h
}

// rawSearchHit is one /api/search entry. folder may be a title string in old
// versions and an object in new ones.
type rawSearchHit struct {
	UID         string          `json:"uid"`
	Title       string          `json:"title"`
	Type        string          `json:"type"`
	URL         string          `json:"url"`
	Tags        []string        `json:"tags"`
	FolderTitle string          `json:"folderTitle"`
	Folder      json.RawMessage `json:"folder"`
}

// hit converts a decoded search row.
func (h rawSearchHit) hit() SearchHit {
	folder := strings.TrimSpace(h.FolderTitle)
	if folder == "" {
		folder = text(h.Folder)
		if folder == "" {
			var obj struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(h.Folder, &obj); err == nil {
				folder = obj.Title
			}
		}
	}
	return SearchHit{
		UID: h.UID, Title: h.Title, Folder: folder, Tags: h.Tags, Type: h.Type, URL: h.URL,
	}
}

// rawAlert is one alert of Grafana's Prometheus-compatible alerts API.
type rawAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    string            `json:"activeAt"`
	Value       string            `json:"value"`
}

// alert converts a decoded alert. An unparseable activeAt leaves the time zero;
// the row then sorts last instead of failing the list.
func (a rawAlert) alert() GrafanaAlert {
	out := GrafanaAlert{
		Labels:      a.Labels,
		Annotations: a.Annotations,
		State:       a.State,
		Value:       a.Value,
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, strings.TrimSpace(a.ActiveAt)); err == nil {
			out.ActiveAt = t
			break
		}
	}
	return out
}

// get performs one GET of an API path and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, op, path string, params url.Values, out any) error {
	u := c.base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctxOr(ctx), http.MethodGet, u, nil)
	if err != nil {
		return c.confError(op, "invalid URL")
	}
	return c.exchange(op, req, out)
}

// post performs one POST of an API path with in as its JSON body.
func (c *Client) post(ctx context.Context, op, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return c.confError(op, "cannot encode the request body")
	}
	req, err := http.NewRequestWithContext(ctxOr(ctx), http.MethodPost, c.base+path, strings.NewReader(string(body)))
	if err != nil {
		return c.confError(op, "invalid URL")
	}
	req.Header.Set("Content-Type", "application/json")
	return c.exchange(op, req, out)
}

// exchange sends req and decodes the JSON answer into out, mapping the status
// code onto an error kind.
func (c *Client) exchange(op string, req *http.Request, out any) error {
	resp, err := c.do(req)
	if resp == nil {
		if err == nil {
			err = c.confError(op, "no response")
		}
		return err
	}
	defer resp.Body.Close()

	payload, rerr := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxBodyBytes))
	if rerr != nil {
		if err != nil {
			return err
		}
		return c.remoteError(op, fmt.Sprintf("cannot read the response: %v", rerr))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if err != nil {
			return err
		}
		return c.statusError(op, resp.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if jerr := json.Unmarshal(payload, out); jerr != nil {
		return &errs.Error{Kind: errs.Parse, Op: "grafana." + op, Host: c.name,
			Msg: "the response did not parse", Err: jerr}
	}
	return nil
}

// statusError classifies a non-2xx answer.
func (c *Client) statusError(op string, code int, payload []byte) error {
	kind := errs.Remote
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		kind = errs.Auth
	case code == http.StatusNotFound:
		kind = errs.NotAvailable
	}
	msg := fmt.Sprintf("status %d", code)
	if detail := errorMessage(payload); detail != "" {
		msg += ": " + detail
	}
	e := &errs.Error{Kind: kind, Op: "grafana." + op, Host: c.name, Msg: msg}
	return e
}

// errorMessage pulls Grafana's error text out of a body, if it has one.
func errorMessage(payload []byte) string {
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	if body.Message != "" {
		return truncate(body.Message, 200)
	}
	return truncate(body.Error, 200)
}

// truncate shortens s to at most n bytes, on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// do sends the request through the httpx client when there is one.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.hc != nil {
		return c.hc.Do(req.Context(), req)
	}
	return c.http.Do(req)
}

// confError is a caller mistake: an empty uid, a bad query.
func (c *Client) confError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Config, Op: "grafana." + op, Host: c.name, Msg: msg}
}

// remoteError is Grafana refusing or failing the request.
func (c *Client) remoteError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Remote, Op: "grafana." + op, Host: c.name, Msg: msg}
}

// ctxOr returns ctx, or a background context when it is nil.
func ctxOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// logOrDiscard keeps the backend's logger non-nil without importing slog in
// every method: callers pass one, tests may not.
func logOrDiscard(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
