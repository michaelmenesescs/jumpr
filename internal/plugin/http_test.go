package plugin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/transport/httpx"
)

// loadTestDef writes one plugin file and loads it, failing the test on any
// problem. It keeps every fixture out of testdata/ and off the network.
func loadTestDef(t *testing.T, text string) Definition {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	ok, probs := LoadDir(dir)
	if len(probs) > 0 {
		t.Fatalf("plugin problems: %v", probs)
	}
	if len(ok) != 1 {
		t.Fatalf("loaded %d definitions, want 1", len(ok))
	}
	return ok[0]
}

// testClient builds the client of a named profile with a short timeout, so a
// broken test fails instead of hanging.
func testClient(t *testing.T, name string) *httpx.Client {
	t.Helper()
	p := &httpx.Profile{Name: name, Timeout: httpx.Duration(5 * time.Second)}
	c, err := p.Client(httpx.Deps{})
	if err != nil {
		t.Fatalf("client %q: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// poolMembersYAML is the SPEC §12 lb-pool-members example, pointed at url.
func poolMembersYAML(url string) string {
	return fmt.Sprintf(`
apiVersion: jumper/v1
kind: Plugin
name: lb-pool-members
description: load balancer pool members
target: http
connection: corp-sso
request:
  method: GET
  url: %s/v1/pools/{{ .Param "pool" }}/members
  headers:
    X-Requested-With: jumper
params:
  - name: pool
    required: true
parse:
  type: jq
  expr: '.members[] | {host: .address, state: .state, conns: .active}'
view: { columns: [host, state, conns] }
emits_hosts: host
`, url)
}

func poolMembersBody() string {
	return `{"members":[
		{"address":"web-ewr-001.example.internal","state":"active","active":12},
		{"address":"web-ewr-002.example.internal","state":"drain","active":0}
	]}`
}

// TestHTTPPoolMembersExample runs the lb-pool-members plugin from SPEC §12
// against an httptest server: request, profile, jq parse, rows and display.
func TestHTTPPoolMembersExample(t *testing.T) {
	var (
		mu  sync.Mutex
		got []*http.Request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Clone(context.Background()))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, poolMembersBody())
	}))
	defer srv.Close()

	def := loadTestDef(t, poolMembersYAML(srv.URL))
	if def.Target != TargetHTTP {
		t.Fatalf("target %q", def.Target)
	}
	h, err := NewHTTP(def, testClient(t, "corp-sso"))
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	if h.Name() != "lb-pool-members" || h.Connection() != "corp-sso" || h.Method() != http.MethodGet {
		t.Fatalf("runner = %q/%q/%q", h.Name(), h.Connection(), h.Method())
	}
	if !h.SafeRead() {
		t.Fatal("a GET is a safe read")
	}

	wantURL := srv.URL + "/v1/pools/web/members"
	if got := h.Display(map[string]string{"pool": "web"}); got != "GET "+wantURL {
		t.Fatalf("Display = %q, want %q", got, "GET "+wantURL)
	}

	rows, err := h.Run(context.Background(), map[string]string{"pool": "web"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (%v)", len(rows), rows)
	}
	if rows[0]["host"] != "web-ewr-001.example.internal" || rows[0]["state"] != "active" {
		t.Fatalf("row 0 = %v", rows[0])
	}
	if got := fmt.Sprint(rows[0]["conns"]); got != "12" {
		t.Fatalf("conns = %q, want 12", got)
	}
	if rows[1]["state"] != "drain" {
		t.Fatalf("row 1 = %v", rows[1])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(got))
	}
	if got[0].Method != http.MethodGet || got[0].URL.Path != "/v1/pools/web/members" {
		t.Fatalf("request = %s %s", got[0].Method, got[0].URL.Path)
	}
	if hdr := got[0].Header.Get("X-Requested-With"); hdr != "jumper" {
		t.Fatalf("header = %q, want jumper", hdr)
	}
}

// TestHTTPEscapesURLValues checks that a parameter value cannot add a path
// segment, a query parameter or a fragment.
func TestHTTPEscapesURLValues(t *testing.T) {
	var (
		mu   sync.Mutex
		last *url.URL
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		u := *r.URL
		last = &u
		mu.Unlock()
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	client := testClient(t, "corp-sso")
	cases := []struct {
		name     string
		template string
		value    string
		check    func(t *testing.T, u *url.URL)
	}{
		{
			name:     "path",
			template: srv.URL + "/v1/pools/{{ .Param \"pool\" }}/members",
			value:    "a b/c?d=1&e=2",
			check: func(t *testing.T, u *url.URL) {
				if u.Path != "/v1/pools/a b/c?d=1&e=2/members" {
					t.Fatalf("path = %q; the value added structure", u.Path)
				}
				if u.RawQuery != "" {
					t.Fatalf("raw query = %q, want empty", u.RawQuery)
				}
				if !strings.Contains(u.EscapedPath(), "%2F") {
					t.Fatalf("escaped path = %q, want %%2F", u.EscapedPath())
				}
			},
		},
		{
			name:     "query",
			template: srv.URL + "/v1/search?pool={{ .Param \"pool\" }}&limit=10",
			value:    "a b&c=d#e",
			check: func(t *testing.T, u *url.URL) {
				q := u.Query()
				if got := q.Get("pool"); got != "a b&c=d#e" {
					t.Fatalf("pool = %q", got)
				}
				if got := q.Get("limit"); got != "10" {
					t.Fatalf("limit = %q", got)
				}
				if len(q) != 2 {
					t.Fatalf("query has %d parameters, want 2: %q", len(q), u.RawQuery)
				}
				if u.Fragment != "" {
					t.Fatalf("fragment = %q, want empty", u.Fragment)
				}
			},
		},
		{
			name:     "fragment",
			template: srv.URL + "/v1/doc#{{ .Param \"pool\" }}",
			value:    "a/b",
			check: func(t *testing.T, u *url.URL) {
				// A fragment is not sent to the server; the value must not
				// have escaped into the path or the query.
				if u.Path != "/v1/doc" || u.RawQuery != "" || u.Fragment != "" {
					t.Fatalf("url = %+v", u)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
apiVersion: jumper/v1
kind: Plugin
name: escape-%s
target: http
connection: corp-sso
request:
  method: GET
  url: '%s'
params:
  - name: pool
parse:
  type: json
`, tc.name, tc.template)
			def := loadTestDef(t, yaml)
			h, err := NewHTTP(def, client)
			if err != nil {
				t.Fatalf("NewHTTP: %v", err)
			}
			if _, err := h.Run(context.Background(), map[string]string{"pool": tc.value}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			mu.Lock()
			u := last
			mu.Unlock()
			if u == nil {
				t.Fatal("no request reached the server")
			}
			tc.check(t, u)
		})
	}
}

// TestHTTPUnsafeMethodNeedsTheBus checks that a write never runs as a read and
// that Step is the only path that sends it, with method and URL as its display.
func TestHTTPUnsafeMethodNeedsTheBus(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
		body  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		body = string(b)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	def := Definition{
		Name:       "restart-pool",
		Target:     TargetHTTP,
		Connection: "corp-sso",
		Request: &RequestDef{
			Method: "POST",
			URL:    srv.URL + "/v1/pools/{{ .Param \"pool\" }}/restart",
			Body:   `{"drain":true}`,
		},
		Parse: ParseDef{Type: ParseJSON},
	}
	h, err := NewHTTP(def, testClient(t, "corp-sso"))
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	if h.SafeRead() {
		t.Fatal("POST without safe: true must not be a read")
	}
	params := map[string]string{"pool": "web"}

	_, err = h.Run(context.Background(), params)
	if kind := errs.KindOf(err); kind != errs.Policy {
		t.Fatalf("Run error = %v (kind %q), want policy", err, kind)
	}
	mu.Lock()
	if len(calls) != 0 {
		mu.Unlock()
		t.Fatalf("the read path sent %v", calls)
	}
	mu.Unlock()

	step := h.Step(params)
	if want := "POST " + srv.URL + "/v1/pools/web/restart"; step.Display != want {
		t.Fatalf("Display = %q, want %q", step.Display, want)
	}
	if strings.Contains(step.Display, "drain") || !strings.Contains(step.Display, srv.URL) {
		t.Fatalf("Display is not method + URL: %q", step.Display)
	}
	res := step.Run(context.Background())
	if res.Err != nil {
		t.Fatalf("step: %v", res.Err)
	}
	if res.RC != http.StatusOK {
		t.Fatalf("rc = %d, want 200", res.RC)
	}
	if !strings.Contains(res.Output, `"ok":true`) {
		t.Fatalf("output = %q", res.Output)
	}
	if res.Duration <= 0 {
		t.Fatalf("duration = %v", res.Duration)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "POST /v1/pools/web/restart" {
		t.Fatalf("calls = %v", calls)
	}
	if body != `{"drain":true}` {
		t.Fatalf("body = %q", body)
	}
}

// TestHTTPDeclaredSafeRead checks that a non-GET request the author declared
// safe: true runs as a read, and that :plugins can see the assertion.
func TestHTTPDeclaredSafeRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_, _ = io.WriteString(w, `[{"name":"one"},{"name":"two"}]`)
	}))
	defer srv.Close()

	def := Definition{
		Name:       "search",
		Target:     TargetHTTP,
		Connection: "corp-sso",
		Request: &RequestDef{
			Method: "POST",
			URL:    srv.URL + "/v1/search",
			Body:   `{"q":"*"}`,
			Safe:   true,
		},
		Parse: ParseDef{Type: ParseJSON},
	}
	h, err := NewHTTP(def, testClient(t, "corp-sso"))
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	if !h.SafeRead() {
		t.Fatal("safe: true must make the request readable")
	}
	rows, err := h.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 2 || rows[1]["name"] != "two" {
		t.Fatalf("rows = %v", rows)
	}
}

// TestHTTPClassifiesStatusErrors checks that response failures keep the kind
// httpx assigned them.
func TestHTTPClassifiesStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		want   errs.Kind
	}{
		{http.StatusUnauthorized, errs.Auth},
		{http.StatusForbidden, errs.Auth},
		{http.StatusNotFound, errs.NotAvailable},
		{http.StatusInternalServerError, errs.Remote},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", tc.status)
			}))
			defer srv.Close()
			h := mustHTTP(t, srv.URL, ParseDef{Type: ParseRaw})
			_, err := h.Run(context.Background(), nil)
			if kind := errs.KindOf(err); kind != tc.want {
				t.Fatalf("status %d: kind %q, want %q (%v)", tc.status, kind, tc.want, err)
			}
		})
	}
}

// TestHTTPTimeout checks the request timeout.
func TestHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	h := mustHTTP(t, srv.URL, ParseDef{Type: ParseJSON})
	h.Timeout = 40 * time.Millisecond
	start := time.Now()
	_, err := h.Run(context.Background(), nil)
	if kind := errs.KindOf(err); kind != errs.Timeout {
		t.Fatalf("kind = %q, want timeout (%v)", kind, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Run took %v; the timeout did not apply", d)
	}
}

// TestHTTPCapsResponseBody checks that the body cap is applied.
func TestHTTPCapsResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("a", 100))
	}))
	defer srv.Close()

	h := mustHTTP(t, srv.URL, ParseDef{Type: ParseRaw})
	h.maxBody = 32
	rows, err := h.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	out, _ := rows[0]["output"].(string)
	if len(out) != 32 {
		t.Fatalf("body = %d bytes, want 32", len(out))
	}
	if HTTPMaxBody != int64(httpx.MaxBodyBytes) || HTTPTimeout != 15*time.Second {
		t.Fatalf("caps = %d/%v, want 16 MiB and 15s", HTTPMaxBody, HTTPTimeout)
	}
}

// TestNewHTTPRejectsBadDefinitions covers the validation a plugin author meets
// before any request is made.
func TestNewHTTPRejectsBadDefinitions(t *testing.T) {
	valid := func() Definition {
		return Definition{
			Name:       "bad",
			Target:     TargetHTTP,
			Connection: "corp-sso",
			Request:    &RequestDef{Method: "GET", URL: "https://lb-api.example.internal/v1"},
			Parse:      ParseDef{Type: ParseJSON},
		}
	}
	cases := []struct {
		name   string
		mutate func(*Definition)
		client bool
	}{
		{"wrong target", func(d *Definition) { d.Target = TargetHost }, true},
		{"no request", func(d *Definition) { d.Request = nil }, true},
		{"no client", func(d *Definition) {}, false},
		{"no connection", func(d *Definition) { d.Connection = "" }, true},
		{"wrong profile", func(d *Definition) { d.Connection = "other" }, true},
		{"bad method", func(d *Definition) { d.Request.Method = "TRACE" }, true},
		{"body on GET", func(d *Definition) { d.Request.Body = "x" }, true},
		{"relative url", func(d *Definition) { d.Request.URL = "/v1" }, true},
		{"ftp url", func(d *Definition) { d.Request.URL = "ftp://lb.example.internal/v1" }, true},
		{"header injection", func(d *Definition) {
			d.Request.Headers = map[string]string{"X-Test": "a\r\nX-Evil: b"}
		}, true},
		{"bad template", func(d *Definition) { d.Request.URL = "https://lb.example.internal/{{ .Bogus }}" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := valid()
			tc.mutate(&def)
			var client *httpx.Client
			if tc.client {
				client = testClient(t, "corp-sso")
			}
			h, err := NewHTTP(def, client)
			if err == nil {
				t.Fatalf("NewHTTP accepted the definition: %+v", h)
			}
			if kind := errs.KindOf(err); kind != errs.Config {
				t.Fatalf("kind = %q, want config (%v)", kind, err)
			}
		})
	}
}

// TestRenderURL covers the escaping rules directly.
func TestRenderURL(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		params  map[string]string
		host    string
		want    string
		wantErr bool
	}{
		{
			name:   "literal",
			text:   "https://lb.example.internal/v1/health",
			want:   "https://lb.example.internal/v1/health",
			params: map[string]string{},
		},
		{
			name:   "path value",
			text:   "https://lb.example.internal/v1/pools/{{ .Param \"pool\" }}/members",
			params: map[string]string{"pool": "a/b?c#d"},
			want:   "https://lb.example.internal/v1/pools/a%2Fb%3Fc%23d/members",
		},
		{
			name:   "query value",
			text:   "https://lb.example.internal/v1?pool={{ .Param \"pool\" }}&limit=1",
			params: map[string]string{"pool": "a b&c=d"},
			want:   "https://lb.example.internal/v1?pool=a+b%26c%3Dd&limit=1",
		},
		{
			name:   "host value",
			text:   "http://{{ .Host }}/metrics",
			host:   "web-ewr-001.example.internal:9100",
			want:   "http://web-ewr-001.example.internal:9100/metrics",
			params: map[string]string{},
		},
		{
			name:   "fragment value",
			text:   "https://lb.example.internal/doc#{{ .Param \"anchor\" }}",
			params: map[string]string{"anchor": "a/b"},
			want:   "https://lb.example.internal/doc#a%2Fb",
		},
		{
			name:    "missing param",
			text:    "https://lb.example.internal/v1/{{ .Param \"pool\" }}",
			params:  map[string]string{},
			wantErr: true,
		},
		{
			name:    "bad construct",
			text:    "https://lb.example.internal/{{ .Param \"pool\" | printf \"%s\" }}",
			params:  map[string]string{"pool": "web"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderURL(tc.text, tc.params, tc.host)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("RenderURL = %q, want an error", got)
				}
				if kind := errs.KindOf(err); kind != errs.Config {
					t.Fatalf("kind = %q, want config", kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("RenderURL: %v", err)
			}
			if got != tc.want {
				t.Fatalf("RenderURL = %q, want %q", got, tc.want)
			}
			if _, err := url.Parse(got); err != nil {
				t.Fatalf("result does not parse: %v", err)
			}
		})
	}
}

// mustHTTP builds an HTTP runner for url whose parse is parse.
func mustHTTP(t *testing.T, rawURL string, parse ParseDef) *HTTP {
	t.Helper()
	def := Definition{
		Name:       "test",
		Target:     TargetHTTP,
		Connection: "corp-sso",
		Request:    &RequestDef{Method: "GET", URL: rawURL},
		Parse:      parse,
	}
	h, err := NewHTTP(def, testClient(t, "corp-sso"))
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	return h
}
