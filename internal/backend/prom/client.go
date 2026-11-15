// Package prom is the Prometheus HTTP API backend (SPEC §8.3): instant and
// range queries, targets, rules, alerts, series and label values, plus the
// health probe.
//
// It is read-only and speaks the standard envelope ({"status": ..., "data":
// ..., "errorType": ..., "error": ..., "warnings": ...}) through an
// internal/transport/httpx client, so proxies, TLS, auth helpers and retries
// are the connection profile's business, not a backend's.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/transport/httpx"
)

// MaxPoints is the most points a range query asks for: a braille chart cannot
// show more, and Prometheus should not build them.
const MaxPoints = 300

// API paths.
const (
	pathQuery       = "/api/v1/query"
	pathQueryRange  = "/api/v1/query_range"
	pathTargets     = "/api/v1/targets"
	pathRules       = "/api/v1/rules"
	pathAlerts      = "/api/v1/alerts"
	pathSeries      = "/api/v1/series"
	pathLabelValues = "/api/v1/label/"
	pathHealthy     = "/-/healthy"
)

// healthTimeout bounds one /-/healthy probe (BUILD-PLAN §8: health checks 5 s).
const healthTimeout = 5 * time.Second

// Envelope is the standard Prometheus response envelope.
type Envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings"`
}

// Sample is one timestamped value. A value Prometheus sends as "NaN", "+Inf"
// or "-Inf" keeps its IEEE meaning.
type Sample struct {
	T time.Time
	V float64
}

// Series is one time series: its label set and its samples. An instant query
// result carries exactly one sample per series.
type Series struct {
	Metric map[string]string
	Values []Sample
}

// Vector is an instant-query result.
type Vector []Series

// Matrix is a range-query result.
type Matrix []Series

// Target is one scrape target.
type Target struct {
	Labels     map[string]string
	ScrapeURL  string
	Health     string
	LastScrape time.Time
	LastError  string
	ScrapePool string
}

// Alert is one alert of /api/v1/alerts or of an alerting rule.
type Alert struct {
	Labels      map[string]string
	Annotations map[string]string
	State       string
	ActiveAt    time.Time
	Value       string
}

// Rule is one rule of a rule group. State is empty for recording rules.
type Rule struct {
	Name      string
	Query     string
	Duration  string
	Health    string
	Type      string
	State     string
	LastError string
	Alerts    []Alert
}

// RuleGroup is one rules file's group.
type RuleGroup struct {
	Name  string
	File  string
	Rules []Rule
}

// ServerHealth is one Prometheus server's answer to the health probe.
type ServerHealth struct {
	Name   string
	Health bool
	Detail string
}

// ClientOptions configures a Client.
type ClientOptions struct {
	// URL is the server's base URL, e.g. https://prometheus.example.internal.
	URL string
	// Client is the connection profile's HTTP client. Nil builds a plain one,
	// which is only useful in tests.
	Client *httpx.Client
	// Name is the configured server name, used in errors and warnings.
	Name string
	// Warn receives the envelope's warnings (one line each). Nil discards.
	Warn func(msg string)
	// Now is the clock. Nil uses the wall clock.
	Now func() time.Time
}

// Client is one Prometheus server. It is safe for concurrent use: it holds only
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
		return nil, errs.New(errs.Config, "prom.client",
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

// warnf forwards one envelope warning to the owner.
func (c *Client) warnf(format string, args ...any) {
	if c.warn == nil {
		return
	}
	c.warn(fmt.Sprintf(format, args...))
}

// Query runs an instant query at ts.
func (c *Client) Query(ctx context.Context, expr string, ts time.Time) (Vector, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, c.confError("query", "an instant query needs an expression")
	}
	params := url.Values{"query": {expr}}
	if !ts.IsZero() {
		params.Set("time", formatTime(ts))
	}
	var block resultBlock
	if err := c.get(ctx, "query", pathQuery, params, &block); err != nil {
		return nil, err
	}
	return block.vector(), nil
}

// QueryRange runs a range query from..to with the given step. A non-positive
// step or an inverted range is an errs.Config error: a caller must choose the
// step with Step.
func (c *Client) QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) (Matrix, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, c.confError("query_range", "a range query needs an expression")
	}
	if step <= 0 {
		return nil, c.confError("query_range", "a range query needs a positive step")
	}
	if !to.After(from) {
		return nil, c.confError("query_range", "the range ends before it starts")
	}
	params := url.Values{
		"query": {expr},
		"start": {formatTime(from)},
		"end":   {formatTime(to)},
		"step":  {formatSeconds(step)},
	}
	var block resultBlock
	if err := c.get(ctx, "query_range", pathQueryRange, params, &block); err != nil {
		return nil, err
	}
	return block.matrix(), nil
}

// Targets returns the scrape targets of every job.
func (c *Client) Targets(ctx context.Context) ([]Target, error) {
	var data struct {
		ActiveTargets []rawTarget `json:"activeTargets"`
	}
	if err := c.get(ctx, "targets", pathTargets, nil, &data); err != nil {
		return nil, err
	}
	out := make([]Target, 0, len(data.ActiveTargets))
	for _, t := range data.ActiveTargets {
		out = append(out, t.target())
	}
	return out, nil
}

// Rules returns every rule group. state is the rule's own state when Prometheus
// sends one, otherwise the worst state of its alerts.
func (c *Client) Rules(ctx context.Context) ([]RuleGroup, error) {
	var data struct {
		Groups []rawRuleGroup `json:"groups"`
	}
	if err := c.get(ctx, "rules", pathRules, nil, &data); err != nil {
		return nil, err
	}
	out := make([]RuleGroup, 0, len(data.Groups))
	for _, g := range data.Groups {
		grp := RuleGroup{Name: g.Name, File: g.File}
		for _, r := range g.Rules {
			grp.Rules = append(grp.Rules, r.rule())
		}
		out = append(out, grp)
	}
	return out, nil
}

// Alerts returns the currently active alerts.
func (c *Client) Alerts(ctx context.Context) ([]Alert, error) {
	var data struct {
		Alerts []rawAlert `json:"alerts"`
	}
	if err := c.get(ctx, "alerts", pathAlerts, nil, &data); err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(data.Alerts))
	for _, a := range data.Alerts {
		out = append(out, a.alert())
	}
	return out, nil
}

// Series returns the label sets matching at least one of match over the
// range from..to. A zero range asks for any time.
func (c *Client) Series(ctx context.Context, match []string, from, to time.Time) ([]map[string]string, error) {
	if len(match) == 0 {
		return nil, c.confError("series", "a series query needs at least one match selector")
	}
	params := url.Values{}
	for _, m := range match {
		params.Add("match[]", m)
	}
	if !from.IsZero() && !to.IsZero() {
		params.Set("start", formatTime(from))
		params.Set("end", formatTime(to))
	}
	var out []map[string]string
	if err := c.get(ctx, "series", pathSeries, params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// LabelValues returns the values of one label.
func (c *Client) LabelValues(ctx context.Context, label string) ([]string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, c.confError("label_values", "a label needs a name")
	}
	var out []string
	path := pathLabelValues + url.PathEscape(label) + "/values"
	if err := c.get(ctx, "label_values", path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Healthy probes /-/healthy. It is the cheapest way to tell whether a
// configured server answers at all; any answer is a Health, never an error.
func (c *Client) Healthy(ctx context.Context) ServerHealth {
	h := ServerHealth{Name: c.name}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+pathHealthy, nil)
	if err != nil {
		h.Detail = string(errs.Config)
		return h
	}
	resp, err := c.do(req)
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

// get performs one GET of an API path and decodes the envelope's data into out.
// A status other than "success" becomes an errs.Remote error carrying the
// Prometheus message; the envelope's warnings go to the Warn callback.
func (c *Client) get(ctx context.Context, op, path string, params url.Values, out any) error {
	u := c.base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctxOr(ctx), http.MethodGet, u, nil)
	if err != nil {
		return c.confError(op, "invalid URL")
	}
	resp, err := c.do(req)
	if resp == nil {
		if err == nil {
			err = c.confError(op, "no response")
		}
		return err
	}
	defer resp.Body.Close()

	body, rerr := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxBodyBytes))
	if rerr != nil {
		if err != nil {
			return err
		}
		return c.remoteError(op, fmt.Sprintf("cannot read the response: %v", rerr))
	}

	var env Envelope
	if jerr := json.Unmarshal(body, &env); jerr != nil {
		// A non-JSON body is not a Prometheus answer: report the transport's
		// status error when there is one, else the parse failure.
		if err != nil {
			return err
		}
		return &errs.Error{Kind: errs.Parse, Op: "prom." + op, Host: c.name,
			Msg: "the response is not a Prometheus envelope", Err: jerr}
	}
	for _, w := range env.Warnings {
		c.warnf("prometheus %s: %s", c.name, w)
	}
	if env.Status != "success" {
		msg := env.Error
		if msg == "" {
			msg = "status " + env.Status
		}
		if env.ErrorType != "" {
			msg = env.ErrorType + ": " + msg
		}
		return c.remoteError(op, msg)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if jerr := json.Unmarshal(env.Data, out); jerr != nil {
		return &errs.Error{Kind: errs.Parse, Op: "prom." + op, Host: c.name,
			Msg: "the response data did not parse", Err: jerr}
	}
	return nil
}

// do sends the request through the httpx client when there is one.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.hc != nil {
		return c.hc.Do(req.Context(), req)
	}
	return c.http.Do(req)
}

// confError is a caller mistake: an empty expression, a bad step.
func (c *Client) confError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Config, Op: "prom." + op, Host: c.name, Msg: msg}
}

// remoteError is Prometheus refusing or failing the query.
func (c *Client) remoteError(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.Remote, Op: "prom." + op, Host: c.name, Msg: msg}
}

// Step returns the smallest whole-second step that yields at most maxPoints
// points (endpoints included) between from and to. It is what every range
// query uses: a chart cannot draw more, and Prometheus should not compute more.
// maxPoints below 2 means MaxPoints.
func Step(from, to time.Time, maxPoints int) time.Duration {
	if maxPoints < 2 {
		maxPoints = MaxPoints
	}
	d := to.Sub(from)
	if d <= 0 {
		return time.Second
	}
	step := time.Duration(math.Ceil(float64(d) / float64(maxPoints-1)))
	if step < time.Second {
		step = time.Second
	}
	if r := step % time.Second; r != 0 {
		step += time.Second - r
	}
	return step
}

// formatTime renders an instant the way Prometheus accepts it: RFC 3339 with
// nanoseconds, or whole seconds when it has none.
func formatTime(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05Z")
	}
	return t.Format(time.RFC3339Nano)
}

// formatSeconds renders a step as the decimal seconds Prometheus expects.
func formatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// ctxOr returns ctx, or a background context when it is nil.
func ctxOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// resultBlock is the data of an instant or range query.
type resultBlock struct {
	ResultType string      `json:"resultType"`
	Result     []rawSeries `json:"result"`
}

// rawSeries is one Prometheus series: value for a vector, values for a matrix.
type rawSeries struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value"`
	Values [][]json.RawMessage `json:"values"`
}

// rawTarget is the subset of a target jumpr shows.
type rawTarget struct {
	Labels     map[string]string `json:"labels"`
	ScrapeURL  string            `json:"scrapeUrl"`
	ScrapePool string            `json:"scrapePool"`
	Health     string            `json:"health"`
	LastScrape time.Time         `json:"lastScrape"`
	LastError  string            `json:"lastError"`
}

// rawAlert is an alert of the alerts API or of an alerting rule.
type rawAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    time.Time         `json:"activeAt"`
	Value       string            `json:"value"`
}

// rawRule is one rule of a group.
type rawRule struct {
	Name      string     `json:"name"`
	Query     string     `json:"query"`
	Duration  float64    `json:"duration"`
	Health    string     `json:"health"`
	Type      string     `json:"type"`
	State     string     `json:"state"`
	LastError string     `json:"lastError"`
	Alerts    []rawAlert `json:"alerts"`
}

// rawRuleGroup is one group of the rules API.
type rawRuleGroup struct {
	Name  string    `json:"name"`
	File  string    `json:"file"`
	Rules []rawRule `json:"rules"`
}

// vector converts a decoded block into a Vector.
func (b resultBlock) vector() Vector {
	out := make(Vector, 0, len(b.Result))
	for _, r := range b.Result {
		s := Series{Metric: r.Metric}
		if v, ok := parseSample(r.Value); ok {
			s.Values = []Sample{v}
		}
		out = append(out, s)
	}
	return out
}

// matrix converts a decoded block into a Matrix.
func (b resultBlock) matrix() Matrix {
	out := make(Matrix, 0, len(b.Result))
	for _, r := range b.Result {
		s := Series{Metric: r.Metric}
		for _, pair := range r.Values {
			if v, ok := parseSample(pair); ok {
				s.Values = append(s.Values, v)
			}
		}
		out = append(out, s)
	}
	return out
}

// target converts a decoded target.
func (t rawTarget) target() Target {
	return Target{
		Labels:     t.Labels,
		ScrapeURL:  t.ScrapeURL,
		Health:     t.Health,
		LastScrape: t.LastScrape,
		LastError:  t.LastError,
		ScrapePool: t.ScrapePool,
	}
}

// alert converts a decoded alert.
func (a rawAlert) alert() Alert {
	return Alert{
		Labels:      a.Labels,
		Annotations: a.Annotations,
		State:       a.State,
		ActiveAt:    a.ActiveAt,
		Value:       a.Value,
	}
}

// rule converts a decoded rule and derives its state when Prometheus did not
// send one.
func (r rawRule) rule() Rule {
	out := Rule{
		Name:      r.Name,
		Query:     r.Query,
		Duration:  durationText(r.Duration),
		Health:    r.Health,
		Type:      r.Type,
		State:     r.State,
		LastError: r.LastError,
	}
	for _, a := range r.Alerts {
		out.Alerts = append(out.Alerts, a.alert())
	}
	if out.State == "" && strings.EqualFold(r.Type, "alerting") {
		out.State = worstAlertState(out.Alerts)
	}
	return out
}

// worstAlertState folds the alerts of a rule into one state.
func worstAlertState(alerts []Alert) string {
	worst := ""
	for _, a := range alerts {
		switch a.State {
		case "firing":
			return "firing"
		case "pending":
			worst = "pending"
		}
	}
	if worst != "" {
		return worst
	}
	return "inactive"
}

// durationText renders a rule's `for` duration in seconds.
func durationText(secs float64) string {
	if secs <= 0 {
		return "0s"
	}
	return time.Duration(secs * float64(time.Second)).String()
}

// parseSample decodes one [timestamp, "value"] pair. A value that does not
// parse becomes NaN, which every renderer shows as a gap; garbage never panics.
func parseSample(pair []json.RawMessage) (Sample, bool) {
	if len(pair) != 2 {
		return Sample{}, false
	}
	ts, ok := parseTimestamp(pair[0])
	if !ok {
		return Sample{}, false
	}
	s := Sample{T: ts}
	var raw string
	if err := json.Unmarshal(pair[1], &raw); err != nil {
		s.V = math.NaN()
		return s, true
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		s.V = math.NaN()
		return s, true
	}
	s.V = v
	return s, true
}

// parseTimestamp decodes a Prometheus timestamp, which is either a JSON number
// or a string, without the nanosecond rounding a float64 would lose.
func parseTimestamp(raw json.RawMessage) (time.Time, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		var num json.Number
		if err := json.Unmarshal(raw, &num); err != nil {
			return time.Time{}, false
		}
		text = num.String()
	}
	secs, fracs, _ := strings.Cut(text, ".")
	sec, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	var nsec int64
	if fracs != "" {
		if len(fracs) > 9 {
			fracs = fracs[:9]
		}
		for len(fracs) < 9 {
			fracs += "0"
		}
		n, err := strconv.ParseInt(fracs, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		nsec = n
	}
	return time.Unix(sec, nsec).UTC(), true
}

// LabelString renders a label set as Prometheus does, sorted and without the
// synthetic __name__ first: `{__name__="up", instance="a:9100", job="node"}`.
// The metric name comes first when present.
func LabelString(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	name := labels["__name__"]
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "__name__" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	first := true
	if name != "" {
		b.WriteString(`__name__="`)
		b.WriteString(name)
		b.WriteByte('"')
		first = false
	}
	for _, k := range keys {
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
