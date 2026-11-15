// Query building for Grafana panels: the /api/ds/query request of one panel
// and the client-side template-variable interpolation that goes into it
// (SPEC §8.4, task 120).
//
// Grafana interpolates a dashboard's variables in the browser before it sends
// a query; jumpr does the same, because /api/ds/query knows nothing about
// template variables. Everything here is pure except Client.QueryPanel, which
// sends one request.

package grafana

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
)

// Vars holds the values of a dashboard's template variables: one or more per
// name. A multi-value variable interpolates to its values joined with "|",
// which is Grafana's default (regex) format for a Prometheus matcher.
type Vars map[string][]string

// QueryVars returns every template variable's current values, keyed by name
// without the leading "$": the form Interpolation consumes.
func (d *Dashboard) QueryVars() Vars {
	if d == nil || len(d.Variables) == 0 {
		return nil
	}
	out := make(Vars, len(d.Variables))
	for _, v := range d.Variables {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			continue
		}
		out[name] = append([]string(nil), v.Current...)
	}
	return out
}

// VarsOf builds Vars from single-valued entries, the form Dashboard.Values
// returns.
func VarsOf(m map[string]string) Vars {
	if len(m) == 0 {
		return nil
	}
	out := make(Vars, len(m))
	for k, v := range m {
		name := strings.TrimPrefix(strings.TrimSpace(k), "$")
		if name == "" || strings.TrimSpace(v) == "" {
			continue
		}
		out[name] = []string{v}
	}
	return out
}

// Get returns the values of one variable, with or without a leading "$".
func (v Vars) Get(name string) []string {
	if v == nil {
		return nil
	}
	return v[strings.TrimPrefix(strings.TrimSpace(name), "$")]
}

// Interpolation is everything substituted into a panel query before it is
// sent: the dashboard's variables and the built-in time variables.
type Interpolation struct {
	// Vars are the dashboard's template variables.
	Vars Vars
	// From and To are the window the queries cover.
	From, To time.Time
	// Interval is $__interval, the step a range query should use.
	Interval time.Duration
	// RateInterval is $__rate_interval, the step a rate() call should use.
	// Zero means four intervals.
	RateInterval time.Duration
	// Range is $__range, the window's width. Zero means To minus From.
	Range time.Duration
	// AllValue replaces a variable's "all" choice. Empty means ".*".
	AllValue string
}

// InterpolateVars replaces the template variables in expr, using vars, the
// window and a data-point budget. It is the shorthand for a one-off query.
func InterpolateVars(expr string, vars Vars, from, to time.Time, maxDataPoints int64) string {
	return NewInterpolation(vars, from, to, maxDataPoints).Apply(expr)
}

// NewInterpolation builds the interpolation of a window and a data-point
// budget: the interval is the window divided by the budget, and
// $__rate_interval is four intervals, which is Grafana's default.
func NewInterpolation(vars Vars, from, to time.Time, maxDataPoints int64) Interpolation {
	iv := Interpolation{Vars: vars, From: from, To: to}
	if span := to.Sub(from); span > 0 {
		iv.Range = span
		if maxDataPoints > 0 {
			iv.Interval = span / time.Duration(maxDataPoints)
		}
	}
	if iv.Interval <= 0 {
		iv.Interval = time.Second
	}
	iv.RateInterval = 4 * iv.Interval
	return iv
}

// withWindow fills whatever the caller left zero from the window it is about
// to send, so BuildPanelQuery works from a bare Interpolation{Vars: vars}.
func (iv Interpolation) withWindow(from, to time.Time, maxDataPoints int64) Interpolation {
	if iv.From.IsZero() {
		iv.From = from
	}
	if iv.To.IsZero() {
		iv.To = to
	}
	if iv.Range <= 0 {
		if span := iv.To.Sub(iv.From); span > 0 {
			iv.Range = span
		}
	}
	if iv.Interval <= 0 && maxDataPoints > 0 && iv.Range > 0 {
		iv.Interval = iv.Range / time.Duration(maxDataPoints)
	}
	if iv.Interval <= 0 {
		iv.Interval = time.Second
	}
	if iv.RateInterval <= 0 {
		iv.RateInterval = 4 * iv.Interval
	}
	return iv
}

// Apply interpolates expr: `$var`, `${var}`, `${var:format}` and the legacy
// `[[var]]` form become their values, `$$` becomes a literal `$`, and the
// built-in time variables ($__interval, $__interval_ms, $__rate_interval,
// $__range, $__range_s, $__from, $__to) are filled from the window. A name
// nothing defines is left exactly as it was, so a typo shows up in the query
// instead of vanishing.
func (iv Interpolation) Apply(expr string) string {
	if expr == "" {
		return expr
	}
	var b strings.Builder
	b.Grow(len(expr))
	for i := 0; i < len(expr); {
		switch {
		case expr[i] == '$':
			if i+1 < len(expr) && expr[i+1] == '$' {
				b.WriteByte('$')
				i += 2
				continue
			}
			if i+1 < len(expr) && expr[i+1] == '{' {
				if end := strings.IndexByte(expr[i+2:], '}'); end >= 0 {
					body := expr[i+2 : i+2+end]
					if val, ok := iv.lookup(body); ok {
						b.WriteString(val)
					} else {
						b.WriteString(expr[i : i+3+end])
					}
					i += 3 + end
					continue
				}
			}
			if name, n := identAt(expr, i+1); n > 0 {
				if val, ok := iv.lookup(name); ok {
					b.WriteString(val)
					i += 1 + n
					continue
				}
			}
			b.WriteByte('$')
			i++
		case expr[i] == '[' && i+1 < len(expr) && expr[i+1] == '[':
			if end := strings.Index(expr[i+2:], "]]"); end >= 0 {
				name := expr[i+2 : i+2+end]
				if val, ok := iv.lookup(name); ok {
					b.WriteString(val)
				} else {
					b.WriteString(expr[i : i+4+end])
				}
				i += 4 + end
				continue
			}
			b.WriteByte(expr[i])
			i++
		default:
			b.WriteByte(expr[i])
			i++
		}
	}
	return b.String()
}

// lookup resolves one variable body, with an optional ":format" suffix.
func (iv Interpolation) lookup(body string) (string, bool) {
	name, format := body, ""
	if k := strings.LastIndexByte(body, ':'); k > 0 {
		name, format = body[:k], body[k+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if val, ok := iv.builtin(name); ok {
		return val, true
	}
	vals := iv.allValues(iv.Vars.Get(name))
	if len(vals) == 0 {
		return "", false
	}
	return joinValues(vals, format), true
}

// allValues replaces a variable's "all" choice with the all value.
func (iv Interpolation) allValues(vals []string) []string {
	if len(vals) == 0 {
		return nil
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "$__all" || strings.EqualFold(v, "all") {
			out = append(out, iv.all())
			continue
		}
		out = append(out, v)
	}
	return out
}

// all is the value a variable's "all" choice interpolates to.
func (iv Interpolation) all() string {
	if v := strings.TrimSpace(iv.AllValue); v != "" {
		return v
	}
	return ".*"
}

// joinValues joins a multi-value variable in the format asked for: "|" unless
// the reference said otherwise.
func joinValues(vals []string, format string) string {
	if len(vals) == 1 {
		return vals[0]
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "csv", "comma":
		return strings.Join(vals, ",")
	case "json":
		enc, err := json.Marshal(vals)
		if err != nil {
			return strings.Join(vals, "|")
		}
		return string(enc)
	}
	return strings.Join(vals, "|")
}

// builtin resolves the time variables. It is the only place jumpr claims a
// name the dashboard cannot define itself.
func (iv Interpolation) builtin(name string) (string, bool) {
	switch name {
	case "__interval":
		return formatInterval(iv.Interval), true
	case "__interval_ms":
		return strconv.FormatInt(iv.Interval.Milliseconds(), 10), true
	case "__rate_interval":
		return formatInterval(iv.RateInterval), true
	case "__range":
		return strconv.FormatInt(int64(iv.Range.Seconds()), 10) + "s", true
	case "__range_s":
		return strconv.FormatInt(int64(iv.Range.Seconds()), 10), true
	case "__from":
		return strconv.FormatInt(iv.From.UnixMilli(), 10), true
	case "__to":
		return strconv.FormatInt(iv.To.UnixMilli(), 10), true
	}
	return "", false
}

// formatInterval renders a step the way Grafana does: milliseconds below a
// second, whole minutes, whole hours, otherwise seconds.
func formatInterval(d time.Duration) string {
	if d <= 0 {
		return "1s"
	}
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	secs := int64(d.Seconds())
	switch {
	case secs >= 3600 && secs%3600 == 0:
		return strconv.FormatInt(secs/3600, 10) + "h"
	case secs >= 60 && secs%60 == 0:
		return strconv.FormatInt(secs/60, 10) + "m"
	}
	return strconv.FormatInt(secs, 10) + "s"
}

// identAt reads a variable name at expr[i:]: letters, digits and underscores,
// not starting with a digit.
func identAt(expr string, i int) (string, int) {
	j := i
	for j < len(expr) {
		c := expr[j]
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		digit := j > i && c >= '0' && c <= '9'
		if !letter && !digit {
			break
		}
		j++
	}
	return expr[i:j], j - i
}

// PanelQuery is one panel's /api/ds/query request, with the refIds and the
// interpolated expressions that went into it.
type PanelQuery struct {
	Request     DSRequest
	Refs        []string
	Expressions []string // "A: up{instance=~\"web.*\"}", for the plan and the finding
}

// MaxDataPointsFor is the data-point budget for a panel cell width: a braille
// chart draws two samples per cell, so asking for more only slows the query
// down (task 120, Pitfalls).
func MaxDataPointsFor(width int) int64 {
	if width < 1 {
		width = 1
	}
	return int64(width) * 2
}

// BuildPanelQuery builds the request that loads p over [from,to]: one query
// per visible target that carries an expression, each with its datasource,
// refId, intervalMs and maxDataPoints, and the window as epoch milliseconds.
// Nothing is sent here.
func BuildPanelQuery(p Panel, iv Interpolation, from, to time.Time, maxDataPoints int64) (PanelQuery, error) {
	if maxDataPoints < 1 {
		maxDataPoints = 1
	}
	wiv := iv.withWindow(from, to, maxDataPoints)
	pq := PanelQuery{Request: DSRequest{
		From: strconv.FormatInt(from.UnixMilli(), 10),
		To:   strconv.FormatInt(to.UnixMilli(), 10),
	}}
	intervalMs := wiv.Interval.Milliseconds()
	if intervalMs < 1 {
		intervalMs = 1
	}
	if span := to.Sub(from).Milliseconds(); span > 0 && intervalMs > span {
		intervalMs = span
	}
	for i, t := range p.Targets {
		if t.Hidden {
			continue
		}
		expr := strings.TrimSpace(wiv.Apply(t.Expression()))
		if expr == "" {
			continue
		}
		ref := strings.TrimSpace(t.RefID)
		if ref == "" {
			ref = refIDFor(i)
		}
		ds := t.Datasource
		if ds.IsZero() {
			ds = p.Datasource
		}
		q := DSQuery{RefID: ref, Datasource: ds, IntervalMs: intervalMs, MaxDataPoints: maxDataPoints}
		if strings.TrimSpace(t.Expr) != "" {
			q.Expr = expr
		} else {
			q.Query = expr
		}
		pq.Request.Queries = append(pq.Request.Queries, q)
		pq.Refs = append(pq.Refs, ref)
		pq.Expressions = append(pq.Expressions, ref+": "+expr)
	}
	if len(pq.Request.Queries) == 0 {
		return PanelQuery{}, &errs.Error{Kind: errs.NotAvailable, Op: "grafana.panel",
			Msg: "the panel has no queries to run"}
	}
	return pq, nil
}

// refIDFor names a target Grafana's way: A, B, C… and A26 after Z.
func refIDFor(i int) string {
	if i < 0 {
		return "A"
	}
	if i < 26 {
		return string(rune('A' + i))
	}
	return "A" + strconv.Itoa(i)
}

// rawDSResponse is a /api/ds/query answer with its frames left undecoded, so
// the schema's labels and field config survive (see frames.go).
type rawDSResponse struct {
	Results map[string]rawDSResult `json:"results"`
}

// rawDSResult is one query's frames or its error.
type rawDSResult struct {
	Frames []json.RawMessage `json:"frames"`
	Error  string            `json:"error"`
	Status int               `json:"status"`
}

// PanelQuerier runs one panel's queries. *Client implements it.
type PanelQuerier interface {
	QueryPanel(ctx context.Context, p Panel, iv Interpolation, from, to time.Time, maxDataPoints int64) ([]Series, error)
}

// QueryPanel runs p's queries through /api/ds/query and decodes the frames
// into series, in refId order. An error string in a result becomes an
// errs.Remote error naming its refId.
func (c *Client) QueryPanel(ctx context.Context, p Panel, iv Interpolation, from, to time.Time, maxDataPoints int64) ([]Series, error) {
	pq, err := BuildPanelQuery(p, iv, from, to, maxDataPoints)
	if err != nil {
		return nil, err
	}
	var resp rawDSResponse
	if err := c.post(ctx, "ds.query", pathDSQuery, pq.Request, &resp); err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(pq.Refs))
	for _, ref := range pq.Refs {
		res, ok := resp.Results[ref]
		if !ok {
			continue
		}
		if msg := strings.TrimSpace(res.Error); msg != "" {
			return nil, c.remoteError("ds.query", ref+": "+msg)
		}
		out = append(out, DecodeFrames(ref, res.Frames)...)
	}
	return out, nil
}
