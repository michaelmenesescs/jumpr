package plugin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/transport/httpx"
)

const (
	// HTTPTimeout bounds one plugin HTTP request (SPEC §12).
	HTTPTimeout = 15 * time.Second
	// HTTPMaxBody caps one plugin response body at 16 MiB.
	HTTPMaxBody = int64(httpx.MaxBodyBytes)
	// httpOp is the errs.Error op of every failure in this file.
	httpOp = "plugin.http"
)

// HTTP is the runner of a `target: http` plugin (SPEC §12): it builds one
// request from the definition's `request` block, sends it through the plugin's
// named connection profile and parses the response into rows.
//
// A read is GET or HEAD. A plugin may read with another method only when it
// declares `safe: true`, the author's assertion that the call changes nothing;
// :plugins shows that assertion. Every other request is a write: Run refuses it
// and Step provides the action bus step that sends it.
//
// HTTP holds no mutable state and is safe for concurrent use.
type HTTP struct {
	def    Definition
	client *httpx.Client

	// Timeout caps one request. NewHTTP sets HTTPTimeout; a shorter deadline
	// on the caller's context still wins.
	Timeout time.Duration
	// Host is substituted for {{ .Host }} in the URL template. Empty for a
	// plugin that is not host-scoped.
	Host string

	// maxBody caps the response body; NewHTTP sets HTTPMaxBody.
	maxBody int64
}

// NewHTTP validates a http or inventory plugin definition and returns its
// runner. client must be the client of the profile the definition names in
// `connection`: a plugin never picks a transport of its own.
func NewHTTP(def Definition, client *httpx.Client) (*HTTP, error) {
	r := def.Request
	switch {
	case def.Target != TargetHTTP && def.Target != TargetInventory:
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: target %q carries no HTTP request", def.Name, def.Target))
	case r == nil:
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: an http plugin needs a request", def.Name))
	case client == nil:
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: no client for connection profile %q", def.Name, def.Connection))
	case def.Connection == "":
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: request needs a connection profile", def.Name))
	case client.Profile() != def.Connection:
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: got the client of profile %q, want %q", def.Name, client.Profile(), def.Connection))
	}
	method, err := requestMethod(def)
	if err != nil {
		return nil, err
	}
	if !methodAllowed(method) {
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: method %s is not allowed (want GET, HEAD, POST, PUT, PATCH or DELETE)", def.Name, method))
	}
	if _, err := parseTemplate(r.URL); err != nil {
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: request.url: %s", def.Name, err))
	}
	lower := strings.ToLower(strings.TrimSpace(r.URL))
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: request.url must start with http:// or https://", def.Name))
	}
	if len(r.Body) > 0 && !bodyAllowed(method) {
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: a %s request cannot carry a body", def.Name, method))
	}
	for k, v := range r.Headers {
		if !validHeader(k, v) {
			return nil, errs.New(errs.Config, httpOp,
				fmt.Sprintf("plugin %q: header %q is not a valid HTTP header", def.Name, k))
		}
	}
	return &HTTP{def: def, client: client, Timeout: HTTPTimeout, maxBody: HTTPMaxBody}, nil
}

// Name is the plugin name.
func (h *HTTP) Name() string { return h.def.Name }

// Connection is the connection profile the plugin sends through.
func (h *HTTP) Connection() string { return h.def.Connection }

// Method is the request method, upper case; an empty method is GET.
func (h *HTTP) Method() string {
	if h.def.Request == nil {
		return http.MethodGet
	}
	m := strings.ToUpper(strings.TrimSpace(h.def.Request.Method))
	if m == "" {
		return http.MethodGet
	}
	return m
}

// SafeRead reports whether the request may run as a read: GET and HEAD always,
// another method only when the plugin declares `safe: true`. :plugins shows a
// non-GET read with that assertion next to it.
func (h *HTTP) SafeRead() bool {
	switch h.Method() {
	case http.MethodGet, http.MethodHead:
		return true
	}
	return h.def.Request != nil && h.def.Request.Safe
}

// URL renders the request URL for params. Every substituted value is escaped
// for the part of the URL it lands in (see RenderURL), and the result must be
// an absolute http:// or https:// URL.
func (h *HTTP) URL(params map[string]string) (string, error) {
	if h.def.Request == nil {
		return "", errs.New(errs.Config, httpOp, fmt.Sprintf("plugin %q has no request", h.def.Name))
	}
	raw, err := RenderURL(h.def.Request.URL, params, h.Host)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: request.url is not a URL", h.def.Name))
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: request.url must be an absolute http:// or https:// URL", h.def.Name))
	}
	return raw, nil
}

// Display renders the plan line of one request: the method and the URL. Headers
// and the body never appear, because either may carry a credential.
func (h *HTTP) Display(params map[string]string) string {
	u, err := h.URL(params)
	if err != nil && h.def.Request != nil {
		u = h.def.Request.URL
	}
	return strings.TrimSpace(h.Method() + " " + u)
}

// Run performs a read and returns the parsed rows. It refuses a request that is
// not a safe read: a write runs only through the action bus, from Step.
func (h *HTTP) Run(ctx context.Context, params map[string]string) ([]map[string]any, error) {
	if !h.SafeRead() {
		return nil, errs.New(errs.Policy, httpOp, fmt.Sprintf(
			"plugin %q: %s changes state; run it from the actions menu", h.def.Name, h.Method()))
	}
	body, _, err := h.do(ctx, params)
	if err != nil {
		return nil, err
	}
	return h.parse(body)
}

// Step is the action bus step of one request. The bus asks for confirmation,
// applies policy and records the result; the step only sends the request.
// Display is the method and the URL.
func (h *HTTP) Step(params map[string]string) action.Step {
	return action.Step{
		Display: h.Display(params),
		Run: func(ctx context.Context) action.StepResult {
			start := time.Now()
			body, status, err := h.do(ctx, params)
			return action.StepResult{
				RC:       execRC(status),
				Output:   string(body),
				Err:      err,
				Duration: time.Since(start),
			}
		},
	}
}

// parse turns the response body into rows with the definition's parse block.
func (h *HTTP) parse(body []byte) ([]map[string]any, error) {
	rows, err := Parse(h.def.Parse, body)
	if err != nil {
		// Wrap, not New: KindOf would drop the cause and break errors.Is/As for
		// the parser's Timeout/Canceled kinds.
		return nil, errs.Wrap(errs.KindOf(err), httpOp, err).WithHost(h.def.Name)
	}
	return rows, nil
}

// do sends the request and returns the capped response body and its status. A
// transport or status failure is an *errs.Error classified by httpx; the body
// is still returned when there is one, so a write step can show it.
func (h *HTTP) do(ctx context.Context, params map[string]string) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, errs.Wrap(errs.KindOf(err), httpOp, err)
	}
	req, err := h.request(ctx, params)
	if err != nil {
		return nil, 0, err
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = HTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := h.client.Do(ctx, req)
	if resp == nil {
		if err == nil {
			err = errs.New(errs.Internal, httpOp, fmt.Sprintf("plugin %q: no response", h.def.Name))
		}
		return nil, 0, err
	}
	defer resp.Body.Close()

	max := h.maxBody
	if max <= 0 {
		max = HTTPMaxBody
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, max))
	switch {
	case rerr != nil && err != nil:
		return body, resp.StatusCode, err
	case rerr != nil:
		return body, resp.StatusCode, errs.New(errs.Remote, httpOp,
			fmt.Sprintf("plugin %q: cannot read the response", h.def.Name))
	case err != nil:
		return body, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// request builds the HTTP request for params.
func (h *HTTP) request(ctx context.Context, params map[string]string) (*http.Request, error) {
	raw, err := h.URL(params)
	if err != nil {
		return nil, err
	}
	method := h.Method()
	var body io.Reader
	if method != http.MethodGet && method != http.MethodHead && h.def.Request.Body != "" {
		body = strings.NewReader(h.def.Request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, errs.New(errs.Config, httpOp,
			fmt.Sprintf("plugin %q: cannot build the request", h.def.Name))
	}
	for k, v := range h.def.Request.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// RenderURL expands a plugin URL template. A value substituted into the path is
// escaped with url.PathEscape and one substituted into the query with
// url.QueryEscape, so a parameter value can never add a path segment, a query
// parameter or a fragment of its own.
func RenderURL(text string, params map[string]string, host string) (string, error) {
	parts, err := parseTemplate(text)
	if err != nil {
		return "", errs.New(errs.Config, httpOp, "request.url: "+err.Error())
	}
	var b strings.Builder
	inQuery := false
	for _, p := range parts {
		switch {
		case p.param != "":
			v, ok := params[p.param]
			if !ok {
				return "", errs.New(errs.Config, httpOp,
					fmt.Sprintf("request.url: parameter %q has no value", p.param))
			}
			b.WriteString(escapeURLValue(v, inQuery))
		case p.host:
			b.WriteString(escapeURLValue(host, inQuery))
		default:
			b.WriteString(p.lit)
			inQuery = advanceQueryState(p.lit, inQuery)
		}
	}
	return b.String(), nil
}

// escapeURLValue escapes one substituted value for its position in the URL.
func escapeURLValue(v string, inQuery bool) string {
	if inQuery {
		return url.QueryEscape(v)
	}
	return url.PathEscape(v)
}

// advanceQueryState tracks whether the next substitution is in the query or in
// the fragment of the URL.
func advanceQueryState(lit string, inQuery bool) bool {
	for i := 0; i < len(lit); i++ {
		switch lit[i] {
		case '?':
			inQuery = true
		case '#':
			inQuery = false
		}
	}
	return inQuery
}

// requestMethod returns the upper-case method of def's request.
func requestMethod(def Definition) (string, error) {
	if def.Request == nil {
		return "", errs.New(errs.Config, httpOp, fmt.Sprintf("plugin %q has no request", def.Name))
	}
	m := strings.ToUpper(strings.TrimSpace(def.Request.Method))
	if m == "" {
		return http.MethodGet, nil
	}
	return m, nil
}

// methodAllowed reports whether a plugin request may use method.
func methodAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// bodyAllowed reports whether method may carry a request body.
func bodyAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return false
	}
	return true
}

// validHeader reports whether name and value form a header net/http can send
// and a plugin author cannot use to smuggle a second header.
func validHeader(name, value string) bool {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t\r\n:") {
		return false
	}
	return !strings.ContainsAny(value, "\r\n\x00")
}

// execRC maps an HTTP status to the rc a bus step reports: -1 when the request
// never reached the server.
func execRC(status int) int {
	if status == 0 {
		return -1
	}
	return status
}
