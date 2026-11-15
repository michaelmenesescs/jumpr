package transport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// stubBackend records which hosts it was asked about and answers with a
// marker so a test can tell which backend ran.
type stubBackend struct {
	name string
	err  error

	mu    sync.Mutex
	hosts []resource.HostID
}

func (s *stubBackend) Name() string { return s.name }

func (s *stubBackend) record(host resource.HostID) {
	s.mu.Lock()
	s.hosts = append(s.hosts, host)
	s.mu.Unlock()
}

func (s *stubBackend) seen() []resource.HostID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]resource.HostID(nil), s.hosts...)
}

func (s *stubBackend) Exec(_ context.Context, host resource.HostID, _ Command) (ExecResult, error) {
	s.record(host)
	res := ExecResult{Host: host, Via: s.name, RC: 0}
	if s.err != nil {
		return res, s.err
	}
	return res, nil
}

func (s *stubBackend) Start(_ context.Context, host resource.HostID, _ Command) (Stream, error) {
	s.record(host)
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

func (s *stubBackend) OpenShell(_ context.Context, host resource.HostID, _ PTYRequest) (Shell, error) {
	s.record(host)
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

func newStubs() (*stubBackend, *stubBackend) {
	return &stubBackend{name: "native"}, &stubBackend{name: "system-ssh"}
}

func TestRouterRoutesByGlob(t *testing.T) {
	native, fallback := newStubs()
	r, err := NewRouter(native, fallback, []RouteRule{
		{Match: "*.legacy.example.internal", Transport: SystemSSH},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	res, err := r.Exec(context.Background(), "db.legacy.example.internal", Command{Script: "true"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Via != "system-ssh" {
		t.Fatalf("via = %q, want system-ssh", res.Via)
	}
	res, err = r.Exec(context.Background(), "web.example.internal", Command{Script: "true"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Via != "native" {
		t.Fatalf("via = %q, want native", res.Via)
	}
	if got := fallback.seen(); len(got) != 1 || got[0] != "db.legacy.example.internal" {
		t.Fatalf("fallback saw %v", got)
	}
	if got := native.seen(); len(got) != 1 || got[0] != "web.example.internal" {
		t.Fatalf("native saw %v", got)
	}
}

func TestRouterLastMatchWins(t *testing.T) {
	native, fallback := newStubs()
	r, err := NewRouter(native, fallback, []RouteRule{
		{Match: "*.example.internal", Transport: SystemSSH},
		{Match: "keep.example.internal", Transport: Native},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	for host, want := range map[resource.HostID]string{
		"other.example.internal": "system-ssh",
		"keep.example.internal":  "native",
	} {
		res, err := r.Exec(context.Background(), host, Command{Script: "true"})
		if err != nil {
			t.Fatalf("Exec(%s): %v", host, err)
		}
		if res.Via != want {
			t.Fatalf("%s went via %q, want %q", host, res.Via, want)
		}
	}
}

func TestRouterStartAndShellRoute(t *testing.T) {
	native, fallback := newStubs()
	r, err := NewRouter(native, fallback, []RouteRule{{Match: "legacy-*", Transport: SystemSSH}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if _, err := r.Start(context.Background(), "legacy-1", Command{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.OpenShell(context.Background(), "legacy-2", PTYRequest{}); err != nil {
		t.Fatalf("OpenShell: %v", err)
	}
	if got := fallback.seen(); len(got) != 2 {
		t.Fatalf("fallback saw %v, want both legacy hosts", got)
	}
	if got := native.seen(); len(got) != 0 {
		t.Fatalf("native saw %v, want nothing", got)
	}
}

func TestRouterNameAndVia(t *testing.T) {
	native, fallback := newStubs()
	r, err := NewRouter(native, fallback, []RouteRule{{Match: "legacy-*", Transport: SystemSSH}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if got := r.Name(); got != "router" {
		t.Fatalf("Name = %q, want router", got)
	}
	if got := r.Via("legacy-1"); got != SystemSSH {
		t.Fatalf("Via(legacy-1) = %q, want system-ssh", got)
	}
	if got := r.Via("web-1"); got != Native {
		t.Fatalf("Via(web-1) = %q, want native", got)
	}
}

func TestRouterNewErrors(t *testing.T) {
	native, fallback := newStubs()
	tests := []struct {
		name     string
		native   RouterBackend
		fallback RouterBackend
		rules    []RouteRule
	}{
		{"no native", nil, fallback, nil},
		{"no fallback", native, nil, []RouteRule{{Match: "h", Transport: SystemSSH}}},
		{"empty match", native, fallback, []RouteRule{{Match: " ", Transport: SystemSSH}}},
		{"bad glob", native, fallback, []RouteRule{{Match: "[", Transport: SystemSSH}}},
		{"unknown transport", native, fallback, []RouteRule{{Match: "h", Transport: "salt"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRouter(tt.native, tt.fallback, tt.rules); errs.KindOf(err) != errs.Config {
				t.Fatalf("err = %v, want a config error", err)
			}
		})
	}
}

func TestRouterSuggestsSystemSSHStanza(t *testing.T) {
	native, fallback := newStubs()
	native.err = errs.New(errs.Config, "",
		"ProxyCommand is not supported by the native transport; use transport: system-ssh for proxied")
	r, err := NewRouter(native, fallback, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	_, err = r.Exec(context.Background(), "proxied", Command{Script: "true"})
	if errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %s, want config (%v)", errs.KindOf(err), err)
	}
	msg := err.Error()
	for _, want := range []string{"transports:", `match: "proxied"`, "transport: system-ssh"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message lacks %q:\n%s", want, msg)
		}
	}
	// The native backend's error is copied, not mutated.
	if strings.Contains(native.err.Error(), "transports:") {
		t.Fatalf("the backend's error was mutated: %v", native.err)
	}
	// Asking twice does not stack stanzas.
	_, err = r.Exec(context.Background(), "proxied", Command{Script: "true"})
	if n := strings.Count(err.Error(), "ssh:\n  transports:"); n != 1 {
		t.Fatalf("stanza appears %d times:\n%s", n, err)
	}
}

func TestRouterLeavesOtherErrorsAlone(t *testing.T) {
	native, fallback := newStubs()
	r, err := NewRouter(native, fallback, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	plain := errors.New("native exploded")
	native.err = plain
	if _, got := r.Exec(context.Background(), "web-1", Command{}); got != plain {
		t.Fatalf("err = %v, want the native error untouched", got)
	}
	config := errs.New(errs.Config, "ssh.dial", "unrelated")
	native.err = config
	if _, got := r.Exec(context.Background(), "web-1", Command{}); got != config {
		t.Fatalf("err = %v, want the config error untouched", got)
	}

	// A fallback error is never annotated.
	fallback.err = config
	r2, err := NewRouter(native, fallback, []RouteRule{{Match: "*", Transport: SystemSSH}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if _, got := r2.Exec(context.Background(), "web-1", Command{}); got != config {
		t.Fatalf("err = %v, want the fallback error untouched", got)
	}
}
