//go:build lab

// M11 lab tests: the `:logs` search and the `w` live tail against the lab's
// Elasticsearch behind the mock SSO proxy (task 122).
//
// These tests are ordinary Go tests behind the `lab` build tag. They skip with
// "lab observability stack not running" unless the lab is up, lab/jumpr names
// an Elasticsearch cluster with at least one log source, the `lab-sso` auth
// helper is built, the cluster accepts a TCP connection and
// JUMPR_AUTH_COOKIE holds the cookie the mock proxy hands out. Nothing here
// runs a proxy or a log shipper: both belong to the lab track (SPEC §22).
//
// The contract these tests expect from the lab stack is documented in
// README.md ("Lab observability stack"):
//
//   - Elasticsearch serves the configured log source's index pattern behind the
//     mock SSO proxy, with the ECS-ish field names of lab/jumpr/config.yaml;
//   - the nginx of a faulted web host writes a line for the `haproxy-443` fault
//     (nginx cannot bind :443), and the lab ships its logs into Elasticsearch.
//
// Both tests drive jumpr's packages, not the TUI: the backend built from the
// same config entry and connection profile the app builds, and the same
// resource.Query `:logs` sends.
package elastic

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

	"jumpr/internal/backend"
	"jumpr/internal/config"
	"jumpr/internal/labtest"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/transport/httpx"
)

const (
	// labSkip is the message every test in this file uses when the stack is
	// not up (task 104, requirement 1).
	labSkip = "lab observability stack not running"
	// labProfile is the connection profile the lab's stack uses.
	labProfile = "lab-sso"
	// labCookieVar is what examples/auth-helper-cookie reads.
	labCookieVar = "JUMPR_AUTH_COOKIE"
	// labFault is the fault that makes a web host's nginx fail.
	labFault = "haproxy-443"
	// labLogWait is how long a line may take to reach the index: the shipper's
	// poll plus Elasticsearch's refresh.
	labLogWait = 3 * time.Minute
	// labPollStep is how often the tests ask again while waiting.
	labPollStep = 5 * time.Second
	// labSearchWindow is how far back the fault's lines are looked for.
	labSearchWindow = 10 * time.Minute
)

// labDomain is the suffix of every lab host (SPEC §22).
const labDomain = ".lab.example.internal"

// labWebHosts are the web hosts of the lab; the fault touches web-01 and
// web-02, and any of the three is a web host a log line may name.
var labWebHosts = map[resource.HostID]bool{
	resource.HostID("web-01" + labDomain): true,
	resource.HostID("web-02" + labDomain): true,
	resource.HostID("web-03" + labDomain): true,
}

// labFaultHost is the host the search and the tail are aimed at.
const labFaultHost = resource.HostID("web-01" + labDomain)

// labStack is what one test needs from the lab's Elasticsearch.
type labStack struct {
	entry   config.ElasticsearchConfig
	backend *Backend
}

// loadLabStack loads lab/jumpr, moves to the repository root (the profile's
// helper path is relative to it) and skips unless the stack is up.
func loadLabStack(t *testing.T) *labStack {
	t.Helper()
	cfg := labtest.Config(t) // skips when the lab is down, chdirs to the root
	root := labtest.Root(t)

	if len(cfg.Elasticsearch) == 0 {
		t.Skipf("%s: lab/jumpr/config.yaml configures no elasticsearch cluster", labSkip)
	}
	entry := cfg.Elasticsearch[0]
	if len(entry.Sources) == 0 {
		t.Skipf("%s: elasticsearch cluster %q has no log source", labSkip, entry.Name)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("lab: home directory: %v", err)
	}
	profiles, err := config.LoadConnections(filepath.Join("lab", "jumpr", "connections.yaml"), home)
	if err != nil {
		t.Fatalf("lab: connections: %v", err)
	}
	p, ok := profiles[labProfile]
	if !ok || p == nil {
		t.Skipf("%s: lab/jumpr/connections.yaml has no %q profile", labSkip, labProfile)
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
		t.Fatalf("lab: elasticsearch client: %v", err)
	}
	sources := make([]LogSource, 0, len(entry.Sources))
	for _, src := range entry.Sources {
		sources = append(sources, LogSource{
			Name:         src.Name,
			Index:        src.Index,
			TimeField:    src.TimeField,
			HostField:    src.HostField,
			MessageField: src.MessageField,
			LevelField:   src.LevelField,
			UnitField:    src.UnitField,
		})
	}
	b := New(Options{
		Clusters: []Cluster{{Name: entry.Name, Client: client, Sources: sources}},
		Redact:   red.String,
	})
	return &labStack{entry: entry, backend: b}
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

// sampleSource is the first configured source, which the query below searches.
func (s *labStack) sampleSource() string {
	return s.entry.Sources[0].Name
}

// list drains the backend's List into rows, one query at a time.
func list(t *testing.T, b backend.Backend, q resource.Query) []resource.Resource {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ch, err := b.List(ctx, q)
	if err != nil {
		t.Fatalf("lab: list %s: %v", q.Kind, err)
	}
	var out []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("lab: list %s: %v", q.Kind, res.Err)
		}
		out = append(out, res.Items...)
	}
	return out
}

// TestLabESLogsReturnFaultedWebHost proves the `:logs` search returns the nginx
// lines a web host wrote when the haproxy-443 fault took its port, inside the
// fault's window, through the `lab-sso` profile.
func TestLabESLogsReturnFaultedWebHost(t *testing.T) {
	stack := loadLabStack(t)
	labCookie(t)

	from := time.Now().Add(-time.Minute)
	labtest.Fault(t, labFault)

	q := resource.Query{
		Kind:   KindLog,
		Hosts:  []resource.HostID{labFaultHost},
		Args:   map[string]string{ArgSource: stack.sampleSource()},
		Window: resource.TimeWindow{From: from, ToNow: true, Label: "-10m"},
	}

	var rows []resource.Resource
	deadline := time.Now().Add(labLogWait)
	for {
		rows = list(t, stack.backend, q)
		if len(rows) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(labPollStep)
	}
	if len(rows) == 0 {
		t.Fatalf("no %q log line for %s within %s of the %s fault; check that the lab ships its nginx logs into Elasticsearch",
			stack.sampleSource(), labFaultHost, labLogWait, labFault)
	}

	for _, r := range rows {
		if r.Ref.Kind != KindLog {
			t.Fatalf("row kind = %q, want %q", r.Ref.Kind, KindLog)
		}
		host, _ := r.Labels["host"]
		if host == "" {
			host = r.Str("host")
		}
		if !labWebHosts[resource.HostID(host)] {
			t.Fatalf("row host = %q, want a lab web host (the query was for %s)", host, labFaultHost)
		}
		if msg := r.Str("message"); strings.TrimSpace(msg) == "" {
			t.Fatalf("row %s has no message", r.Ref.Name)
		}
		at, ok := r.Fields["time"].(time.Time)
		if !ok {
			continue
		}
		if at.Before(from.Add(-time.Second)) || at.After(time.Now().Add(time.Minute)) {
			t.Fatalf("row %s time %v is outside the search window opened at %v", r.Ref.Name, at, from)
		}
	}
	t.Logf("lab: %d %q line(s) for %s through %q", len(rows), stack.sampleSource(), labFaultHost, labProfile)
}

// TestLabESTailFollowsFaultedWebHost proves `w` follows the same source: a tail
// started before the fault delivers the lines the fault writes, with a
// search_after cursor that never repeats a line.
func TestLabESTailFollowsFaultedWebHost(t *testing.T) {
	stack := loadLabStack(t)
	labCookie(t)

	ctx, cancel := context.WithTimeout(context.Background(), labLogWait+time.Minute)
	defer cancel()

	ch, err := stack.backend.Tail(ctx, TailOptions{Query: LogQuery{
		Cluster: stack.entry.Name,
		Source:  stack.sampleSource(),
		Hosts:   []resource.HostID{labFaultHost},
		Window:  resource.TimeWindow{From: time.Now().Add(-time.Minute), ToNow: true, Label: "-1m"},
	}})
	if err != nil {
		t.Fatalf("lab: tail: %v", err)
	}

	labtest.Fault(t, labFault)

	seen := map[string]bool{}
	var rows []LogRow
	deadline := time.After(labLogWait)
	for len(rows) == 0 {
		select {
		case res, ok := <-ch:
			if !ok {
				t.Fatalf("the tail closed without delivering a line for %s", labFaultHost)
			}
			if res.Err != nil {
				t.Fatalf("lab: tail poll: %v", res.Err)
			}
			for _, r := range res.Rows {
				key := r.Index + "/" + r.ID
				if seen[key] {
					t.Fatalf("the tail delivered %s twice; search_after must not repeat a hit", key)
				}
				seen[key] = true
				rows = append(rows, r)
			}
		case <-deadline:
			t.Fatalf("the tail delivered no line for %s within %s of the %s fault", labFaultHost, labLogWait, labFault)
		}
	}
	for _, r := range rows {
		if !labWebHosts[resource.HostID(r.Host)] {
			t.Fatalf("tail row host = %q, want a lab web host", r.Host)
		}
		if strings.TrimSpace(r.Message) == "" {
			t.Fatalf("tail row %s/%s has no message", r.Index, r.ID)
		}
	}
	t.Logf("lab: the tail delivered %d %q line(s) for %s", len(rows), stack.sampleSource(), labFaultHost)
}
