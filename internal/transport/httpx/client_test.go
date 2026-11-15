package httpx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jumpr/internal/errs"
)

// newClient builds a client from p with a backoff that does not wait, so retry
// tests stay fast.
func newClient(t *testing.T, p *Profile, deps Deps) *Client {
	t.Helper()
	if p.Name == "" {
		p.Name = "test"
	}
	c, err := p.Client(deps)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.backoff = func(int) time.Duration { return 0 }
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

// get performs a GET and returns the body when the status was fine.
func get(t *testing.T, c *Client, url string) (*http.Response, string, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := c.Do(context.Background(), req)
	if resp == nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 512)
	for {
		n, rerr := resp.Body.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp, b.String(), err
}

func TestClientAuth(t *testing.T) {
	cases := []struct {
		name  string
		auth  Auth
		check func(t *testing.T, h http.Header)
	}{
		{
			name: "basic",
			auth: Auth{Type: AuthBasic, Username: "monitor", Password: "hunter2"},
			check: func(t *testing.T, h http.Header) {
				t.Helper()
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("monitor:hunter2"))
				if got := h.Get("Authorization"); got != want {
					t.Fatalf("Authorization = %q, want %q", got, want)
				}
			},
		},
		{
			name: "bearer",
			auth: Auth{Type: AuthBearer, Token: "tok-123"},
			check: func(t *testing.T, h http.Header) {
				t.Helper()
				if got := h.Get("Authorization"); got != "Bearer tok-123" {
					t.Fatalf("Authorization = %q", got)
				}
			},
		},
		{
			name: "headers",
			auth: Auth{Type: AuthHeaders, Headers: map[string]string{"X-Api-Key": "key-9"}},
			check: func(t *testing.T, h http.Header) {
				t.Helper()
				if got := h.Get("X-Api-Key"); got != "key-9" {
					t.Fatalf("X-Api-Key = %q", got)
				}
			},
		},
		{
			name: "cookie",
			auth: Auth{Type: AuthCookie, Cookie: Cookie{Name: "_oauth2_proxy", Value: "cookie-value", Domain: "127.0.0.1"}},
			check: func(t *testing.T, h http.Header) {
				t.Helper()
				if got := h.Get("Cookie"); got != "_oauth2_proxy=cookie-value" {
					t.Fatalf("Cookie = %q", got)
				}
			},
		},
		{
			name: "none",
			auth: Auth{},
			check: func(t *testing.T, h http.Header) {
				t.Helper()
				if got := h.Get("Authorization"); got != "" {
					t.Fatalf("Authorization = %q, want none", got)
				}
				if got := h.Get("Cookie"); got != "" {
					t.Fatalf("Cookie = %q, want none", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = r.Header.Clone()
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			p := &Profile{Auth: tc.auth}
			c := newClient(t, p, Deps{})
			resp, _, err := get(t, c, srv.URL)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			mu.Lock()
			defer mu.Unlock()
			tc.check(t, seen)
		})
	}
}

func TestClientCookieNotSentToAnotherDomain(t *testing.T) {
	var mu sync.Mutex
	var cookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cookie = r.Header.Get("Cookie")
		mu.Unlock()
	}))
	defer srv.Close()

	p := &Profile{Auth: Auth{Type: AuthCookie, Cookie: Cookie{Name: "c", Value: "v", Domain: ".example.internal"}}}
	c := newClient(t, p, Deps{})
	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if cookie != "" {
		t.Fatalf("cookie was sent to a host outside its domain: %q", cookie)
	}
}

func TestClientExtraHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Requested-By")
	}))
	defer srv.Close()

	p := &Profile{Headers: map[string]string{"X-Requested-By": "jumpr"}}
	c := newClient(t, p, Deps{})
	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "jumpr" {
		t.Fatalf("X-Requested-By = %q", got)
	}
}

// caFile writes the test server's certificate as the profile CA bundle.
func caFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

func TestClientCustomCA(t *testing.T) {
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("secure"))
	}))
	defer srv.Close()

	p := &Profile{TLS: TLSConfig{CAFile: caFile(t, srv)}}
	c := newClient(t, p, Deps{})
	_, body, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("get through the configured CA: %v", err)
	}
	if body != "secure" || hits != 1 {
		t.Fatalf("body = %q, hits = %d", body, hits)
	}
}

func TestClientUnknownCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	p := &Profile{}
	c := newClient(t, p, Deps{})
	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("an unknown CA was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Auth {
		t.Fatalf("kind = %q, want auth (%v)", kind, err)
	}
}

func TestClientInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var warned []string
	p := &Profile{TLS: TLSConfig{InsecureSkipVerify: true}}
	c := newClient(t, p, Deps{Warn: func(msg string) { warned = append(warned, msg) }})
	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get with insecure_skip_verify: %v", err)
	}
	warnings := c.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "insecure_skip_verify") {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(warned) != 1 || warned[0] != warnings[0] {
		t.Fatalf("Deps.Warn got %v, want the same loud warning", warned)
	}
}

func TestClientStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		want   errs.Kind
	}{
		{http.StatusUnauthorized, errs.Auth},
		{http.StatusForbidden, errs.Auth},
		{http.StatusNotFound, errs.NotAvailable},
		{http.StatusRequestTimeout, errs.Timeout},
		{http.StatusInternalServerError, errs.Remote},
		{http.StatusBadGateway, errs.Remote},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			p := &Profile{Auth: Auth{Type: AuthBearer, Token: "sup3rs3cret"}}
			c := newClient(t, p, Deps{})
			resp, _, err := get(t, c, srv.URL)
			if err == nil {
				t.Fatalf("status %d produced no error", tc.status)
			}
			if resp == nil || resp.StatusCode != tc.status {
				t.Fatalf("the response is still readable: %+v", resp)
			}
			if kind := errs.KindOf(err); kind != tc.want {
				t.Fatalf("kind = %q, want %q (%v)", kind, tc.want, err)
			}
		})
	}
}

func TestClientErrorsAreRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	const token = "tok-super-secret-value"
	p := &Profile{
		Name:    "corp",
		Auth:    Auth{Type: AuthBearer, Token: token},
		Headers: map[string]string{"X-Api-Key": "api-key-secret"},
	}
	c := newClient(t, p, Deps{})
	_, _, err := get(t, c, srv.URL+"/?api_key=querys3cret&x=1")
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, secret := range []string{token, "api-key-secret", "querys3cret"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error leaks a credential: %q", msg)
		}
	}
	if !strings.Contains(msg, "corp") && !strings.Contains(msg, "401") {
		t.Fatalf("error = %q, want the status or the profile name", msg)
	}
}

func TestClientTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	p := &Profile{Timeout: Duration(20 * time.Millisecond)}
	c := newClient(t, p, Deps{})
	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("the request did not time out")
	}
	if kind := errs.KindOf(err); kind != errs.Timeout {
		t.Fatalf("kind = %q, want timeout (%v)", kind, err)
	}
}

func TestClientUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := &Profile{Timeout: Duration(2 * time.Second)}
	c := newClient(t, p, Deps{})
	_, _, err = get(t, c, "http://"+addr+"/")
	if err == nil {
		t.Fatal("a closed port was reachable")
	}
	if kind := errs.KindOf(err); kind != errs.Unreachable {
		t.Fatalf("kind = %q, want unreachable (%v)", kind, err)
	}
}

func TestClientRetriesGETOn503(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	p := &Profile{Retries: 2}
	c := newClient(t, p, Deps{})
	resp, body, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestClientDoesNotRetryPOST(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := &Profile{Retries: 2}
	c := newClient(t, p, Deps{})
	var out map[string]any
	err := c.PostJSON(context.Background(), srv.URL, map[string]string{"a": "b"}, &out)
	if err == nil {
		t.Fatal("no error from a 503")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("POST attempts = %d, want 1", got)
	}
	if kind := errs.KindOf(err); kind != errs.Remote {
		t.Fatalf("kind = %q, want remote", kind)
	}
}

func TestClientGetAndPostJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "n": 3})
		case http.MethodPost:
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}
			var in map[string]string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if in["name"] != "jumpr" {
				t.Errorf("request body = %v", in)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "42"})
		}
	}))
	defer srv.Close()

	p := &Profile{}
	c := newClient(t, p, Deps{})

	var out struct {
		OK bool `json:"ok"`
		N  int  `json:"n"`
	}
	if err := c.GetJSON(context.Background(), srv.URL, &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if !out.OK || out.N != 3 {
		t.Fatalf("GetJSON decoded %+v", out)
	}

	var created map[string]string
	if err := c.PostJSON(context.Background(), srv.URL, map[string]string{"name": "jumpr"}, &created); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if created["id"] != "42" {
		t.Fatalf("PostJSON decoded %v", created)
	}
}

func TestClientJSONParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()

	p := &Profile{}
	c := newClient(t, p, Deps{})
	var out map[string]any
	err := c.GetJSON(context.Background(), srv.URL, &out)
	if err == nil {
		t.Fatal("broken JSON was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Parse {
		t.Fatalf("kind = %q, want parse", kind)
	}
}

func TestCappedBody(t *testing.T) {
	body := newCappedBody(io.NopCloser(strings.NewReader(strings.Repeat("x", 100))), 8)
	defer body.Close()
	var b strings.Builder
	for {
		buf := make([]byte, 64)
		n, err := body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if got := b.Len(); got != 8 {
		t.Fatalf("read %d bytes, want the 8-byte cap", got)
	}
}

func TestClientOneTransport(t *testing.T) {
	var conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	p := &Profile{}
	c := newClient(t, p, Deps{})
	for i := 0; i < 3; i++ {
		if _, _, err := get(t, c, srv.URL); err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Fatalf("connections = %d, want 1 reused transport", got)
	}
}

func TestClientHelperAuthBuilds(t *testing.T) {
	p := &Profile{Name: "sso", Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{"true"}}}}
	c, err := p.Client(Deps{})
	if err != nil {
		t.Fatalf("a helper profile must build: %v", err)
	}
	defer c.Close()
	if c.helper == nil {
		t.Fatal("a helper profile has no helper cache")
	}
}
