package app

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConnections writes connections.yaml into dir and returns dir.
func writeConnections(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "connections.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write connections.yaml: %v", err)
	}
	return dir
}

// caFileFor writes the test server's certificate as a CA bundle.
func caFileFor(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

// runDoctor runs DoctorConn against dir and returns its output and result.
func runDoctor(t *testing.T, dir, profile, url string, verbose bool) (string, DoctorResult, error) {
	t.Helper()
	var out strings.Builder
	res, err := DoctorConn(context.Background(), &out, profile, url, DoctorOptions{
		ConfigDir: dir,
		Home:      t.TempDir(),
		Verbose:   verbose,
	})
	return out.String(), res, err
}

func TestDoctorConnSuccess(t *testing.T) {
	const token = "tok-secret-value-123456"
	var gotAuth, gotAccept string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-42")
		w.Header().Set("Set-Cookie", "session=topsecret-cookie-value")
		_, _ = w.Write([]byte(`{"status":"ok","serving":true}`))
	}))
	defer srv.Close()
	ca := caFileFor(t, srv)
	dir := writeConnections(t, fmt.Sprintf(`
connections:
  lab:
    tls:
      ca_file: %q
    auth:
      type: bearer
      token: %q
    headers:
      Accept: application/json
    timeout: 5s
`, ca, token))

	out, res, err := runDoctor(t, dir, "lab", srv.URL, false)
	if err != nil {
		t.Fatalf("DoctorConn: %v\n%s", err, out)
	}
	if !res.OK {
		t.Fatalf("OK = false, status %d\n%s", res.Status, out)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("server saw Authorization %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("server saw Accept %q", gotAccept)
	}

	for _, want := range []string{
		"profile", "lab (valid)",
		"proxy", "none (direct)",
		"dns",
		"connect",
		"TLS 1.", "Acme Co", "configured CA",
		"auth", "bearer",
		"200 OK", "application/json",
		`{"status":"ok"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
	if strings.Count(out, "ok") < 6 || strings.Contains(out, "FAIL") {
		t.Errorf("every step should pass\n%s", out)
	}
	// Nothing sensitive reaches the terminal.
	for _, secret := range []string{token, "topsecret-cookie-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks %q\n%s", secret, out)
		}
	}
	// Response headers only appear with --verbose.
	if strings.Contains(out, "X-Request-Id") {
		t.Errorf("headers leaked without --verbose\n%s", out)
	}

	vout, _, verr := runDoctor(t, dir, "lab", srv.URL, true)
	if verr != nil {
		t.Fatalf("verbose DoctorConn: %v\n%s", verr, vout)
	}
	if !strings.Contains(vout, "X-Request-Id: req-42") {
		t.Errorf("--verbose does not show the response headers\n%s", vout)
	}
	if !strings.Contains(vout, "Set-Cookie") || strings.Contains(vout, "topsecret-cookie-value") {
		t.Errorf("--verbose must show Set-Cookie redacted\n%s", vout)
	}
}

func TestDoctorConnRedirectToLogin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>please sign in</html>"))
			return
		}
		http.Redirect(w, r, "/login?next=%2F", http.StatusFound)
	}))
	defer srv.Close()
	dir := writeConnections(t, fmt.Sprintf(`
connections:
  lab:
    tls:
      ca_file: %q
`, caFileFor(t, srv)))

	out, res, err := runDoctor(t, dir, "lab", srv.URL, false)
	if err != nil {
		t.Fatalf("DoctorConn: %v\n%s", err, out)
	}
	if res.OK {
		t.Fatalf("a login redirect must not count as ok\n%s", out)
	}
	if !strings.Contains(out, "redirected to login: auth not accepted") {
		t.Errorf("output does not name the login redirect\n%s", out)
	}
	if !strings.Contains(out, "302") || !strings.Contains(out, "/login") {
		t.Errorf("output does not show the redirect target\n%s", out)
	}
}

func TestDoctorConnHelper(t *testing.T) {
	t.Setenv("JUMPR_DOCTOR_HELPER", "1")
	var sawHeader, sawCookie bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("Authorization") == "Bearer helper-secret-token"
		c, err := r.Cookie("_oauth2_proxy")
		sawCookie = err == nil && c.Value == "helper-cookie-value"
		if !sawHeader || !sawCookie {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("authenticated"))
	}))
	defer srv.Close()
	dir := writeConnections(t, fmt.Sprintf(`
connections:
  sso:
    tls:
      ca_file: %q
    auth:
      type: helper
      helper:
        command: [%q, "-test.run=^TestDoctorConnHelperProcess$"]
`, caFileFor(t, srv), os.Args[0]))

	out, res, err := runDoctor(t, dir, "sso", srv.URL, false)
	if err != nil {
		t.Fatalf("DoctorConn: %v\n%s", err, out)
	}
	if !res.OK {
		t.Fatalf("OK = false, status %d\n%s", res.Status, out)
	}
	if !sawHeader || !sawCookie {
		t.Fatalf("the helper's credentials did not reach the server: header=%v cookie=%v\n%s",
			sawHeader, sawCookie, out)
	}
	for _, want := range []string{
		"auth", "helper",
		"exit", "0",
		"headers", "Authorization",
		"cookies", "_oauth2_proxy",
		"expires", "2026-11-12T18:00:00Z",
		"200 OK", "authenticated",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
	for _, secret := range []string{"helper-secret-token", "helper-cookie-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks %q\n%s", secret, out)
		}
	}
}

// TestDoctorConnHelperProcess is not a real test: it is the auth helper the
// TestDoctorConnHelper profile runs, re-executing the test binary on demand.
func TestDoctorConnHelperProcess(t *testing.T) {
	if os.Getenv("JUMPR_DOCTOR_HELPER") != "1" {
		t.Skip("auth helper subprocess only")
	}
	fmt.Print(`{"headers":{"Authorization":"Bearer helper-secret-token"},` +
		`"cookies":[{"name":"_oauth2_proxy","value":"helper-cookie-value","domain":".example.internal"}],` +
		`"expires_at":"2026-11-12T18:00:00Z"}`)
	os.Exit(0)
}

// TestDoctorConnHelperFailure is not a real test: it is the failing auth
// helper the TestDoctorConnHelperFailure profile runs, re-executing the test
// binary on demand. It exits non-zero after printing a JSON doc, so the probe
// must not accept the run.
func TestDoctorConnHelperProcessFail(t *testing.T) {
	if os.Getenv("JUMPR_DOCTOR_HELPER_FAIL") != "1" {
		t.Skip("failing auth helper subprocess only")
	}
	fmt.Print(`{"headers":{"Authorization":"Bearer never-used"}}`)
	fmt.Fprint(os.Stderr, "the login window was closed")
	os.Exit(3)
}

func TestDoctorConnHelperFailure(t *testing.T) {
	t.Setenv("JUMPR_DOCTOR_HELPER_FAIL", "1")
	var sawAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("anonymous ok"))
	}))
	defer srv.Close()
	dir := writeConnections(t, fmt.Sprintf(`
connections:
  sso:
    tls:
      ca_file: %q
    auth:
      type: helper
      helper:
        command: [%q, "-test.run=^TestDoctorConnHelperProcessFail$"]
`, caFileFor(t, srv), os.Args[0]))

	out, res, err := runDoctor(t, dir, "sso", srv.URL, false)
	if err != nil {
		t.Fatalf("DoctorConn: %v\n%s", err, out)
	}
	if res.OK {
		t.Fatalf("a failed helper must not count as ok, even on a 2xx\n%s", out)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want the 200 the server sent", res.Status)
	}
	if sawAuth != "" {
		t.Fatalf("the failed helper's credentials reached the server: %q", sawAuth)
	}
	for _, want := range []string{"helper", "FAIL", "exit", "3", "login window was closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
}

// TestDoctorConnUnfollowedRedirect covers the helper-profile client of
// SPEC §7.2, which stops at a redirect so the helper can be refreshed: the
// final response is the 3xx itself and its Location must still be reported and
// recognised as a login page.
func TestDoctorConnUnfollowedRedirect(t *testing.T) {
	orig, err := url.Parse("https://wiki.example.internal/page")
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": {"/login?next=%2Fpage"}},
		Request:    &http.Request{URL: orig},
	}
	status, target := redirectInfo(resp, orig)
	if status != http.StatusFound {
		t.Fatalf("status = %d, want 302", status)
	}
	if target != "https://wiki.example.internal/login?next=%2Fpage" {
		t.Fatalf("target = %q", target)
	}
	if !looksLikeLogin(target, orig) {
		t.Fatalf("%q must be recognised as a login redirect", target)
	}
}

func TestDoctorConnStatusFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	dir := writeConnections(t, fmt.Sprintf(`
connections:
  lab:
    tls:
      ca_file: %q
`, caFileFor(t, srv)))

	out, res, err := runDoctor(t, dir, "lab", srv.URL, false)
	if err != nil {
		t.Fatalf("DoctorConn: %v\n%s", err, out)
	}
	if res.OK {
		t.Fatalf("a 500 must not count as ok\n%s", out)
	}
	if res.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.Status)
	}
	if !strings.Contains(out, "500 Internal Server Error") || !strings.Contains(out, "FAIL") {
		t.Errorf("output does not report the failure\n%s", out)
	}
}

func TestDoctorConnUnknownProfile(t *testing.T) {
	dir := writeConnections(t, "connections:\n  lab:\n    timeout: 5s\n")

	out, res, err := runDoctor(t, dir, "nope", "https://example.internal/", false)
	if err == nil {
		t.Fatalf("an unknown profile must fail\n%s", out)
	}
	if !strings.Contains(err.Error(), "no profile") {
		t.Fatalf("error = %v, want it to name the missing profile", err)
	}
	if res.OK {
		t.Fatal("OK = true for a missing profile")
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("output does not report the failure\n%s", out)
	}
}

func TestDoctorConnBadURL(t *testing.T) {
	dir := writeConnections(t, "connections:\n  lab:\n    timeout: 5s\n")
	if _, _, err := runDoctor(t, dir, "lab", "example.internal/no-scheme", false); err == nil {
		t.Fatal("a URL without a scheme must be rejected")
	}
}
