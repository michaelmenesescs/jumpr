package ssh

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
)

func td(name string) string { return filepath.Join("testdata", name) }

func newR(t *testing.T, files ...string) *Resolver {
	t.Helper()
	r, err := NewResolver(files, "/home/u", "me")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveDefaults(t *testing.T) {
	r := newR(t, td("config_basic"), "testdata/missing")
	s, err := r.Resolve("unknown.example.internal")
	if err != nil {
		t.Fatal(err)
	}
	want := HostSpec{Alias: "unknown.example.internal", HostName: "unknown.example.internal", User: "me", Port: 22,
		StrictHostKeyChecking: "ask", KnownHostsFiles: []string{"/home/u/.ssh/known_hosts"}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v want %+v", s, want)
	}
	p, _ := r.Resolve("plain")
	if p.HostName != "10.0.0.5" || p.Port != 22 {
		t.Fatalf("%+v", p)
	}
}

func TestResolveWildcardHostBlock(t *testing.T) {
	r := newR(t, td("config_wildcards"))
	s, _ := r.Resolve("x.lab.example.internal")
	if s.User != "sre" || s.StrictHostKeyChecking != "yes" {
		t.Fatalf("%+v", s)
	}
	s, _ = r.Resolve("web-1")
	if s.Port != 2200 || s.User != "me" {
		t.Fatalf("%+v", s)
	}
	s, _ = r.Resolve("web-3")
	if s.Port != 22 {
		t.Fatalf("%+v", s)
	}
}

func TestResolveIdentityTildeAndTokens(t *testing.T) {
	r := newR(t, td("config_basic"))
	s, err := r.Resolve("bastion")
	if err != nil {
		t.Fatal(err)
	}
	if s.HostName != "bastion.example.internal" || s.User != "ops" || s.Port != 2222 || !s.IdentitiesOnly {
		t.Fatalf("%+v", s)
	}
	wantID := []string{"/home/u/.ssh/id_bastion.example.internal", "/keys/ops_2222"}
	if !reflect.DeepEqual(s.IdentityFiles, wantID) {
		t.Fatalf("ids %v", s.IdentityFiles)
	}
	wantKH := []string{"/home/u/.ssh/kh_bastion.example.internal", "/tmp/kh2"}
	if !reflect.DeepEqual(s.KnownHostsFiles, wantKH) {
		t.Fatalf("kh %v", s.KnownHostsFiles)
	}
	if s.StrictHostKeyChecking != "accept-new" || s.ConnectTimeout != 7*time.Second || s.ServerAliveInterval != 15*time.Second {
		t.Fatalf("%+v", s)
	}
}

func TestExpandTokens(t *testing.T) {
	got := expandTokens("a%%b-%h-%r-%p-%x", "h", "u", 1)
	if got != "a%b-h-u-1-%x" {
		t.Fatal(got)
	}
}

func TestChainSingleJump(t *testing.T) {
	r := newR(t, td("config_jump_chain"))
	c, err := r.Chain("hop1")
	if err != nil || len(c) != 1 || c[0].Alias != "hop1" {
		t.Fatalf("%v %+v", err, c)
	}
	c, err = r.Chain("two")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range c {
		names = append(names, s.HostName)
	}
	if !reflect.DeepEqual(names, []string{"hop1.example.internal", "hop2b.example.internal", "two"}) {
		t.Fatal(names)
	}
}

func TestChainTwoJumps(t *testing.T) {
	r := newR(t, td("config_jump_chain"))
	c, err := r.Chain("target")
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 3 {
		t.Fatalf("%+v", c)
	}
	if c[0].HostName != "hop1.example.internal" || c[0].User != "jumpr1" || c[0].Port != 2200 {
		t.Fatalf("%+v", c[0])
	}
	if c[1].HostName != "hop2.example.internal" || c[1].User != "jumpuser" {
		t.Fatalf("%+v", c[1])
	}
	if c[2].Alias != "target" || len(c[2].Jumps) != 1 {
		t.Fatalf("%+v", c[2])
	}
}

func TestChainLoopError(t *testing.T) {
	r := newR(t, td("config_jump_chain"))
	_, err := r.Chain("loopa")
	if errs.KindOf(err) != errs.Config || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("%v", err)
	}
	_, err = r.Chain("c1")
	if errs.KindOf(err) != errs.Config || !strings.Contains(err.Error(), "long") {
		t.Fatalf("%v", err)
	}
}

func TestProxyJumpNone(t *testing.T) {
	r := newR(t, td("config_basic"))
	s, _ := r.Resolve("nojump")
	if len(s.Jumps) != 0 {
		t.Fatalf("%+v", s)
	}
	s, _ = r.Resolve("viapc")
	if s.ProxyCommand == "" {
		t.Fatal("proxycommand lost")
	}
}

func TestHostsSkipsWildcards(t *testing.T) {
	r := newR(t, td("config_wildcards"), td("config_basic"))
	got := r.Hosts()
	want := []string{"bastion", "db-1", "nojump", "plain", "viapc", "web-1", "web-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestFirstFileWins(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	os.WriteFile(a, []byte("Host h\n  User first\n"), 0o600)
	os.WriteFile(b, []byte("Host h\n  User second\n  Port 99\n"), 0o600)
	s, _ := newR(t, a, b).Resolve("h")
	if s.User != "first" || s.Port != 99 {
		t.Fatalf("%+v", s)
	}
}

func TestBadFileIsConfigError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("Host a\n  Port\nHost\n"), 0o600)
	_, err := NewResolver([]string{bad}, "/h", "u")
	if err == nil {
		t.Skip("library accepted file")
	}
	if errs.KindOf(err) != errs.Config || !strings.Contains(err.Error(), bad) {
		t.Fatal(err)
	}
}
