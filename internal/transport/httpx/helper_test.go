package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
)

// TestMain doubles as the fake auth helper: when the environment says so, the
// test binary behaves like a helper instead of running the tests. That keeps
// the helper a real process without a script file or a second build.
func TestMain(m *testing.M) {
	if mode := os.Getenv("HTTPX_HELPER_MODE"); mode != "" {
		os.Exit(runFakeHelper(mode))
	}
	os.Exit(m.Run())
}

// runFakeHelper is the helper's behaviour, selected by HTTPX_HELPER_MODE.
func runFakeHelper(mode string) int {
	runs := bumpCounter(os.Getenv("HTTPX_HELPER_COUNTER"))
	switch mode {
	case "json":
		headers := map[string]string{
			"Authorization": "Bearer tok-" + strconv.Itoa(runs),
			"X-Helper-Run":  strconv.Itoa(runs),
		}
		if len(os.Args) > 1 {
			headers["X-Helper-Arg"] = os.Args[1]
		}
		doc := map[string]any{
			"headers": headers,
			"cookies": []map[string]string{{
				"name":   "_oauth2_proxy",
				"value":  "cv-" + strconv.Itoa(runs),
				"domain": os.Getenv("HTTPX_HELPER_COOKIE_DOMAIN"),
			}},
		}
		if exp := os.Getenv("HTTPX_HELPER_EXPIRES"); exp != "" {
			if d, err := time.ParseDuration(exp); err == nil {
				doc["expires_at"] = time.Now().Add(d).UTC().Format(time.RFC3339)
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(doc); err != nil {
			return 1
		}
		return 0
	case "bad":
		fmt.Fprint(os.Stdout, "<html>definitely not json</html>")
		return 0
	case "fail":
		fmt.Fprintln(os.Stderr, "sso-login: could not reach the IdP (token=leak-me-1234)")
		return 3
	case "sleep":
		time.Sleep(10 * time.Second)
		return 0
	case "print":
		fmt.Fprint(os.Stdout, os.Getenv("HTTPX_HELPER_VALUE"))
		return 0
	case "keychain":
		// Like `security find-generic-password -a <name>`: the name follows
		// -a; without it, the name is the last argument.
		name := os.Args[len(os.Args)-1]
		for i := 1; i+1 < len(os.Args); i++ {
			if os.Args[i] == "-a" {
				name = os.Args[i+1]
				break
			}
		}
		fmt.Fprint(os.Stdout, "keychain-"+name)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	return 2
}

// bumpCounter appends one byte to path and returns the new size: the number of
// helper runs so far.
func bumpCounter(path string) int {
	if path == "" {
		return 0
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return 0
	}
	defer f.Close()
	if _, err := f.WriteString("x"); err != nil {
		return 0
	}
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	return int(fi.Size())
}

// helperRuns counts how many times the fake helper ran in one test.
type helperRuns struct{ path string }

// newHelperRuns installs a counter file for the fake helper.
func newHelperRuns(t *testing.T) *helperRuns {
	t.Helper()
	h := &helperRuns{path: filepath.Join(t.TempDir(), "runs")}
	t.Setenv("HTTPX_HELPER_COUNTER", h.path)
	return h
}

// count returns the number of helper runs so far.
func (h *helperRuns) count() int {
	fi, err := os.Stat(h.path)
	if err != nil {
		return 0
	}
	return int(fi.Size())
}

// setHelperMode selects what the fake helper does.
func setHelperMode(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("HTTPX_HELPER_MODE", mode)
}

// newHelperClient builds a helper-profile client named "sso".
func newHelperClient(t *testing.T, p *Profile, deps Deps) *Client {
	t.Helper()
	if p.Name == "" {
		p.Name = "sso"
	}
	if p.Auth.Type == "" {
		p.Auth.Type = AuthHelper
	}
	return newClient(t, p, deps)
}

// headerRecorder records the headers and path of every request.
type headerRecorder struct {
	mu     sync.Mutex
	header http.Header
	path   string
}

func (r *headerRecorder) record(req *http.Request) {
	r.mu.Lock()
	r.header = req.Header.Clone()
	r.path = req.URL.Path
	r.mu.Unlock()
}

func (r *headerRecorder) get() (http.Header, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.header, r.path
}

func TestHelperAppliesJSON(t *testing.T) {
	rec := &headerRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{
		Command: []string{os.Args[0], "{{ .URL }}"},
	}}}
	c := newHelperClient(t, p, Deps{})

	if _, body, err := get(t, c, srv.URL+"/api"); err != nil {
		t.Fatalf("get: %v", err)
	} else if body != "ok" {
		t.Fatalf("body = %q", body)
	}
	h, _ := rec.get()
	if got := h.Get("Authorization"); got != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want the helper's header", got)
	}
	if got := h.Get("Cookie"); !strings.Contains(got, "_oauth2_proxy=cv-1") {
		t.Fatalf("Cookie = %q, want the helper's cookie", got)
	}
	if got, want := h.Get("X-Helper-Arg"), srv.URL+"/api"; got != want {
		t.Fatalf("{{ .URL }} rendered %q, want %q", got, want)
	}
	if runs.count() != 1 {
		t.Fatalf("helper runs = %d, want 1", runs.count())
	}
}

func TestHelperArgvExpandsHomeAndProfile(t *testing.T) {
	home := t.TempDir()
	p := &Profile{Name: "sso", Auth: Auth{Type: AuthHelper, Helper: &Helper{
		Command: []string{"~/helpers/sso-login", "{{ .Profile }}", "plain"},
	}}}
	c := newHelperClient(t, p, Deps{Home: home})

	argv, err := c.helperArgv(nil)
	if err != nil {
		t.Fatalf("helperArgv: %v", err)
	}
	if want := filepath.Join(home, "helpers", "sso-login"); argv[0] != want {
		t.Fatalf("argv[0] = %q, want %q", argv[0], want)
	}
	if argv[1] != "sso" {
		t.Fatalf("{{ .Profile }} rendered %q", argv[1])
	}
	if argv[2] != "plain" {
		t.Fatalf("a plain argument changed: %q", argv[2])
	}
}

// TestHelperArgvNoShell checks that the helper is executed directly: an
// argument full of shell syntax reaches the helper byte for byte.
func TestHelperArgvNoShell(t *testing.T) {
	rec := &headerRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
	}))
	defer srv.Close()

	newHelperRuns(t)
	setHelperMode(t, "json")
	payload := "a; echo pwned `id`"
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{
		Command: []string{os.Args[0], payload},
	}}}
	c := newHelperClient(t, p, Deps{})

	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	h, _ := rec.get()
	if got := h.Get("X-Helper-Arg"); got != payload {
		t.Fatalf("helper argument = %q, want %q (a shell would have split it)", got, payload)
	}
}

func TestHelperCached(t *testing.T) {
	var hits int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
	c := newHelperClient(t, p, Deps{})

	for i := 0; i < 3; i++ {
		if _, _, err := get(t, c, srv.URL); err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
	}
	if runs.count() != 1 {
		t.Fatalf("helper runs = %d, want 1 cached run", runs.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 3 {
		t.Fatalf("requests = %d, want 3", hits)
	}
}

func TestHelperCacheTTLExpires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{
		Command:  []string{os.Args[0]},
		CacheTTL: Duration(20 * time.Millisecond),
	}}}
	c := newHelperClient(t, p, Deps{})

	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, _, err := get(t, c, srv.URL); err != nil {
		t.Fatalf("get after the ttl: %v", err)
	}
	if runs.count() != 2 {
		t.Fatalf("helper runs = %d, want 2 after cache_ttl", runs.count())
	}
}

func TestHelperExpiresAtWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	t.Setenv("HTTPX_HELPER_EXPIRES", "-1s")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{
		Command:  []string{os.Args[0]},
		CacheTTL: Duration(time.Hour),
	}}}
	c := newHelperClient(t, p, Deps{})

	for i := 0; i < 2; i++ {
		if _, _, err := get(t, c, srv.URL); err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
	}
	if runs.count() != 2 {
		t.Fatalf("helper runs = %d, want 2 for an expired expires_at", runs.count())
	}
}

func TestHelperRefreshesOn401Once(t *testing.T) {
	var mu sync.Mutex
	var hits int
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
	c := newHelperClient(t, p, Deps{})

	_, body, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("a 401 must be refreshed, not returned: %v", err)
	}
	if body != "ok" {
		t.Fatalf("body = %q", body)
	}
	if runs.count() != 2 {
		t.Fatalf("helper runs = %d, want exactly one refresh", runs.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 2 {
		t.Fatalf("requests = %d, want the request retried once", hits)
	}
	if len(auths) != 2 || auths[0] != "Bearer tok-1" || auths[1] != "Bearer tok-2" {
		t.Fatalf("credentials per attempt = %v", auths)
	}
}

func TestHelperRefreshGivesUpWithAuthError(t *testing.T) {
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
	c := newHelperClient(t, p, Deps{})

	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("a twice-stale session must be an error")
	}
	if kind := errs.KindOf(err); kind != errs.Auth {
		t.Fatalf("kind = %q, want auth (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), `"sso"`) {
		t.Fatalf("the error does not name the profile: %v", err)
	}
	if runs.count() != 2 {
		t.Fatalf("helper runs = %d, want the helper run once for the refresh", runs.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 2 {
		t.Fatalf("requests = %d, want the request retried exactly once", hits)
	}
}

func TestHelperBadJSONIsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	newHelperRuns(t)
	setHelperMode(t, "bad")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
	c := newHelperClient(t, p, Deps{})

	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("bad helper JSON was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Auth {
		t.Fatalf("kind = %q, want auth (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), `"sso"`) {
		t.Fatalf("the error does not name the profile: %v", err)
	}
	if strings.Contains(err.Error(), "definitely not json") {
		t.Fatalf("stdout leaked into the error: %v", err)
	}
}

func TestHelperFailureShowsRedactedStderr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	newHelperRuns(t)
	setHelperMode(t, "fail")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
	c := newHelperClient(t, p, Deps{})

	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("a failing helper was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Auth {
		t.Fatalf("kind = %q, want auth (%v)", kind, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not reach the IdP") {
		t.Fatalf("the dialog text is missing the helper's stderr: %v", msg)
	}
	if strings.Contains(msg, "leak-me-1234") {
		t.Fatalf("stderr leaked a credential: %v", msg)
	}
	if stderr := c.HelperStderr(); !strings.Contains(stderr, "could not reach") {
		t.Fatalf("HelperStderr() = %q", stderr)
	} else if strings.Contains(stderr, "leak-me-1234") {
		t.Fatalf("HelperStderr() leaked a credential: %q", stderr)
	}
}

func TestHelperTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	newHelperRuns(t)
	setHelperMode(t, "sleep")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
	c := newHelperClient(t, p, Deps{})
	c.helper.timeout = 100 * time.Millisecond

	start := time.Now()
	_, _, err := get(t, c, srv.URL)
	if err == nil {
		t.Fatal("a helper that never returns was accepted")
	}
	if kind := errs.KindOf(err); kind != errs.Timeout {
		t.Fatalf("kind = %q, want timeout (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), `"sso"`) {
		t.Fatalf("the error does not name the profile: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the helper timeout was not enforced: %s", elapsed)
	}
}

func TestHelperCookiesScopedByDomain(t *testing.T) {
	t.Run("outside the domain", func(t *testing.T) {
		rec := &headerRecorder{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record(r)
		}))
		defer srv.Close()

		newHelperRuns(t)
		setHelperMode(t, "json")
		t.Setenv("HTTPX_HELPER_COOKIE_DOMAIN", ".example.internal")
		p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
		c := newHelperClient(t, p, Deps{})

		if _, _, err := get(t, c, srv.URL); err != nil {
			t.Fatalf("get: %v", err)
		}
		h, _ := rec.get()
		if got := h.Get("Cookie"); got != "" {
			t.Fatalf("cookie was sent outside its domain: %q", got)
		}
		if got := h.Get("Authorization"); got != "Bearer tok-1" {
			t.Fatalf("headers are not domain-scoped: %q", got)
		}
	})

	t.Run("inside the domain", func(t *testing.T) {
		rec := &headerRecorder{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record(r)
		}))
		defer srv.Close()

		newHelperRuns(t)
		setHelperMode(t, "json")
		t.Setenv("HTTPX_HELPER_COOKIE_DOMAIN", "127.0.0.1")
		p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
		c := newHelperClient(t, p, Deps{})

		if _, _, err := get(t, c, srv.URL); err != nil {
			t.Fatalf("get: %v", err)
		}
		if h, _ := rec.get(); !strings.Contains(h.Get("Cookie"), "_oauth2_proxy=cv-1") {
			t.Fatalf("Cookie = %q", h.Get("Cookie"))
		}
	})
}

func TestHelperConcurrentSingleRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{Command: []string{os.Args[0]}}}}
	c := newHelperClient(t, p, Deps{})

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			if err != nil {
				errCh <- err
				return
			}
			resp, err := c.Do(context.Background(), req)
			if err != nil {
				errCh <- err
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent get: %v", err)
	}
	if runs.count() != 1 {
		t.Fatalf("helper runs = %d, want one shared run", runs.count())
	}
}

func TestHelperRedirectToLoginRefreshes(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		hits++
		n := hits
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Location", "/login?next=%2Fapi")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{
		Command:   []string{os.Args[0]},
		RefreshOn: []string{"401", "403", "302-to-login"},
	}}}
	c := newHelperClient(t, p, Deps{})

	_, body, err := get(t, c, srv.URL+"/api")
	if err != nil {
		t.Fatalf("a redirect to login must be refreshed, not returned: %v", err)
	}
	if body != "ok" {
		t.Fatalf("body = %q", body)
	}
	if runs.count() != 2 {
		t.Fatalf("helper runs = %d, want exactly one refresh", runs.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("requests = %v, want two", paths)
	}
	for _, p := range paths {
		if p == "/login" {
			t.Fatalf("the client followed the login redirect: %v", paths)
		}
	}
}

func TestHelperIgnoresUnmatchedRedirect(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Location", "/moved")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	runs := newHelperRuns(t)
	setHelperMode(t, "json")
	p := &Profile{Auth: Auth{Type: AuthHelper, Helper: &Helper{
		Command:   []string{os.Args[0]},
		RefreshOn: []string{"302-to-login"},
	}}}
	c := newHelperClient(t, p, Deps{})

	resp, _, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatalf("an unrelated redirect must not be an error: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the redirect itself", resp.StatusCode)
	}
	if runs.count() != 1 {
		t.Fatalf("helper runs = %d, want 1", runs.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("requests = %d, want 1", hits)
	}
}
