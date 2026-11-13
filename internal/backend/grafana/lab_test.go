//go:build lab

// M11 lab tests: a lab dashboard's panels render through /api/ds/query behind
// the mock SSO proxy (task 122).
//
// These tests are ordinary Go tests behind the `lab` build tag. They skip with
// "lab observability stack not running" unless the lab is up, lab/jumper names
// a Grafana server, the `lab-sso` auth helper is built, the server accepts a
// TCP connection and JUMPER_AUTH_COOKIE holds the cookie the mock proxy hands
// out. Nothing here runs a proxy: the mock SSO proxy belongs to the lab track
// (SPEC §22).
//
// The contract these tests expect from the lab stack is documented in
// README.md ("Lab observability stack"):
//
//   - Grafana answers /api/search, /api/dashboards/uid/<uid> and
//     /api/ds/query through the proxy, and at least one of its dashboards has a
//     timeseries panel with a datasource query;
//   - that datasource reaches the lab's Prometheus, so a panel over the last
//     fifteen minutes has samples.
package grafana

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumper/internal/config"
	"jumper/internal/labtest"
	"jumper/internal/redact"
	"jumper/internal/transport/httpx"
)

const (
	// labSkip is the message every test in this file uses when the stack is
	// not up (task 104, requirement 1).
	labSkip = "lab observability stack not running"
	// labProfile is the connection profile the lab's stack uses.
	labProfile = "lab-sso"
	// labCookieVar is what examples/auth-helper-cookie reads.
	labCookieVar = "JUMPER_AUTH_COOKIE"
	// labDashboards is how many search hits are opened before giving up on
	// finding a renderable timeseries panel.
	labDashboards = 10
	// labPanelWindow is the range a panel is queried over: wide enough that a
	// 15s-scrape dashboard has samples even on a quiet lab.
	labPanelWindow = 15 * time.Minute
	// labMaxDataPoints is the data-point budget of the panel query.
	labMaxDataPoints = 100
)

// labStack is what one test needs from the lab's Grafana.
type labStack struct {
	entry  config.GrafanaConfig
	client *Client
}

// loadLabStack loads lab/jumper, moves to the repository root (the profile's
// helper path is relative to it) and skips unless the stack is up.
func loadLabStack(t *testing.T) *labStack {
	t.Helper()
	cfg := labtest.Config(t) // skips when the lab is down, chdirs to the root
	root := labtest.Root(t)

	if len(cfg.Grafana) == 0 {
		t.Skipf("%s: lab/jumper/config.yaml configures no grafana server", labSkip)
	}
	entry := cfg.Grafana[0]

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("lab: home directory: %v", err)
	}
	profiles, err := config.LoadConnections(filepath.Join("lab", "jumper", "connections.yaml"), home)
	if err != nil {
		t.Fatalf("lab: connections: %v", err)
	}
	p, ok := profiles[labProfile]
	if !ok || p == nil {
		t.Skipf("%s: lab/jumper/connections.yaml has no %q profile", labSkip, labProfile)
	}
	if p.Auth.Helper == nil || len(p.Auth.Helper.Command) == 0 {
		t.Fatalf("lab: profile %q has no auth helper", labProfile)
	}
	helper := p.Auth.Helper.Command[0]
	if !filepath.IsAbs(helper) {
		helper = filepath.Join(root, helper)
	}
	if _, err := os.Stat(helper); err != nil {
		t.Skipf("%s: build the auth helper first: go build -o lab/.keys/auth-helper ./examples/auth-helper-cookie", labSkip)
	}
	if err := labReachable(entry.URL); err != nil {
		t.Skipf("%s: %s is not reachable: %v", labSkip, entry.URL, err)
	}

	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("lab: redactor: %v", err)
	}
	deps := httpx.Deps{Env: labEnv(), Home: home, Redactor: red}

	hc, err := p.Client(deps)
	if err != nil {
		t.Fatalf("lab: profile %q: %v", labProfile, err)
	}
	t.Cleanup(func() { _ = hc.Close() })

	client, err := NewClient(ClientOptions{URL: entry.URL, Name: entry.Name, Client: hc})
	if err != nil {
		t.Fatalf("lab: grafana client: %v", err)
	}
	return &labStack{entry: entry, client: client}
}

// labEnv returns the process environment as a map, without touching it.
func labEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// labReachable reports whether the URL's host accepts a TCP connection. It is
// the cheapest honest answer to "is the stack up"; the proxy answers nothing
// without a cookie.
func labReachable(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// labCookie skips when no cookie was exported: without it the stack cannot be
// authenticated at all, which is the same as the stack being absent.
func labCookie(t *testing.T) string {
	t.Helper()
	cookie := strings.TrimSpace(os.Getenv(labCookieVar))
	if cookie == "" {
		t.Skipf("%s: export %s to the cookie the lab mock SSO proxy hands out", labSkip, labCookieVar)
	}
	return cookie
}

// TestLabGrafanaTimeseriesPanelRenders proves a lab dashboard's timeseries panel
// renders through /api/ds/query: at least one panel comes back with samples,
// through the `lab-sso` profile.
func TestLabGrafanaTimeseriesPanelRenders(t *testing.T) {
	stack := loadLabStack(t)
	labCookie(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	hits, err := stack.client.Search(ctx, "")
	if err != nil {
		t.Fatalf("lab: /api/search through %q: %v", labProfile, err)
	}
	if len(hits) == 0 {
		t.Fatalf("lab: grafana %s has no dashboard to render", stack.entry.URL)
	}
	if len(hits) > labDashboards {
		hits = hits[:labDashboards]
	}

	to := time.Now()
	from := to.Add(-labPanelWindow)
	var tried []string
	for _, hit := range hits {
		dash, err := stack.client.Dashboard(ctx, hit.UID)
		if err != nil {
			t.Fatalf("lab: dashboard %q: %v", hit.UID, err)
		}
		panels := labTimeseriesPanels(dash.Panels)
		if len(panels) == 0 {
			tried = append(tried, fmt.Sprintf("%s: no timeseries panel", dash.Title))
			continue
		}
		iv := NewInterpolation(dash.QueryVars(), from, to, labMaxDataPoints)
		for _, panel := range panels {
			series, err := stack.client.QueryPanel(ctx, panel, iv, from, to, labMaxDataPoints)
			if err != nil {
				t.Fatalf("lab: panel %q of dashboard %q (/api/ds/query): %v", panel.Title, dash.Title, err)
			}
			points := 0
			for _, s := range series {
				points += len(s.Points)
			}
			if points == 0 {
				tried = append(tried, fmt.Sprintf("%s/%s: no samples", dash.Title, panel.Title))
				continue
			}
			t.Logf("lab: dashboard %q panel %q rendered %d sample(s) in %d series",
				dash.Title, panel.Title, points, len(series))
			return
		}
	}
	t.Fatalf("no timeseries panel of grafana %s rendered samples through /api/ds/query; tried: %s",
		stack.entry.URL, strings.Join(tried, "; "))
}

// labTimeseriesPanels flattens the panels of a dashboard into the timeseries
// panels that carry at least one target, walking row containers too.
func labTimeseriesPanels(panels []Panel) []Panel {
	var out []Panel
	var walk func([]Panel)
	walk = func(ps []Panel) {
		for _, p := range ps {
			if p.IsRow() {
				walk(p.Panels)
				continue
			}
			if strings.EqualFold(p.Type, "timeseries") && len(p.Targets) > 0 {
				out = append(out, p)
			}
			if len(p.Panels) > 0 {
				walk(p.Panels)
			}
		}
	}
	walk(panels)
	return out
}
