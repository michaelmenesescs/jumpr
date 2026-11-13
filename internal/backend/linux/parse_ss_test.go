package linux

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const ssSamples = "" +
	"tcp   LISTEN 0      511          0.0.0.0:443        0.0.0.0:*    users:((\"nginx\",pid=2010,fd=6),(\"nginx\",pid=2014,fd=6))\n" +
	"tcp   LISTEN 0      128             [::]:22            [::]:*    users:((\"sshd\",pid=1290,fd=4))\n" +
	"udp   UNCONN 0      0      127.0.0.53%lo:53         0.0.0.0:*    users:((\"systemd-resolve\",pid=612,fd=13))\n" +
	"tcp   ESTAB  0      0         10.20.4.17:22      10.20.0.5:51122 users:((\"sshd\",pid=88901,fd=4))\n" +
	"tcp   LISTEN 0      4096               *:9100             *:*\n"

func ssReadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "linux", "ss", "rocky9", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func TestParseSSSamples(t *testing.T) {
	socks, skipped := ParseSS(ssSamples)
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(socks) != 5 {
		t.Fatalf("sockets = %d, want 5", len(socks))
	}

	https := socks[0]
	if https.Proto != "tcp" || https.State != "LISTEN" || https.RecvQ != 0 || https.SendQ != 511 {
		t.Errorf("https = %+v", https)
	}
	if https.LocalAddr != "0.0.0.0" || https.LocalPort != "443" {
		t.Errorf("https local = %q:%q", https.LocalAddr, https.LocalPort)
	}
	if https.PeerAddr != "0.0.0.0" || https.PeerPort != "*" {
		t.Errorf("https peer = %q:%q", https.PeerAddr, https.PeerPort)
	}
	wantProcs := []SockProc{{Name: "nginx", PID: 2010, FD: 6}, {Name: "nginx", PID: 2014, FD: 6}}
	if !reflect.DeepEqual(https.Procs, wantProcs) {
		t.Errorf("https procs = %+v, want %+v", https.Procs, wantProcs)
	}
	if !https.Listening() {
		t.Errorf("https should be listening")
	}

	ssh := socks[1]
	if ssh.LocalAddr != "[::]" || ssh.LocalPort != "22" {
		t.Errorf("ipv6 local = %q:%q", ssh.LocalAddr, ssh.LocalPort)
	}
	if len(ssh.Procs) != 1 || ssh.Procs[0] != (SockProc{Name: "sshd", PID: 1290, FD: 4}) {
		t.Errorf("ipv6 procs = %+v", ssh.Procs)
	}

	dns := socks[2]
	if dns.Proto != "udp" || dns.State != "UNCONN" {
		t.Errorf("dns = %+v", dns)
	}
	if dns.LocalAddr != "127.0.0.53%lo" || dns.LocalPort != "53" {
		t.Errorf("scoped local = %q:%q", dns.LocalAddr, dns.LocalPort)
	}
	if !dns.Listening() {
		t.Errorf("udp UNCONN should be listening")
	}

	estab := socks[3]
	if estab.State != "ESTAB" || estab.LocalPort != "22" || estab.PeerPort != "51122" {
		t.Errorf("estab = %+v", estab)
	}
	if estab.Listening() {
		t.Errorf("ESTAB must not be listening")
	}

	noProcs := socks[4]
	if noProcs.LocalAddr != "*" || noProcs.LocalPort != "9100" || noProcs.PeerAddr != "*" || noProcs.PeerPort != "*" {
		t.Errorf("no-procs addresses = %+v", noProcs)
	}
	if noProcs.Procs != nil {
		t.Errorf("no-procs Procs = %+v, want nil", noProcs.Procs)
	}
}

func TestParseSSFixture(t *testing.T) {
	socks, skipped := ParseSS(ssReadFixture(t, "ss.txt"))
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(socks) != 5 {
		t.Fatalf("sockets = %d, want 5", len(socks))
	}
	got := ListenersOnPort(socks, "22")
	if len(got) != 1 || got[0].Proto != "tcp" || got[0].LocalAddr != "[::]" {
		t.Fatalf("listeners on 22 = %+v", got)
	}
	if got := ListenersOnPort(socks, "53"); len(got) != 1 || got[0].Proto != "udp" {
		t.Fatalf("listeners on 53 = %+v", got)
	}
	if got := ListenersOnPort(socks, "51122"); len(got) != 0 {
		t.Fatalf("51122 is a peer port, got %+v", got)
	}
}

func TestParseSSGarbageDoesNotPanic(t *testing.T) {
	for _, in := range []string{"", "\n", "tcp LISTEN", "tcp LISTEN x y 1.2.3.4:1 1.2.3.4:2", "users:((\"x\",pid=bad,fd=1))"} {
		socks, skipped := ParseSS(in)
		if skipped < 0 {
			t.Errorf("input %q gave negative skipped count", in)
		}
		for _, s := range socks {
			if s.LocalPort == "" && s.LocalAddr == "" {
				t.Errorf("input %q produced an empty socket: %+v", in, s)
			}
		}
	}
}

func TestSocketListening(t *testing.T) {
	cases := []struct {
		sock Socket
		want bool
	}{
		{Socket{Proto: "tcp", State: "LISTEN"}, true},
		{Socket{Proto: "tcp6", State: "LISTEN"}, true},
		{Socket{Proto: "udp", State: "UNCONN"}, true},
		{Socket{Proto: "udp6", State: "UNCONN"}, true},
		{Socket{Proto: "tcp", State: "UNCONN"}, false},
		{Socket{Proto: "tcp", State: "ESTAB"}, false},
		{Socket{Proto: "udp", State: "ESTAB"}, false},
		{Socket{}, false},
	}
	for _, c := range cases {
		if got := c.sock.Listening(); got != c.want {
			t.Errorf("%+v.Listening() = %v, want %v", c.sock, got, c.want)
		}
	}
}

func TestListenersOnPort(t *testing.T) {
	socks := []Socket{
		{Proto: "tcp", State: "LISTEN", LocalPort: "8080"},
		{Proto: "tcp", State: "ESTAB", LocalPort: "8080"},
		{Proto: "udp", State: "UNCONN", LocalPort: "8080"},
		{Proto: "tcp", State: "LISTEN", LocalPort: "9090"},
	}
	got := ListenersOnPort(socks, "8080")
	if len(got) != 2 {
		t.Fatalf("got %d listeners, want 2: %+v", len(got), got)
	}
	if got[0].Proto != "tcp" || got[1].Proto != "udp" {
		t.Errorf("unexpected order/content: %+v", got)
	}
	if got := ListenersOnPort(socks, "1"); len(got) != 0 {
		t.Errorf("partial port match, got %+v", got)
	}
	if got := ListenersOnPort(nil, "8080"); got != nil {
		t.Errorf("nil input gave %+v", got)
	}
}
