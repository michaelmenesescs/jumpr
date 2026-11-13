package elastic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/transport/httpx"
)

// healthTimeout bounds one `GET /` probe (BUILD-PLAN §8: health checks 5 s).
const healthTimeout = 5 * time.Second

// fieldCapsLimit caps the fields one `_field_caps` returns; no completion list
// needs more and a cluster with thousands of mappings stays readable.
const fieldCapsLimit = 2000

// catFields are the `_cat/indices` columns the source browser shows.
const catFields = "index,health,status,docs.count,store.size"

// allowedEndings are the only API endpoints jumper may call (SPEC §8.5). A path
// is read-only when it is `/` (the version probe) or ends with one of these,
// optionally after an index pattern.
var allowedEndings = []string{"/_search", "/_field_caps", "/_cat/indices", "/_pit", "/_resolve/index"}

// indexPatternRE matches an index pattern jumper is willing to put in a URL
// path: index names, wildcards, comma lists and cross-cluster `remote:index`
// patterns. Anything else (a slash, a space, a query string) is refused, so a
// pattern can never reach a different endpoint.
var indexPatternRE = regexp.MustCompile(`^[A-Za-z0-9_.*+,:<>\-]+$`)

// Hit is one `_search` hit.
type Hit struct {
	Index  string
	ID     string
	Source map[string]any
	Sort   []any
}

// Page is one page of `_search` results.
type Page struct {
	Hits []Hit
	// FailedShards counts the shards that did not answer. Partial results are
	// still returned; a page with no hits and failed shards is an error.
	FailedShards int
}

// Field is one searchable field of an index pattern.
type Field struct {
	Name string
	Type string
}

// IndexInfo is one index, alias or data stream `_resolve/index` returned.
type IndexInfo struct {
	Name       string
	Kind       string // "index", "alias" or "data_stream"
	Aliases    []string
	DataStream string
}

// CatIndex is one row of `_cat/indices`.
type CatIndex struct {
	Index     string
	Health    string
	Status    string
	Docs      int64
	StoreSize string
}

// ServerHealth is one cluster's answer to `GET /`.
type ServerHealth struct {
	Name    string
	Version string
	Cluster string
	Health  bool
	Detail  string
}

// ClientOptions configures a Client.
type ClientOptions struct {
	// URL is the cluster's base URL, e.g. https://elastic.example.internal:9200.
	URL string
	// Client is the connection profile's HTTP client. Nil builds a plain one,
	// which is only useful in tests.
	Client *httpx.Client
	// Name is the configured cluster name, used in errors and warnings.
	Name string
	// Warn receives non-fatal notes. Nil discards.
	Warn func(msg string)
	// Now is the clock. Nil uses the wall clock.
	Now func() time.Time
}

// Client is one Elasticsearch cluster. It is safe for concurrent use: it holds
// only immutable state.
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
		return nil, errs.New(errs.Config, "elastic.client",
			fmt.Sprintf("cluster %q: %q is not an http(s) URL", o.Name, o.URL))
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

// Name returns the configured cluster name.
func (c *Client) Name() string { return c.name }

// BaseURL returns the cluster's base URL.
func (c *Client) BaseURL() string { return c.base }

// ReadOnlyPath reports whether method and path are a request the read-only
// guarantee allows (SPEC §8.5): GET or POST to `_search`, `_field_caps`,
// `_resolve`, `_cat/indices` or `_pit`, plus `GET /` for the version probe.
// Everything else — `_bulk`, `_doc`, `_delete_by_query`, DELETE of a point in
// time — is refused before a socket is opened.
func ReadOnlyPath(method, p string) bool {
	switch method {
	case http.MethodGet, http.MethodPost:
	default:
		return false
	}
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") {
		return false
	}
	if strings.Contains(p, "..") || strings.ContainsAny(p, " \t\r\n?#") {
		return false
	}
	if p == "/" {
		return method == http.MethodGet
	}
	for _, end := range allowedEndings {
		if p == end || strings.HasSuffix(p, end) {
			return true
		}
		if strings.HasPrefix(p, end+"/") {
			return true
		}
	}
	return false
}

// Search runs one page of one log source. query_string text reaches the body
// only through encoding/json.
func (c *Client) Search(ctx context.Context, o SearchOptions) (Page, error) {
	src := o.Source.Normalized()
	if src.Index == "" {
		return Page{}, c.confError("search", "the log source has no index pattern")
	}
	body := BuildSearch(o)
	var resp searchResponse
	if err := c.exchange(ctx, "search", http.MethodPost, "/"+src.Index+"/_search", nil, body, &resp); err != nil {
		return Page{}, err
	}
	page := Page{FailedShards: resp.Shards.Failed}
	for _, h := range resp.Hits.Hits {
		page.Hits = append(page.Hits, Hit{Index: h.Index, ID: h.ID, Source: h.Source, Sort: h.Sort})
	}
	if len(page.Hits) == 0 && page.FailedShards > 0 {
		return Page{}, c.remoteError("search",
			fmt.Sprintf("%d shards failed and the page is empty", page.FailedShards))
	}
	if page.FailedShards > 0 {
		c.warnf("elastic %s: %d shards failed; the page is partial", c.name, page.FailedShards)
	}
	return page, nil
}

// FieldCaps lists the searchable fields of an index pattern, sorted by name.
// fields is the comma-separated pattern to ask about; empty asks for all.
func (c *Client) FieldCaps(ctx context.Context, index, fields string) ([]Field, error) {
	if err := checkIndex(index); err != nil {
		return nil, c.confError("field_caps", err.Error())
	}
	if strings.TrimSpace(fields) == "" {
		fields = "*"
	}
	params := url.Values{"fields": {fields}, "ignore_unavailable": {"true"}}
	var resp struct {
		Fields map[string]map[string]struct {
			Type string `json:"type"`
		} `json:"fields"`
	}
	if err := c.exchange(ctx, "field_caps", http.MethodGet, "/"+index+"/_field_caps", params, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Field, 0, len(resp.Fields))
	for name, types := range resp.Fields {
		f := Field{Name: name}
		for enc, info := range types {
			f.Type = info.Type
			if f.Type == "" {
				f.Type = enc
			}
			break
		}
		out = append(out, f)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	if len(out) > fieldCapsLimit {
		out = out[:fieldCapsLimit]
	}
	return out, nil
}

// Resolve lists the indices, aliases and data streams an index pattern matches
// (`_resolve/index`).
func (c *Client) Resolve(ctx context.Context, index string) ([]IndexInfo, error) {
	if err := checkIndex(index); err != nil {
		return nil, c.confError("resolve", err.Error())
	}
	var resp struct {
		Indices []struct {
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		} `json:"indices"`
		Aliases []struct {
			Name    string   `json:"name"`
			Indices []string `json:"indices"`
		} `json:"aliases"`
		DataStreams []struct {
			Name string `json:"name"`
		} `json:"data_streams"`
	}
	if err := c.exchange(ctx, "resolve", http.MethodGet, "/_resolve/index/"+index, nil, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]IndexInfo, 0, len(resp.Indices)+len(resp.Aliases)+len(resp.DataStreams))
	for _, i := range resp.Indices {
		out = append(out, IndexInfo{Name: i.Name, Kind: "index", Aliases: i.Aliases})
	}
	for _, a := range resp.Aliases {
		out = append(out, IndexInfo{Name: a.Name, Kind: "alias"})
	}
	for _, d := range resp.DataStreams {
		out = append(out, IndexInfo{Name: d.Name, Kind: "data_stream", DataStream: d.Name})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// CatIndices lists the concrete indices behind an index pattern.
func (c *Client) CatIndices(ctx context.Context, index string) ([]CatIndex, error) {
	if err := checkIndex(index); err != nil {
		return nil, c.confError("cat.indices", err.Error())
	}
	params := url.Values{"format": {"json"}, "h": {catFields}, "s": {"index"}}
	var raw []struct {
		Index     string `json:"index"`
		Health    string `json:"health"`
		Status    string `json:"status"`
		Docs      string `json:"docs.count"`
		StoreSize string `json:"store.size"`
	}
	if err := c.exchange(ctx, "cat.indices", http.MethodGet, "/_cat/indices/"+index, params, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]CatIndex, 0, len(raw))
	for _, r := range raw {
		docs, _ := strconv.ParseInt(strings.TrimSpace(r.Docs), 10, 64)
		out = append(out, CatIndex{
			Index: r.Index, Health: r.Health, Status: r.Status, Docs: docs, StoreSize: r.StoreSize,
		})
	}
	return out, nil
}

// OpenPIT opens a point in time over an index pattern for keepAlive (e.g.
// "1m"). The client never closes one: closing is a DELETE, which the read-only
// rule refuses, so a point in time is left to expire on its own keep-alive.
func (c *Client) OpenPIT(ctx context.Context, index, keepAlive string) (string, error) {
	if err := checkIndex(index); err != nil {
		return "", c.confError("pit.open", err.Error())
	}
	if strings.TrimSpace(keepAlive) == "" {
		keepAlive = pitKeepAlive
	}
	params := url.Values{"keep_alive": {keepAlive}}
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.exchange(ctx, "pit.open", http.MethodPost, "/"+index+"/_pit", params, nil, &resp); err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.ID) == "" {
		return "", c.remoteError("pit.open", "the cluster returned no point-in-time id")
	}
	return resp.ID, nil
}

// Healthy probes `GET /`. Any answer is a ServerHealth, never an error.
func (c *Client) Healthy(ctx context.Context) ServerHealth {
	h := ServerHealth{Name: c.name}
	ctx, cancel := context.WithTimeout(ctxOr(ctx), healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/", nil)
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
		ClusterName string `json:"cluster_name"`
		Version     struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
	h.Version = strings.TrimSpace(body.Version.Number)
	h.Cluster = strings.TrimSpace(body.ClusterName)
	if h.Version == "" {
		h.Detail = "no version in the answer"
		return h
	}
	h.Health = true
	h.Detail = h.Version
	if h.Cluster != "" {
		h.Detail = h.Cluster + " " + h.Version
	}
	return h
}

// searchResponse is the `_search` answer.
type searchResponse struct {
	Shards struct {
		Total      int `json:"total"`
		Successful int `json:"successful"`
		Skipped    int `json:"skipped"`
		Failed     int `json:"failed"`
	} `json:"_shards"`
	Hits struct {
		Hits []struct {
			Index  string         `json:"_index"`
			ID     string         `json:"_id"`
			Source map[string]any `json:"_source"`
			Sort   []any          `json:"sort"`
		} `json:"hits"`
	} `json:"hits"`
}

// exchange sends one read-only request and decodes the JSON answer.
func (c *Client) exchange(ctx context.Context, op, method, p string, params url.Values, in, out any) error {
	if !ReadOnlyPath(method, p) {
		return c.confError(op, fmt.Sprintf("refused %s %s: not a read-only endpoint", method, p))
	}
	u := c.base + p
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return c.confError(op, "cannot encode the request body")
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctxOr(ctx), method, u, body)
	if err != nil {
		return c.confError(op, "invalid URL")
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.roundTrip(op, req, out)
}

// roundTrip sends req and decodes the JSON answer into out, mapping the status
// code and Elasticsearch's error envelope onto an error kind.
func (c *Client) roundTrip(op string, req *http.Request, out any) error {
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
		// Elasticsearch explains a failure in its body; prefer that over the
		// transport's status line, which httpx already built.
		if errorCause(payload) != "" {
			return c.statusError(op, resp.StatusCode, payload)
		}
		if err != nil {
			return err
		}
		return c.statusError(op, resp.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if jerr := json.Unmarshal(payload, out); jerr != nil {
		return &errs.Error{Kind: errs.Parse, Op: "elastic." + op, Host: c.name,
			Msg: "the response did not parse", Err: jerr}
	}
	return nil
}

// statusError classifies a non-2xx answer, preferring the root cause of
// Elasticsearch's error envelope.
func (c *Client) statusError(op string, code int, payload []byte) error {
	kind := errs.Remote
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		kind = errs.Auth
	case code == http.StatusNotFound:
		kind = errs.NotAvailable
	}
	msg := fmt.Sprintf("status %d", code)
	if detail := errorCause(payload); detail != "" {
		msg += ": " + detail
	}
	return &errs.Error{Kind: kind, Op: "elastic." + op, Host: c.name, Msg: msg}
}

// errorCause pulls the first root cause out of an error envelope:
// {"error": {"root_cause": [{"type": ..., "reason": ...}], ...}}.
func errorCause(payload []byte) string {
	var env struct {
		Error struct {
			RootCause []struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"root_cause"`
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return ""
	}
	if len(env.Error.RootCause) > 0 {
		rc := env.Error.RootCause[0]
		return causeText(rc.Type, rc.Reason)
	}
	return causeText(env.Error.Type, env.Error.Reason)
}

// causeText renders one cause as "reason (type)".
func causeText(typ, reason string) string {
	reason = truncate(strings.TrimSpace(reason), 200)
	typ = strings.TrimSpace(typ)
	switch {
	case reason == "":
		return typ
	case typ == "":
		return reason
	}
	return reason + " (" + typ + ")"
}

// checkIndex validates an index pattern before it goes into a URL path.
func checkIndex(index string) error {
	index = strings.TrimSpace(index)
	if index == "" {
		return fmt.Errorf("an index pattern is required")
	}
	if !indexPatternRE.MatchString(index) {
		return fmt.Errorf("%q is not an index pattern", index)
	}
	return nil
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

// warnf forwards one non-fatal note to the owner.
func (c *Client) warnf(format string, args ...any) {
	if c.warn == nil {
		return
	}
	c.warn(fmt.Sprintf(format, args...))
}

// do sends the request through the httpx client when there is one.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.hc != nil {
		return c.hc.Do(req.Context(), req)
	}
	return c.http.Do(req)
}

// confError is a caller mistake: a bad index pattern, a refused path.
func (c *Client) confError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Config, Op: "elastic." + op, Host: c.name, Msg: msg}
}

// remoteError is Elasticsearch refusing or failing the request.
func (c *Client) remoteError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Remote, Op: "elastic." + op, Host: c.name, Msg: msg}
}

// ctxOr returns ctx, or a background context when it is nil.
func ctxOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
