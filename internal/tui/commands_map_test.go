package tui

import (
	"strings"
	"testing"

	"jumpr/internal/svcmap"
)

func TestBuildHostMapFromSockets(t *testing.T) {
	web := parseSockets("web-01", strings.Join([]string{
		"addr 10.0.0.11",
		"listen 0.0.0.0:443 nginx nginx.service",
		"listen 127.0.0.53%lo:53 systemd-resolve systemd-resolved.service",
		"conn 10.0.0.11:443 198.51.100.7:51000 nginx nginx.service", // inbound, outside the inventory
		"conn 10.0.0.11:40112 10.0.0.21:5432 cart cart.service",     // out to db-01's postgres
		"conn 10.0.0.11:40113 10.0.0.21:5432 cart cart.service",     // a second connection, same edge
		"conn 10.0.0.11:40200 203.0.113.9:443 curl session-3.scope", // out to the world, not a service unit
		"garbage line",
	}, "\n"))
	db := parseSockets("db-01", strings.Join([]string{
		"addr 10.0.0.21",
		"listen [::ffff:10.0.0.21]:5432 postgres postgresql@16-main.service",
		"conn 10.0.0.21:5432 10.0.0.11:40112 postgres postgresql@16-main.service",
	}, "\n"))
	got := strings.Join(buildHostMap([]hostSockets{web, db}).Lines(false), "\n")
	for _, want := range []string{
		"Unit cart.service on web-01",
		"└─▶ Unit postgresql@16-main.service on db-01  :5432/TCP   [observed · socket · 2 forwarded]",
		"Process curl on web-01",
		"└─▶ External 203.0.113.9  :443/TCP",
		"1 inbound connection from addresses outside the inventory",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "nginx.service on web-01\n└") {
		t.Errorf("an inbound connection was drawn as an outbound edge:\n%s", got)
	}
}

func TestSocketOwnerWords(t *testing.T) {
	if v := socketOwner("h", "nginx", "nginx.service"); v.Kind != svcmap.Unit {
		t.Errorf("a service unit is a %s, want Unit", v.Kind)
	}
	if v := socketOwner("h", "bash", "session-1.scope"); v.Kind != svcmap.Process {
		t.Errorf("a scope is a %s, want Process", v.Kind)
	}
	if v := socketOwner("h", "-", "-"); v.Kind != svcmap.Host {
		t.Errorf("an unknown owner is a %s, want Host", v.Kind)
	}
}

func TestReachScriptQuotesAndValidates(t *testing.T) {
	if _, _, err := parseEndpoint("db;rm -rf /:5432"); err == nil {
		t.Fatal("a shell metacharacter in the name was accepted")
	}
	if _, _, err := parseEndpoint("db-01:70000"); err == nil {
		t.Fatal("port 70000 was accepted")
	}
	name, port, err := parseEndpoint("db-01.example.internal:5432")
	if err != nil || name != "db-01.example.internal" || port != 5432 {
		t.Fatalf("parseEndpoint = %q %d %v", name, port, err)
	}
	s := reachScript(name, port, true, true)
	for _, want := range []string{"T=db-01.example.internal;", "P=5432;", "openssl s_client", "https://db-01.example.internal:5432/"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}

func TestTraceVerdict(t *testing.T) {
	near := []string{"dns    ok    db is 10.0.0.21", "route  ok    10.0.0.21 dev eth0", "tcp    FAIL  10.0.0.21:5432 refused"}
	if v := traceVerdict("web", "db", 5432, near, []string{"listen FAIL  nothing listens on tcp/5432"}, nil); !strings.Contains(v, "nothing listens") {
		t.Errorf("verdict = %q", v)
	}
	if v := traceVerdict("web", "db", 5432, near, []string{"listen ok    0.0.0.0:5432 postgres"}, nil); !strings.Contains(v, "in between") {
		t.Errorf("verdict = %q", v)
	}
	if v := traceVerdict("web", "db", 5432, near, []string{"listen ok    127.0.0.1:5432 postgres", "listen ok    [::1]:5432 postgres"}, nil); !strings.Contains(v, "loopback only") {
		t.Errorf("verdict = %q", v)
	}
	if v := traceVerdict("web", "db", 5432, near[:2], nil, nil); !strings.Contains(v, "path is open") {
		t.Errorf("verdict = %q", v)
	}
}
